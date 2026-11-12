package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The price feed's control surface (ADR-163 §5): the platform's, an
// org's and a team's own feed list — CRUD with CAS — and the manual sync
// that forces one feed or all of them, answering what changed per id;
// beside them, a team's effective view (pricefeed_effective.go).
// The routing-policy routes are the precedent: one document per scope,
// super-admin / org-admin / team-admin managed, every write through
// validation, a stale CAS loses as a loud 409.

func (s *Server) registerPriceFeedRoutes() {
	s.mux.Handle("GET /api/admin/price-feeds", s.requireSuperAdmin(http.HandlerFunc(s.handleGetPlatformPriceFeeds)))
	s.mux.Handle("PUT /api/admin/price-feeds", s.requireSuperAdmin(http.HandlerFunc(s.handlePutPlatformPriceFeeds)))
	s.mux.Handle("POST /api/admin/price-feeds/sync", s.requireSuperAdmin(http.HandlerFunc(s.handleSyncPlatformPriceFeeds)))
	s.mux.Handle("GET /api/orgs/{id}/price-feeds", s.requireAuth(http.HandlerFunc(s.handleGetOrgPriceFeeds)))
	s.mux.Handle("PUT /api/orgs/{id}/price-feeds", s.requireAuth(http.HandlerFunc(s.handlePutOrgPriceFeeds)))
	s.mux.Handle("POST /api/orgs/{id}/price-feeds/sync", s.requireAuth(http.HandlerFunc(s.handleSyncOrgPriceFeeds)))
	s.mux.Handle("GET /api/teams/{id}/price-feeds", s.requireAuth(http.HandlerFunc(s.handleGetTeamPriceFeeds)))
	s.mux.Handle("PUT /api/teams/{id}/price-feeds", s.requireAuth(http.HandlerFunc(s.handlePutTeamPriceFeeds)))
	s.mux.Handle("POST /api/teams/{id}/price-feeds/sync", s.requireAuth(http.HandlerFunc(s.handleSyncTeamPriceFeeds)))
	s.mux.Handle("GET /api/teams/{id}/price-feeds/effective", s.requireAuth(http.HandlerFunc(s.handleGetTeamPriceFeedsEffective)))
}

// priceFeedView is one feed's GET answer: the config as stored and the
// state as it reads NOW — status derived at read time, because
// staleness arrives with the clock, not with a write. A declaration's
// answer is its config: status declared, its entry count and the
// config's write stamp, no window.
type priceFeedView struct {
	Feed pricefeed.FeedConfig `json:"feed"`
	// RedactedParams names, sorted, the params Feed.Params does not render
	// as stored (redactFeedParams): a client that writes a read list back
	// restates their values, or the PUT stores the redaction in their place.
	RedactedParams []string           `json:"redacted_params,omitempty"`
	Status         string             `json:"status"` // ok | stale | failing | never | declared
	Entries        int                `json:"entries,omitempty"`
	RefreshedAt    *time.Time         `json:"refreshed_at,omitempty"`
	DeclaredAt     *time.Time         `json:"declared_at,omitempty"`
	FreshFor       pricefeed.Duration `json:"fresh_for,omitempty"`
	LastError      string             `json:"last_error,omitempty"`
}

// priceFeedsView is the scope's whole answer: the stored list with its
// CAS stamp, every feed's live state folded in.
type priceFeedsView struct {
	Scope     string          `json:"scope"` // "platform" | "org" | "team"
	Origin    string          `json:"origin"`
	Feeds     []priceFeedView `json:"feeds"`
	UpdatedAt *time.Time      `json:"updated_at,omitempty"`
	UpdatedBy string          `json:"updated_by,omitempty"`
	Sources   []string        `json:"source_types"` // the registered vocabulary
}

// priceFeedsPutReq is the PUT body: the whole feed list replaces the
// document (an empty list clears the feeds but keeps the document),
// `null` clears the record outright. Absent feeds is a 400 — a body
// naming nothing is a misfire, and a blind rewrite would bump the CAS
// stamp under a concurrent editor.
type priceFeedsPutReq struct {
	Feeds             json.RawMessage `json:"feeds"`
	ExpectedUpdatedAt *time.Time      `json:"expected_updated_at,omitempty"`
}

