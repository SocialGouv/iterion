package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/routing"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tracerName is the OTel instrumentation name for server spans. Both
// the launch and resume handlers create root spans here so the runner
// pod can hang per-node spans off of them via NATS trace propagation.
const tracerName = "github.com/SocialGouv/iterion/pkg/server"

// workflowSourceChangedErrorCode is the stable wire code consumed by the
// studio's force-resume affordance. Keep human prose in "error" for display,
// but never require clients to parse it.
const workflowSourceChangedErrorCode = "workflow_source_changed"

// --- Request / response shapes ---

type launchRunRequest struct {
	FilePath string `json:"file_path"`
	// Source is the workflow contents uploaded inline. In cloud mode the
	// studio sends this so the server pod doesn't need a shared
	// filesystem; FilePath is then advisory (used for display + as the
	// AST parserPath). When both are set, Source wins.
	Source string `json:"source,omitempty"`
	// BotID names a catalog bundle (e.g. "whats-next") to launch. In cloud
	// mode it lets the server resolve the bot's source off the pod's own
	// bots/ tree (like the webhook/scheduler/board/trigger launchers) so a
	// client need not upload the bytes, and it is carried on the LaunchSpec
	// so the runner mirrors the bundle's skills. Optional: a catalog-shaped
	// FilePath ("bots/<name>/main.bot") is inferred to the same id.
	BotID string            `json:"bot_id,omitempty"`
	RunID string            `json:"run_id,omitempty"`
	Vars  map[string]string `json:"vars,omitempty"`
	// Preset is the name of an in-source preset (presets: block) to
	// apply before Vars. Maps directly to LaunchSpec.Preset; the engine
	// records it on Run.Preset for resume.
	Preset string `json:"preset,omitempty"`
	// Timeout is a Go-style duration string ("30m", "2h"). Empty disables.
	Timeout string `json:"timeout,omitempty"`
	// MergeInto is the worktree-finalization merge target. See
	// runview.LaunchSpec.MergeInto.
	MergeInto string `json:"merge_into,omitempty"`
	// BranchName overrides the storage branch name created on the
	// worktree's HEAD. See runview.LaunchSpec.BranchName.
	BranchName string `json:"branch_name,omitempty"`
	// MergeStrategy is "squash" (default) or "merge". See
	// runview.LaunchSpec.MergeStrategy.
	MergeStrategy string `json:"merge_strategy,omitempty"`
	// AutoMerge: when true, the engine performs the merge at end of
	// run; when false (default), merge is deferred to a UI action.
	AutoMerge bool `json:"auto_merge,omitempty"`
	// Attachments maps the workflow's attachment names to upload IDs
	// returned by POST /api/runs/uploads. The launch handler promotes
	// each upload from the staging area into the run-scoped store
	// before kicking off execution.
	Attachments map[string]string `json:"attachments,omitempty"`
	// Backend, when non-empty, overrides the workflow's `default_backend:`
	// for this run only. Node-level explicit `backend:` declarations
	// still win. Honored in the in-process spawnRun path; detached mode
	// (ITERION_RUNS_DETACHED=1) logs a warning and ignores it.
	Backend string `json:"backend,omitempty"`
	// Compress is the run-level command-output-compression override
	// ("on"|"ultra"|"off"). Empty inherits the workflow/node compress: DSL
	// then ITERION_COMPRESS. See runview.LaunchSpec.Compress.
	Compress string `json:"compress,omitempty"`
	// AutoMemory is the run-level auto-memory (MEMORY.md) override
	// ("on"|"off"). Empty inherits the workflow/node auto_memory: DSL then
	// ITERION_AUTO_MEMORY. See docs/memory-and-knowledge.md.
	AutoMemory string `json:"auto_memory,omitempty"`
	// LoopBudgetGuard is the run-level override for the loop back-edge
	// affordability guard ("on"|"off"). Empty inherits the workflow
	// loop_budget_guard: DSL then ITERION_LOOP_BUDGET_GUARD. See docs/dsl.md.
	LoopBudgetGuard string `json:"loop_budget_guard,omitempty"`
	// Supervisors is the run-level kill switch for DSL-declared
	// `supervisor NAME:` watchers ("on"|"off"). Empty inherits
	// ITERION_SUPERVISORS; the default is on. See docs/supervisors.md.
	Supervisors string `json:"supervisors,omitempty"`
	// Permission is the run-level tool-permission-gate mode override
	// ("off"|"ask"|"deny"). Empty inherits the workflow/node permission:
	// DSL then ITERION_PERMISSION. See docs/permissions.md.
	Permission string `json:"permission,omitempty"`
	// ReviewMode is the run-level mono/dual review-topology override
	// ("auto"|"mono"|"dual") for bi-model review-loop bots. Empty/"auto"
	// resolves from detected providers at launch. See pkg/reviewtopology.
	ReviewMode string `json:"review_mode,omitempty"`
	// ModelOverrides are launch-time per-node/-group backend+model overrides
	// (studio Launch dropdowns). Each targets nodes by selector (node id, id
	// glob, or kind keyword) and wins over the node's DSL backend:/model:.
	// See runview.ModelOverrideEntry. The current queue contract carries them
	// to cloud runners as well, where the executor applies them (issue #513).
	ModelOverrides []runview.ModelOverrideEntry `json:"model_overrides,omitempty"`
	// RoutingPolicy is the launch-frozen outcome contract: what
	// "success" and "blocked" mean for this run (bot-DSL expressions
	// over the terminal outputs), where a success lands, and which
	// actions a consumer may take automatically. Validated and hashed
	// here; immutable afterwards.
	RoutingPolicy *store.RoutingPolicy `json:"routing_policy,omitempty"`
	// Fallback is the operator's ordered run-level fallback chain, taken
	// when an agent node's primary or preceding stage fails. It applies only
	// to agent nodes that declare no `fallbacks:` of their own and never to judges.
	// A single object is promoted to a one-stage chain for compatibility.
	// Omitted = none. See ADR-087.
	Fallback launchFallback `json:"fallback,omitempty"`
	// Budget carries run-level budget-cap overrides for the workflow's
	// `budget:` block — the HTTP twin of the CLI --max-* flags. Non-zero
	// fields win over the DSL/recipe budget; zero fields inherit. A bad
	// max_duration is a 400. Not supported for queued cloud runs yet
	// (rejected with a 400, never silently dropped).
	Budget *launchBudgetSpec `json:"budget,omitempty"`
	// Cap. 3 sharding fields. When ParentRunID is non-empty, this
	// launch is a shard child of an existing parent run; the server
	// propagates the fields to the persisted Run document and (in
	// cloud mode) to the published RunMessage so runner pods and the
	// studio can render parent/child relationships. The hidden CLI
	// command `iterion __scan-shards --mode=cloud` POSTs runs with
	// these set; the API is also reachable by other callers.
	ParentRunID string `json:"parent_run_id,omitempty"`
	ShardIndex  int    `json:"shard_index,omitempty"`
	ShardCount  int    `json:"shard_count,omitempty"`
	ShardLabel  string `json:"shard_label,omitempty"`
	// CallbackURL, when set, is an http/https endpoint iterion POSTs a
	// run-completion webhook to when the run terminates (see pkg/notify
	// + docs/outbound-callbacks.md). Lets a programmatic caller (chat
	// adapter, CI bridge) be told the run finished without polling. The
	// delivery passes an SSRF guard.
	CallbackURL string `json:"callback_url,omitempty"`
	// CallbackToken is echoed back verbatim in the completion payload so
	// the receiver can correlate the callback to its originating request
	// (e.g. a chat thread id) without server-side state.
	CallbackToken string `json:"callback_token,omitempty"`
	// RepoURL / RepoRef aim this run at a git repository: the cloud
	// runner clones it (prepareRepoWorkspace) before sandboxing —
	// the launch-form "Target repository" section. Cloud-only: a
	// local-mode server rejects a repo-targeted launch explicitly
	// (no forge stores → no credential source for an authed clone).
	RepoURL string `json:"repo_url,omitempty"`
	RepoRef string `json:"repo_ref,omitempty"`
	// ConnectionID names the team's forge connection whose managed
	// token authenticates the clone/push: the server pins
	// SecretOverrides[forge secret] to the connection's managed secret
	// — the same Tier-0 pinning webhook launches use. Tenant-checked
	// against the caller's active team (cross-team ids 404).
	ConnectionID string `json:"connection_id,omitempty"`
	// CallbackAnswerNode optionally names the node whose latest artifact
	// holds the run's user-facing answer (the "final_answer" field).
	// Empty → the notifier scans all artifact nodes for "final_answer".
	CallbackAnswerNode string `json:"callback_answer_node,omitempty"`
}

