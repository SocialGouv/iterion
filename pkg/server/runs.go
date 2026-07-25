package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tracerName is the OTel instrumentation name for server spans. Both
// the launch and resume handlers create root spans here so the runner
// pod can hang per-node spans off of them via NATS trace propagation.
const tracerName = "github.com/SocialGouv/iterion/pkg/server"

// registerRunRoutes wires the /api/runs surface onto the server's
// mux. Called from routes() after the studio endpoints so the run
// console is opt-in: a server constructed without a store dir
// (s.runs == nil) silently skips registration and behaves exactly
// like the studio-only build.
func (s *Server) registerRunRoutes() {
	if s.runs == nil {
		return
	}
	s.mux.HandleFunc("GET /api/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/runs/global-active", s.handleListGlobalActiveRuns)
	s.mux.HandleFunc("POST /api/runs", s.handleLaunchRun)
	s.mux.HandleFunc("POST /api/runs/preview-cost", s.handlePreviewCost)
	s.mux.HandleFunc("POST /api/runs/uploads", s.handleUploadAttachment)
	s.mux.HandleFunc("GET /api/runs/{id}/attachments/{name}", s.handleServeAttachment)
	s.mux.HandleFunc("GET /api/runs/{id}/attachments/{name}/url", s.handlePresignAttachment)
	s.mux.HandleFunc("GET /api/server/info", s.handleServerInfo)
	s.mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/runs/{id}/children", s.handleListRunChildren)
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.handleGetRunEvents)
	s.mux.HandleFunc("GET /api/runs/{id}/workflow", s.handleGetRunWorkflow)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts", s.handleListAllArtifacts)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}", s.handleListArtifacts)
	s.mux.HandleFunc("GET /api/runs/{id}/artifacts/{node}/{version}", s.handleGetArtifact)
	s.mux.HandleFunc("GET /api/runs/{id}/tools/{toolUseID}/{kind}", s.handleGetToolBlob)
	s.mux.HandleFunc("GET /api/runs/{id}/plans", s.handleListPlans)
	s.mux.HandleFunc("GET /api/runs/{id}/notes", s.handleListNotes)
	s.mux.HandleFunc("POST /api/runs/{id}/notes", s.handleAddNote)
	s.mux.HandleFunc("GET /api/runs/{id}/tags", s.handleGetRunTags)
	s.mux.HandleFunc("PUT /api/runs/{id}/tags", s.handleSetRunTags)
	s.mux.HandleFunc("GET /api/runs/{id}/artifact-files", s.handleListArtifactFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/artifact-files/{path...}", s.handleGetArtifactFile)
	s.mux.HandleFunc("GET /api/runs/{id}/files", s.handleListRunFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/files/touched", s.handleListRunTouchedFiles)
	s.mux.HandleFunc("GET /api/runs/{id}/files/diff", s.handleGetRunFileDiff)
	s.mux.HandleFunc("GET /api/runs/{id}/files/preview/{path...}", s.handlePreviewRunWorktreeImage)
	s.mux.HandleFunc("GET /api/runs/{id}/files/content", s.handleGetRunFileContent)
	s.mux.HandleFunc("PUT /api/runs/{id}/files/content", s.handleSaveRunFileContent)
	s.mux.HandleFunc("GET /api/runs/{id}/commits", s.handleListRunCommits)
	s.mux.HandleFunc("GET /api/runs/{id}/commits/{sha}", s.handleGetRunCommit)
	s.mux.HandleFunc("GET /api/runs/{id}/commits/{sha}/diff", s.handleGetRunCommitFileDiff)
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.handleCancelRun)
	s.mux.HandleFunc("POST /api/runs/{id}/pause", s.handlePauseRun)
	s.mux.HandleFunc("POST /api/runs/{id}/fork", s.handleForkRun)
	s.mux.HandleFunc("GET /api/runs/{id}/skills", s.handleListRunSkills)
	s.mux.HandleFunc("GET /api/runs/{id}/session-board", s.handleGetSessionBoard)
	s.mux.HandleFunc("GET /api/runs/{id}/queue-messages", s.handleListQueuedMessages)
	s.mux.HandleFunc("POST /api/runs/{id}/queue-message", s.handleQueueMessage)
	s.mux.HandleFunc("DELETE /api/runs/{id}/queue-message/{msgID}", s.handleCancelQueuedMessage)
	s.mux.HandleFunc("POST /api/runs/{id}/watch/{issueID}", s.handleAddWatch)
	s.mux.HandleFunc("DELETE /api/runs/{id}/watch/{issueID}", s.handleRemoveWatch)
	s.mux.HandleFunc("POST /api/runs/{id}/resume", s.handleResumeRun)
	s.mux.HandleFunc("POST /api/runs/{id}/merge", s.handleMergeRun)
	s.mux.HandleFunc("POST /api/runs/{id}/commit-and-finalize", s.handleCommitAndFinalize)
	s.mux.HandleFunc("GET /api/runs/{id}/merge/conflicts", s.handleGetMergeConflicts)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/resolve", s.handleResolveMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/resolve-with-agent", s.handleResolveConflictWithAgent)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/finalize", s.handleFinalizeMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/merge/conflicts/abort", s.handleAbortMergeConflict)
	s.mux.HandleFunc("POST /api/runs/{id}/rename", s.handleRenameRun)
	s.mux.HandleFunc("DELETE /api/runs/{id}", s.handleDeleteRun)
	s.mux.HandleFunc("GET /api/ws/runs/{id}", s.handleRunWebSocket)
	s.mux.HandleFunc("GET /api/runs/{id}/preview", s.handlePreviewProxy)
	s.mux.HandleFunc("GET /api/runs/{id}/browser/cdp", s.handleBrowserCDP)
	s.mux.HandleFunc("POST /api/runs/{id}/browser/attach", s.handleBrowserAttach)
}

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
	// See runview.ModelOverrideEntry.
	ModelOverrides []runview.ModelOverrideEntry `json:"model_overrides,omitempty"`
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
	// CallbackAnswerNode optionally names the node whose latest artifact
	// holds the run's user-facing answer (the "final_answer" field).
	// Empty → the notifier scans all artifact nodes for "final_answer".
	CallbackAnswerNode string `json:"callback_answer_node,omitempty"`
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
}

type cancelRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

// --- Handlers ---

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := runview.ListFilter{
		Workflow: q.Get("workflow"),
		Repo:     q.Get("repo"),
		Bundle:   q.Get("bot"),
		Node:     q.Get("node"),
	}
	if status := q.Get("status"); status != "" {
		filter.Status = store.RunStatus(status)
	}
	if since := q.Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid since (want RFC3339): %v", err)
			return
		}
		filter.Since = t
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		filter.Limit = n
	}
	out, err := s.runs.ListCtx(r.Context(), filter)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list runs: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"runs": out})
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
	// Cloud mode has no operator filesystem, so a bare workspace file_path
	// can't be read. But a CATALOG bot (bots/<name>) IS present on both the
	// server and runner pod images — resolve it off the pod FS (like the
	// webhook/scheduler/board/trigger launchers) and carry BotID so the
	// runner mirrors the bundle's skills. This is what lets the studio
	// launch Nexie/Revi/etc. by id or catalog path without uploading bytes.
	botID := strings.TrimSpace(req.BotID)
	if s.cfg.Mode == "cloud" && req.Source == "" {
		if src, p, id, ok := s.catalogBotSource(req.BotID, req.FilePath); ok {
			req.Source, req.FilePath, botID = src, p, id
		}
	}
	// Anything that is not a resolvable catalog bundle still requires inline
	// Source in cloud: the server pod cannot read an arbitrary operator path.
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

	res, err := s.runs.Launch(ctx, runview.LaunchSpec{
		FilePath:           absPath,
		Source:             req.Source,
		BotID:              botID,
		RunID:              req.RunID,
		Vars:               req.Vars,
		Preset:             req.Preset,
		Timeout:            timeout,
		MergeInto:          req.MergeInto,
		BranchName:         req.BranchName,
		MergeStrategy:      store.MergeStrategy(req.MergeStrategy),
		AutoMerge:          req.AutoMerge,
		AttachmentPromote:  promote,
		Backend:            req.Backend,
		Compress:           req.Compress,
		Permission:         req.Permission,
		ReviewMode:         req.ReviewMode,
		ModelOverrides:     req.ModelOverrides,
		Budget:             budget,
		ParentRunID:        req.ParentRunID,
		ShardIndex:         req.ShardIndex,
		ShardCount:         req.ShardCount,
		ShardLabel:         req.ShardLabel,
		CallbackURL:        req.CallbackURL,
		CallbackToken:      req.CallbackToken,
		CallbackAnswerNode: req.CallbackAnswerNode,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			span.SetStatus(codes.Error, "server draining")
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

// resolveCrossStore inspects the `?store=` query parameter and, when
// it's a permitted iterion store path under $HOME/.iterion/, returns a
// fresh read-only RunStore rooted there. Used by the read-only run
// endpoints so the desktop banner can deep-link into a run living in a
// different store (typically the global ~/.iterion/runs/ slot, or a
// per-project store not currently attached) without spawning a
// dedicated daemon.
//
// Returns (nil, "", nil) when ?store= is absent → callers fall through
// to the daemon's primary s.runs Service.
//
// Security: the path MUST resolve under $HOME/.iterion/ after symlink
// resolution; anything else is rejected with a clear error so a
// malicious ?store=/etc/.. can't read arbitrary host paths.
func (s *Server) resolveCrossStore(r *http.Request) (store.RunStore, string, error) {
	raw := r.URL.Query().Get("store")
	if raw == "" {
		return nil, "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, "", fmt.Errorf("cross-store: $HOME not resolvable")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: invalid path: %w", err)
	}
	// Symlink-safe containment check.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: resolve %s: %w", abs, err)
	}
	allowedRoot, err := filepath.EvalSymlinks(filepath.Join(home, ".iterion"))
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: resolve allowed root: %w", err)
	}
	if resolved != allowedRoot && !strings.HasPrefix(resolved, allowedRoot+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("cross-store: %q is outside $HOME/.iterion/ — refused", raw)
	}
	rs, err := store.New(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("cross-store: open %s: %w", resolved, err)
	}
	return rs, resolved, nil
}

