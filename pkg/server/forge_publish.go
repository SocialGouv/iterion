package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// ---------------------------------------------------------------------------
// Deterministic forge review publishing (tokenless-in-workspace).
//
// A run that reviews a PR must NOT hold a forge credential to post its
// findings: workspace-mounted tokens freeze at launch (a GitHub App
// installation token lives ~1h) and hand a write credential to an LLM
// agent. Instead the server mints a per-run grant at launch time
// (injectForgePublishVars), the bot's DETERMINISTIC publish node POSTs its
// findings to /api/v1/forge/publish-review with that token, and the server
// posts the review through the team connection's LIVE forge client (App
// connections mint a fresh installation token per call). Mirrors the board
// MCP HTTP transport's X-Iterion-Run token pattern (mcp_board_handler.go).
// ---------------------------------------------------------------------------

// forgePublishDefaultTTL caps how long a publish grant stays alive. Same
// rationale as boardMCPDefaultTTL: long enough for any realistic run, short
// enough that a leaked token from a crashed run expires on its own.
const forgePublishDefaultTTL = 24 * time.Hour

// forgePublishMaxTokens bounds the in-memory registry.
const forgePublishMaxTokens = 1024

// ForgePublishGrant scopes one run's publish token: reviews may only be
// posted through this team's connection, on this repo.
type ForgePublishGrant struct {
	TeamID       string    `json:"team_id"`
	ConnectionID string    `json:"connection_id"`
	Repo         string    `json:"repo"`
	ExpiresAt    time.Time `json:"-"`
}

// ForgePublishTokenStore is the per-run forge-publish token registry. The
// in-memory *ForgePublishTokenRegistry is single-replica; the Valkey impl
// (valkey_stores.go) shares tokens across replicas so a run's POST can land
// on any server pod.
type ForgePublishTokenStore interface {
	Register(token string, g ForgePublishGrant) error
	Revoke(token string)
	lookup(token string) (ForgePublishGrant, bool)
}

// ForgePublishTokenRegistry is the in-memory ForgePublishTokenStore.
type ForgePublishTokenRegistry struct {
	mu     sync.RWMutex
	tokens map[string]ForgePublishGrant
	now    func() time.Time // injectable for tests
}

// NewForgePublishTokenRegistry returns an empty registry.
func NewForgePublishTokenRegistry() *ForgePublishTokenRegistry {
	return &ForgePublishTokenRegistry{tokens: map[string]ForgePublishGrant{}, now: time.Now}
}

// Register stores a token with its grant. A full registry is an error: the
// token would never authorize, so the caller must not hand it out.
func (r *ForgePublishTokenRegistry) Register(token string, g ForgePublishGrant) error {
	g.ExpiresAt = r.now().Add(forgePublishDefaultTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.tokens {
		if r.now().After(v.ExpiresAt) {
			delete(r.tokens, k)
		}
	}
	if len(r.tokens) >= forgePublishMaxTokens {
		return fmt.Errorf("forge publish token registry full (%d tokens)", forgePublishMaxTokens)
	}
	r.tokens[token] = g
	return nil
}

// Revoke removes the token; subsequent calls with it 401.
func (r *ForgePublishTokenRegistry) Revoke(token string) {
	r.mu.Lock()
	delete(r.tokens, token)
	r.mu.Unlock()
}

func (r *ForgePublishTokenRegistry) lookup(token string) (ForgePublishGrant, bool) {
	r.mu.RLock()
	g, ok := r.tokens[token]
	r.mu.RUnlock()
	if !ok || r.now().After(g.ExpiresAt) {
		return ForgePublishGrant{}, false
	}
	return g, true
}

// ---------------------------------------------------------------------------
// HTTP endpoint
// ---------------------------------------------------------------------------

type publishReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	// LineEnd (inclusive) marks a multi-line span when > Line.
	LineEnd int    `json:"line_end,omitempty"`
	Body    string `json:"body"`
	// Suggestion is the literal replacement text for the span, rendered by
	// the provider as its one-click suggestion block.
	Suggestion string `json:"suggestion,omitempty"`
}

type publishReviewRequest struct {
	PRURL   string `json:"pr_url"`
	Summary string `json:"summary"`
	// Mode "inline" (default) posts the comments inline; "summary" folds
	// everything into a single review body.
	Mode     string                 `json:"mode,omitempty"`
	Comments []publishReviewComment `json:"comments,omitempty"`
}