// launchFallback accepts the original single-object request and the ordered
// array form. Its default JSON marshaler always emits the canonical array.
type launchFallback []runview.FallbackEntry

func (f *launchFallback) UnmarshalJSON(data []byte) error {
	raw := bytes.TrimSpace(data)
	if len(raw) == 0 {
		return fmt.Errorf("empty fallback JSON")
	}
	switch raw[0] {
	case 'n':
		if !bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("invalid fallback JSON %q", raw)
		}
		*f = nil
		return nil
	case '[':
		var entries []runview.FallbackEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("decode fallback chain: %w", err)
		}
		*f = entries
		return nil
	case '{':
		var entry runview.FallbackEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("decode legacy fallback: %w", err)
		}
		*f = []runview.FallbackEntry{entry}
		return nil
	default:
		return fmt.Errorf("fallback must be an object or array")
	}
}

// launchBudgetSpec is the wire shape of launchRunRequest.Budget. Field
// types mirror ir.Budget (MaxDuration stays a Go duration string, parsed
// via time.ParseDuration at admission).
type launchBudgetSpec struct {
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
}

// toOverrides projects the wire shape onto the engine's override type,
// returning nil when every field is zero (no override requested).
func (b *launchBudgetSpec) toOverrides() *ir.BudgetOverrides {
	if b == nil {
		return nil
	}
	o := ir.BudgetOverrides{
		MaxCostUSD:          b.MaxCostUSD,
		MaxTokens:           b.MaxTokens,
		MaxDuration:         b.MaxDuration,
		MaxIterations:       b.MaxIterations,
		MaxParallelBranches: b.MaxParallelBranches,
	}
	if o.IsZero() {
		return nil
	}
	return &o
}

type launchRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	// QueuePosition is the 1-based place in the local pipeline-concurrency
	// queue when the launch was deferred (the machine was at its
	// max-concurrent-pipelines cap). 0 when the run started immediately.
	// The studio uses it to tell the operator "queued at position N"
	// instead of a misleading "running".
	QueuePosition int `json:"queue_position,omitempty"`
}

type resumeRunRequest struct {
	FilePath string `json:"file_path,omitempty"` // optional; falls back to run.FilePath
	// Source carries the workflow contents inline. Used in cloud mode
	// when the resumer (studio) wants to push a possibly-modified
	// workflow without depending on the server pod's filesystem.
	Source  string         `json:"source,omitempty"`
	Answers map[string]any `json:"answers,omitempty"`
	Force   bool           `json:"force,omitempty"`
	Timeout string         `json:"timeout,omitempty"`
	// Attachments carries ad-hoc upload IDs (from POST /api/runs/uploads)
	// the operator attached to this answer without the workflow declaring
	// a `file` field — the "here is a diagram explaining my feedback"
	// case. They are promoted to run attachments and surfaced to the
	// workflow on the reserved `_attachments` answer key. A DECLARED
	// `file` field instead carries its upload inline in Answers as
	// `{"upload_id": "..."}`. See runs_answer_uploads.go.
	Attachments []string `json:"attachments,omitempty"`
	// Budget is the this-resume cap ask — the wire counterpart of the
	// CLI's --max-cost-usd / --max-duration / --max-tokens flags on
	// `iterion resume`. Non-nil beats the run doc's persisted launch
	// ask, honouring the "raise the cap + resume" recovery on remote
	// runs where an operator can no longer edit a local .bot to widen
	// the cap. Zero fields inherit. Wired through
	// runview.ResumeSpec.Budget → cloudpublisher.SubmitResume. #652 part 2.
	Budget *launchBudgetSpec `json:"budget,omitempty"`
}

