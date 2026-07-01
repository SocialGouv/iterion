package dispatcher

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// EngineRunner is the production Runner: each Dispatch compiles a
// fresh executor for the requested RunID and drives the iterion
// runtime engine until completion or cancellation.
//
// The workflow source can be a plain `.bot` file, a `.botz`
// archive, or an unpacked bundle directory. Bundles are opened once
// at NewEngineRunner — the bundle handle is shared across dispatches,
// then released via Close() when the dispatcher shuts down.
type EngineRunner struct {
	workflow     *ir.Workflow
	workflowPath string
	workflowHash string
	bundle       *bundle.Bundle // nil for plain .bot
	bundleClean  func() error   // no-op when bundle is nil
	closeOnce    sync.Once      // guards bundleClean against concurrent/repeat Close
	closeErr     error          // result of the single bundleClean run
	logger       *iterlog.Logger
}

// NewEngineRunner pre-compiles the workflow at workflowPath. The
// resulting EngineRunner can dispatch concurrently across many issues
// using the same compiled IR.
func NewEngineRunner(workflowPath string, logger *iterlog.Logger) (*EngineRunner, error) {
	if workflowPath == "" {
		return nil, fmt.Errorf("engine runner: workflow path required")
	}
	r := &EngineRunner{
		workflowPath: workflowPath,
		logger:       logger,
		bundleClean:  func() error { return nil },
	}

	kind, err := bundle.Detect(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("engine runner: detect %s: %w", workflowPath, err)
	}
	switch kind {
	case bundle.KindBundle:
		opened, cleanup, openErr := bundle.Open(workflowPath, "")
		if openErr != nil {
			return nil, fmt.Errorf("engine runner: open bundle %s: %w", workflowPath, openErr)
		}
		wf, h, compileErr := runview.CompileBundleWorkflow(opened.IterPath, opened)
		if compileErr != nil {
			_ = cleanup()
			return nil, fmt.Errorf("engine runner: compile bundle %s: %w", workflowPath, compileErr)
		}
		r.workflow = wf
		r.workflowHash = h
		r.workflowPath = opened.IterPath
		r.bundle = opened
		r.bundleClean = cleanup
	case bundle.KindBundleDir:
		opened, openErr := bundle.OpenDir(workflowPath)
		if openErr != nil {
			return nil, fmt.Errorf("engine runner: open bundle dir %s: %w", workflowPath, openErr)
		}
		wf, h, compileErr := runview.CompileBundleWorkflow(opened.IterPath, opened)
		if compileErr != nil {
			return nil, fmt.Errorf("engine runner: compile bundle dir %s: %w", workflowPath, compileErr)
		}
		r.workflow = wf
		r.workflowHash = h
		r.workflowPath = opened.IterPath
		r.bundle = opened
	default:
		wf, h, compileErr := runview.CompileWorkflowWithHash(workflowPath)
		if compileErr != nil {
			return nil, fmt.Errorf("engine runner: compile %s: %w", workflowPath, compileErr)
		}
		r.workflow = wf
		r.workflowHash = h
	}
	return r, nil
}

// Workflow returns the compiled IR. Useful for callers that want to
// validate dispatch.vars keys against the workflow's declared vars at
// startup.
func (r *EngineRunner) Workflow() *ir.Workflow { return r.workflow }

// DeclaredVars returns the set of var names the compiled workflow
// declares. The routeKey argument is ignored — a single-workflow runner
// has no routing — but kept so EngineRunner and RoutingRunner share the
// `DeclaredVars(string) map[string]struct{}` shape the dispatcher type-
// asserts in buildSpec. Returns nil when the workflow is unset.
//
// The dispatcher uses this to warn at dispatch time when a per-ticket
// bot_arg names a var the routed workflow does not declare: resolveVars
// (pkg/runtime/engine.go) silently drops undeclared input keys, so an
// unvalidated bot_arg would otherwise reach the bot as if unset with no
// signal anywhere.
func (r *EngineRunner) DeclaredVars(string) map[string]struct{} {
	if r.workflow == nil {
		return nil
	}
	out := make(map[string]struct{}, len(r.workflow.Vars))
	for name := range r.workflow.Vars {
		out[name] = struct{}{}
	}
	return out
}

