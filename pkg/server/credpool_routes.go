package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	s.mux.Handle("PUT /api/me/pool/{source}/{ref}", s.requireAuth(http.HandlerFunc(s.handlePutMyPledge)))
	s.mux.Handle("DELETE /api/me/pool/{source}/{ref}", s.requireAuth(http.HandlerFunc(s.handleDeleteMyPledge)))
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
	Source string `json:"source"`
	Ref    string `json:"ref"`
	KeyID  string `json:"key_id,omitempty"`
	// Metered is true when spending this credential costs the lender real
	// money per token, rather than drawing on a plan they already pay for.
	// The UI must stop calling the figures estimates when it is set.
	Metered bool `json:"metered"`
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
	Source        string  `json:"source"`
	Ref           string  `json:"ref"`
	Metered       bool    `json:"metered"`
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
		views = append(views, s.toPledgeView(r, p, now, s.stillHolds(r, p, connected)))
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
	// KeyID names WHICH of the donor's API keys is lent. Required for an
	// api_key pledge: a donor may hold several per provider and chooses
	// deliberately rather than letting a resolver pick.
	KeyID string `json:"key_id"`
}

func (s *Server) handlePutMyPledge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	cred := credpool.Credential{
		Source: credpool.CredentialSource(r.PathValue("source")),
		Ref:    r.PathValue("ref"),
	}
	if !cred.Source.Valid() {
		httpError(w, http.StatusBadRequest, "unknown credential source %q (want oauth|api_key)", cred.Source)
		return
	}
	var req pledgeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: %v", err)
		return
	}
	cred.KeyID = strings.TrimSpace(req.KeyID)

	// Lending requires actually holding the credential. Refusing here —
	// rather than storing an inert pledge — is what stops a donor believing
	// they are contributing when they are not.
	rec, cerr := s.verifyLendable(r, id.UserID, cred)
	if cerr != nil {
		httpError(w, cerr.status, "%s", cerr.msg)
		return
	}
	poolID := s.poolIDForUser(r)
	if poolID == "" {
		// Name the org: the pool is per-org, so "this instance" would blame
		// the wrong thing for a contributor whose active org simply runs no
		// pool while another one does.
		httpError(w, http.StatusNotFound,
			"your active organisation runs no credential pool, so there is nothing to contribute to — an owner enables one under the team's Credential pool settings")
		return
	}

	pledgeID := credpool.PledgeID(id.UserID, cred.Source, cred.Ref)
	p, err := s.credPoolPledges.Get(r.Context(), pledgeID)
	if err != nil && !errors.Is(err, credpool.ErrNotFound) {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	p.ID, p.PoolID, p.UserID, p.Credential = pledgeID, poolID, id.UserID, cred
	p.Limits, p.Window, p.Bots = req.Limits, req.Window, req.Bots
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	if reconnected(p, rec) {
		p.Health, p.HealthDetail, p.ConsecutiveAuthFailures = credpool.HealthOK, "", 0
	} else {
		s.logger.Info("credential pool: %s updated terms but their %s credential is still parked (%s)", id.UserID, cred, p.Health)
	}
	if err := p.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// Lending a metered key with no spend ceiling is an open invoice on the
	// donor's own account. A subscription may be lent uncapped — the plan is
	// paid for either way — but real money may not.
	if cred.Source.Metered() && p.Limits.MaxUSDPerDay <= 0 && p.Limits.MaxUSDPerWeek <= 0 {
		httpError(w, http.StatusBadRequest,
			"set a daily or weekly spend ceiling: this key is billed per token, so sharing it without one is an open invoice on your account")
		return
	}
	if err := s.credPoolPledges.Upsert(r.Context(), p); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("credential pool: %s %s their %s credential", id.UserID, enabledVerb(p.Enabled), cred)
	// The credential was just verified above; don't re-read it.
	writeJSON(w, s.toPledgeView(r, p, time.Now().UTC(), true))
}