// rejectCrossStoreWrite returns true (and writes 409 cross_store_readonly)
// when the request carries ?store= — symmetric to the WS handlers'
// rejection of cancel/answer on cross-store connections. Callers must
// `return` immediately when this returns true. The path itself isn't
// re-validated here (resolveCrossStore covers that on the read paths);
// any write attempt with ?store= set is refused, on the principle that
// only the owning daemon may mutate a run.
func (s *Server) rejectCrossStoreWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("store") == "" {
		return false
	}
	s.httpErrorFor(w, r, http.StatusConflict,
		"cross_store_readonly: this operation is not available for cross-store runs — open the owning daemon")
	return true
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		snap, err := runview.BuildSnapshot(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "run not found in cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, snap)
		return
	}
	snap, err := s.runs.SnapshotCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	s.writeJSONFor(w, r, snap)
}

// handleListRunChildren returns the shard/child subtree of a run — every
// run whose ParentRunID equals {id}, ordered by created_at ascending
// (T4b, refs #125). The tree UI that renders these under a run is a
// separate follow-up; this endpoint is the data source.
func (s *Server) handleListRunChildren(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		children, err := runview.BuildChildrenFromStore(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "list run children from cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, map[string]any{"runs": children})
		return
	}
	children, err := s.runs.ListChildren(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list run children: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"runs": children})
}

func (s *Server) handleGetRunEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	q := r.URL.Query()
	from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		events, err := xs.LoadEventsRange(r.Context(), id, from, to, runview.MaxEventsPerPage)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "load events from cross-store: %v", err)
			return
		}
		if events == nil {
			events = []*store.Event{}
		}
		s.writeJSONFor(w, r, map[string]any{"events": events})
		return
	}
	events, err := s.runs.LoadEventsCtx(r.Context(), id, from, to)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load events: %v", err)
		return
	}
	if events == nil {
		events = []*store.Event{}
	}
	s.writeJSONFor(w, r, map[string]any{"events": events})
}

func (s *Server) handleGetRunWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if xs, _, err := s.resolveCrossStore(r); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	} else if xs != nil {
		// Cross-store: re-use the IR-→-wire projection so the studio
		// receives the same shape it expects from the same-store path.
		// One-shot — no cache (the daemon serves cross-store reads
		// rarely; cache-hit ratio wouldn't justify the lock).
		wf, err := runview.BuildWireWorkflowFromStore(r.Context(), xs, id)
		if err != nil {
			s.httpErrorFor(w, r, http.StatusNotFound, "load workflow from cross-store: %v", err)
			return
		}
		s.writeJSONFor(w, r, wf)
		return
	}
	wf, err := s.runs.LoadWireWorkflowCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "load workflow: %v", err)
		return
	}
	s.writeJSONFor(w, r, wf)
}

// handleListAllArtifacts returns the latest published artifact per node
// for a run (the centralized, label-grouped Artifacts view). Tenant-scoped
// like handleListArtifacts.
func (s *Server) handleListAllArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	out, err := s.runs.ListAllArtifacts(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifacts: %v", err)
		return
	}
	if out == nil {
		out = []runview.RunArtifactSummary{}
	}
	s.writeJSONFor(w, r, map[string]any{"artifacts": out})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	if id == "" || node == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id or node")
		return
	}
	// Tenant scoping: load the run under the caller's context first so
	// the mongo tenant filter rejects cross-tenant requests before we
	// touch the filesystem-backed ListArtifacts (which has no tenant
	// awareness of its own).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	out, err := s.runs.ListArtifacts(id, node)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifacts: %v", err)
		return
	}
	if out == nil {
		out = []runview.ArtifactSummary{}
	}
	s.writeJSONFor(w, r, map[string]any{"artifacts": out})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	versionStr := r.PathValue("version")
	if id == "" || node == "" || versionStr == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id, node, or version")
		return
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil || version < 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid version")
		return
	}
	a, err := s.runs.LoadArtifactCtx(r.Context(), id, node, version)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact not found: %v", err)
		return
	}
	s.writeJSONFor(w, r, a)
}

