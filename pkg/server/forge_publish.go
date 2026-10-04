package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
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

// forgePublishDefaultTTL caps how long a publish grant stays alive: long
// enough for any realistic run, short enough that a leaked token from a
// crashed run expires on its own.
//
// "Any realistic run" is NOT the run's wall-clock budget. A run blocked on a
// provider usage window is parked and resumed by the retry sweeper up to
// retrypolicy.DefaultMaxWait later — a weekly forfait cap resets as much as
// seven days out. At the previous flat 24h the grant was dead long before the
// resumed run reached its publish node, so the review completed and then had
// no way to post its verdict or its gate status: the required check stayed on
// whatever the interruption left it, and the PR waited on an answer that had
// actually been computed. The grant therefore has to outlive the longest wait
// the retry machinery can schedule, plus a margin for the resumed run itself.
//
// This is the CEILING, not the ordinary life of a grant: a run's terminal
// outcome brings the expiry forward to the post-run grace (postRunGrace, in
// expireForgePublishGrantForRun), so only a run that is genuinely waiting out
// a quota window keeps the full window. What limits the damage meanwhile is
// what the grant can do — post a review and a commit status on ONE repo,
// re-enforced against the grant's (team, connection, repo) at every use.
const forgePublishDefaultTTL = retrypolicy.DefaultMaxWait + 24*time.Hour

// forgePublishMaxTokens bounds the in-memory registry (the backend used when
// no Valkey is configured).
//
// It scales with the TTL because Register evicts only EXPIRED entries before
// checking the cap: the ceiling is "live grants", and a grant lives at most
// one TTL. The terminal-outcome eviction is what keeps the steady state near
// "gating launches in flight" instead of "gating launches per TTL", but a
// deployment whose runs all park on a quota window still accumulates, so the
// cap keeps the TTL's shape.
//
// Saturation is no longer silent: Register's error refuses the launch
// (errForgePublishGrantUnavailable) rather than starting a run that claims the
// gate context and can never answer it.
const forgePublishMaxTokens = 1024 * int(forgePublishDefaultTTL/(24*time.Hour))

// ForgePublishGrant scopes one run's publish token: reviews may only be
// posted through this team's connection, on this repo.
type ForgePublishGrant struct {
	TeamID       string `json:"team_id"`
	ConnectionID string `json:"connection_id"`
	Repo         string `json:"repo"`
	// Bot names the bot this grant was minted for. It is the FALLBACK record
	// of which bot to relaunch when a gating run dies: the recovery reads the
	// run's own BotID first and falls back here (gateRelaunchBotID), because
	// an inline .bot launch persists none — and the answer is part of the
	// relaunch claim key, so it decides whether a second death on a head is
	// the same bot's or a peer's.
	//
	// It is NOT what decides that a run owed a verdict: the reconciler anchors
	// on the repo's pinned gate_context, because a repo shares one context
	// across several gating bots on purpose (see forge_gate_reconcile.go).
	Bot string `json:"bot,omitempty"`
	// Verdict is the merge-gate verdict posted with this grant, recorded by
	// the publish endpoint (recordGateVerdict). The status on the forge cannot
	// say whose it is — its target URL is the review, where reviewers land —
	// so this record is how the reconciler knows a run's owed verdict is
	// posted, without reading the forge. It names the grant, not the run: it
	// speaks for the run only while the grant is not Shared.
	Verdict *gateVerdict `json:"verdict,omitempty"`
	// Refusal is a verdict the gate REFUSED to post (recordGateRefusal): the
	// audited pin was stale or unreadable, so nothing was written — not
	// success, not failure, not pending. The run then finishes CLEANLY, and
	// without this record no lane can tell "the run left no verdict because
	// it died" from "the gate declined its verdict": the reconciler would
	// paint "review died — push again" over a refusal, or stand down on a
	// moved head nothing else will ever answer (#1632). Same attribution rule
	// as Verdict: it is this run's only while the grant is not Shared.
	Refusal *gateRefusal `json:"refusal,omitempty"`
	// Deferred is a verdict whose post the forge refused for a rate limit or a
	// transient failure, kept for the reconciler to post once the wait is over
	// (deferGateVerdict).
	Deferred *gateDeferral `json:"deferred,omitempty"`
	// Shared marks a grant a second run publishes with — a launch that pinned
	// the token, a fork (shareGrant) — so no one run's verdict or end may cut
	// it back to the post-run grace (the end of a gating run that names its
	// reviewed revision still brings it to the gate grace,
	// forgePublishGateGrace). CutBack marks the opposite,
	// set in the same update that shortens the grant to the post-run grace
	// (cutBack): a grant cut back can no longer be shared, and a pinned launch
	// on it is refused. The two are decided under one write, so exactly one of
	// them wins a race.
	Shared    bool      `json:"shared,omitempty"`
	CutBack   bool      `json:"cut_back,omitempty"`
	ExpiresAt time.Time `json:"-"`
}

// gateVerdict is one verdict the publish endpoint posted: which head, which
// check, which state, when.
type gateVerdict struct {
	SHA     string    `json:"sha"`
	Context string    `json:"context"`
	State   string    `json:"state"`
	At      time.Time `json:"at"`
}