// priceFeedsSyncReq is the sync body: a feed name refreshes that feed
// alone, absent refreshes every ENABLED feed of the scope.
type priceFeedsSyncReq struct {
	Name string `json:"name,omitempty"`
}

// priceFeedsSyncResponse is the sync answer: one outcome per refreshed
// feed, per-id changes included.
type priceFeedsSyncResponse struct {
	Results []pricefeed.SyncOutcome `json:"results"`
}

func (s *Server) handleGetOrgPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canViewOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this org")
		return
	}
	s.writePriceFeedsView(w, r, "org", pricefeed.OrgConfigDocID(orgID))
}

func (s *Server) handlePutOrgPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	docID := pricefeed.OrgConfigDocID(orgID)
	s.putPriceFeeds(w, r, "org", docID, func(meta map[string]any) {
		s.auditOrg(r, orgID, "org.price_feeds.updated", "price_feeds", docID, meta)
	})
}

func (s *Server) handleSyncOrgPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgID := r.PathValue("id")
	if !s.canManageOrg(r.Context(), id, orgID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "org admin or owner required")
		return
	}
	s.syncPriceFeeds(w, r, pricefeed.OrgConfigDocID(orgID))
}

func (s *Server) handleGetTeamPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "not a member of this team")
		return
	}
	s.writePriceFeedsView(w, r, "team", pricefeed.TeamConfigDocID(teamID))
}

func (s *Server) handlePutTeamPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	docID := pricefeed.TeamConfigDocID(teamID)
	s.putPriceFeeds(w, r, "team", docID, func(meta map[string]any) {
		s.auditTenant(r, teamID, "team.price_feeds.updated", "price_feeds", docID, meta)
	})
}

func (s *Server) handleSyncTeamPriceFeeds(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin (or org admin) required")
		return
	}
	s.syncPriceFeeds(w, r, pricefeed.TeamConfigDocID(teamID))
}

// The platform level: the deployment's own feed list, folded below every
// org and team. Super-admin on every verb — the route wrap
// (requireSuperAdmin) is the gate — and the write is audited in the
// platform log.

func (s *Server) handleGetPlatformPriceFeeds(w http.ResponseWriter, r *http.Request) {
	s.writePriceFeedsView(w, r, "platform", pricefeed.PlatformConfigDocID)
}

func (s *Server) handlePutPlatformPriceFeeds(w http.ResponseWriter, r *http.Request) {
	s.putPriceFeeds(w, r, "platform", pricefeed.PlatformConfigDocID, func(meta map[string]any) {
		s.auditPlatform(r, "", "platform.price_feeds.updated", "price_feeds", pricefeed.PlatformConfigDocID, meta)
	})
}

func (s *Server) handleSyncPlatformPriceFeeds(w http.ResponseWriter, r *http.Request) {
	s.syncPriceFeeds(w, r, pricefeed.PlatformConfigDocID)
}

// writePriceFeedsView answers the GET view. A deployment without the
// feed stores (local mode) answers the empty view with the registered
// source vocabulary — the level is genuinely unset there and the CLI
// stays usable.
func (s *Server) writePriceFeedsView(w http.ResponseWriter, r *http.Request, scope, docID string) {
	view := priceFeedsView{Scope: scope, Origin: "default", Sources: pricefeed.SourceTypes()}
	if s.priceFeedConfigs != nil {
		rec, err := s.priceFeedConfigs.Get(r.Context(), docID)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if rec != nil {
			view.Origin = "db"
			stamp := rec.UpdatedAt
			view.UpdatedAt = &stamp
			view.UpdatedBy = rec.UpdatedBy
			for _, feed := range rec.Feeds {
				view.Feeds = append(view.Feeds, s.priceFeedViewOf(r.Context(), docID, rec, feed))
			}
		}
	}
	s.writeJSONFor(w, r, view)
}