// handleGetToolBlob streams a slice of a per-tool-call I/O sidecar
// blob (written by the hooks layer when an input/output exceeded the
// inline threshold). Used by the studio's Tools tab to lazy-fetch
// large bodies on demand: events carry only a 4 KB preview + a ref,
// the rest is served paginated from here.
//
// Query params:
//   - offset (int64, default 0): byte offset to start at
//   - limit  (int64, default 0 = "all from offset"): cap bytes returned
//
// Response: raw bytes (Content-Type: text/plain; charset=utf-8) plus
//   - X-Tool-Total-Size: full blob size in bytes
//   - X-Tool-Eof: "true" when offset+len(body) == total, "false" otherwise
//
// Errors:
//   - 400 missing id/toolUseID/kind or kind not in {input,output}
//   - 404 blob not found (call never produced one — i.e. fit inline)
//   - 503 store doesn't satisfy ToolBlobStore (both filesystem and Mongo do)
func (s *Server) handleGetToolBlob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	toolUseID := r.PathValue("toolUseID")
	kind := r.PathValue("kind")
	if id == "" || toolUseID == "" || kind == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id, toolUseID, or kind")
		return
	}
	if kind != "input" && kind != "output" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "kind must be input or output")
		return
	}
	// Tenant scoping (see handleListArtifacts): reject cross-tenant
	// before reading the blob. Dormant today (mongo lacks ToolBlobStore
	// → 503 below) but keeps the guard uniform for when it lands.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	q := r.URL.Query()
	var offset, limit int64
	if v := q.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	body, total, eof, err := s.runs.ReadToolBlobCtx(r.Context(), id, toolUseID, kind, offset, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.httpErrorFor(w, r, http.StatusNotFound, "tool blob not found")
			return
		}
		if strings.Contains(err.Error(), "unavailable for this store") {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "tool blobs unavailable in this backend")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read tool blob: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Tool-Total-Size", strconv.FormatInt(total, 10))
	if eof {
		w.Header().Set("X-Tool-Eof", "true")
	} else {
		w.Header().Set("X-Tool-Eof", "false")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// handleListPlans returns the chronological plan snapshots captured for a
// run — the agents' TodoWrite/todo_write living TODO lists (filesystem
// runs/<id>/plans/ or the Mongo run_plans collection in cloud mode).
// Ascending seq order (chronological). Returns an empty array (not 404)
// for a valid run that captured no plans — e.g. an agent that never
// called TodoWrite, or a run predating the feature — so the studio's
// Plans panel renders a clean empty state. Tenant-scoped like the other
// run sub-resource handlers (load the run under the caller's context
// first so the mongo tenant filter rejects cross-tenant requests).
func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	plans, err := s.runs.ListPlanSnapshotsCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list plans: %v", err)
		return
	}
	if plans == nil {
		plans = []store.PlanSnapshot{}
	}
	s.writeJSONFor(w, r, map[string]any{"plans": plans})
}

// handleListArtifactFiles returns the manifest of tool-produced files
// (run reports, SBOMs, …) dropped under runs/<id>/artifact_files by
// in-sandbox tools. Returns an empty array (not 404) when the run
// produced no files — distinguishes "valid run, nothing to download"
// from "no such run", which the studio's Artifacts panel renders as
// an empty state.
func (s *Server) handleListArtifactFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant gate: the S3 read path keys on runID only (no tenant prefix),
	// so cross-tenant isolation MUST be enforced here by loading the run
	// under the caller's tenant ctx first — mirrors handleGetToolBlob.
	// Without it a caller could list another team's artifact files.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	files, err := s.runs.ListArtifactFilesCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifact files: %v", err)
		return
	}
	if files == nil {
		files = []store.RunFileInfo{}
	}
	s.writeJSONFor(w, r, map[string]any{"files": files})
}

// handleGetArtifactFile streams one tool-produced file by relative path.
// Single byte ranges are supported so browser audio/video elements can start
// and seek without first buffering the entire object. Path-traversal guards
// live in the store layer. Errors map to 404 to keep path-probing attacks from
// distinguishing missing-file vs traversal-rejected vs unsupported stores.
func (s *Server) handleGetArtifactFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if id == "" || relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or file path")
		return
	}
	// Tenant gate before the tenant-blind S3 read (see handleListArtifactFiles).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact file not found")
		return
	}
	rc, info, err := s.runs.OpenArtifactFileCtx(r.Context(), id, relPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact file not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", artifactFileContentType(info.Path))
	w.Header().Set("Accept-Ranges", "bytes")
	if !info.ModifiedAt.IsZero() {
		w.Header().Set("Last-Modified", info.ModifiedAt.UTC().Format(http.TimeFormat))
	}
	// Disposition: `inline` by default lets browsers preview .md /
	// .json / images directly; `?download=1` switches to `attachment`
	// for the studio's Download button (the HTML5 `download` attribute
	// alone is unreliable across embedded WebViews + same-origin
	// previewable types). Filename hint is the basename of the path.
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(info.Path)))

	start, length, partial, rangeErr := artifactByteRange(r.Header.Get("Range"), info.Size)
	if rangeErr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size))
		http.Error(w, "requested range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if partial {
		end := start + length - 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	if r.Method == http.MethodHead {
		return
	}
	if start > 0 {
		if _, err := io.CopyN(io.Discard, rc, start); err != nil {
			s.logger.Warn("artifact file range seek failed for run %s path %s: %v", id, info.Path, err)
			return
		}
	}
	var copyErr error
	if partial {
		_, copyErr = io.CopyN(w, rc, length)
	} else {
		_, copyErr = io.Copy(w, rc)
	}
	if copyErr != nil {
		// Body partially written by now — can't surface a clean error
		// status. Log via the standard server error path; the client
		// will see a truncated response.
		s.logger.Warn("artifact file copy failed for run %s path %s: %v", id, info.Path, copyErr)
	}
}