type publishReviewResponse struct {
	Published         bool   `json:"published"`
	Provider          string `json:"provider"`
	ReviewURL         string `json:"review_url"`
	CommentsPosted    int    `json:"comments_posted"`
	SuggestionsPosted int    `json:"suggestions_posted"`
	// Verified reports whether comments_posted was confirmed by a
	// follow-up forge read, not assumed from the create call.
	Verified bool `json:"verified"`
	// Fallback is "" | "summary" | "partial" (see forge.ReviewResult).
	Fallback string `json:"fallback,omitempty"`
}

// handleForgePublishReview authenticates the per-run token, validates the
// payload against the grant's pinned repo, resolves the team connection's
// LIVE forge client and posts the review. Every failure is a clear 4xx/5xx —
// no silent fallback (erreurs-explicites).
func (s *Server) handleForgePublishReview(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Iterion-Run")
	if token == "" {
		httpError(w, http.StatusUnauthorized, "missing X-Iterion-Run header")
		return
	}
	if s.forgePublishTokens == nil || s.forgeConnections == nil {
		httpError(w, http.StatusNotFound, "forge publishing is not enabled on this server (no forge connections wired)")
		return
	}
	grant, ok := s.forgePublishTokens.lookup(token)
	if !ok {
		httpError(w, http.StatusUnauthorized, "unknown or expired run token")
		return
	}

	var req publishReviewRequest
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "inline"
	}
	if mode != "inline" && mode != "summary" {
		httpError(w, http.StatusBadRequest, "mode must be \"inline\" or \"summary\" (got %q)", req.Mode)
		return
	}
	host, repo, number, err := forge.ParsePullURL(req.PRURL)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !strings.EqualFold(repo, grant.Repo) {
		httpError(w, http.StatusForbidden, "run token is scoped to repo %q, not %q", grant.Repo, repo)
		return
	}
	if strings.TrimSpace(req.Summary) == "" && len(req.Comments) == 0 {
		httpError(w, http.StatusBadRequest, "nothing to publish: summary and comments are both empty")
		return
	}
	for i, c := range req.Comments {
		if strings.TrimSpace(c.Path) == "" || c.Line <= 0 || strings.TrimSpace(c.Body) == "" {
			httpError(w, http.StatusBadRequest, "comment %d: path, positive line and body are required", i)
			return
		}
	}

	conn, err := s.forgeConnections.Get(r.Context(), grant.ConnectionID)
	if err != nil || conn.TenantID != grant.TeamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return
	}
	if connHost := hostOfURL(conn.BaseURL()); connHost == "" || !strings.EqualFold(connHost, host) {
		httpError(w, http.StatusBadRequest, "pr_url host %q is not on the connection's forge host", host)
		return
	}

	rc, err := s.reviewClientFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusBadGateway, "forge client: %v", err)
		return
	}
	if rc == nil {
		httpError(w, http.StatusNotImplemented, "provider %s has no PR review client", conn.Provider)
		return
	}

	review := forge.NewReview{Body: req.Summary}
	if mode == "inline" {
		for _, c := range req.Comments {
			review.Comments = append(review.Comments, forge.ReviewComment{
				Path: c.Path, Line: c.Line, LineEnd: c.LineEnd, Body: c.Body, Suggestion: c.Suggestion,
			})
		}
	} else if len(req.Comments) > 0 {
		// Summary mode still carries the findings — folded into the body,
		// never dropped.
		review.Body += "\n\n" + forge.FoldCommentsMarkdown(toForgeComments(req.Comments))
	}

	res, err := rc.CreatePullReview(r.Context(), grant.Repo, number, review)
	if err != nil {
		httpError(w, http.StatusBadGateway, "create pull review: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("forge publish: %s %s#%d → %d inline comment(s) (verified=%v fallback=%q)",
			conn.Provider, grant.Repo, number, res.CommentsPosted, res.Verified, res.Fallback)
	}
	writeJSON(w, publishReviewResponse{
		Published:         true,
		Provider:          string(conn.Provider),
		ReviewURL:         res.URL,
		CommentsPosted:    res.CommentsPosted,
		SuggestionsPosted: res.SuggestionsPosted,
		Verified:          res.Verified,
		Fallback:          res.Fallback,
	})
}

// reviewClientFor resolves a connection's forge.ReviewClient. The
// forgeReviewClientFor field is a test seam; nil uses the real admin client.
// Returns (nil, nil) when the provider has no review capability.
func (s *Server) reviewClientFor(ctx context.Context, conn forge.Connection) (forge.ReviewClient, error) {
	if s.forgeReviewClientFor != nil {
		return s.forgeReviewClientFor(ctx, conn)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, err
	}
	rc, ok := admin.(forge.ReviewClient)
	if !ok {
		return nil, nil
	}
	return rc, nil
}