func (s *Server) handleLaunchRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	// Launch admission: suspend → concurrency → rate → cost cap →
	// monthly run quota (which also meters). Super-admin bypasses.
	if _, d := s.gateLaunch(r.Context()); d != nil {
		s.writeLaunchDenial(w, r, d)
		return
	}
	// Root span for the launch path. Keeping it on the request ctx
	// means the OTel HTTP middleware (when wired) sees it as a child
	// of the inbound HTTP server span. The detached ctx below
	// preserves the span context so the runner-side trace remains a
	// single connected trace.
	spanCtx, span := otel.Tracer(tracerName).Start(r.Context(), "iterion.api.launch_run")
	defer span.End()

	var req launchRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		span.SetStatus(codes.Error, "invalid request")
		return
	}
	if req.FilePath == "" && req.Source == "" && req.BotID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "file_path, source or bot_id is required")
		span.SetStatus(codes.Error, "missing file_path/source/bot_id")
		return
	}
	if req.RoutingPolicy != nil {
		// Refuse a malformed contract BEFORE any work happens — a bad
		// expression discovered at the terminal would strand a finished
		// run behind an unreadable policy.
		if perr := routing.Validate(req.RoutingPolicy); perr != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "%v", perr)
			return
		}
		req.RoutingPolicy.Hash = req.RoutingPolicy.ComputeHash()
	}
	// Cloud mode has no operator filesystem, so a bare workspace file_path
	// can't be read. Resolve the bot through the tiered authority instead
	// (bot_resolver.go): the caller's TEAM-AUTHORED bot first, then a
	// PLATFORM override, then the baked catalog off the pod FS. Stored bots
	// run their store content inline, with the materialized bundle merged at
	// compile and the ref stamped for the runner; catalog bots resolve like
	// the webhook/scheduler/board/trigger launchers. This is what lets the
	// studio launch Nexie/Revi/etc. by id or catalog path without uploading
	// bytes — and what makes an override effective on the very next launch.
	botID := strings.TrimSpace(req.BotID)
	var launchLB *launchBot
	if s.cfg.Mode == "cloud" && req.Source == "" {
		id, _ := auth.FromContext(r.Context())
		lb, lbErr := s.resolveBotTiered(r.Context(), id.TeamID, req.BotID, req.FilePath)
		if lbErr != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "resolve bot: %v", lbErr)
			span.SetStatus(codes.Error, "bot resolution failed")
			return
		}
		if lb != nil {
			launchLB = lb
			defer launchLB.Cleanup()
			req.Source, req.FilePath, botID = lb.Source, lb.Path, lb.BotID
		}
	}
	// Anything that is not a resolvable bot still requires inline Source in
	// cloud: the server pod cannot read an arbitrary operator path.
	if s.cfg.Mode == "cloud" && req.Source == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "cloud mode: source or a catalog bot_id is required (file_path is not portable across the server pod's filesystem)")
		span.SetStatus(codes.Error, "cloud mode requires source")
		return
	}
	absPath, pathErr := s.resolveWorkflowPath(req.FilePath, req.Source)
	if pathErr != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid file_path: %v", pathErr)
		span.SetStatus(codes.Error, "invalid file_path")
		return
	}
	timeout, err := parseTimeout(req.Timeout)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid timeout: %v", err)
		span.SetStatus(codes.Error, "invalid timeout")
		return
	}
	budget := req.Budget.toOverrides()
	if budget != nil {
		// Validate max_duration at admission so the caller gets a 400
		// with the offending value instead of a launch-time failure.
		// Cloud mode forwards the overrides on queue.RunMessage.Budget;
		// the runner applies them under its multitenant ceiling.
		if err := budget.Validate(); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid budget: %v", err)
			span.SetStatus(codes.Error, "invalid budget")
			return
		}
	}
	// Repo-targeted launch (the "Target repository" section): resolve the
	// forge context on the request ctx (auth identity) BEFORE detaching.
	var repoProjectPath string
	var repoSecretOverrides map[string]string
	if req.RepoURL != "" || req.ConnectionID != "" {
		if s.cfg.Mode != "cloud" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "repo-targeted launches need cloud mode — on a local studio, clone the repo and launch from its checkout")
			span.SetStatus(codes.Error, "repo target on local mode")
			return
		}
		if req.RepoURL == "" || req.ConnectionID == "" || s.forgeConnections == nil || s.forgeOrchestrator == nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "repo_url and connection_id go together (and need forge integrations enabled)")
			span.SetStatus(codes.Error, "incomplete repo target")
			return
		}
		id, _ := auth.FromContext(r.Context())
		conn, err := s.forgeConnections.Get(r.Context(), req.ConnectionID)
		if err != nil || conn.TenantID != id.TeamID {
			// Cross-team ids 404 (non-enumeration), like every forge route.
			s.httpErrorFor(w, r, http.StatusNotFound, "connection not found")
			span.SetStatus(codes.Error, "connection not found")
			return
		}
		// A watch-only connection cannot clone or push. The orchestrator
		// refuses it too, but only once the launch is under way — say it here,
		// where the operator picked it, instead of failing in the pod.
		if conn.IsSecurityReadOnly() {
			s.httpErrorFor(w, r, http.StatusUnprocessableEntity,
				"connection %s is watch-only (Dependabot alerts only) — it cannot clone or push; pick the team's runtime connection", conn.ID)
			span.SetStatus(codes.Error, "watch-only connection")
			return
		}
		// The repo must live on the connection's forge host: the managed
		// token is only ever aimed at the host it belongs to.
		base := strings.TrimSuffix(conn.BaseURL(), "/")
		if !strings.HasPrefix(req.RepoURL, base+"/") {
			s.httpErrorFor(w, r, http.StatusBadRequest, "repo_url is not on the connection's forge host")
			span.SetStatus(codes.Error, "repo host mismatch")
			return
		}
		// Fail at launch, not three hours in: a repo outside a "selected
		// repositories" App installation can only fail at push time.
		if err := s.forgeRepoReachable(r.Context(), conn, strings.TrimSuffix(strings.TrimPrefix(req.RepoURL, base+"/"), ".git")); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
			span.SetStatus(codes.Error, "repo unreachable by connection")
			return
		}
		secID, err := s.forgeOrchestrator.EnsureManagedSecret(store.WithTenant(r.Context(), conn.TenantID), &conn, id.UserID)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadGateway, "forge token for the clone: %v", err)
			span.SetStatus(codes.Error, "managed secret")
			return
		}
		repoSecretOverrides = map[string]string{"forge_token": secID}
		repoProjectPath = strings.TrimSuffix(strings.TrimPrefix(req.RepoURL, base+"/"), ".git")
		// Canonicalize to the .git clone URL: the runner clones with
		// http.followRedirects=false (SSRF hardening), and GitLab 301s a
		// bare repo path to its .git twin — which that git config turns
		// into a hard clone failure. GitHub serves both, so the trap only
		// springs on GitLab; canonicalizing here keeps the launch request
		// tolerant of either spelling.
		req.RepoURL = forge.CloneURLFor(base, repoProjectPath)
	}

	// A launch that targets a pull request gets the repo's launch policy (its
	// pinned gate_context) and a per-run forge-publish grant, so the bot's
	// publish node posts through the server's live forge client instead of a
	// workspace-mounted token. Same composition as the board lane — a launch
	// from the studio form must gate under the same context a webhook does.
	if launchID, _ := auth.FromContext(r.Context()); launchID.TeamID != "" {
		req.Vars = s.applyPRLaunchContext(r.Context(), launchID.TeamID, req.ConnectionID, req.BotID, req.Vars, r)
	}

	// Detach lifecycle from the HTTP request context so a client
	// disconnect doesn't abort the run, but keep the trace span so
	// the runner-side span chains under this one. context.WithoutCancel
	// (Go 1.21+) gives us exactly that combination.
	ctx := context.WithoutCancel(spanCtx)

	var promote runtime.AttachmentPromoteFunc
	if len(req.Attachments) > 0 {
		mapping := req.Attachments
		promote = func(promoteCtx context.Context, runID string) error {
			_, _, err := s.promoteStaged(promoteCtx, runID, mapping)
			return err
		}
	}

	spec := runview.LaunchSpec{
		FilePath:          absPath,
		Source:            req.Source,
		BotID:             botID,
		RunID:             req.RunID,
		Vars:              req.Vars,
		Preset:            req.Preset,
		Timeout:           timeout,
		MergeInto:         req.MergeInto,
		BranchName:        req.BranchName,
		MergeStrategy:     store.MergeStrategy(req.MergeStrategy),
		AutoMerge:         req.AutoMerge,
		AttachmentPromote: promote,
		Backend:           req.Backend,
		Compress:          req.Compress,
		AutoMemory:        req.AutoMemory,
		LoopBudgetGuard:   req.LoopBudgetGuard,
		Supervisors:       req.Supervisors,
		Permission:        req.Permission,
		ReviewMode:        req.ReviewMode,
		// The manual path resolves the retry chain like every automated
		// one. Skipping it here would let a bot declaring
		// `retry: usage_window: off` be auto-retried anyway whenever a
		// human pressed Launch — a declared directive silently violated on
		// the one path where the author is watching.
		RetryPolicy:        s.resolveRunRetryPolicy(botID),
		ModelOverrides:     req.ModelOverrides,
		RoutingPolicy:      req.RoutingPolicy,
		Fallback:           req.Fallback,
		Budget:             budget,
		ParentRunID:        req.ParentRunID,
		ShardIndex:         req.ShardIndex,
		ShardCount:         req.ShardCount,
		ShardLabel:         req.ShardLabel,
		CallbackURL:        req.CallbackURL,
		CallbackToken:      req.CallbackToken,
		CallbackAnswerNode: req.CallbackAnswerNode,
		RepoURL:            req.RepoURL,
		RepoRef:            req.RepoRef,
		ProjectPath:        repoProjectPath,
		SecretOverrides:    repoSecretOverrides,
	}
	// Stored-bot resolution (team/platform): the compile-time bundle dir and
	// the runner-side ref ride the spec. Threaded from the SAME resolution
	// that produced req.Source — never re-fetched, so a push racing this
	// request cannot pair this launch's IR with newer resources.
	launchLB.StampBundle(&spec)
	res, err := s.runs.Launch(ctx, spec)
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			span.SetStatus(codes.Error, "server draining")
			return
		}
		if errors.Is(err, runtime.ErrUsageCapped) {
			// A quota refusal, not a malformed request: the same launch
			// succeeds once the window reopens, and the message says when.
			s.httpErrorFor(w, r, http.StatusTooManyRequests, "%v", err)
			span.SetStatus(codes.Error, "usage cap reached")
			return
		}
		if s.writeQueueOutageError(w, r, "launch", err) {
			span.RecordError(err)
			span.SetStatus(codes.Error, "queue unavailable")
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "launch: %v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "launch failed")
		return
	}
	span.SetAttributes(attribute.String("iterion.run_id", res.RunID))
	// A deferred launch (over the local concurrency cap) reports the queued
	// status + position so the studio doesn't claim "running" for a
	// pipeline still waiting for a slot. QueuePosition is 0 for the normal
	// immediate-start path (and always 0 in cloud mode, which uses the
	// NATS queue, not this local gate).
	status := store.RunStatusRunning
	if res.QueuePosition > 0 {
		status = store.RunStatusQueued
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, launchRunResponse{RunID: res.RunID, Status: string(status), QueuePosition: res.QueuePosition})
}