// artifactByteRange parses the single-range subset browsers use for media.
// Multiple ranges would require a multipart/byteranges response; rejecting
// them is preferable to silently serving the wrong bytes. A missing header
// returns the whole object (partial=false).
func artifactByteRange(header string, size int64) (start, length int64, partial bool, err error) {
	if header == "" {
		return 0, size, false, nil
	}
	if size <= 0 || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, fmt.Errorf("invalid byte range")
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("unsupported byte range")
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid byte range")
	}
	if parts[0] == "" {
		suffix, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || suffix <= 0 {
			return 0, 0, false, fmt.Errorf("invalid byte range suffix")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, suffix, true, nil
	}
	start, parseErr := strconv.ParseInt(parts[0], 10, 64)
	if parseErr != nil || start < 0 || start >= size {
		return 0, 0, false, fmt.Errorf("invalid byte range start")
	}
	end := size - 1
	if parts[1] != "" {
		end, parseErr = strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || end < start {
			return 0, 0, false, fmt.Errorf("invalid byte range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true, nil
}

// artifactFileContentType picks a sensible MIME type by extension.
// Conservative — falls back to application/octet-stream for unknown
// extensions to keep browsers from auto-executing untrusted payloads
// (an in-sandbox tool could emit anything; the recipe's name doesn't
// guarantee semantic content).
func artifactFileContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	// Audio/video produced by media pipelines — without these the studio's
	// inline players get application/octet-stream and refuse to render.
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".opus":
		return "audio/opus"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping: confirm the caller can see this run before any
	// cancel mutation. In cloud mode Cancel descends into the publisher
	// with a non-request context, so without this gate the mongo tenant
	// filter never runs and a cross-tenant run could be cancelled by id.
	// (Also rejects a malformed id via the store's path-component check.)
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	// Log cancel intent with source attribution. Mystery context-canceled
	// failures during a run mid-flight typically trace back to either this
	// HTTP endpoint or the WS `cancel` envelope (handleCancel in runs_ws.go);
	// emitting a line per call site lets us tell the two apart without
	// instrumenting the runtime itself.
	if s.logger != nil {
		s.logger.Info("server: cancel run %q via HTTP from %s", id, r.RemoteAddr)
	}
	if err := s.runs.Cancel(id); err != nil {
		// If the run is not currently active in this process, the
		// operator's "cancel" intent depends on the persisted status:
		//   - dispatcher-spawned + running: the runview Manager only
		//     tracks manual studio launches, so cancel falls through
		//     here. The dispatcher owns its own cancel funcs keyed by
		//     runID — try that path before giving up.
		//   - already terminal (finished / failed / cancelled / merged):
		//     idempotent — return current state, no-op.
		//   - paused_waiting_human / failed_resumable: the operator
		//     wants to abandon the partial work. Flip the persisted
		//     status to cancelled, emit run_cancelled, and finalize the
		//     worktree so the studio's merge UI can act on whatever
		//     commits the run produced before it stalled.
		if errors.Is(err, runview.ErrRunNotActive) {
			if s.cfg.Dispatcher != nil && s.cfg.Dispatcher.CancelRun(id) {
				w.WriteHeader(http.StatusAccepted)
				s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: "cancelling"})
				return
			}
			r2, loadErr := s.runs.LoadRunCtx(r.Context(), id)
			if loadErr != nil {
				s.httpErrorFor(w, r, http.StatusNotFound, "run not active and not on disk: %v", loadErr)
				return
			}
			if cancelled, cancelErr := s.runs.CancelInactiveCtx(r.Context(), id); cancelErr == nil && cancelled {
				w.WriteHeader(http.StatusAccepted)
				s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: string(store.RunStatusCancelled)})
				return
			} else if cancelErr != nil {
				s.logger.Warn("server: cancel inactive run %s: %v", id, cancelErr)
			}
			w.WriteHeader(http.StatusAccepted)
			s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: string(r2.Status)})
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "cancel: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, cancelRunResponse{RunID: id, Status: "cancelling"})
}

// watchResponse is the body of the watch endpoints — the run's full
// subscription set after the mutation, so the studio can replace its
// local view without re-fetching the snapshot.
type watchResponse struct {
	RunID           string   `json:"run_id"`
	WatchedIssueIDs []string `json:"watched_issue_ids"`
}

// handleAddWatch subscribes a run to a native-kanban issue (MVP3b) so
// the watch coordinator forwards that issue's future board transitions
// to the run as queued messages.
func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request) {
	s.mutateWatch(w, r, true)
}

// handleRemoveWatch unsubscribes a run from a native-kanban issue.
func (s *Server) handleRemoveWatch(w http.ResponseWriter, r *http.Request) {
	s.mutateWatch(w, r, false)
}

func (s *Server) mutateWatch(w http.ResponseWriter, r *http.Request, add bool) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	issueID := r.PathValue("issueID")
	if id == "" || issueID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or issue id")
		return
	}
	rs := s.runs.RunStore()
	var (
		watched []string
		err     error
	)
	if add {
		watched, err = rs.AddWatchedIssues(r.Context(), id, []string{issueID})
	} else {
		watched, err = rs.RemoveWatchedIssues(r.Context(), id, []string{issueID})
	}
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "watch: %v", err)
		return
	}
	if watched == nil {
		watched = []string{}
	}
	s.writeJSONFor(w, r, watchResponse{RunID: id, WatchedIssueIDs: watched})
}

// pauseRunResponse is the body of POST /api/runs/{id}/pause. Mirrors
// cancelRunResponse — both expose a coarse client-friendly status
// snapshot the studio uses to update the RunHeader optimistically
// before the WS event arrives.
type pauseRunResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
}