// priceFeedViewOf folds one feed's config with its derived state. The
// view is MEMBER-readable, the source params carry credentials (a
// litellm token): token-shaped params render redacted, named in
// RedactedParams, and the stored error truncates. The write path replaces
// the whole list with what it receives, so a client that writes a read
// list back restates every param RedactedParams names — `iterion remote
// price-feeds declare` refuses to write a list holding one.
//
// A static feed (a declaration) is judged by its config alone, the way
// the fold reads it: declared, stamped with the config's write, whatever
// state document an earlier fetched feed of that name left; a stored
// declaration the write rules refuse reads failing, the refusal named —
// the fold steps it down. A fetched feed reads only a state written for
// its own source type (StateRecord.Of): a retyped feed reads never until
// its first refresh.
func (s *Server) priceFeedViewOf(ctx context.Context, docID string, rec *pricefeed.ConfigRecord, feed pricefeed.FeedConfig) priceFeedView {
	v := priceFeedView{Feed: feed, Status: pricefeed.StateNever, FreshFor: feed.FreshFor()}
	v.Feed.Params, v.RedactedParams = redactFeedParams(feed.Params)
	if src, ok := pricefeed.SourceFor(feed.Type); ok {
		if static, ok := src.(pricefeed.StaticSource); ok {
			v.FreshFor = 0
			entries, err := static.Entries(feed.Params)
			if err != nil {
				v.Status = pricefeed.StateFailing
				v.LastError = truncateFeedError(err.Error())
				return v
			}
			v.Status = pricefeed.StateDeclared
			v.Entries = len(entries)
			stamp := rec.UpdatedAt
			v.DeclaredAt = &stamp
			return v
		}
	}
	if s.priceFeedStates == nil {
		return v
	}
	state, err := s.priceFeedStates.Get(ctx, pricefeed.StateDocID(docID, feed.Name))
	if err != nil {
		return v
	}
	if state = state.Of(feed.Type); state == nil {
		return v
	}
	v.Status = priceFeedDerivedStatus(state, time.Now())
	v.Entries = len(state.Entries)
	stamp := state.RefreshedAt
	v.RefreshedAt = &stamp
	v.LastError = truncateFeedError(state.LastError)
	return v
}

// redactFeedParams replaces token-shaped values with a marker and
// sanitises URL-shaped ones, and names, sorted, the params whose rendering
// differs from the stored value. Keyed on the key's and value's SHAPE, not
// a list: token, api-token, bearer-key all read — and a URL carries its
// own credentials (userinfo, a ?token= query) whatever its param name
// (the gate's finding: `https://tariffs.example/feed?token=SECRET`
// reached org members verbatim).
func redactFeedParams(params map[string]string) (map[string]string, []string) {
	if params == nil {
		return nil, nil
	}
	out := make(map[string]string, len(params))
	var redacted []string
	for k, v := range params {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "secret") {
			out[k] = "(redacted)"
		} else {
			out[k] = sanitizeFeedURL(v)
		}
		if out[k] != v {
			redacted = append(redacted, k)
		}
	}
	sort.Strings(redacted)
	return out, redacted
}

// sanitizeFeedURL strips a URL-shaped value's userinfo and query — the
// two credential channels a URL carries. Non-URL values pass through.
func sanitizeFeedURL(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return v
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" {
		return v
	}
	credentialed := *u
	credentialed.User = nil
	credentialed.RawQuery = ""
	credentialed.ForceQuery = false
	credentialed.Fragment = ""
	credentialed.RawFragment = ""
	return credentialed.String()
}

// truncateFeedError bounds the stored error a view renders: refresh
// errors embed fetched URLs, and a member's screen is not the place for
// internal topology.
func truncateFeedError(err string) string {
	if len(err) <= 160 {
		return err
	}
	return err[:157] + "…"
}

// execFeedsAllowedForCaller reports whether the PUT body's feed list is
// free of exec-type feeds, or the caller is a super-admin (who may write
// them).
func (s *Server) execFeedsAllowedForCaller(r *http.Request, feeds json.RawMessage) bool {
	if s.isSuperAdmin(r) {
		return true
	}
	var list []pricefeed.FeedConfig
	if err := json.Unmarshal(feeds, &list); err != nil {
		return true // the shape error surfaces at the decode below
	}
	for _, feed := range list {
		if feed.Type == pricefeed.TypeExec {
			return false
		}
	}
	return true
}

