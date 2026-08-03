package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Credential-pool endpoints — two audiences, deliberately separate.
//
//   - /api/me/pool/… belongs to the DONOR. It is where someone lends their
//     subscription, sets the ceilings, watches what their quota served, and
//     switches it off. Everything here is scoped to the caller's own user
//     id; a donor can never read or touch another's pledge.
//   - /api/teams/{id}/pool belongs to the OPERATOR: the pool's existence,
//     its audience policy, and the roster of who is lending.
//
// A donor's credential itself is never exposed by any of these — it stays
// sealed in pkg/secrets and is only ever unsealed into a run bundle.
func (s *Server) registerCredPoolRoutes() {
	s.mux.Handle("GET /api/me/pool", s.requireAuth(http.HandlerFunc(s.handleGetMyPledges)))
	s.mux.Handle("PUT /api/me/pool/{kind}", s.requireAuth(http.HandlerFunc(s.handlePutMyPledge)))
	s.mux.Handle("DELETE /api/me/pool/{kind}", s.requireAuth(http.HandlerFunc(s.handleDeleteMyPledge)))
	s.mux.Handle("GET /api/me/pool/history", s.requireAuth(http.HandlerFunc(s.handleMyPoolHistory)))

	s.mux.Handle("GET /api/teams/{id}/pool", s.requireAuth(http.HandlerFunc(s.handleGetTeamPool)))
	s.mux.Handle("PUT /api/teams/{id}/pool", s.requireAuth(http.HandlerFunc(s.handlePutTeamPool)))
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// pledgeView is what a donor sees about their own contribution: the terms
// they set, the live state, and what has been drawn today and this week.
type pledgeView struct {
	Kind string `json:"kind"`
	// Connected reports whether the donor still has a credential of this
	// kind connected. A pledge without one is inert — say so rather than
	// showing an "active" contribution that can never be served.
	Connected bool             `json:"connected"`
	Enabled   bool             `json:"enabled"`
	Status    string           `json:"status"`
	Limits    credpool.Limits  `json:"limits"`
	Window    *credpool.Window `json:"window,omitempty"`
	Bots      []string         `json:"bots,omitempty"`
	Health    string           `json:"health"`
	// HealthDetail tells the donor what to do about an unhealthy pledge.
	HealthDetail  string  `json:"health_detail,omitempty"`
	CooldownUntil *string `json:"cooldown_until,omitempty"`
	LastServedAt  *string `json:"last_served_at,omitempty"`
	// Usage figures are ESTIMATES (see pkg/credpool's doc): on a
	// subscription the provider bills nothing per call, so spend is derived
	// from token counts. The UI must present them as approximate.
	Today     usageView `json:"today"`
	ThisWeek  usageView `json:"this_week"`
	UpdatedAt string    `json:"updated_at,omitempty"`
}

type usageView struct {
	Runs         int     `json:"runs"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// leaseView is one entry of a donor's history: which bot ran, for which
// team, and what it drew. The requester is named because accountability is
// the counterpart of the trust a donor extends.
type leaseView struct {
	RunID       string  `json:"run_id"`
	BotID       string  `json:"bot_id,omitempty"`
	TenantID    string  `json:"tenant_id,omitempty"`
	RequesterID string  `json:"requester_id,omitempty"`
	CostUSD     float64 `json:"cost_usd"`
	Outcome     string  `json:"outcome,omitempty"`
	Closed      bool    `json:"closed"`
	AcquiredAt  string  `json:"acquired_at"`
}

// poolView is the operator's read of a pool: its policy plus the roster,
// with each donor's live state. Donors are identified but their limits are
// their own business — the operator sees availability, not terms.
type poolView struct {
	ID       string            `json:"id"`
	OrgID    string            `json:"org_id"`
	Name     string            `json:"name,omitempty"`
	Enabled  bool              `json:"enabled"`
	Audience credpool.Audience `json:"audience"`
	Donors   []donorView       `json:"donors"`
}

type donorView struct {
	UserID        string  `json:"user_id"`
	Kind          string  `json:"kind"`
	Status        string  `json:"status"`
	Health        string  `json:"health"`
	CooldownUntil *string `json:"cooldown_until,omitempty"`
	LastServedAt  *string `json:"last_served_at,omitempty"`
	TodayRuns     int     `json:"today_runs"`
	TodayCostUSD  float64 `json:"today_cost_usd"`
}

// ---------------------------------------------------------------------------
// Donor endpoints (/api/me/pool)
// ---------------------------------------------------------------------------

func (s *Server) handleGetMyPledges(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	pledges, err := s.credPoolPledges.ListByUser(r.Context(), id.UserID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	now := time.Now().UTC()
	connected := s.connectedKinds(r, id.UserID)
	views := make([]pledgeView, 0, len(pledges))
	for _, p := range pledges {
		views = append(views, s.toPledgeView(r, p, now, connected[p.Kind]))
	}
	writeJSON(w, struct {
		Pledges []pledgeView `json:"pledges"`
		// PoolID is the pool a new pledge would join, so the UI can tell a
		// user whether lending is even available to them.
		PoolID string `json:"pool_id,omitempty"`
	}{Pledges: views, PoolID: s.poolIDForUser(r)})
}

// pledgeRequest is the donor-supplied body. Every limit is optional; zero
// means "no limit on this axis", which is a deliberate choice the UI must
// make legible rather than a default we invent for them.
type pledgeRequest struct {
	Enabled *bool            `json:"enabled"`
	Limits  credpool.Limits  `json:"limits"`
	Window  *credpool.Window `json:"window"`
	Bots    []string         `json:"bots"`
}

func (s *Server) handlePutMyPledge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	kind := secrets.OAuthKind(r.PathValue("kind"))
	if !kind.Valid() {
		httpError(w, http.StatusBadRequest, "unknown credential kind %q", kind)
		return
	}
	var req pledgeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	// Lending requires actually having connected the subscription. Refusing
	// here — rather than storing an inert pledge — is what stops a donor
	// believing they are contributing when they are not.
	rec, err := s.oauthStore.Get(r.Context(), id.UserID, kind)
	if err != nil {
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			httpError(w, http.StatusConflict, "connect your %s subscription before sharing it", kind)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	poolID := s.poolIDForUser(r)
	if poolID == "" {
		httpError(w, http.StatusNotFound, "no credential pool accepts contributions on this instance")
		return
	}

	pledgeID := credpool.PledgeID(id.UserID, string(kind))
	p, err := s.credPoolPledges.Get(r.Context(), pledgeID)
	if err != nil && !errors.Is(err, credpool.ErrNotFound) {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	p.ID, p.PoolID, p.UserID, p.Kind = pledgeID, poolID, id.UserID, string(kind)
	p.Limits, p.Window, p.Bots = req.Limits, req.Window, req.Bots
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	if reconnected(p, rec) {
		p.Health, p.HealthDetail, p.ConsecutiveAuthFailures = credpool.HealthOK, "", 0
	} else {
		s.logger.Info("credential pool: %s updated terms but their %s credential is still parked (%s)", id.UserID, kind, p.Health)
	}
	if err := p.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.credPoolPledges.Upsert(r.Context(), p); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("credential pool: %s %s their %s subscription", id.UserID, enabledVerb(p.Enabled), kind)
	// The connection was just verified above; don't re-read it.
	writeJSON(w, s.toPledgeView(r, p, time.Now().UTC(), true))
}

// reconnected reports whether a pledge's parked state may be cleared: the
// credential must have been RE-CONNECTED since the pledge was parked.
//
// The signal is CreatedAt, not UpdatedAt. Only sealOAuthRecord — the
// connect and paste paths — stamps CreatedAt; UpdatedAt is bumped every
// time the background worker rotates the access token, roughly hourly. A
// provider can keep refreshing tokens for a subscription it has otherwise
// revoked, so trusting UpdatedAt would let a dead credential un-park itself
// on the next tick and burn another round of borrowers' runs discovering
// the 401 — indefinitely.
//
// A healthy pledge is always "clear".
func reconnected(p credpool.Pledge, rec secrets.OAuthRecord) bool {
	if p.Health == credpool.HealthOK || p.Health == "" {
		return true
	}
	return rec.CreatedAt.After(p.UpdatedAt)
}

func enabledVerb(enabled bool) string {
	if enabled {
		return "is sharing"
	}
	return "paused sharing of"
}

func (s *Server) handleDeleteMyPledge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	kind := r.PathValue("kind")
	if err := s.credPoolPledges.Delete(r.Context(), credpool.PledgeID(id.UserID, kind)); err != nil {
		if errors.Is(err, credpool.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("credential pool: %s withdrew their %s contribution", id.UserID, kind)
	w.WriteHeader(http.StatusNoContent)
}

// handleMyPoolHistory answers "what did my quota actually run". Without
// this the donation is a blank cheque; with it, lending stays an informed
// decision the donor can revisit.
func (s *Server) handleMyPoolHistory(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	leases, err := s.credPoolLeases.ListByDonor(r.Context(), id.UserID, 100)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]leaseView, 0, len(leases))
	for _, l := range leases {
		views = append(views, leaseView{
			RunID:       l.RunID,
			BotID:       l.BotID,
			TenantID:    l.TenantID,
			RequesterID: l.RequesterID,
			CostUSD:     l.CostUSD,
			Outcome:     l.Outcome,
			Closed:      l.Closed,
			AcquiredAt:  l.AcquiredAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, struct {
		Leases []leaseView `json:"leases"`
	}{Leases: views})
}

// poolIDForUser resolves the pool a caller's contribution joins: the one
// owned by their ACTIVE ORG, and nothing else.
//
// There is deliberately no "the instance has only one pool, use that"
// fallback. On a multi-tenant instance that would attach a contributor's
// personal subscription to a stranger's org the moment their own org had no
// pool — a donation made to someone they never chose, which no amount of
// later configuration can retroactively consent to. "" means the UI says so
// plainly instead.
func (s *Server) poolIDForUser(r *http.Request) string {
	id, _ := auth.FromContext(r.Context())
	if id.OrgID == "" {
		return ""
	}
	p, err := s.credPoolPools.GetByOrg(r.Context(), id.OrgID)
	if err != nil {
		if !errors.Is(err, credpool.ErrNotFound) {
			s.logger.Warn("credential pool: resolve pool of org %s: %v", id.OrgID, err)
		}
		return ""
	}
	return p.ID
}

// connectedKinds reports which credential kinds a user still has
// connected, in ONE read — a per-pledge lookup would be a query per row of
// the donor's own dashboard.
func (s *Server) connectedKinds(r *http.Request, userID string) map[string]bool {
	out := map[string]bool{}
	records, err := s.oauthStore.ListByUser(r.Context(), userID)
	if err != nil {
		s.logger.Warn("credential pool: list oauth connections of %s: %v", userID, err)
		return out
	}
	for _, rec := range records {
		out[string(rec.Kind)] = true
	}
	return out
}

func (s *Server) toPledgeView(r *http.Request, p credpool.Pledge, now time.Time, connected bool) pledgeView {
	v := pledgeView{
		Kind:          p.Kind,
		Enabled:       p.Enabled,
		Limits:        p.Limits,
		Window:        p.Window,
		Bots:          p.Bots,
		Health:        string(p.Health),
		HealthDetail:  p.HealthDetail,
		CooldownUntil: optRFC3339(p.CooldownUntil),
		LastServedAt:  optRFC3339(p.LastServedAt),
	}
	if !p.UpdatedAt.IsZero() {
		v.UpdatedAt = p.UpdatedAt.Format(time.RFC3339)
	}
	v.Connected = connected
	_, status := p.Available(now, "")
	v.Status = string(status)
	if day, week, err := s.credPoolLedger.Usage(r.Context(), p.ID, now); err == nil {
		v.Today = toUsageView(day)
		v.ThisWeek = toUsageView(week)
		// A donor at their ceiling reads "exhausted", not "active" — the
		// ledger, not the pledge, holds that truth. Asked through the SAME
		// inputs the ledger admits with, INCLUDING the allowance already
		// promised to in-flight runs: without that term a donor whose
		// remaining budget is fully committed would read "Sharing" while
		// every new launch is refused.
		committed := 0.0
		if s.credPoolLeases != nil {
			if _, c, cerr := s.credPoolLeases.LiveCommitment(r.Context(), p.ID, "", now); cerr == nil {
				committed = c
			}
		}
		if status == credpool.StatusActive &&
			p.Limits.Deny(day.Runs+1, day.CostUSD+committed, week.CostUSD+committed) != credpool.DenyNone {
			v.Status = string(credpool.StatusExhausted)
		}
	}
	return v
}

func toUsageView(u credpool.Usage) usageView {
	return usageView{Runs: u.Runs, CostUSD: u.CostUSD, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
}

// ---------------------------------------------------------------------------
// Operator endpoints (/api/teams/{id}/pool)
// ---------------------------------------------------------------------------

func (s *Server) handleGetTeamPool(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	orgID := s.orgIDForPoolTeam(r, teamID)
	if orgID == "" {
		httpError(w, http.StatusNotFound, "this team has no parent org to own a pool")
		return
	}
	// Who lends, and how much they have given, is the contributors' own
	// business: an ordinary member sees the policy, not the roster.
	withDonors := s.canManageOrg(r.Context(), id, orgID)
	pool, err := s.credPoolPools.GetByOrg(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, credpool.ErrNotFound) {
			// Not an error: describe the pool that WOULD be created, so the
			// UI renders the same form for the first save as for later ones.
			writeJSON(w, poolView{OrgID: orgID, Donors: []donorView{}})
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, s.toPoolView(r, pool, withDonors))
}

type poolRequest struct {
	Name     *string            `json:"name"`
	Enabled  *bool              `json:"enabled"`
	Audience *credpool.Audience `json:"audience"`
}

func (s *Server) handlePutTeamPool(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	orgID := s.orgIDForPoolTeam(r, teamID)
	if orgID == "" {
		httpError(w, http.StatusNotFound, "this team has no parent org to own a pool")
		return
	}
	// The pool document is keyed by ORG, so this route mutates policy for
	// every team under it. Gating on the addressed team would let an admin
	// of any one team widen who may spend the whole org's contributed
	// subscriptions — a decision that belongs to the org.
	if !s.canManageOrg(r.Context(), id, orgID) {
		httpError(w, http.StatusForbidden, "changing the credential pool is an org-level decision")
		return
	}
	var req poolRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	pool, err := s.credPoolPools.GetByOrg(r.Context(), orgID)
	if err != nil {
		if !errors.Is(err, credpool.ErrNotFound) {
			httpError(w, http.StatusInternalServerError, "%s", err.Error())
			return
		}
		pool = credpool.Pool{ID: orgID, OrgID: orgID}
	}
	if req.Name != nil {
		pool.Name = strings.TrimSpace(*req.Name)
	}
	if req.Enabled != nil {
		pool.Enabled = *req.Enabled
	}
	if req.Audience != nil {
		pool.Audience = *req.Audience
	}
	if err := s.credPoolPools.Upsert(r.Context(), pool); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// Widening who may spend contributors' subscriptions is exactly the
	// kind of decision that must leave a trace — recorded on the ORG, since
	// that is who the pool belongs to and who must be able to see it.
	s.auditOrg(r, orgID, "credpool.updated", "cred_pool", pool.ID, map[string]any{
		"enabled":      pool.Enabled,
		"all_teams":    pool.Audience.AllTeams,
		"contributors": pool.Audience.Contributors,
		"teams":        len(pool.Audience.Teams),
		"orgs":         len(pool.Audience.Orgs),
	})
	writeJSON(w, s.toPoolView(r, pool, true))
}

// orgIDForPoolTeam resolves the org that owns a team's pool, preferring
// the JWT's active org (no extra read) and falling back to the team row.
func (s *Server) orgIDForPoolTeam(r *http.Request, teamID string) string {
	id, _ := auth.FromContext(r.Context())
	if id.OrgID != "" && id.TeamID == teamID {
		return id.OrgID
	}
	st := s.authStore()
	if st == nil {
		return ""
	}
	t, err := st.GetTeam(r.Context(), teamID)
	if err != nil {
		return ""
	}
	return t.OrgID
}

func (s *Server) toPoolView(r *http.Request, pool credpool.Pool, withDonors bool) poolView {
	v := poolView{
		ID: pool.ID, OrgID: pool.OrgID, Name: pool.Name,
		Enabled: pool.Enabled, Audience: pool.Audience,
		Donors: []donorView{},
	}
	if !withDonors {
		return v
	}
	pledges, err := s.credPoolPledges.ListByPool(r.Context(), pool.ID)
	if err != nil {
		s.logger.Warn("credential pool: list donors of %s: %v", pool.ID, err)
		return v
	}
	now := time.Now().UTC()
	ids := make([]string, 0, len(pledges))
	for _, p := range pledges {
		ids = append(ids, p.ID)
	}
	usage, err := s.credPoolLedger.UsageMany(r.Context(), ids, now)
	if err != nil {
		s.logger.Warn("credential pool: donor usage of %s: %v", pool.ID, err)
	}
	for _, p := range pledges {
		_, status := p.Available(now, "")
		u := usage[p.ID]
		v.Donors = append(v.Donors, donorView{
			UserID:        p.UserID,
			Kind:          p.Kind,
			Status:        string(status),
			Health:        string(p.Health),
			CooldownUntil: optRFC3339(p.CooldownUntil),
			LastServedAt:  optRFC3339(p.LastServedAt),
			TodayRuns:     u.Runs,
			TodayCostUSD:  u.CostUSD,
		})
	}
	return v
}