func (s *Server) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant scoping: confirm the caller can see this run before the
	// pause mutation (see handleCancelRun for the cloud-mode rationale).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if s.logger != nil {
		s.logger.Info("server: pause run %q via HTTP from %s", id, r.RemoteAddr)
	}
	if err := s.runs.Pause(id); err != nil {
		if errors.Is(err, runview.ErrRunNotActive) {
			// 409 is the right code for "operator pause is meaningless
			// right now" — either the run is terminal or it's running
			// in another process (cloud mode). The studio hides the
			// Pause button in both cases; this is a defensive guard
			// against double-clicks racing with status changes.
			s.httpErrorFor(w, r, http.StatusConflict, "run is not active in this process")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "pause: %v", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, pauseRunResponse{RunID: id, Status: "pause_requested"})
}

// forkRunRequest is the body of POST /api/runs/{id}/fork. Mirrors
// runview.ForkSpec but kept as a separate type so the HTTP wire shape
// stays decoupled from the service struct (we can deprecate fields
// without breaking ForkSpec consumers).
type forkRunRequest struct {
	NodeID     string         `json:"node_id"`
	TurnIndex  int            `json:"turn_index,omitempty"`
	RewindCode bool           `json:"rewind_code,omitempty"`
	ForkName   string         `json:"fork_name,omitempty"`
	NewInputs  map[string]any `json:"new_inputs,omitempty"`
}

func (s *Server) handleForkRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req forkRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "decode fork request: %v", err)
		return
	}
	if req.NodeID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "node_id is required")
		return
	}
	if s.logger != nil {
		s.logger.Info("server: fork run %q at node %q from %s", id, req.NodeID, r.RemoteAddr)
	}
	result, err := s.runs.Fork(r.Context(), runview.ForkSpec{
		RunID:      id,
		NodeID:     req.NodeID,
		TurnIndex:  req.TurnIndex,
		RewindCode: req.RewindCode,
		ForkName:   req.ForkName,
		NewInputs:  req.NewInputs,
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "fork: %v", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.writeJSONFor(w, r, result)
}

func (s *Server) handleListRunSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	skills, err := s.runs.ListRunBundleSkills(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list skills: %v", err)
		return
	}
	s.writeJSONFor(w, r, skills)
}

// handleGetSessionBoard returns the LLM-curated Session-board spec for a
// run (the widgets shown beneath the task list on the Tasks tab). Returns
// a zero-value spec (version 0, no widgets) when curation never ran for
// this run — never a 404 — so the studio can render the deterministic
// task board alone.
func (s *Server) handleGetSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	spec, err := s.runs.SessionBoard(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load session board: %v", err)
		return
	}
	s.writeJSONFor(w, r, spec)
}

func (s *Server) handleListQueuedMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	msgs, err := s.runs.ListQueuedMessages(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list queued messages: %v", err)
		return
	}
	if msgs == nil {
		msgs = []store.QueuedUserMessage{}
	}
	s.writeJSONFor(w, r, map[string]any{"messages": msgs})
}

type queueMessageRequest struct {
	Text string `json:"text"`
	// Skills is the optional list of bundle skill names the operator
	// attached to this message. Each referenced SKILL.md is mirrored
	// into the run's .claude/skills/ before the engine injects the
	// message into the agent's conversation. Sticky — the skill stays
	// loaded for the rest of the run.
	Skills []string `json:"skills,omitempty"`
}

func (s *Server) handleQueueMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req queueMessageRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "text is required")
		return
	}
	var qopts []runview.QueueMessageOption
	if len(req.Skills) > 0 {
		qopts = append(qopts, runview.WithMessageSkills(req.Skills))
	}
	msg, err := s.runs.QueueMessage(r.Context(), id, req.Text, qopts...)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "queue message: %v", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.writeJSONFor(w, r, msg)
}

func (s *Server) handleCancelQueuedMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	msgID := r.PathValue("msgID")
	if id == "" || msgID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or message id")
		return
	}
	if err := s.runs.CancelQueuedMessage(r.Context(), id, msgID); err != nil {
		switch {
		case errors.Is(err, store.ErrQueuedMessageNotFound):
			s.httpErrorFor(w, r, http.StatusNotFound, "queued message not found")
		case errors.Is(err, store.ErrQueuedMessageStatusConflict):
			s.httpErrorFor(w, r, http.StatusConflict, "queued message already delivered or cancelled")
		default:
			s.httpErrorFor(w, r, http.StatusInternalServerError, "cancel queued message: %v", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		if filePath == "" && req.Source == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "file_path or source is required (run has no persisted FilePath)")
			span.SetStatus(codes.Error, "missing file_path/source")
			return
		}
	}
	// Cloud mode has no operator filesystem, so resume must carry inline
	// source — UNLESS the run's (persisted) FilePath is a catalog bundle
	// present on the pod, which we read off the pod FS just like launch.
	// The run's BotID is already persisted + propagated by SubmitResume, so
	// the runner still mirrors skills; here we only need the source bytes.
	if s.cfg.Mode == "cloud" && req.Source == "" {
		if src, _, _, ok := s.catalogBotSource("", filePath); ok {
			req.Source = src
		}
	}
	if s.cfg.Mode == "cloud" && req.Source == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "cloud mode: source or a catalog bot is required (file_path is not portable across the server pod's filesystem)")
		span.SetStatus(codes.Error, "cloud mode requires source")
		return
	}
	absPath, pathErr := s.resolveWorkflowPath(filePath, req.Source)
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
	res, err := s.runs.Resume(ctx, runview.ResumeSpec{
		RunID:    id,
		FilePath: absPath,
		Source:   req.Source,
		Answers:  req.Answers,
		Force:    req.Force,
		Timeout:  timeout,
	})
	if err != nil {
		if errors.Is(err, runtime.ErrServerDraining) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "server is draining: %v", err)
			span.SetStatus(codes.Error, "server draining")
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "resume: %v", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "resume failed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	s.writeJSONFor(w, r, launchRunResponse{RunID: res.RunID, Status: string(store.RunStatusRunning)})
}