// httpErr is a status + message a handler surfaces verbatim.
type httpErr struct {
	status int
	msg    string
}

// verifyLendable checks the caller actually holds what they are offering,
// and returns the stored OAuth record when the offer is a subscription (the
// caller needs its timestamps for the re-park decision).
//
// The api_key branch is the load-bearing one: a pledge must never become a
// way to expose a key that is not the donor's own, so the key is required
// to be user-scoped TO THEM. A team-wide key is the team's to spend, and
// lending it would let one member hand the whole team's credential to the
// pool.
func (s *Server) verifyLendable(r *http.Request, userID string, cred credpool.Credential) (secrets.OAuthRecord, *httpErr) {
	switch cred.Source {
	case credpool.SourceOAuth:
		kind := secrets.OAuthKind(cred.Ref)
		if !kind.Valid() {
			return secrets.OAuthRecord{}, &httpErr{http.StatusBadRequest, fmt.Sprintf("unknown subscription %q", cred.Ref)}
		}
		rec, err := s.oauthStore.Get(r.Context(), userID, kind)
		if err != nil {
			if errors.Is(err, secrets.ErrOAuthNotFound) {
				return secrets.OAuthRecord{}, &httpErr{http.StatusConflict, fmt.Sprintf("connect your %s subscription before sharing it", kind)}
			}
			return secrets.OAuthRecord{}, &httpErr{http.StatusInternalServerError, err.Error()}
		}
		return rec, nil

	case credpool.SourceAPIKey:
		if s.apiKeys == nil {
			return secrets.OAuthRecord{}, &httpErr{http.StatusServiceUnavailable, "API keys are not enabled on this instance"}
		}
		if cred.KeyID == "" {
			return secrets.OAuthRecord{}, &httpErr{http.StatusBadRequest, "name the key you are lending (key_id)"}
		}
		// Owner-scoped rather than tenant-scoped: a donor lends a key of
		// their own, and which team they happen to be looking at when they
		// do it is not the question. GetOwned already refuses a key that
		// is not theirs, so a miss here is genuinely "no such key of
		// yours" — the ownership check below then only has the
		// team-scoped case left to reject.
		k, err := s.apiKeys.GetOwned(r.Context(), cred.KeyID, userID)
		if err != nil {
			if errors.Is(err, secrets.ErrApiKeyNotFound) {
				return secrets.OAuthRecord{}, &httpErr{http.StatusNotFound, "no such API key of yours — only a personal key can be pooled"}
			}
			return secrets.OAuthRecord{}, &httpErr{http.StatusInternalServerError, err.Error()}
		}
		if k.ScopeUserID == "" || k.ScopeUserID != userID {
			return secrets.OAuthRecord{}, &httpErr{http.StatusForbidden, "that key is not yours to lend — only a personal key can be pooled"}
		}
		if string(k.Provider) != cred.Ref {
			return secrets.OAuthRecord{}, &httpErr{http.StatusBadRequest, fmt.Sprintf("that key is a %s key, not %s", k.Provider, cred.Ref)}
		}
		// CreatedAt carries the reconnection signal, exactly as the OAuth
		// branch does: a key added AFTER the pledge was parked is what
		// clears the parked state. A zero record would read as "never
		// reconnected", leaving a donor who lends a fresh key stuck with a
		// pledge that can never serve again and no repair path short of
		// withdrawing it.
		return secrets.OAuthRecord{CreatedAt: k.CreatedAt}, nil
	}
	return secrets.OAuthRecord{}, &httpErr{http.StatusBadRequest, fmt.Sprintf("unknown credential source %q", cred.Source)}
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
	src := credpool.CredentialSource(r.PathValue("source"))
	ref := r.PathValue("ref")
	if err := s.credPoolPledges.Delete(r.Context(), credpool.PledgeID(id.UserID, src, ref)); err != nil {
		if errors.Is(err, credpool.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.logger.Info("credential pool: %s withdrew their %s/%s contribution", id.UserID, src, ref)
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
// The active org comes from the JWT on the browser path; an `iap_` bearer
// carries a team but no org, so the team's own org is read instead — the
// same two-step the launch gate does (server/launch_gate.go orgForTeam).
// Without it every CLI caller resolves to no pool at all, and `iterion
// remote pool lend` can never work.
func (s *Server) poolIDForUser(r *http.Request) string {
	id, _ := auth.FromContext(r.Context())
	orgID := id.OrgID
	if orgID == "" {
		orgID = s.orgIDOfTeam(r.Context(), id.TeamID)
	}
	if orgID == "" {
		return ""
	}
	p, err := s.credPoolPools.GetByOrg(r.Context(), orgID)
	if err != nil {
		if !errors.Is(err, credpool.ErrNotFound) {
			s.logger.Warn("credential pool: resolve pool of org %s: %v", orgID, err)
		}
		return ""
	}
	return p.ID
}

// orgIDOfTeam reads a team's parent org, "" on any miss.
func (s *Server) orgIDOfTeam(ctx context.Context, teamID string) string {
	if teamID == "" {
		return ""
	}
	st := s.authStore()
	if st == nil {
		return ""
	}
	t, err := st.GetTeam(ctx, teamID)
	if err != nil {
		return ""
	}
	return t.OrgID
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

// stillHolds reports whether the donor still has the credential a pledge
// offers. A pledge whose credential is gone is inert — say so rather than
// showing an "active" contribution that can never be served.
func (s *Server) stillHolds(r *http.Request, p credpool.Pledge, connectedOAuth map[string]bool) bool {
	switch p.Source {
	case credpool.SourceOAuth:
		return connectedOAuth[p.Ref]
	case credpool.SourceAPIKey:
		if s.apiKeys == nil || p.KeyID == "" {
			return false
		}
		// Owner-scoped: a donor whose active team differs from the team
		// their key is scoped to would otherwise be told their own
		// contribution is gone.
		k, err := s.apiKeys.GetOwned(r.Context(), p.KeyID, p.UserID)
		return err == nil && k.ScopeUserID == p.UserID
	}
	return false
}

func (s *Server) toPledgeView(r *http.Request, p credpool.Pledge, now time.Time, connected bool) pledgeView {
	v := pledgeView{
		Source:        string(p.Source),
		Ref:           p.Ref,
		KeyID:         p.KeyID,
		Metered:       p.Source.Metered(),
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
		// A donor who can no longer be drawn on must not read "active" — the
		// ledger, not the pledge, holds that truth, and it is asked here
		// through the SAME rule it admits with.
		//
		// Why the two branches: a launch is also refused while the donor's
		// remaining allowance is fully promised to runs in flight, or while
		// every slot they allowed is busy. Calling that "exhausted" would
		// tell a contributor they gave their whole day when they have spent
		// nothing yet, so it reads "serving" and clears as the runs end.
		// Exhausted is reserved for what they REALLY gave.
		liveRuns, committed := 0, 0.0
		if s.credPoolLeases != nil {
			if n, c, cerr := s.credPoolLeases.LiveCommitment(r.Context(), p.ID, "", now); cerr == nil {
				liveRuns, committed = n, c
			}
		}
		atSlotCap := p.Limits.MaxConcurrentRuns > 0 && liveRuns >= p.Limits.MaxConcurrentRuns
		switch {
		case status != credpool.StatusActive:
			// Health/window/pause already decided; the ledger adds nothing.
		case p.Limits.Deny(day.Runs+1, day.CostUSD, week.CostUSD) != credpool.DenyNone:
			v.Status = string(credpool.StatusExhausted)
		case liveRuns > 0 && (atSlotCap ||
			p.Limits.Deny(day.Runs+1, day.CostUSD+committed, week.CostUSD+committed) != credpool.DenyNone):
			v.Status = string(credpool.StatusServing)
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
			Source:        string(p.Source),
			Ref:           p.Ref,
			Metered:       p.Source.Metered(),
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