// putPriceFeeds validates and stores (or clears) one scope's feed list —
// the document docID — then records the write through audit, the
// scope's own log.
func (s *Server) putPriceFeeds(w http.ResponseWriter, r *http.Request, scope, docID string, audit func(meta map[string]any)) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.priceFeedConfigs == nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "price-feed store unavailable on this deployment")
		return
	}
	var req priceFeedsPutReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if req.Feeds == nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "feeds is required (a list to replace, null to clear the record)")
		return
	}
	prevUpdatedAt := time.Time{}
	if rec, err := s.priceFeedConfigs.Get(r.Context(), docID); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	} else if rec != nil {
		prevUpdatedAt = rec.UpdatedAt
	}
	if req.ExpectedUpdatedAt != nil && !req.ExpectedUpdatedAt.Equal(prevUpdatedAt) {
		s.httpErrorFor(w, r, http.StatusConflict, "price feeds changed concurrently — re-read and retry")
		return
	}

	// The exec source type is the platform operator's capability (a
	// command that runs ON THE SERVER): a tenant admin must not buy
	// command execution with a feed list. Super-admin may configure one;
	// the execution-side hatch (priceFeedExecEnv) is the second layer.
	if !s.execFeedsAllowedForCaller(r, req.Feeds) {
		s.httpErrorFor(w, r, http.StatusForbidden, "exec price feeds are the platform operator's — super-admin only, and the deployment must export %s=1", priceFeedExecEnv)
		return
	}

	cleared := string(bytes.TrimSpace(req.Feeds)) == "null"
	auditMeta := map[string]any{"cleared": cleared}
	if cleared {
		wrote, err := s.priceFeedConfigs.DeleteIfUnchanged(r.Context(), docID, prevUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if !wrote {
			s.httpErrorFor(w, r, http.StatusConflict, "price feeds changed concurrently — re-read and retry")
			return
		}
	} else {
		var feeds []pricefeed.FeedConfig
		if err := json.Unmarshal(req.Feeds, &feeds); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "feeds must be a list (or null to clear): %v", err)
			return
		}
		rec := &pricefeed.ConfigRecord{Feeds: feeds, UpdatedBy: s.requestUserID(r)}
		if err := pricefeed.ValidateConfig(rec); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
			return
		}
		auditMeta["feeds"] = len(feeds)
		wrote, err := s.priceFeedConfigs.PutIfUnchanged(r.Context(), docID, rec, prevUpdatedAt)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
			return
		}
		if !wrote {
			s.httpErrorFor(w, r, http.StatusConflict, "price feeds changed concurrently — re-read and retry")
			return
		}
	}
	audit(auditMeta)
	s.writePriceFeedsView(w, r, scope, docID)
}

// syncPriceFeeds is the manual trigger (ADR-163 §5): one feed by name,
// or every enabled feed of the scope. The answer reports what changed —
// per id old → new, the feed's source and freshness — so an operator
// sees what a forced sync actually moved.
func (s *Server) syncPriceFeeds(w http.ResponseWriter, r *http.Request, docID string) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.priceFeedConfigs == nil || s.priceFeedStates == nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "price-feed store unavailable on this deployment")
		return
	}
	var req priceFeedsSyncReq
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
			return
		}
	}
	rec, err := s.priceFeedConfigs.Get(r.Context(), docID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "%v", err)
		return
	}
	if rec == nil || len(rec.Feeds) == 0 {
		s.httpErrorFor(w, r, http.StatusNotFound, "no price feed is configured at this level")
		return
	}
	var outcomes []pricefeed.SyncOutcome
	for _, feed := range rec.Feeds {
		if req.Name != "" {
			if feed.Name != req.Name {
				continue
			}
		} else if !feed.Enabled {
			continue
		}
		outcome, err := s.priceFeedRefresh(r.Context(), docID, feed)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "feed %s: %v", feed.Name, err)
			return
		}
		outcomes = append(outcomes, outcome)
	}
	if req.Name != "" && len(outcomes) == 0 {
		s.httpErrorFor(w, r, http.StatusNotFound, "no feed named %q at this level", req.Name)
		return
	}
	s.writeJSONFor(w, r, priceFeedsSyncResponse{Results: outcomes})
}