// resolveWorkflowPath returns the absolute path the engine should
// associate with a launch / resume / answer call.
//
// Resolution rules:
//   - cloud mode + source set: return filePath as a logical label;
//     the publisher carries Source inline to the runner pod.
//   - local mode + source set: the SPA bundles the studio buffer
//     alongside file_path (e.g. for imported / freshly-saved
//     recipes — see studio/src/components/Toolbar/Toolbar.tsx).
//     The downstream subprocess (`iterion run <path>`) reads from
//     disk, so a relative basename relative to the desktop
//     process cwd would ENOENT. We materialise Source into a
//     stable per-store cache and return that absolute path.
//   - local mode without source: run through safePath as before;
//     on miss, fall back to embedded recipes shipped with the
//     binary (see materializeEmbeddedRecipe).
func (s *Server) resolveWorkflowPath(filePath, source string) (string, error) {
	if source != "" {
		if s.cfg.Mode == "cloud" {
			return filePath, nil
		}
		if materialised, ok := s.materializeInlineSource(filePath, source); ok {
			return materialised, nil
		}
		// Materialisation failed (no writable cache dir) — surface a
		// clear error rather than letting the subprocess ENOENT
		// further down the chain.
		return "", fmt.Errorf("cannot materialise inline source: no writable store/work directory configured")
	}
	abs, err := s.safePath(filePath)
	if err == nil {
		return abs, nil
	}
	// safePath rejected the input. On resume of an inline-launched run
	// (where the SPA uploaded source on launch but not on resume), the
	// persisted FilePath points at the server's inline-source cache —
	// which lives next to the run store, OUTSIDE the current WorkDir.
	// Trust paths in our own cache: the materialised file is the same
	// content the run was launched with, by construction.
	if cached, ok := s.resolveCachedInlineSource(filePath); ok {
		return cached, nil
	}
	if cached, ok := s.materializeEmbeddedRecipe(filePath); ok {
		return cached, nil
	}
	return "", err
}

// catalogBotSource resolves an inline source for a catalog bot referenced by
// an explicit bot id or a catalog-shaped file path. It reads the bundle's
// main.bot off the server pod's own bots/ tree (the same FS-read the
// webhook/scheduler/board/trigger launchers use via resolveBotSource) and
// returns the source, its absolute path, and the resolved bot id.
//
// This is what lets a CLOUD launch/resume reference a catalog bundle
// (whats-next, review-pr, …) without the client uploading its bytes: the
// bundle is present on both the server and runner pod images, and returning
// the bot id lets the caller set LaunchSpec.BotID so the runner mirrors the
// bundle's skills. ok=false when the id/path does not resolve to a real
// catalog bundle — the caller then keeps the strict cloud "source required"
// gate, so an arbitrary operator/workspace path is never read off the pod.
func (s *Server) catalogBotSource(botID, filePath string) (source, path, resolvedID string, ok bool) {
	id := strings.TrimSpace(botID)
	if id == "" {
		id = inferCatalogBotID(filePath)
	}
	if id == "" {
		return "", "", "", false
	}
	p, src, err := s.resolveBotSource(id)
	if err != nil {
		return "", "", "", false
	}
	return src, p, id, true
}

// inferCatalogBotID extracts a catalog bot name from a workflow file path:
// "bots/whats-next/main.bot", "/opt/iterion/bots/whats-next/main.bot",
// "examples/foo/main.bot", "whats-next/main.bot", "whats-next", and
// "hello.bot" all map to their bundle-dir / basename. Returns "" for an
// absolute path with no bots|examples segment (an arbitrary workspace file
// that must still carry inline source in cloud). The returned id is only a
// candidate — catalogBotSource confirms it against the real catalog.
func inferCatalogBotID(filePath string) string {
	fp := filepath.ToSlash(strings.TrimSpace(filePath))
	if fp == "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(fp, "./"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "bots" || parts[i] == "examples" {
			return parts[i+1]
		}
	}
	if filepath.IsAbs(filePath) {
		return ""
	}
	if len(parts) >= 2 && parts[len(parts)-1] == "main.bot" {
		return parts[len(parts)-2]
	}
	if len(parts) == 1 {
		return strings.TrimSuffix(parts[0], ".bot")
	}
	return ""
}