func toForgeComments(in []publishReviewComment) []forge.ReviewComment {
	out := make([]forge.ReviewComment, 0, len(in))
	for _, c := range in {
		out = append(out, forge.ReviewComment{Path: c.Path, Line: c.Line, LineEnd: c.LineEnd, Body: c.Body, Suggestion: c.Suggestion})
	}
	return out
}

func hostOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// ---------------------------------------------------------------------------
// Launch-time grant minting + var injection
// ---------------------------------------------------------------------------

// forgePublishVarURL / forgePublishVarToken are the launch vars the server
// injects; a bot opts in by declaring them in its vars: block (undeclared
// launch vars are dropped by the IR, so blind injection is safe).
const (
	forgePublishVarURL   = "forge_publish_url"
	forgePublishVarToken = "forge_publish_token"
)

// injectForgePublishVars mints a per-run forge-publish grant and injects the
// forge_publish_url / forge_publish_token launch vars when (a) the launch
// carries a pr_url var, and (b) a team forge connection covers that PR's
// host+repo. Returns vars unchanged (and logs why, when relevant) otherwise —
// the bot's deterministic publish node then reports an explicit
// "no endpoint bound" skip instead of silently doing nothing.
//
// preferredConnID pins the connection (repo-targeted launches); empty falls
// back to the team's repo integrations, then to a connection host match.
func (s *Server) injectForgePublishVars(ctx context.Context, teamID, preferredConnID string, vars map[string]string, r *http.Request) map[string]string {
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return vars
	}
	prURL := strings.TrimSpace(vars["pr_url"])
	if prURL == "" {
		return vars
	}
	if strings.TrimSpace(vars[forgePublishVarToken]) != "" {
		// The caller pinned its own grant — don't overwrite.
		return vars
	}
	base := s.publicBaseURL(r)
	if base == "" {
		if s.logger != nil {
			s.logger.Warn("forge publish: no public base URL (set PublicURL); deterministic review publishing disabled for this launch")
		}
		return vars
	}
	host, repo, _, err := forge.ParsePullURL(prURL)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("forge publish: %v; deterministic review publishing disabled for this launch", err)
		}
		return vars
	}
	conn, ok := s.forgeConnectionForPR(ctx, teamID, preferredConnID, host, repo)
	if !ok {
		if s.logger != nil {
			s.logger.Warn("forge publish: no team %s connection covers %s/%s; deterministic review publishing disabled for this launch", teamID, host, repo)
		}
		return vars
	}
	token := newBoardMCPToken()
	if token == "" {
		return vars
	}
	if err := s.forgePublishTokens.Register(token, ForgePublishGrant{TeamID: teamID, ConnectionID: conn.ID, Repo: repo}); err != nil {
		if s.logger != nil {
			s.logger.Error("forge publish: %v; deterministic review publishing disabled for this launch", err)
		}
		return vars
	}
	if vars == nil {
		vars = map[string]string{}
	}
	vars[forgePublishVarURL] = base + "/api/v1/forge/publish-review"
	vars[forgePublishVarToken] = token
	return vars
}

// forgeConnectionForPR picks the team connection to publish through:
// the pinned connection when given, else the connection of a repo
// integration matching the repo slug, else the first team connection on the
// PR's forge host.
func (s *Server) forgeConnectionForPR(ctx context.Context, teamID, preferredConnID, host, repo string) (forge.Connection, bool) {
	matches := func(c forge.Connection) bool {
		return c.TenantID == teamID && strings.EqualFold(hostOfURL(c.BaseURL()), host)
	}
	if preferredConnID != "" {
		if c, err := s.forgeConnections.Get(ctx, preferredConnID); err == nil && matches(c) {
			return c, true
		}
	}
	if s.forgeIntegrations != nil {
		if ris, err := s.forgeIntegrations.ListByTenant(ctx, teamID); err == nil {
			for _, ri := range ris {
				if !strings.EqualFold(ri.RepoFullName, repo) {
					continue
				}
				if c, err := s.forgeConnections.Get(ctx, ri.ConnectionID); err == nil && matches(c) {
					return c, true
				}
			}
		}
	}
	conns, err := s.forgeConnections.ListByTenant(ctx, teamID)
	if err != nil {
		return forge.Connection{}, false
	}
	for _, c := range conns {
		if matches(c) {
			return c, true
		}
	}
	return forge.Connection{}, false
}

// publicBaseURL is the origin runs use to call back into this server:
// cfg.PublicURL when configured (cloud / self-hosted), else the launch
// request's own host (local studio).
func (s *Server) publicBaseURL(r *http.Request) string {
	if base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicURL), "/"); base != "" {
		return base
	}
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