// Close releases any resources tied to the workflow source — in
// particular, removes the extraction directory of a `.botz` archive.
// Safe to call multiple times AND concurrently: the cleanup runs exactly
// once (sync.Once), so two racing Close calls can't both invoke
// RemoveAll on the extraction dir (the prior nil-check + swap was a
// read-read-write race that let both pass).
func (r *EngineRunner) Close() error {
	r.closeOnce.Do(func() {
		if r.bundleClean != nil {
			r.closeErr = r.bundleClean()
		}
	})
	return r.closeErr
}

// Dispatch implements Runner. Opens the store, builds an executor for
// this RunID, registers an event observer that bridges every event to
// spec.OnEvent, and drives engine.Run to completion.
func (r *EngineRunner) Dispatch(ctx context.Context, spec DispatchSpec) error {
	if spec.StoreDir == "" {
		return fmt.Errorf("engine runner: spec.StoreDir is required")
	}
	baseStore, err := store.New(spec.StoreDir, store.WithLogger(r.logger))
	if err != nil {
		return fmt.Errorf("engine runner: open store: %w", err)
	}
	// Wrap so spec.OnEvent fires on EVERY AppendEvent — high-frequency
	// tool_started/tool_called events emitted by the backend hooks
	// (pkg/backend/model/hooks.go) write straight to the store and
	// would otherwise skip the runtime engine's WithEventObserver hook.
	// The dispatcher's stall heartbeat depends on these events; the
	// 2026-05-21 dogfood saw runs cancelled at the 10min mark because
	// only ~5 engine-level events ever made it to OnEvent.
	s := newHeartbeatStore(baseStore, spec.OnEvent)

	// Tee the dispatcher's main logger to a per-run run.log file
	// alongside events.jsonl. Without this, the studio's run-log
	// viewer renders "No log captured" on every dispatcher-spawned
	// run because the file simply doesn't exist (the in-process
	// runner has nobody writing it — vs the CLI runner which calls
	// the same helper). The executor + engine below both pick up
	// this wrapped logger so claude_code's per-turn lines, tool
	// hints, and budget warnings all land in the file the SPA
	// tails.
	runLogger, logCloser := store.TeeRunLog(
		r.logger, r.logger.Level(),
		spec.StoreDir,
		spec.RunID,
	)
	if logCloser != nil {
		defer func() {
			if cerr := logCloser.Close(); cerr != nil {
				r.logger.Warn("engine runner: close run.log: %v", cerr)
			}
		}()
	}

	// Wire the operator-chatbox inbox so chatbox messages queued during
	// a dispatcher run are drained mid-iteration by both claw (via
	// opts.Inbox in the generation loop) and claude_code (via the
	// PostToolUse hook on the delegate). Without this the operator's
	// message stays `queued` for the entire run because nothing binds a
	// hook to the per-run queue.
	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: r.workflow,
		Store:    s,
		Inbox:    &model.StoreInboxBinder{Store: s},
		RunID:    spec.RunID,
		Logger:   runLogger,
		StoreDir: spec.StoreDir,
	})
	if err != nil {
		return fmt.Errorf("engine runner: build executor: %w", err)
	}
	if c, ok := any(exec).(io.Closer); ok {
		defer func() {
			if cerr := c.Close(); cerr != nil {
				r.logger.Warn("engine runner: executor close: %v", cerr)
			}
		}()
	}

	opts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithWorkflowHash(r.workflowHash),
		runtime.WithFilePath(r.workflowPath),
		runtime.WithRunName(store.GenerateRunName(r.workflowPath + ":" + spec.RunID)),
		// Without this, every transient hiccup (http2 timeout against
		// the ChatGPT-codex endpoint, an LLM rate-limit 429, a DNS
		// flutter, …) fails the run terminally at the first attempt —
		// the runview/studio launch path wires the same dispatcher and
		// gets 6 exponential-backoff retries on network transients,
		// but dispatcher-spawned bot runs had no recovery policy at
		// all. Today's dogfood caught it: Nexie's explore retried
		// gracefully through two http2 timeouts, then feature_dev's
		// reviewer_gpt hit the same error and died on the first try.
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
	}
	// Stamp the issue back-reference so the studio's RunHeader can
	// link to the originating kanban ticket. Only set when the
	// dispatcher actually populated the spec — direct CLI / studio
	// launches leave these empty and the Source field stays nil.
	if spec.Issue != nil && spec.Issue.ID != "" {
		opts = append(opts, runtime.WithSource(&store.RunSource{
			Kind:            store.RunSourceKindDispatcher,
			IssueID:         spec.Issue.ID,
			IssueIdentifier: spec.Issue.Identifier,
			IssueTitle:      spec.Issue.Title,
		}))
	}
	opts = append(opts,
		// Per-issue workspace becomes the runtime workDir so ${PROJECT_DIR}
		// in bot var defaults expands to the dispatcher's isolated
		// worktree path, not the daemon's cwd (= host repo). Without
		// this, docs-refresh's `workspace_dir: "${PROJECT_DIR}"` resolved
		// to the host repo and fix_claude Edit calls landed directly
		// on the operator's working tree (2026-05-21 dogfood). The
		// after_create hook in dispatch_defaults.go seeds the path
		// via `git worktree add --detach`.
		runtime.WithWorkDir(spec.WorkspacePath),
		runtime.WithEventObserver(func(evt store.Event) {
			if spec.OnEvent != nil {
				spec.OnEvent(string(evt.Type))
			}
		}),
	)
	if r.bundle != nil {
		opts = append(opts, runtime.WithBundle(r.bundle))
	}
	// Enforce the per-(store, UTC-day) spend cap when the dispatcher wired
	// one onto the spec. Without this, dispatcher-launched runs neither
	// record spend into the shared ledger nor self-pause, so the
	// dispatcher's refreshCostCap gate would read a ledger nobody writes
	// to and the cap would never fire — the primary surface for
	// limits.max_cost_per_day_usd. WithDailyCap(nil) is inert.
	if spec.DailyCap != nil {
		opts = append(opts, runtime.WithDailyCap(spec.DailyCap))
	}
	eng := runtime.New(r.workflow, s, exec, opts...)

	// Resume the prior run iff the dispatcher's scheduleRetry tagged
	// this dispatch as a resume — the engine's Resume picks up at the
	// failing node, reuses the worktree, and skips re-execution of
	// already-completed upstream nodes. The dispatcher only sets
	// ResumeFromRunID when the prior run is actually resumable
	// (failed_resumable / cancelled / paused_operator); a fresh runID
	// means a clean start.
	if spec.ResumeFromRunID != "" {
		return eng.Resume(ctx, spec.ResumeFromRunID, nil)
	}

	// Resolve the mono/dual review topology for a fresh dispatch (no-op
	// unless the workflow declares a review_mode var). A per-ticket
	// bot_arg review_mode wins over auto-detection; no dispatcher-level
	// flag override, so pass "". Mirrors the CLI and runview surfaces.
	if spec.Vars == nil {
		spec.Vars = map[string]any{}
	}
	if mode, family, injected := reviewtopology.InjectIfDeclared(r.workflow, spec.Vars, detect.Detect(ctx), ""); injected {
		r.logger.Info("review topology: %s%s", mode, func() string {
			if family != "" {
				return " (family " + family + ")"
			}
			return ""
		}())
	}
	return eng.Run(ctx, spec.RunID, spec.Vars)
}