func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	// A resume re-enters the engine (node execution + budget/cost spend),
	// so it is a run launch for admission purposes: it passes the same
	// gate as handleLaunchRun (suspend, concurrency, rate, cost cap,
	// monthly quota — a resume consumes run budget like a launch), else
	// a capped org keeps executing in-flight work via operator/auto
	// resume. Super-admin bypasses.
	if _, d := s.gateLaunch(r.Context()); d != nil {
		s.writeLaunchDenial(w, r, d)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	spanCtx, span := otel.Tracer(tracerName).Start(r.Context(), "iterion.api.resume_run",
		trace.WithAttributes(attribute.String("iterion.run_id", id)))
	defer span.End()
	var req resumeRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		span.SetStatus(codes.Error, "invalid request")
		return
	}
	// The budget ask is validated at admission, before any store access:
	// a malformed max_duration ("4 hours") would otherwise ride
	// RunMessage.Budget onto the queue, fail the runner's
	// applyBudgetOverrides on EVERY redelivery, and burn the delivery
	// budget into a DLQ park. Same gate as handleLaunchRun.
	budget := req.Budget.toOverrides()
	if budget != nil {
		if err := budget.Validate(); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid budget: %v", err)
			span.SetStatus(codes.Error, "invalid budget")
			return
		}
	}
	// Load the run once: its persisted FilePath is the fallback when the body
	// omits one, and its TenantID is required to scope the resume's Mongo
	// queries (see below). LoadRunCtx looks a run up by id without a tenant
	// filter, so it is safe to call before the tenant is on the context.
	runMeta, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		span.SetStatus(codes.Error, "run not found")
		return
	}
	// Resolve file path: explicit body wins, falling back to the
	// FilePath persisted at launch.
	filePath := req.FilePath
	if filePath == "" {
		filePath = runMeta.FilePath
	}
	// Shared with the retry sweeper so the automated resume resolves its
	// source exactly like this one (see resolveResumeSource).
	absPath, resolvedSource, resumeLB, pathErr := s.resolveResumeSource(r.Context(), runMeta.BotSourceTenant, filePath, req.Source, runMeta.WorkflowSource)
	if pathErr != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", pathErr)
		span.SetStatus(codes.Error, "resume source unresolvable")
		return
	}
	defer resumeLB.Cleanup()
	req.Source = resolvedSource
	timeout, err := parseTimeout(req.Timeout)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid timeout: %v", err)
		span.SetStatus(codes.Error, "invalid timeout")
		return
	}

	ctx := context.WithoutCancel(spanCtx)
	// Scope the resume to a tenant: Resume runs tenant-scoped store queries
	// (cloud Mongo) that panic without a tenant on the context — the HTTP
	// middleware authenticates the /api/runs route but does NOT stamp the store
	// tenant marker (unlike the /api/teams/{id}/… routes). Prefer the run's own
	// TenantID (the authoritative owner — a super-admin may resume a run in any
	// team); fall back to the caller's active team when the run has none, which
	// happens for a run orphaned before it ever executed (the runner stamps
	// TenantID at execution start, so a never-run queued/failed row can be
	// empty). Empty on a local single-tenant store → WithTenant no-ops.
	tenantID := runMeta.TenantID
	if tenantID == "" {
		if id, ok := auth.FromContext(r.Context()); ok {
			tenantID = id.TeamID
		}
	}
	ctx = store.WithTenant(ctx, tenantID)

	// Promote any operator upload attached to this answer BEFORE handing
	// the answers to the engine: a `file` field must already be a
	// resolvable attachment by the time the resumed workflow renders a
	// prompt referencing it. Tenant-scoped (the store writes below need
	// the marker the line above stamps). A failure here is the
	// operator's problem to fix and retry — 400, run untouched, staging
	// intact.
	answers := req.Answers
	if len(req.Attachments) > 0 || hasUploadEnvelope(answers) {
		// Promotion consumes the staging, so a resume that Resume is
		// going to reject anyway must not get that far: the studio's
		// force-resume retry re-sends the SAME upload ids, and they
		// would already be gone. Only paid for on an upload-carrying
		// resume — it compiles the workflow a second time.
		pfSpec := runview.ResumeSpec{
			RunID:    id,
			FilePath: absPath,
			Source:   req.Source,
			Answers:  answers,
			Force:    req.Force,
		}
		if resumeLB != nil {
			pfSpec.BundleDir, pfSpec.BotBundle = resumeLB.BundleDir, resumeLB.Ref
		}
		if pfErr := s.runs.PreflightResume(ctx, pfSpec); pfErr != nil {
			s.writeResumeError(w, r, pfErr)
			span.RecordError(pfErr)
			span.SetStatus(codes.Error, "resume preflight failed")
			return
		}
		pausedNode := ""
		if runMeta.Checkpoint != nil {
			pausedNode = runMeta.Checkpoint.PausedNodeID()
		}
		promoted, promoteErr := s.promoteAnswerUploads(ctx, id, pausedNode, answers, req.Attachments)
		if promoteErr != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "attach upload: %v", promoteErr)
			span.RecordError(promoteErr)
			span.SetStatus(codes.Error, "attach upload failed")
			return
		}
		answers = promoted
	}

	resumeSpec := runview.ResumeSpec{
		RunID:    id,
		FilePath: absPath,
		Source:   req.Source,
		Answers:  answers,
		Force:    req.Force,
		Timeout:  timeout,
		Budget:   budget,
	}
	if resumeLB != nil {
		resumeSpec.BundleDir, resumeSpec.BotBundle = resumeLB.BundleDir, resumeLB.Ref
	}
	res, err := s.runs.Resume(ctx, resumeSpec)
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			span.SetStatus(codes.Error, "server draining")
			return
		}
		s.writeResumeError(w, r, err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "resume failed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, launchRunResponse{RunID: res.RunID, Status: string(store.RunStatusRunning)})
}

