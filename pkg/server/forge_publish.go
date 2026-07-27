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
	// Gate, when present and enabled, drives the deterministic merge-gate
	// commit status posted onto the PR head SHA (see publishReviewGate).
	Gate *publishReviewGate `json:"gate,omitempty"`
}

// publishReviewGate carries the reviewer bot's DETERMINISTIC gate verdict:
// how many findings meet the blocking threshold. The server maps it onto a
// commit status (success when BlockingCount == 0, else failure) named Context
// on the PR head SHA. The count is computed by the bot from the finding
// severities — the server never re-judges, it only posts the status. This
// keeps the gate deterministic (a count) while the LLM only produces content.
type publishReviewGate struct {
	// Enabled gates the whole feature; a false/absent gate posts no status
	// (today's advisory-only behaviour).
	Enabled bool `json:"enabled"`
	// Context is the status check name branch protection matches on
	// (default "revi/review").
	Context string `json:"context,omitempty"`
	// BlockingCount is the number of findings at or above Threshold.
	BlockingCount int `json:"blocking_count"`
	// Threshold is the severity floor that blocks (for the description only).
	Threshold string `json:"threshold,omitempty"`
	// TotalFindings is the full kept-finding count (for the description).
	TotalFindings int `json:"total_findings"`
	// Note, when set, REPLACES the rendered description. The bot uses it to
	// state the real reason a gate is red when that reason is not "N blocking
	// findings" — e.g. its own output was unreadable and the count is a
	// fail-closed placeholder. Describing that as a blocking finding sends the
	// operator hunting for one that was never published (erreurs-explicites).
	Note string `json:"note,omitempty"`
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
	// Gate* report the merge-gate commit-status outcome. GatePosted is false
	// when no gate was requested OR posting failed (GateError then explains).
	// A gate failure never fails the publish — the review already landed.
	GatePosted  bool   `json:"gate_posted"`
	GateState   string `json:"gate_state,omitempty"`
	GateContext string `json:"gate_context,omitempty"`
	GateSHA     string `json:"gate_sha,omitempty"`
	GateError   string `json:"gate_error,omitempty"`
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

	// Merge gate: post the deterministic revi/review commit status on the PR
	// head SHA. Additive — a failure here never fails the publish (the review
	// already landed), it is reported in the response + logged.
	gate := s.postGateStatus(r.Context(), conn, grant.Repo, number, req.Gate, res.URL)
	if s.logger != nil && gate.requested {
		if gate.posted {
			s.logger.Info("forge gate: %s %s#%d @%s → %s (%q)", conn.Provider, grant.Repo, number, gate.sha, gate.state, gate.context)
		} else {
			s.logger.Warn("forge gate: %s %s#%d → not posted: %s", conn.Provider, grant.Repo, number, gate.errText)
		}
	}

	writeJSON(w, publishReviewResponse{
		Published:         true,
		Provider:          string(conn.Provider),
		ReviewURL:         res.URL,
		CommentsPosted:    res.CommentsPosted,
		SuggestionsPosted: res.SuggestionsPosted,
		Verified:          res.Verified,
		Fallback:          res.Fallback,
		GatePosted:        gate.posted,
		GateState:         gate.state,
		GateContext:       gate.context,
		GateSHA:           gate.sha,
		GateError:         gate.errText,
	})
}

// defaultGateContext is the commit-status check name the merge gate posts
// under when the bot pins none. Kept BOT-AGNOSTIC (the engine must not bake a
// specific bot's persona in — see CLAUDE.md): a reviewer bot names its own
// check via gate.context (Revi sends "revi/review"); this neutral fallback
// only applies when a gate arrives with an empty context.
const defaultGateContext = "merge-gate"

// forgeGateClient is the capability the merge gate needs: resolve the PR head
// SHA, then post a commit status on it. Satisfied by the github/gitlab/forgejo
// admin clients (both methods live on the same *AdminClient).
type forgeGateClient interface {
	GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error)
	SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error
}

// gateClientFor resolves a connection's forgeGateClient. The forgeGateClientFor
// field is a test seam; nil uses the real admin client. Returns (nil, nil) when
// the provider has no commit-status capability.
func (s *Server) gateClientFor(ctx context.Context, conn forge.Connection) (forgeGateClient, error) {
	if s.forgeGateClientFor != nil {
		return s.forgeGateClientFor(ctx, conn)
	}
	admin, err := s.forgeAdminFor(ctx, conn)
	if err != nil {
		return nil, err
	}
	gc, ok := admin.(forgeGateClient)
	if !ok {
		return nil, nil
	}
	return gc, nil
}

// gateOutcome is the internal result of posting the gate status.
type gateOutcome struct {
	requested bool   // a gate was requested (enabled)
	posted    bool   // the status landed on the forge
	state     string // "success" | "failure" (when posted)
	context   string // the status check name used
	sha       string // the head SHA the status was posted on
	errText   string // why not posted (when !posted)
}

// postGateStatus posts the deterministic merge-gate commit status. It resolves
// the PR head SHA (so the status lands on the exact revision under review),
// maps the bot's blocking-count verdict to success/failure, and writes the
// commit status through the connection's live admin client. Every failure is
// reported (never silently swallowed) but non-fatal to the publish.
func (s *Server) postGateStatus(ctx context.Context, conn forge.Connection, repo string, number int, gate *publishReviewGate, reviewURL string) gateOutcome {
	if gate == nil || !gate.Enabled {
		return gateOutcome{}
	}
	out := gateOutcome{requested: true, context: strings.TrimSpace(gate.Context)}
	if out.context == "" {
		out.context = defaultGateContext
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil {
		out.errText = "gate client: " + err.Error()
		return out
	}
	if gc == nil {
		out.errText = "provider " + string(conn.Provider) + " has no commit-status capability"
		return out
	}
	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil {
		out.errText = "resolve head sha: " + err.Error()
		return out
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		out.errText = "forge returned no head sha for the PR"
		return out
	}
	out.sha = pr.HeadSHA

	threshold := strings.TrimSpace(gate.Threshold)
	if threshold == "" {
		threshold = "high"
	}
	state := forge.CommitStateSuccess
	desc := fmt.Sprintf("no blocking findings (≥%s); %d total", threshold, gate.TotalFindings)
	if gate.BlockingCount > 0 {
		state = forge.CommitStateFailure
		desc = fmt.Sprintf("%d blocking finding(s) ≥%s — address them (a push re-reviews) or a maintainer overrides", gate.BlockingCount, threshold)
	}
	// An explicit note is the truth the bot knows and the count cannot express.
	if n := strings.TrimSpace(gate.Note); n != "" {
		desc = n
	}
	out.state = string(state)

	if err := gc.SetCommitStatus(ctx, repo, out.sha, forge.CommitStatus{
		State:       state,
		Context:     out.context,
		Description: desc,
		TargetURL:   reviewURL,
	}); err != nil {
		out.errText = "set commit status: " + err.Error()
		return out
	}
	out.posted = true
	return out
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