// gateRefusal is one verdict the publish endpoint REFUSED to post: the pin it
// was audited against, the head that made it stale, the check, a short reason
// fit for a status description, and when. The reconciler reads it to answer
// the head the refusal left bare — with the refusal's own diagnosis, never
// with "review died".
type gateRefusal struct {
	AuditedSHA string    `json:"audited_sha"`
	HeadSHA    string    `json:"head_sha"`
	Context    string    `json:"context"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// grantTenantMismatchReason is the typed refusal a publish grant earns when
// it does not belong to the run carrying it. Named so a log line, an audit
// row and a test all say the same word.
const grantTenantMismatchReason = "grant_tenant_mismatch"

// auditActionGrantTenantMismatch is the audit action the refusal records on
// the RUN's tenant — the tenant whose run tried to speak as another.
const auditActionGrantTenantMismatch = "forge.grant.tenant_mismatch"

// grantUntrustedMintReason is the typed token for "no grant was minted for
// this launch, because its workspace holds code the tenant did not write".
// It is an OUTCOME, not a refusal: the launch proceeds without a grant, which
// is what the fork review lane is for. Named so a log line, an audit row and
// a test say the same word.
const grantUntrustedMintReason = "grant_untrusted_mint"

// auditActionGrantUntrustedMint is its audit action, on the launching tenant.
const auditActionGrantUntrustedMint = "forge.grant.untrusted_mint"

// grantUntrustedRunReason is the typed refusal a publish attempt earns when
// the RUN's workspace holds code the tenant did not write. Distinct word from
// the tenant mismatch on purpose: the operator action is different (this one
// is never a misconfiguration to correct — it is the lane working).
const grantUntrustedRunReason = "grant_untrusted_run"

// auditActionGrantUntrustedRun is its audit action, recorded on the run's own
// tenant so the team sees that something asked to publish on their behalf
// from an untrusted workspace.
const auditActionGrantUntrustedRun = "forge.grant.untrusted_run"

// runOwnsGrant proves a publish grant belongs to the run that carries it, and
// is the ONE place that decides it: every reader holding a run funnels through
// here, so the rule cannot hold at one surface and not the next.
//
// It exists because the grant token is a launch VAR. The scope checks each
// reader already performs prove the grant is SELF-consistent — its connection
// belongs to its team, its repo matches the pull request, its host matches the
// connection — and a grant minted for another tenant passes every one of them.
// What none of them asks is whether that tenant is the run's. Unasked, a run
// carrying another tenant's token has iterion comment on that tenant's pull
// request, post its REQUIRED commit status, and — on the auto-fix lane —
// launch a code-pushing bot into that tenant, all under that tenant's forge
// identity and against its budget.
//
// A run that states NO tenant is not refused: only the Mongo store stamps one
// (store/mongo.stampTenant), so a filesystem-backed single-tenant deployment
// states none — and a deployment with one tenant has no second tenant to
// protect. A run that DOES state one must match exactly; a grant that names no
// tenant fails that comparison, which is correct, since a cloud connection
// always has one.
//
// Loud on every refusal, on purpose: a Warn (never the token — an audit trail
// that leaks the credential is a second incident) plus an audit row on the
// run's own tenant, so the team whose run attempted the crossing sees it. The
// sweep re-offers a dead run for its whole lookback, so an unresolved refusal
// repeats; that is the intended shape — a crossing that persists must stay
// visible — and the rows collapse in one query on the run id.
func (s *Server) runOwnsGrant(run *store.Run, grant ForgePublishGrant, what string) bool {
	if run == nil {
		return false
	}
	// Trust before tenancy: a fork-lane run and a trusted run of the SAME
	// tenant are indistinguishable to the tenant comparison below, so that
	// check alone would let an untrusted run present any grant its own team
	// holds. This is the belt to injectForgePublishVars' braces — that one
	// refuses to MINT, this one refuses to HONOUR, and they are on opposite
	// sides of the run's creation so no single mistake clears both.
	if !run.Trust.Trusted() {
		if s.logger != nil {
			s.logger.Warn("forge gate: %s for run %s refused (%s): the run's workspace holds code the tenant did not write (trust=%q), which is never allowed to publish — nothing posted",
				what, run.ID, grantUntrustedRunReason, string(run.Trust))
		}
		s.auditSystem(strings.TrimSpace(run.TenantID), "forge-gate", auditActionGrantUntrustedRun, "run", run.ID, map[string]any{
			"reason":     grantUntrustedRunReason,
			"trust":      string(run.Trust),
			"grant_repo": grant.Repo,
			"surface":    what,
		})
		return false
	}
	runTenant := strings.TrimSpace(run.TenantID)
	if runTenant == "" || strings.EqualFold(runTenant, strings.TrimSpace(grant.TeamID)) {
		return true
	}
	if s.logger != nil {
		s.logger.Warn("forge gate: %s for run %s refused (%s): the run belongs to tenant %q but its publish grant was minted for tenant %q — nothing posted",
			what, run.ID, grantTenantMismatchReason, runTenant, grant.TeamID)
	}
	s.auditSystem(runTenant, "forge-gate", auditActionGrantTenantMismatch, "run", run.ID, map[string]any{
		"reason":       grantTenantMismatchReason,
		"grant_tenant": grant.TeamID,
		"grant_repo":   grant.Repo,
		"surface":      what,
	})
	return false
}

// ForgePublishTokenStore is the per-run forge-publish token registry. The
// in-memory *ForgePublishTokenRegistry is single-replica; the Valkey impl
// (valkey_stores.go) shares tokens across replicas so a run's POST can land
// on any server pod.
type ForgePublishTokenStore interface {
	Register(token string, g ForgePublishGrant) error
	Revoke(token string)
	// expireIn brings a live grant's expiry forward to now+d, never pushes it
	// out, and does nothing for an unknown token. It is how a grant stops
	// outliving its run without being revoked outright — the merge-gate
	// repair still needs to read it for its own window after the run dies.
	expireIn(token string, d time.Duration)
	lookup(token string) (ForgePublishGrant, bool)
	// update applies fn to a live grant and stores it back with its expiry
	// untouched: never resurrecting a grant that expired or was revoked
	// meanwhile, never pushing its expiry out. It reports false for an
	// unknown token, and an error when the store could not answer.
	update(token string, fn func(*ForgePublishGrant)) (bool, error)
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

// expireIn brings the grant's expiry forward. Never later: a caller shortening
// a window must not be able to extend one.
func (r *ForgePublishTokenRegistry) expireIn(token string, d time.Duration) {
	at := r.now().Add(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.tokens[token]
	if !ok || !at.Before(g.ExpiresAt) {
		return
	}
	g.ExpiresAt = at
	r.tokens[token] = g
}

func (r *ForgePublishTokenRegistry) update(token string, fn func(*ForgePublishGrant)) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.tokens[token]
	if !ok || r.now().After(g.ExpiresAt) {
		return false, nil
	}
	expires := g.ExpiresAt
	fn(&g)
	g.ExpiresAt = expires
	r.tokens[token] = g
	return true, nil
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
	// AuditedSHA pins the verdict to the revision the bot actually read. The
	// endpoint resolves the PR head itself, so without this pin the two can
	// differ — the bot audits A, a push lands B, and A's verdict certifies B.
	// When set and no longer the head, the status is REFUSED rather than
	// retargeted: a gate that follows the head certifies whatever arrived last.
	// Absent, the status still posts and the response reports it unpinned.
	AuditedSHA string `json:"audited_sha,omitempty"`
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
	// GateSHAUnpinned reports a gate that carried no audited_sha, so the status
	// landed on whatever head the endpoint resolved. It is the measurement that
	// says when refusing an unpinned gate outright becomes safe: while any
	// bundle in the fleet still omits the pin, this is true somewhere.
	GateSHAUnpinned bool `json:"gate_sha_unpinned,omitempty"`
	// SkippedReason explains a publish that did not land (a forge error).
	// Present with published=false; the gate may still have been posted.
	SkippedReason string `json:"skipped_reason,omitempty"`
	// DroppedComments lists comments the request carried that could not be
	// anchored (no path, no positive line, or no body). They are dropped
	// rather than fatal, but reported so the caller's output contract can be
	// fixed instead of the loss going unnoticed.
	DroppedComments []string `json:"dropped_comments,omitempty"`
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
	// No runOwnsGrant here, and that is the contract rather than an omission:
	// the request names no run — the token IS the authority on this endpoint —
	// so there is no run tenant to compare the grant against. The crossing is
	// refused where both facts exist instead, at injectForgePublishVars, which
	// is why a run can never be handed a grant of another team to present here.

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
	// Unanchorable comments are DROPPED, not fatal. Rejecting the batch on
	// one bad entry cost a whole review — and, because the merge gate is
	// posted after a successful publish, the gate status with it. A reviewer
	// that legitimately produces a finding it cannot anchor to a file+line
	// (an architectural observation, a cross-file concern) must not be able
	// to take the anchored findings and the merge signal down with it.
	//
	// They are reported back in the response (dropped_comments) rather than
	// swallowed: the bot's output contract is still wrong, and a silent drop
	// would hide that.
	kept := make([]publishReviewComment, 0, len(req.Comments))
	var droppedComments []string
	for i, c := range req.Comments {
		if strings.TrimSpace(c.Path) == "" || c.Line <= 0 || strings.TrimSpace(c.Body) == "" {
			droppedComments = append(droppedComments,
				fmt.Sprintf("comment %d: needs a path, a positive line and a body", i))
			continue
		}
		kept = append(kept, c)
	}
	req.Comments = kept
	// Everything unpublishable is only a hard error when NOTHING is left to
	// say — a review with no summary and no anchorable comment has no
	// content, which is a different failure from a malformed one.
	if strings.TrimSpace(req.Summary) == "" && len(req.Comments) == 0 {
		httpError(w, http.StatusBadRequest, "nothing publishable: summary is empty and every comment was unanchorable (%s)",
			strings.Join(droppedComments, "; "))
		return
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

	res, reviewErr := rc.CreatePullReview(r.Context(), grant.Repo, number, review)
	if reviewErr != nil {
		// The gate is a COUNT the bot already computed; it does not depend on
		// the comments landing. Post it before giving up, then report the
		// publish failure. Coupling the two meant one forge hiccup left the
		// PR's required check permanently absent — indistinguishable from
		// "never reviewed", and unblockable by another review.
		decision := newGateDecision(s.gateNow())
		gate := s.postGateStatus(r.Context(), conn, grant.Repo, number, req.Gate, "", decision)
		if !s.deferGateVerdict(token, grant.Repo, number, req.Gate, "", decision, &gate) && !gate.posted && !gate.superseded {
			s.releaseGateDecision(r.Context(), gate.markKey, decision)
		}
		s.recordGateVerdict(token, gate, decision)
		s.recordGateRefusal(token, gate)
		if s.logger != nil {
			s.logger.Warn("forge publish: %s %s#%d review failed (%v); gate posted=%v state=%q",
				conn.Provider, grant.Repo, number, reviewErr, gate.posted, gate.state)
		}
		writeJSONStatus(w, http.StatusBadGateway, publishReviewResponse{
			Published:       false,
			Provider:        string(conn.Provider),
			SkippedReason:   fmt.Sprintf("create pull review: %v", reviewErr),
			DroppedComments: droppedComments,
			GatePosted:      gate.posted,
			GateState:       gate.state,
			GateContext:     gate.context,
			GateSHA:         gate.sha,
			GateError:       gate.errText,
			GateSHAUnpinned: gate.shaUnpinned,
		})
		return
	}
	if s.logger != nil {
		s.logger.Info("forge publish: %s %s#%d → %d inline comment(s) (verified=%v fallback=%q)",
			conn.Provider, grant.Repo, number, res.CommentsPosted, res.Verified, res.Fallback)
	}

	// Merge gate: post the deterministic revi/review commit status on the PR
	// head SHA. Additive — a failure here never fails the publish (the review
	// already landed), it is reported in the response + logged.
	decision := newGateDecision(s.gateNow())
	gate := s.postGateStatus(r.Context(), conn, grant.Repo, number, req.Gate, res.URL, decision)
	if !s.deferGateVerdict(token, grant.Repo, number, req.Gate, res.URL, decision, &gate) && !gate.posted && !gate.superseded {
		s.releaseGateDecision(r.Context(), gate.markKey, decision)
	}
	s.recordGateVerdict(token, gate, decision)
	s.recordGateRefusal(token, gate)
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
		DroppedComments:   droppedComments,
		GatePosted:        gate.posted,
		GateState:         gate.state,
		GateContext:       gate.context,
		GateSHA:           gate.sha,
		GateError:         gate.errText,
		GateSHAUnpinned:   gate.shaUnpinned,
	})

	// Reviewer bookkeeping — the two halves of the re-request gesture, one
	// per forge. GitLab's request has to be OPENED (self-assign as reviewer:
	// what makes the forge-native "Re-request review" button exist on the
	// reviewed MR, clicking it relaunching through the inbound webhook's
	// on-demand lane). GitHub's has to be CLOSED (withdraw the armed logins'
	// pending request, which GitHub never lifts by itself because the review
	// is posted by the App, not by the requested account) — without which
	// the pastille stays pending forever and re-adding the reviewer is not a
	// repeatable gesture.
	//
	// STRICTLY behind the gate status and the response: both are cosmetic,
	// and their forge round-trips must never sit in front of a required
	// check (a client disconnect in that window used to kill the gate post
	// on a review that had landed). Detached from the request context (a
	// disconnect must not cancel them), bounded, and recover-carrying via
	// goSafe. Providers whose admin client doesn't carry a capability are a
	// deliberate non-implementation — see forge.ReviewerAssigner and
	// forge.ReviewRequestWithdrawer.
	// Each half takes its OWN deadline off the one detached parent. They are
	// never both implemented on a single provider today (the capability pins
	// in forge_publish_gate_test.go keep them disjoint), but one shared
	// deadline would make that a load-bearing coincidence: a hung assigner
	// would hand the withdrawal an already-dead context.
	//
	// They stay SEQUENTIAL in one goroutine, which means a panic in the first
	// skips the second (goSafe recovers at the top, not between them). Left
	// that way deliberately: on a given provider only one of the two resolves
	// a capability at all, and the withdrawal is idempotent — the next
	// publish on that PR withdraws what this one missed. A recover between
	// the halves would buy nothing an already best-effort, self-healing call
	// does not already have.
	settleParent := context.WithoutCancel(r.Context())
	saCtx, saCancel := context.WithTimeout(settleParent, 30*time.Second)
	s.goSafe("forge-publish-reviewer-settle", func() {
		defer saCancel()
		s.selfAssignReviewer(saCtx, conn, grant.Repo, number)
		wCtx, wCancel := context.WithTimeout(settleParent, 30*time.Second)
		defer wCancel()
		s.withdrawReviewRequests(wCtx, conn, grant, number)
	})
}

// defaultGateContext is the commit-status check name the merge gate posts
// under when the bot pins none. Kept BOT-AGNOSTIC (the engine must not bake a
// specific bot's persona in — see CLAUDE.md): a reviewer bot names its own
// check via gate.context (Revi sends "revi/review"); this neutral fallback
// only applies when a gate arrives with an empty context.
const defaultGateContext = "merge-gate"

// isCommitID reports whether s is shaped like a git object id: 7 to 40 hex
// characters. The floor matters — `equalSHA` matches a PREFIX, so a one- or
// two-character "pin" would match almost any head and the guard would wave
// through what it exists to stop. 7 is what git itself abbreviates to, and
// what the reviewer bundle's own `looks_like_sha` accepts.
func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// forgeGateClient is the capability the merge gate needs: resolve the PR head
// SHA, then post a commit status on it. Satisfied by the github/gitlab/forgejo
// admin clients (both methods live on the same *AdminClient).
type forgeGateClient interface {
	GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error)
	SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error
}

// selfAssignReviewer adds the connection's own identity to the PR/MR
// reviewer set through the forge.ReviewerAssigner capability, when the
// provider's admin client carries it. Best-effort by contract: the review
// already landed, so nothing here may fail the publish — a capability miss
// is a Debug (deliberate non-implementation), a forge refusal a Warn.
// The forgeReviewerAssignerFor field is a test seam; nil uses the real
// admin client.
func (s *Server) selfAssignReviewer(ctx context.Context, conn forge.Connection, repo string, number int) {
	var ra forge.ReviewerAssigner
	if s.forgeReviewerAssignerFor != nil {
		ra = s.forgeReviewerAssignerFor(ctx, conn)
	} else {
		admin, err := s.forgeAdminFor(ctx, conn)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("forge publish: %s %s#%d: cannot resolve admin client for reviewer self-assign: %v", conn.Provider, repo, number, err)
			}
			return
		}
		ra, _ = admin.(forge.ReviewerAssigner)
	}
	if ra == nil {
		if s.logger != nil {
			s.logger.Debug("forge publish: %s carries no reviewer self-assign capability — the re-request-review button rides the forge's own reviewer handling (see forge.ReviewerAssigner)", conn.Provider)
		}
		return
	}
	if err := ra.AddSelfAsPullReviewer(ctx, repo, number); err != nil {
		if s.logger != nil {
			s.logger.Warn("forge publish: %s %s#%d: reviewer self-assign failed (re-request button may be absent; /revi still re-reviews): %v", conn.Provider, repo, number, err)
		}
		return
	}
	if s.logger != nil {
		s.logger.Debug("forge publish: %s %s#%d: bot self-assigned as reviewer", conn.Provider, repo, number)
	}
}

// withdrawReviewRequests closes the gesture that armed this review: it
// removes the webhook's ReviewRequestLogins from the PR's requested
// reviewers, through the forge.ReviewRequestWithdrawer capability. On GitHub
// nothing else will — a review request is lifted only when the REQUESTED
// account submits the review, and the review is posted by <app_slug>[bot] —
// so without this the "review requested" pastille stays pending forever and
// re-adding the reviewer is not a repeatable gesture.
//
// Best-effort by contract, exactly like selfAssignReviewer: the review and
// the gate status already landed, so nothing here may fail the publish.
// The armed logins are resolved BEFORE any forge round-trip, so a webhook
// that never armed the lane — the overwhelming majority — costs two store
// reads and no forge call at all.
//
// It fires on EVERY successful publish, not only on the deliveries the
// re-request lane launched: the handler has no notion of which lane produced
// the review, a /revi comment or a board launch leaves an equally pending
// request when one was standing, and the read-then-intersect contract makes
// a withdrawal with nothing to withdraw a no-op.
//
// The forgeReviewRequestWithdrawerFor field is a test seam; nil uses the
// real admin client.
func (s *Server) withdrawReviewRequests(ctx context.Context, conn forge.Connection, grant ForgePublishGrant, number int) {
	logins, ok := s.armedReviewRequestLogins(ctx, grant)
	if !ok {
		return
	}

	var rw forge.ReviewRequestWithdrawer
	if s.forgeReviewRequestWithdrawerFor != nil {
		rw = s.forgeReviewRequestWithdrawerFor(ctx, conn)
	} else {
		admin, err := s.forgeAdminFor(ctx, conn)
		if err != nil {
			s.logWarn("forge publish: %s %s#%d: cannot resolve admin client to withdraw the review request: %v",
				conn.Provider, grant.Repo, number, err)
			return
		}
		rw, _ = admin.(forge.ReviewRequestWithdrawer)
	}
	if rw == nil {
		s.logDebug("forge publish: %s carries no review-request withdrawal capability — the forge lifts the request itself when the requested account posts (see forge.ReviewRequestWithdrawer)", conn.Provider)
		return
	}

	removed, err := rw.WithdrawPullReviewRequests(ctx, grant.Repo, number, logins)
	if err != nil {
		// A connection short of the grant lands here as a
		// *forge.PermissionError naming pull_requests:write. The review is
		// already published and the gate already posted — degrading either
		// over a pending pastille would be the worse failure.
		s.logWarn("forge publish: %s %s#%d: review request not withdrawn for %v (the request stays pending, so the reviewer must be removed by hand before it can be re-added; needs pull_requests:write): %v",
			conn.Provider, grant.Repo, number, logins, err)
		return
	}
	if len(removed) == 0 {
		s.logDebug("forge publish: %s %s#%d: none of %v had a pending review request — nothing to withdraw",
			conn.Provider, grant.Repo, number, logins)
		return
	}
	s.logDebug("forge publish: %s %s#%d: withdrew the review request of %v — the gesture is re-armable",
		conn.Provider, grant.Repo, number, removed)
}

// armedReviewRequestLogins resolves the review identities the publish grant's
// repo has armed, via the webhook config behind its integration row. false
// (with a Debug) when the lane is not armed — which is the common case and
// must cost no forge round-trip.
//
// Read WITHOUT the tenant filter and cross-checked against the grant's team
// after the fact, the way repoLaunchPolicy does: the store is keyed by the
// grant's own tenant, and a config answering for another one would arm this
// repo's write from a different team's settings.
func (s *Server) armedReviewRequestLogins(ctx context.Context, grant ForgePublishGrant) ([]string, bool) {
	if s.forgeIntegrations == nil || s.webhookConfigs == nil {
		return nil, false
	}
	integration, err := s.forgeIntegrations.GetByConnRepo(store.WithoutTenantFilter(ctx), grant.TeamID, grant.ConnectionID, grant.Repo)
	if err != nil || integration.WebhookID == "" {
		s.logDebug("forge publish: %s: no webhook integration behind the publish grant — no review request to withdraw", grant.Repo)
		return nil, false
	}
	cfg, err := s.webhookConfigs.Get(store.WithoutTenantFilter(ctx), integration.WebhookID)
	if err != nil || cfg.TenantID != grant.TeamID {
		s.logDebug("forge publish: %s: webhook config %s unreadable or foreign to team %s — no review request to withdraw",
			grant.Repo, integration.WebhookID, grant.TeamID)
		return nil, false
	}
	logins := cfg.NormalizedReviewRequestLogins()
	if len(logins) == 0 {
		s.logDebug("forge publish: %s: review_request_logins is empty — the re-request lane is not armed, nothing to withdraw", grant.Repo)
		return nil, false
	}
	return logins, true
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

// recordGateVerdict writes a posted verdict on the grant that posted it, so
// the reconciler can tell the run's owed verdict is on the forge without
// reading it back (settleOwnVerdict). Best-effort: the verdict is already on
// the forge, and without a record the reconciler reads the forge instead.
// A record that fails must not leave an EARLIER one standing — the reconciler
// would trust a verdict the forge no longer shows — so the earlier record is
// cleared, and the line says whether that landed.
//
// A verdict that lands also retires the grant's deferral on the same check,
// unless that one was decided later: it is still owed. A deferral on another
// check is another check's verdict.
func (s *Server) recordGateVerdict(token string, gate gateOutcome, decision gateDecision) {
	if !gate.posted || s.forgePublishTokens == nil {
		return
	}
	v := &gateVerdict{SHA: gate.sha, Context: gate.context, State: gate.state, At: time.Now().UTC()}
	_, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		g.Verdict = v
		// A posted verdict retires any earlier refusal: the run DID certify
		// in the end, and a refusal is episode-scoped — no later reconcile
		// may diagnose one that is moot (clearGateRefusal's twin).
		g.Refusal = nil
		if d := g.Deferred; d != nil && gateContextOf(&d.Gate) == gate.context && !d.Decision.newerThan(decision) {
			g.Deferred = nil
		}
	})
	if err == nil {
		return
	}
	_, clearErr := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) { g.Verdict = nil })
	if s.logger == nil {
		return
	}
	if clearErr != nil {
		s.logger.Warn("forge gate: %s posted on %s but not recorded on its grant (%v), and an earlier record could not be cleared (%v) — the reconciler may trust it over the forge",
			gate.state, shortSHA(gate.sha), err, clearErr)
		return
	}
	s.logger.Warn("forge gate: %s posted on %s but not recorded on its grant — the reconciler will read the forge for it: %v", gate.state, shortSHA(gate.sha), err)
}

// recordGateRefusal writes a REFUSED verdict on the grant that carried it, so
// the reconciler can answer the head the refusal left bare — the refusal
// itself posted nothing, and the run finishing cleanly is exactly why no
// other lane ever learns of it (#1632). Best-effort like recordGateVerdict:
// a record that fails leaves the reconciler on the pre-#1632 behaviour (a
// moved head stands down, an unmoved one reads "review died"), which is wrong
// but never unsafe — so it is a Warn, not a failed publish.
func (s *Server) recordGateRefusal(token string, gate gateOutcome) {
	if gate.refusal == nil || s.forgePublishTokens == nil {
		return
	}
	if _, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		g.Refusal = gate.refusal
	}); err != nil && s.logger != nil {
		s.logger.Warn("forge gate: %s was refused (%s) but the refusal could not be recorded on its grant: %v — the reconciler cannot tell it from a death",
			gate.context, gate.errText, err)
	}
}

// clearGateRefusal retires a recorded refusal once the reconciler has
// consumed or settled the episode it belongs to. A refusal is episode-scoped
// state on an episode-blind store: the endpoint that records it knows no run
// (the token is the authority), so the record cannot be stamped with the
// episode — clearing at consumption is what keeps a later dead episode of
// the same run from inheriting a diagnosis it never earned. The clear is a
// compare-and-swap on the CONSUMED refusal's identity (like replaceDeferral):
// a refusal recorded after it — a fresher episode, inside the post→clear
// window — is not this pass's to retire, or "honored at most once" degrades
// into "possibly zero". Best-effort: a failed clear costs a wrong diagnosis
// on one later reconcile, never a wrong write.
func (s *Server) clearGateRefusal(token string, consumed *gateRefusal) {
	if s.forgePublishTokens == nil || consumed == nil {
		return
	}
	if _, err := s.forgePublishTokens.update(token, func(g *ForgePublishGrant) {
		if g.Refusal != nil && g.Refusal.At.Equal(consumed.At) &&
			strings.EqualFold(g.Refusal.AuditedSHA, consumed.AuditedSHA) &&
			strings.EqualFold(g.Refusal.Context, consumed.Context) {
			g.Refusal = nil
		}
	}); err != nil && s.logger != nil {
		s.logger.Warn("forge gate: a consumed refusal could not be cleared from its grant: %v — a later episode of the run may inherit the diagnosis", err)
	}
}

// boundedRunes truncates s to at most n runes, ellipsis included — the
// status-description budget the refusal reason feeds. n <= 0 yields "".
func boundedRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

type gateOutcome struct {
	requested bool   // a gate was requested (enabled)
	posted    bool   // the status landed on the forge
	state     string // "success" | "failure" (when posted)
	context   string // the status check name used
	sha       string // the head SHA the status was posted on
	errText   string // why not posted (when !posted)
	// shaUnpinned records a gate that carried no audited_sha: the status landed
	// on whatever head was resolved, with nothing tying it to what was read.
	shaUnpinned bool
	// rateLimited reports a refusal the forge gave for a rate limit, and
	// resetAt when it said its wait ends (zero: it said nothing).
	rateLimited bool
	resetAt     time.Time
	// transient reports a refusal worth retrying: the forge failed on its own
	// side (5xx), or never answered (noteTransient), or the verdict could not
	// be ordered against newer ones.
	transient bool
	// superseded reports a verdict not posted because a newer one owns the
	// head (claimGateDecision).
	superseded bool
	// refusal records a verdict the gate REFUSED to post for its pin — stale
	// or unreadable audited_sha — so the reconciler can answer the head the
	// refusal left bare with the right diagnosis (#1632). Nil otherwise.
	refusal *gateRefusal
	// markKey is the verdict-order mark this decision claimed (set once the
	// claim was granted), for the caller that must release it again.
	markKey string
}

// postGateStatus posts the deterministic merge-gate commit status. It resolves
// the PR head SHA (so the status lands on the exact revision under review),
// maps the bot's blocking-count verdict to success/failure, and writes the
// commit status through the connection's live admin client. Every failure is
// reported (never silently swallowed) but non-fatal to the publish.
func (s *Server) postGateStatus(ctx context.Context, conn forge.Connection, repo string, number int, gate *publishReviewGate, reviewURL string, decision gateDecision) gateOutcome {
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
		out.noteRateLimit(err)
		out.noteTransient(err)
		return out
	}
	if gc == nil {
		out.errText = "provider " + string(conn.Provider) + " has no commit-status capability"
		return out
	}
	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil {
		out.errText = "resolve head sha: " + err.Error()
		out.noteRateLimit(err)
		out.noteTransient(err)
		return out
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		out.errText = "forge returned no head sha for the PR"
		return out
	}
	// A pull request that already merged or closed takes no verdict. The head
	// resolved above is the PRE-merge revision — nobody merges it any more,
	// nothing consults the check, and the branch it describes is scheduled for
	// deletion — so a status there is a statement about a revision that left
	// the merge decision. This is the ONE place every bot's gate status
	// crosses, which is what keeps the rule out of each bot's tail.
	//
	// An EMPTY state is a provider that does not report one, never a closure:
	// the same predicate the relaunch, auto-fix and reconcile lanes use, for
	// the same reason — suppressing a required check on a guess deadlocks the
	// pull request.
	if pr.State != "" && pr.State != "open" {
		out.errText = "pull request is " + pr.State + " — no gate status on a head that left the merge decision"
		return out
	}
	// A verdict is a statement about the revision the bot READ, and the head was
	// resolved just now — between the two, a push may have landed. Certifying the
	// current head with a verdict about an earlier one is how an unaudited
	// revision clears a required check. Refuse rather than retarget: the same
	// chokepoint every bot's gate status crosses, so the rule stays out of each
	// bot's tail.
	//
	// An ABSENT pin still posts. Refusing it would blank the required check on
	// every repository whose bundle predates the pin, which deadlocks the very
	// pull requests the gate exists to protect. It is not silent: the outcome
	// says unpinned and the caller logs it, and that signal is what turns
	// "refuse unpinned too" into a measurable decision.
	// Normalised ONCE: the emptiness check, the comparison and the status all
	// have to read the same value, or the rule differs from the thing it guards.
	head := strings.TrimSpace(pr.HeadSHA)
	audited := strings.TrimSpace(gate.AuditedSHA)
	switch {
	case gate.AuditedSHA == "":
		// Absent — the decided legacy post.
		out.shaUnpinned = true
	case !isCommitID(audited):
		// PRESENT but unreadable is a third state, not "absent": a bot that
		// meant to pin and rendered an unsubstituted template, a ref name or a
		// blank would otherwise degrade silently to the unpinned certificate.
		// It is also not "the head moved" — reporting it that way sends the
		// reader diffing two revisions, or hunting a push that never happened.
		out.errText = "audited_sha " + strconv.Quote(audited) + " is not a commit id (7-40 hex) — " +
			"refusing to certify on a pin that cannot be read"
		out.refusal = &gateRefusal{
			AuditedSHA: audited, HeadSHA: head, Context: out.context,
			Reason: "its audited_sha " + strconv.Quote(boundedRunes(audited, 24)) + " is not a commit id",
			At:     s.gateNow(),
		}
		return out
	case !equalSHA(head, audited):
		// Full SHAs on both sides: this message's whole job is to tell two
		// revisions apart, and abbreviating both is how it names one twice.
		out.errText = "the head moved since the audit (audited " + audited +
			", head is now " + head + ") — no status on a revision nobody audited"
		out.refusal = &gateRefusal{
			AuditedSHA: audited, HeadSHA: head, Context: out.context,
			Reason: "it audited " + shortSHA(audited) + " but the head moved to " + shortSHA(head),
			At:     s.gateNow(),
		}
		return out
	}
	out.sha = head

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

	// The check is claimed for this decision before the post, so a verdict
	// decided earlier and posted later does not overwrite a newer one.
	markKey := gateDecisionKey(conn, repo, out.sha, out.context)
	mark := gateMark{Decision: decision, Status: gateMarkStatus{State: string(state), Description: desc, TargetURL: reviewURL}}
	switch ok, err := s.claimGateDecision(ctx, markKey, mark); {
	case err != nil:
		out.errText = "order the verdict against newer ones: " + err.Error()
		out.transient = true
		return out
	case !ok:
		out.errText = "a newer verdict on " + out.sha + " supersedes it"
		out.superseded = true
		return out
	}
	out.markKey = markKey
	if err := gc.SetCommitStatus(ctx, repo, out.sha, forge.CommitStatus{
		State:       state,
		Context:     out.context,
		Description: desc,
		TargetURL:   reviewURL,
	}); err != nil {
		out.errText = "set commit status: " + err.Error()
		out.noteRateLimit(err)
		out.noteTransient(err)
		if !out.rateLimited && !out.transient {
			// A refusal a retry repeats: this verdict will neither post nor
			// wait, and its claim must not supersede older deferred verdicts
			// nobody else will answer.
			s.releaseGateDecision(ctx, markKey, decision)
		}
		return out
	}
	out.posted = true
	s.reassertNewerVerdict(ctx, gc, markKey, repo, out.sha, out.context, decision)
	// Warned HERE rather than at the caller: a certificate that landed without
	// a pin is the measurement the "absent still posts" decision rests on, and
	// the caller has two exits — the review-failure branch answers 502 and
	// never reached this line's counterpart, which is the one path where an
	// unpinned status lands and nothing says so.
	if out.shaUnpinned && s.logger != nil {
		s.logger.Warn("forge gate: %s %s @%s posted UNPINNED (no audited_sha) — the verdict is not tied to the revision the bot read; bump this repo's bundle",
			out.context, repo, out.sha)
	}
	return out
}

// postApproveGateStatus writes the operator's manual force-green through the
// same verdict-order authority every other gate-status writer crosses
// (#1590): the approve is a DECISION taken at click time, claimed before the
// write — so a reconciler's synthetic failure anchored at the run's death
// (before the click) is refused the claim and can never paint over it, and a
// deferred replay reusing its ORIGINAL decision is likewise superseded.
// releaseGateDecision on a failed write, reassertNewerVerdict after a landed
// one (which can never re-post a synthetic — the guard there). superseded
// means a newer decision claimed the check between the click and the claim:
// nothing was written, and the caller must say so rather than report an
// approval that is not on the forge.
func (s *Server) postApproveGateStatus(ctx context.Context, conn forge.Connection, gc forgeGateClient, repo, sha, gateCtx, desc, targetURL string) (superseded bool, err error) {
	decision := newGateDecision(s.gateNow())
	markKey := gateDecisionKey(conn, repo, sha, gateCtx)
	mark := gateMark{Decision: decision, Status: gateMarkStatus{
		State: string(forge.CommitStateSuccess), Description: desc, TargetURL: targetURL,
	}}
	switch ok, claimErr := s.claimGateDecision(ctx, markKey, mark); {
	case claimErr != nil:
		return false, fmt.Errorf("order the approval against newer verdicts: %w", claimErr)
	case !ok:
		return true, nil
	}
	if err := gc.SetCommitStatus(ctx, repo, sha, forge.CommitStatus{
		State:       forge.CommitStateSuccess,
		Context:     gateCtx,
		Description: desc,
		TargetURL:   targetURL,
	}); err != nil {
		// The approval will neither post nor wait: free its claim.
		s.releaseGateDecision(ctx, markKey, decision)
		return false, err
	}
	s.reassertNewerVerdict(ctx, gc, markKey, repo, sha, gateCtx, decision)
	return false, nil
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

// forgePublishVars is the COMPLETE set of launch vars the grant path mints —
// store.ServerMintedLaunchVars, the one list the mint, the withdrawal below and
// every client write path (store.DropServerMintedVars) read, so none can drift
// from another.
func forgePublishVars() [4]string {
	return store.ServerMintedLaunchVars
}

// forgePublishVarURL / forgePublishVarToken are the launch vars the server
// injects; a bot opts in by declaring them in its vars: block (undeclared
// launch vars are dropped by the IR, so blind injection is safe).
// forgePublishVarPRState is the read half's endpoint, injected alongside them
// and authenticated by the SAME token: a delivery tail asks it whether the
// pull request is still open before it pushes onto its branch.
const (
	forgePublishVarURL       = store.ForgePublishURLVar
	forgePublishVarToken     = store.ForgePublishTokenVar
	forgePublishVarPRState   = store.ForgePRStateURLVar
	forgePublishVarPreflight = store.ForgeDeliveryPreflightURLVar
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
//
// A caller-pinned token is honoured rather than overwritten — but only for the
// launching team. The publish endpoint holds no run, so there the token IS the
// authority and it cannot tell whose run presents it; this is the only place
// the two facts (which team launches, which team the grant names) are both in
// hand, which makes it the door. A pin resolving to ANOTHER team's grant is
// refused and nothing launches.
//
// An UNRESOLVABLE pin is not a crossing and stays honoured: the default token
// registry is in-memory, so a restart empties it, and refusing there would
// turn a stale token into a failed launch instead of a run that merely cannot
// publish (the endpoint answers 401).
// trust is the launch's own verdict on who wrote the code the run will hold.
// It is a PARAMETER and not a field read off a run because at this point in
// the tail there IS no run — the grant is minted before the launcher is
// called, so "read the marker from the run document" is not available here.
// An untrusted launch is refused a grant outright, and the refusal is an
// error rather than a silent "no endpoint bound": the whole point of the
// lane that sets it is that its output never reaches the forge, and a
// capability that goes missing quietly is one nobody notices coming back.
func (s *Server) injectForgePublishVars(ctx context.Context, teamID, preferredConnID, botID string, vars map[string]string, r *http.Request, trust store.RunTrust) (map[string]string, mintedGrant, error) {
	prURL := strings.TrimSpace(vars["pr_url"])
	// Ahead of ALL THREE early returns — the unwired-server one just below,
	// the no-pr_url one, and the caller-pin one. A check placed after any of
	// them lets a caller hand an untrusted launch a grant simply by putting
	// one in vars: each returns the vars it was given. The unwired case is
	// the least obvious and not hypothetical — a deployment with no forge
	// connections mints nothing, so nothing would overwrite a caller-supplied
	// forge_publish_url naming another deployment entirely.
	//
	// Deleting from a map is a no-op on absent keys, so placing this first
	// costs a trusted launch nothing. Trusted() and not "== fork", so an
	// unrecognised trust loses the grant too.
	if !trust.Trusted() {
		for _, k := range forgePublishVars() {
			delete(vars, k)
		}
		// The WITHDRAWAL is the guarantee; it is not a reason to refuse the
		// launch. An earlier revision returned an error here, and that was a
		// defect of exactly the shape this repo warns about — a hardening
		// that closes the path it exists to serve: reviewPRVars always sets
		// pr_url, so EVERY fork-lane review would have failed to launch, and
		// the lane could never have worked. A grant-less review is precisely
		// what the lane is.
		//
		// Typed, greppable and logged all the same — silence is the thing
		// forbidden, not continuing. The loud half lives where something
		// actually ASKS to publish: runOwnsGrant refuses and audits there,
		// holding a run, which is the only place the question is real.
		if prURL != "" {
			if s.logger != nil {
				s.logger.Info("forge gate: %s: %s: no publish grant minted for an untrusted workspace (trust=%q) — the run reviews without one",
					grantUntrustedMintReason, prURL, string(trust))
			}
			s.auditSystem(teamID, "forge-gate", auditActionGrantUntrustedMint, "launch", prURL, map[string]any{
				"reason": grantUntrustedMintReason,
				"trust":  string(trust),
				"bot":    strings.TrimSpace(botID),
			})
		}
		return vars, "", nil
	}
	dropMaskedGrant(vars)
	if s == nil || s.forgePublishTokens == nil || s.forgeConnections == nil {
		return vars, "", nil
	}
	if prURL == "" {
		return vars, "", nil
	}
	if pinned := strings.TrimSpace(vars[forgePublishVarToken]); pinned != "" {
		if grant, ok := s.forgePublishTokens.lookup(pinned); ok &&
			!strings.EqualFold(strings.TrimSpace(grant.TeamID), strings.TrimSpace(teamID)) {
			return vars, "", fmt.Errorf("%w: the launch pins a forge publish grant minted for team %q, but this launch belongs to team %q",
				errForgePublishGrantTenant, grant.TeamID, teamID)
		}
		// The caller pinned its own grant — don't overwrite, and this launch
		// minted nothing: a token it merely found is not its to revoke. Mark
		// it shared: two runs now publish with one grant, and neither run's
		// verdict nor its end may cut it back (shareGrant). A grant that
		// cannot be marked, or that was already cut back to its post-run
		// grace, is refused like one that cannot be minted: this run's verdict
		// would be unpostable once the short grace ran out. A grant that is
		// not found is honoured and warned about instead — the in-memory
		// registry forgets every grant on a restart, so "not found" may be a
		// stale token, and a run that cannot publish beats a launch that
		// fails. A launch without the pin mints its own grant.
		cutBack, found, err := s.shareGrant(pinned)
		switch {
		case err != nil:
			return vars, "", fmt.Errorf("%w: marking the pinned grant shared: %w", errForgePublishGrantUnavailable, err)
		case cutBack:
			return vars, "", fmt.Errorf("%w: the pinned grant was cut back to its post-run grace — launch without %s to mint a fresh one",
				errForgePublishGrantUnavailable, forgePublishVarToken)
		case !found && s.logger != nil:
			s.logger.Warn("forge publish: a launch pins a publish grant that is expired or revoked — its publish will be refused; launch without %s to mint a fresh one",
				forgePublishVarToken)
		}
		return vars, "", nil
	}
	base := s.publicBaseURL(r)
	if base == "" {
		if s.logger != nil {
			s.logger.Warn("forge publish: no public base URL (set PublicURL); deterministic review publishing disabled for this launch")
		}
		return vars, "", nil
	}
	host, repo, _, err := forge.ParsePullURL(prURL)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("forge publish: %v; deterministic review publishing disabled for this launch", err)
		}
		return vars, "", nil
	}
	conn, ok := s.forgeConnectionForPR(ctx, teamID, preferredConnID, host, repo)
	if !ok {
		if s.logger != nil {
			s.logger.Warn("forge publish: no team %s connection covers %s/%s; deterministic review publishing disabled for this launch", teamID, host, repo)
		}
		return vars, "", nil
	}
	token := newBoardMCPToken()
	if token == "" {
		return vars, "", nil
	}
	if err := s.forgePublishTokens.Register(token, ForgePublishGrant{TeamID: teamID, Bot: strings.TrimSpace(botID), ConnectionID: conn.ID, Repo: repo}); err != nil {
		// A launch that reaches here has a connection covering the PR: it is
		// gating-shaped, and the caller is about to claim the repo's gate
		// context on this head. Proceeding without a grant is the "pending
		// forever" shape — the run cannot publish its verdict, and the
		// reconciler that repairs a dead claim reads the grant to know where
		// to speak, so it abstains ("not a gating run") and nothing ever
		// answers the claim. Refuse instead: the claim is posted AFTER the
		// launch, so a launch refused here leaves nothing to release.
		if s.logger != nil {
			s.logger.Error("forge publish: %v; refusing the launch on %s/%s rather than starting a run that cannot publish its verdict", err, host, repo)
		}
		return vars, "", fmt.Errorf("%w: %s/%s: %w", errForgePublishGrantUnavailable, host, repo, err)
	}
	if vars == nil {
		vars = map[string]string{}
	}
	vars[forgePublishVarURL] = base + "/api/v1/forge/publish-review"
	vars[forgePublishVarPRState] = base + "/api/v1/forge/pull-request"
	vars[forgePublishVarPreflight] = base + "/api/v1/forge/delivery-preflight"
	vars[forgePublishVarToken] = token
	return vars, mintedGrant(token), nil
}

// dropMaskedGrant deletes from vars a server-minted secret var whose value is
// the mask a read surface showed: a client re-sending the inputs it was shown
// pins no grant, and no launch may store the mask as its token.
func dropMaskedGrant(vars map[string]string) {
	for _, name := range store.ServerMintedSecretVars {
		if strings.TrimSpace(vars[name]) == store.RedactedLaunchVar {
			delete(vars, name)
		}
	}
}

// mintedGrant is the token THIS launch minted, empty when it minted none. It
// travels out of the mint instead of being reconstructed by comparing the
// launch vars before and after: every other token in those vars — an
// operator's pin, one a repo's launch policy fills in, one a board card
// carried — belongs to someone else, and a launch that fails must not end it.
type mintedGrant string

// revokeUnlaunchedGrant revokes the grant this launch minted, once the launch
// is known to have started no run: nothing holds the token, and an orphan
// grant would occupy the registry until its TTL.
func (s *Server) revokeUnlaunchedGrant(minted mintedGrant) {
	tok := strings.TrimSpace(string(minted))
	if tok == "" || s == nil || s.forgePublishTokens == nil {
		return
	}
	s.forgePublishTokens.Revoke(tok)
}

// withoutCardGrant is a board card's bot_args made ready to launch. A publish
// grant is minted per launch and never rides a card (applyPRLaunchContext), so
// the token a card carries — the mask a view showed, a grant that expired, an
// earlier grant of the card's own team — is dropped and the launch mints its
// own. A live grant of ANOTHER team is refused instead
// (errForgePublishGrantTenant): a crossing to answer, never a token to launder
// into a fresh grant. A server with no grant registry mints none, so a token
// there was pinned by its operator and only the mask is dropped. vars is never
// mutated.
func (s *Server) withoutCardGrant(teamID string, vars map[string]string) (map[string]string, error) {
	tok := strings.TrimSpace(vars[forgePublishVarToken])
	switch {
	case s == nil || s.forgePublishTokens == nil:
		return vars, nil
	case tok == "" || tok == store.RedactedLaunchVar:
		// Including the ENDPOINTS: they are minted with the token and name
		// where it is spent, so a card that carries them without a live
		// grant would aim this launch's own grant at the card's URL.
		return store.DropServerMintedVars(vars), nil
	}
	if grant, ok := s.forgePublishTokens.lookup(tok); ok &&
		!strings.EqualFold(strings.TrimSpace(grant.TeamID), strings.TrimSpace(teamID)) {
		return vars, fmt.Errorf("%w: the card carries a forge publish grant minted for team %q, but it launches for team %q",
			errForgePublishGrantTenant, grant.TeamID, teamID)
	}
	return store.DropServerMintedVars(vars), nil
}

// applyPRLaunchContext gives a launch that targets a pull request the two
// things only the server can supply: the target repo's launch policy — which
// is where the shared gate_context a required check is named by lives — and a
// forge-publish grant. It is the composition for lanes that hold no webhook
// config: the cloud board coordinator, which launches a card from its BotArgs
// alone, and the studio/API launch. Without it a bot pushes its commits and
// then has no endpoint to post its verdict or its gate status to, leaving the
// repo's required check stale on a head nobody will re-judge.
//
// Resolved at launch and deliberately never carried on a card: a grant
// expires, so a card claimed hours after it was created would hold a dead
// token, and a board document is the wrong place to persist a credential at
// rest. Reading the repo's policy here too means a re-provision between
// carding and claiming is honoured.
//
// The repo's policy goes UNDER the caller's vars: what is already on the
// launch is a deliberate per-run pin and outranks a repo-wide default.
//
// No AllowsBot check, unlike the webhook lanes: that list is admission control
// over which bots an EXTERNAL event may launch. Both callers here are already
// authenticated team surfaces, and the grant is scoped to the (team,
// connection, repo) the team is provisioned on and re-enforced at the publish
// endpoint.
//
// The fork guard, however, IS shared with the webhook lanes: the launch pair
// is the same (<base>.CloneURL + the PR's head branch), so a PR whose head
// is not proven to live in the base repo is refused here too — the returned
// error carries the refusal, and the caller launches nothing.
func (s *Server) applyPRLaunchContext(ctx context.Context, teamID, preferredConnID, botID string, vars map[string]string, r *http.Request) (map[string]string, mintedGrant, error) {
	prURL := strings.TrimSpace(vars["pr_url"])
	if prURL == "" {
		return vars, "", nil
	}
	if host, repo, number, err := forge.ParsePullURL(prURL); err == nil {
		if ri, ok := s.repoIntegrationFor(ctx, teamID, host, repo); ok {
			if preferredConnID == "" {
				// Pin the grant to the connection the policy came from.
				preferredConnID = ri.ConnectionID
			}
			fillVarGaps(vars, s.repoLaunchPolicy(ctx, ri, botID))
		}
		conn, proven, err := s.prLaunchForkGuard(ctx, teamID, preferredConnID, prURL, host, repo, number)
		if err != nil {
			return vars, "", err
		}
		if proven {
			// The grant is minted on the connection the PR was proven
			// through, so the identity that read the head is the one that
			// posts the verdict.
			preferredConnID = conn.ID
		}
	}
	// The launch surfaces that hold no webhook payload — the studio/API
	// launch and the cloud board coordinator — are operator-authenticated and
	// pass prLaunchForkGuard above, so the workspace they name is the
	// tenant's own. Stated rather than inferred: if one of them ever grows a
	// path that admits an outsider's tree, this is the line that has to
	// change with it.
	return s.injectForgePublishVars(ctx, teamID, preferredConnID, botID, vars, r, store.RunTrustDefault)
}

// errForgePublishGrantTenant marks a launch that pinned a forge publish grant
// belonging to another team — the operator's request is inadmissible, not a
// forge that could not be asked, so the HTTP lane answers 422.
var errForgePublishGrantTenant = errors.New("forge publish grant tenant mismatch")

// errPRLaunchForkGuard marks a launch the fork guard refused — the operator's
// pull request is not admissible, as opposed to a forge that could not be
// asked — so the HTTP lane answers 422 rather than 502.
var errPRLaunchForkGuard = errors.New("fork guard")

// errPRLaunchNoPullCapability marks a launch pinned to a connection whose
// provider cannot read pull requests at all: same-repo can never be proven
// through it, so no retry helps and the answer is not 502.
var errPRLaunchNoPullCapability = errors.New("connection cannot read pull requests")

// prLaunchContextStatus maps a refusal from applyPRLaunchContext onto the HTTP
// status a launch surface answers with. The whole table lives here because the
// FALL-THROUGH is the dangerous half: 502 tells the caller "the forge could not
// be asked, try again", so a refusal a retry can never fix has to be named or
// it reads as an outage the operator hammers.
//
//	errPRLaunchForkGuard             422  the pull request is not admissible
//	errForgePublishGrantTenant       422  the launch pins ANOTHER team's grant
//	errPRLaunchNoPullCapability      422  the connection cannot prove same-repo
//	errForgePublishGrantUnavailable  503  the server's own grant capacity, retriable as-is
//	anything else                    502  a forge that could not be asked
//
// The webhook lane already answers the tenant crossing 422
// (insertAndLaunchWebhook); this is the same verdict on the surfaces that hold
// no delivery.
func prLaunchContextStatus(err error) int {
	switch {
	case errors.Is(err, errPRLaunchForkGuard),
		errors.Is(err, errForgePublishGrantTenant),
		errors.Is(err, errPRLaunchNoPullCapability):
		return http.StatusUnprocessableEntity
	case errors.Is(err, errForgePublishGrantUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

// prLaunchForkGuard is the fork guard of the launch surfaces that hold no
// webhook payload — the studio/API launch and the cloud board coordinator —
// over the same launch pair the webhook lanes guard: <base>.CloneURL + the
// PR's head branch. It reads the PR through the team connection covering the
// PR's host+repo (the one the publish grant is minted on) and requires the
// head branch to be PROVEN to live in the base repo, refused with the webhook
// lanes' own wording (forkGuardRefusal): on a fork PR the checkout misses, or
// worse hits a same-named branch of the BASE repo and a code-pushing bot
// commits onto it. Fail-CLOSED on resolution: a PR the forge cannot answer
// for is refused, never launched on a guess.
//
// Returns the connection the proof was read through (proven=true), or
// proven=false with no error when no team connection covers the PR's host:
// the server then makes no launch pair for that host — a repo-targeted
// launch needs a connection on the repo's host, and a board card's repo rides
// the webhook lane that already guarded it — so there is nothing to decide
// and the launch keeps its shape (no policy, no grant), said at Debug.
func (s *Server) prLaunchForkGuard(ctx context.Context, teamID, preferredConnID, prURL, host, repo string, number int) (forge.Connection, bool, error) {
	conn, ok := s.forgeConnectionForPR(ctx, teamID, preferredConnID, host, repo)
	if !ok {
		if s.logger != nil {
			s.logger.Debug("fork guard: no team %s connection covers %s/%s — nothing to prove for %s", teamID, host, repo, prURL)
		}
		return forge.Connection{}, false, nil
	}
	gc, err := s.gateClientFor(ctx, conn)
	if err != nil {
		return conn, false, fmt.Errorf("fork guard: %s: cannot read the pull request through connection %s: %w", prURL, conn.ID, err)
	}
	if gc == nil {
		return conn, false, fmt.Errorf("%w: fork guard: %s: provider %s cannot read pull requests, so same-repo cannot be proven",
			errPRLaunchNoPullCapability, prURL, conn.Provider)
	}
	pr, err := gc.GetPullRequest(ctx, repo, number)
	if err != nil {
		return conn, false, fmt.Errorf("fork guard: %s: PR resolution: %w", prURL, err)
	}
	if reason := forkGuardRefusalFor(pr, repo); reason != "" {
		return conn, false, fmt.Errorf("%w: %s: %s", errPRLaunchForkGuard, prURL, reason)
	}
	return conn, true, nil
}

// repoLaunchPolicy composes a repo's launch-var layers for ONE bot, in the
// order every webhook lane applies them (forgePREventTargets): the co-enabled
// bots' manifest union first, then this bot's own manifest rule, then the
// operator's per-repo overrides. Skipping the per-bot layer is what hands one
// bot another's settings; skipping the union resolves a manifest-supplied
// gate_context on the webhook lanes and nowhere else.
//
// Falls back to the integration's operator vars alone when the webhook config
// is unreadable or belongs to another tenant: that half is the authoritative
// one, and losing the union only costs a default.
func (s *Server) repoLaunchPolicy(ctx context.Context, ri forge.RepoIntegration, botID string) map[string]string {
	if s.webhookConfigs == nil || ri.WebhookID == "" {
		return ri.LaunchVars
	}
	cfg, err := s.webhookConfigs.Get(store.WithoutTenantFilter(ctx), ri.WebhookID)
	if err != nil || cfg.TenantID != ri.TenantID {
		return ri.LaunchVars
	}
	policy := mergeVarsInto(map[string]string{}, cfg.LaunchVars)
	for _, rule := range cfg.BotRules {
		if rule.BotID == botID {
			mergeVarsInto(policy, rule.LaunchVars)
			break
		}
	}
	return mergeVarsInto(policy, cfg.OperatorLaunchVars)
}

// repoIntegrationFor finds a team's integration for a repo on a given forge
// host. The host is part of the identity: the same slug on another forge is a
// different repo, and applying its policy — or minting a grant on its
// connection — would cross two unrelated projects. A watch-only
// connection's row is not a candidate either: the security-read App is
// provisioned on the repos it watches, and its row carries neither a launch
// policy nor a connection that can post — selecting it would hand
// forgeConnectionForPR a connection it must reject, and the host-wide
// fallback then lands on a connection that does not cover the repo at all.
func (s *Server) repoIntegrationFor(ctx context.Context, teamID, host, repo string) (forge.RepoIntegration, bool) {
	if s.forgeIntegrations == nil || s.forgeConnections == nil || strings.TrimSpace(repo) == "" {
		return forge.RepoIntegration{}, false
	}
	ris, err := s.forgeIntegrations.ListByTenant(ctx, teamID)
	if err != nil {
		return forge.RepoIntegration{}, false
	}
	var best forge.RepoIntegration
	found := false
	for _, ri := range ris {
		if !strings.EqualFold(ri.RepoFullName, repo) {
			continue
		}
		conn, cerr := s.forgeConnections.Get(ctx, ri.ConnectionID)
		if cerr != nil || conn.TenantID != teamID || conn.IsSecurityReadOnly() || !strings.EqualFold(hostOfURL(conn.BaseURL()), host) {
			continue
		}
		// One repo provisioned twice on the same host — through two connections,
		// which happens when a repo is re-provisioned onto another one and the
		// first is left behind. The store's order is not stable and this choice
		// decides both the policy and the connection the verdict is posted
		// under, so take the LATEST provisioning: it is the operator's current
		// intent, and the older row is the stale one. Id breaks an exact tie.
		if !found || ri.CreatedAt.After(best.CreatedAt) ||
			(ri.CreatedAt.Equal(best.CreatedAt) && ri.ID < best.ID) {
			best, found = ri, true
		}
	}
	return best, found
}

// forgeConnectionForPR picks the team connection to publish through:
// the pinned connection when given, else the connection of the repo's
// LATEST integration on the PR's forge host, else the LATEST team
// connection on that host. A nil forgeConnections store yields (empty, false):
// every caller (approve, publish, pending, reconcile) inherits the guard
// here instead of repeating it.
func (s *Server) forgeConnectionForPR(ctx context.Context, teamID, preferredConnID, host, repo string) (forge.Connection, bool) {
	if s == nil || s.forgeConnections == nil {
		return forge.Connection{}, false
	}
	matches := func(c forge.Connection) bool {
		// A watch-only connection sits on the same host and would be picked by
		// the "first connection on this host" fallback below — then every
		// review comment and commit status posted through it would 403: its
		// App holds no pull_requests/statuses grant, by design.
		return c.TenantID == teamID && !c.IsSecurityReadOnly() &&
			strings.EqualFold(hostOfURL(c.BaseURL()), host)
	}
	if preferredConnID != "" {
		if c, err := s.forgeConnections.Get(ctx, preferredConnID); err == nil && matches(c) {
			return c, true
		} else if s.logger != nil {
			// Falling through to another connection is the historical
			// behaviour and stays — but an operator's explicit pin being
			// replaced must never be silent, or the verdict is published under
			// an identity nobody chose and nothing says so.
			s.logger.Warn("forge: pinned connection %s is not usable for %s on %s — resolving another", preferredConnID, repo, host)
		}
	}
	// The repo's integration, LATEST provisioning first — the same choice
	// repoIntegrationFor makes for the policy, so a repo re-provisioned onto
	// a newer connection posts under that one and not the row left behind.
	if ri, ok := s.repoIntegrationFor(ctx, teamID, host, repo); ok {
		if c, err := s.forgeConnections.Get(ctx, ri.ConnectionID); err == nil && matches(c) {
			return c, true
		}
	}
	conns, err := s.forgeConnections.ListByTenant(ctx, teamID)
	if err != nil {
		return forge.Connection{}, false
	}
	// The LATEST matching connection wins, not the first: ListByTenant sorts
	// created_at ascending on both stores, so a repo re-provisioned onto a
	// newer connection would otherwise inherit the stale one — the rule
	// repoIntegrationForRepo applies to the integration lookup. Id breaks an
	// exact created_at tie. Publish, pending and reconcile all read through
	// this helper.
	var best forge.Connection
	found := false
	for _, c := range conns {
		if !matches(c) {
			continue
		}
		if !found || c.CreatedAt.After(best.CreatedAt) ||
			(c.CreatedAt.Equal(best.CreatedAt) && c.ID < best.ID) {
			best, found = c, true
		}
	}
	if found {
		return best, true
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