// writeResumeError preserves the normal human-readable error response and
// adds a stable code for the one resume failure the studio must act on.
func (s *Server) writeResumeError(w http.ResponseWriter, r *http.Request, err error) {
	if s.writeQueueOutageError(w, r, "resume", err) {
		return
	}
	if runtime.IsWorkflowSourceChanged(err) {
		s.writeJSONError(w, r, http.StatusBadRequest, map[string]any{
			"error":      fmt.Sprintf("resume: %v", err),
			"error_code": workflowSourceChangedErrorCode,
		})
		return
	}
	s.httpErrorFor(w, r, http.StatusBadRequest, "resume: %v", err)
}

// writeQueueOutageError is shared by launch and both resume error sites
// (upload preflight and publication). errors.As deliberately handles the
// wrapping and errors.Join shapes produced when queue publication and the
// compensating run-status update both fail.
func (s *Server) writeQueueOutageError(w http.ResponseWriter, r *http.Request, operation string, err error) bool {
	var queueErr *runview.QueueUnavailableError
	if !errors.As(err, &queueErr) {
		return false
	}
	s.writeJSONError(w, r, http.StatusServiceUnavailable, map[string]any{
		"error":      fmt.Sprintf("%s: %v", operation, queueErr),
		"error_code": queueErr.Code(),
		"retryable":  queueErr.Retryable(),
	})
	return true
}

// parseTimeout accepts an empty string (no timeout) or a Go duration
// string. Negative values are rejected.
func parseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("timeout must not be negative")
	}
	return d, nil
}