// resolveCachedInlineSource returns filePath unchanged when it points at an
// existing file under the server's inline-source cache directory. Used as a
// fallback in resolveWorkflowPath when safePath rejects an absolute path
// that the server itself wrote during a previous inline launch.
func (s *Server) resolveCachedInlineSource(filePath string) (string, bool) {
	if !filepath.IsAbs(filePath) {
		return "", false
	}
	cacheRoot := s.inlineSourceCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	cacheAbs, err := filepath.Abs(cacheRoot)
	if err != nil {
		return "", false
	}
	clean := filepath.Clean(filePath)
	if !pathContains(cacheAbs, clean) {
		return "", false
	}
	info, err := os.Stat(clean)
	if err != nil || info.IsDir() {
		return "", false
	}
	return clean, true
}

// materializeInlineSource writes the SPA-provided inline workflow content
// into a stable per-store cache directory and returns its absolute
// path. The cache lives at <storeDir>/inline-sources/<sha12>-<basename>:
//   - the file persists for the lifetime of the run store (resume,
//     inspect, report all keep working without needing the original
//     buffer to still be open in the studio);
//   - identical source content reuses the same cache file (idempotent);
//   - different content for the same basename does NOT overwrite —
//     each run's persisted FilePath uniquely identifies the bytes it
//     was launched with, so resume always replays the original source
//     even when a newer launch of the same recipe touched the cache.
//
// When filePath is empty (an studio-only buffer that was never saved on
// disk), we synthesise a basename of "inline.bot" so the cache layout
// stays predictable.
//
// Returns ok=false when no writable cache dir can be derived — the
// caller surfaces a 400 rather than letting the subprocess ENOENT.
func (s *Server) materializeInlineSource(filePath, source string) (string, bool) {
	base := filepath.Base(filePath)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "inline.bot"
	}
	cacheRoot := s.inlineSourceCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(source))
	prefix := hex.EncodeToString(sum[:6])
	dst := filepath.Join(cacheRoot, prefix+"-"+base)
	if err := os.WriteFile(dst, []byte(source), 0o644); err != nil {
		return "", false
	}
	return dst, true
}

// inlineSourceCacheDir picks the directory under which inline-source
// recipes are materialised. Mirrors store.ResolveStoreDir's git-style
// discovery (walks up from WorkDir looking for an existing .iterion)
// so the cache lands alongside the actual run store. A divergent
// fallback would let materialisation succeed but leave the spawned
// runner subprocess unable to find the recipe (the runner resolves
// its store via the same git-style walk, so it would look at the
// ancestor .iterion, not <workDir>/.iterion).
func (s *Server) inlineSourceCacheDir() string {
	storeDir := s.resolvedStoreDir()
	if storeDir == "" {
		storeDir = filepath.Join(os.TempDir(), "iterion-inline-sources")
	}
	return filepath.Join(storeDir, "inline-sources")
}

// resolvedStoreDir returns the canonical run-store directory the
// runview Service is rooted at, mirroring the resolution rule used
// at server construction (server.go: store.ResolveStoreDir(...)).
// Empty when neither StoreDir nor WorkDir was configured (e.g. tests
// that build a Config{} directly with no FS context).
func (s *Server) resolvedStoreDir() string {
	if s.cfg.StoreDir == "" && s.cfg.WorkDir == "" {
		return ""
	}
	return store.ResolveStoreDir(s.cfg.WorkDir, s.cfg.StoreDir)
}

// materializeEmbeddedRecipe writes an embedded recipe into a stable
// per-run-store directory (one copy per binary release) and returns
// its absolute path. The lookup key is filePath as given; the caller
// passes whatever the API received, so a UI that lists recipes by
// basename ("feature-dev/main.bot" or another embedded bot path) all
// resolve correctly.
//
// We materialise rather than reading from embed.FS at execution time
// because the engine, parser, and several runtime helpers operate on
// real filesystem paths (worktree relative paths, file-watcher,
// sandbox bind-mounts). Materialisation keeps that contract intact at
// the cost of a tiny one-time disk write per recipe per run-store.
//
// Returns ok=false when the recipe is not in the embed FS, or when
// the server has no writable store dir to cache it under.
func (s *Server) materializeEmbeddedRecipe(filePath string) (string, bool) {
	data, ok := bots.Get(filePath)
	if !ok {
		return "", false
	}
	cacheRoot := s.embeddedRecipeCacheDir()
	if cacheRoot == "" {
		return "", false
	}
	dst := filepath.Join(cacheRoot, filePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", false
	}
	// Idempotent: skip the write if the cached file already matches.
	// Compare bytes, not just length — a same-length but changed recipe
	// must be rewritten, not silently served from the stale cache.
	if existing, err := os.ReadFile(dst); err == nil && bytes.Equal(existing, data) {
		return dst, true
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", false
	}
	return dst, true
}

// embeddedRecipeCacheDir returns the directory under the run store
// where embedded recipes are materialised, or "" when no store dir is
// configured (in which case embedded recipes are unavailable). Mirrors
// store.ResolveStoreDir's git-style discovery so the cache lands
// alongside the actual run store — a divergent fallback (e.g.
// <workDir>/.iterion when ResolveStoreDir picked an ancestor's
// .iterion) would create stale recipes in a directory the engine
// never reads.
func (s *Server) embeddedRecipeCacheDir() string {
	storeDir := s.resolvedStoreDir()
	if storeDir == "" {
		storeDir = filepath.Join(os.TempDir(), "iterion-embedded-recipes")
	}
	return filepath.Join(storeDir, "embedded-recipes")
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
