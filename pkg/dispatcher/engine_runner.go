package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RunLauncher is the ADR-046 single-launch-authority seam. Given a
// runview.LaunchSpec it launches the run and blocks until it reaches a
// terminal status, returning the run's terminal Go error (nil=finished,
// runtime.ErrRunPaused/ErrRunPausedOperator on a pause, the failure error
// otherwise). When an EngineRunner is built WithRunLauncher AND
// ITERION_DISPATCH_VIA_SERVICE is set, a fresh (non-resume, non-bundle)
// Dispatch routes through it instead of building a private engine — so the
// dispatcher, the trigger spine, the CLI and the studio all converge on
// runview.Service.Launch as the one launch path.
type RunLauncher interface {
	LaunchAndWait(ctx context.Context, spec runview.LaunchSpec) error
}

// ServiceRunLauncher adapts a *runview.Service to RunLauncher. The Service
// MUST be constructed against the SAME store dir the dispatcher writes to,
// so the run records it creates are the ones the dispatcher's resume /
// stall / cost-cap checks read back. It captures the run's terminal error
// via LaunchSpec.OnOutcome and returns it once LaunchResult.Done closes, so
// the caller sees the same typed error the direct engine.Run would have.
type ServiceRunLauncher struct{ Svc *runview.Service }

// LaunchAndWait implements RunLauncher.
func (l ServiceRunLauncher) LaunchAndWait(ctx context.Context, spec runview.LaunchSpec) error {
	var outcome error
	spec.OnOutcome = func(err error) { outcome = err }
	res, err := l.Svc.Launch(ctx, spec)
	if err != nil {
		return err
	}
	// Done closes after OnOutcome fires (both in the run goroutine, the
	// close a defer that runs on return); the channel close is the
	// happens-before edge that publishes `outcome` to this goroutine.
	<-res.Done
	return outcome
}

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
	// sealer is the local secret store's master-key sealer, built once (lazy —
	// resolves the key on first use) and reused across dispatches so a
	// secret-declaring bot doesn't pay a keychain round-trip per run. The
	// per-run store is rebuilt in Dispatch to pick up secret edits.
	sealer secrets.Sealer
	// launcher, when non-nil, is the ADR-046 convergence seam: a fresh
	// (non-resume, plain-.bot) Dispatch routes through it — gated on the
	// ITERION_DISPATCH_VIA_SERVICE env — instead of building a private
	// engine. nil keeps today's direct-engine path (the default).
	launcher RunLauncher
}

// EngineRunnerOption configures an EngineRunner at construction time.
type EngineRunnerOption func(*EngineRunner)

// WithRunLauncher wires the ADR-046 launch-authority seam. See RunLauncher.
func WithRunLauncher(l RunLauncher) EngineRunnerOption {
	return func(r *EngineRunner) { r.launcher = l }
}

// NewEngineRunner pre-compiles the workflow at workflowPath. The
// resulting EngineRunner can dispatch concurrently across many issues
// using the same compiled IR.
func NewEngineRunner(workflowPath string, logger *iterlog.Logger, opts ...EngineRunnerOption) (*EngineRunner, error) {
	if workflowPath == "" {
		return nil, fmt.Errorf("engine runner: workflow path required")
	}
	var warn func(string, ...any)
	if logger != nil {
		warn = logger.Warn
	}
	r := &EngineRunner{
		workflowPath: workflowPath,
		logger:       logger,
		bundleClean:  func() error { return nil },
		// Lazy: no keychain/keyfile access until the first Seal/Open during a
		// run that actually materialises a secret.
		sealer: secrets.NewLazyLocalSealer(store.GlobalIterionDataDir(), warn),
	}
	for _, opt := range opts {
		opt(r)
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
	// ADR-046 convergence: route a fresh, plain-.bot dispatch through the
	// single launch authority (runview.Service.Launch) when wired + enabled.
	// Resume dispatches and bundle-backed runners stay on the direct path —
	// the checkpoint/worktree reuse and the shared bundle handle live there.
	if r.launcher != nil && r.bundle == nil && spec.ResumeFromRunID == "" && dispatchViaServiceEnabled() {
		return r.dispatchViaService(ctx, spec)
	}
	baseStore, err := store.New(spec.StoreDir, store.WithLogger(r.logger))
	if err != nil {
		return fmt.Errorf("engine runner: open store: %w", err)
	}
	// Hold the run flock for the run's lifetime — the liveness signal the
	// orphan reconciler probes (runview.reconcileOrphans). A dispatcher-owned
	// run is neither in runview's manager nor a detached-.pid runner, so
	// without the lock a server-side reconcile tick flips it failed while
	// its engine and delegate subprocesses are still working.
	if lock, lerr := baseStore.LockRun(ctx, spec.RunID); lerr == nil {
		defer func() { _ = lock.Unlock() }()
	} else {
		r.logger.Warn("engine runner: run lock %s unavailable (%v) — the orphan reconciler may misjudge this run as dead", spec.RunID, lerr)
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
	execSpec := runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: r.workflow,
		Store:    s,
		Inbox:    &model.StoreInboxBinder{Store: s},
		RunID:    spec.RunID,
		Logger:   runLogger,
		StoreDir: spec.StoreDir,
		// The dispatcher runs a bot per ticket, so it has a real identity to
		// key bot-scoped memory on. Empty for a standalone `.bot`, where the
		// executor falls back to the workflow name — the same rule every other
		// launch surface follows, so a bot dispatched here and the same bot run
		// from the CLI or the studio share one memory instead of three.
		BotID: runview.ResolveBotID("", r.bundleName(), r.workflowPath),
	}
	// Local (self-hosted) secret injection: the dispatcher's in-process runner
	// is only ever the local path (never a cloud runner pod), so resolve the
	// workflow's declared secrets from the local sealed store — the same wiring
	// the CLI/studio launch paths use — so a dispatched bot that declares
	// `secrets:` gets them injected instead of running with them unset. Gated on
	// declared secrets so a secretless catalog bot never touches the store. The
	// sealer is the cached lazy one (r.sealer); the store is rebuilt per run to
	// pick up secret edits between dispatches.
	if len(r.workflow.Secrets) > 0 {
		lstore, lerr := secrets.LocalStoreForProject(spec.StoreDir)
		if lerr != nil {
			return fmt.Errorf("engine runner: local secrets store: %w", lerr)
		}
		execSpec.LocalSecrets = lstore
		execSpec.LocalSealer = r.sealer
	}
	exec, err := runview.BuildExecutor(execSpec)
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

// dispatchViaService (ADR-046) runs a fresh dispatch through the shared
// launch authority instead of a private engine. It translates the
// DispatchSpec's four dispatcher invariants into the matching LaunchSpec
// fields — WorkspacePath→WorkDir, DailyCap→DailyCap, Issue→SourceRef,
// OnEvent→ExtraObservers (fired on EVERY store AppendEvent, matching the
// heartbeatStore the direct path installs) — and blocks on the launcher,
// returning the run's terminal error so the dispatcher's park/retry/stall
// logic is byte-identical.
func (r *EngineRunner) dispatchViaService(ctx context.Context, spec DispatchSpec) error {
	ls := runview.LaunchSpec{
		FilePath: r.workflowPath,
		RunID:    spec.RunID,
		Vars:     stringifyVars(spec.Vars),
		WorkDir:  spec.WorkspacePath,
		DailyCap: spec.DailyCap,
	}
	if spec.Issue != nil && spec.Issue.ID != "" {
		ls.SourceRef = &store.RunSource{
			Kind:            store.RunSourceKindDispatcher,
			IssueID:         spec.Issue.ID,
			IssueIdentifier: spec.Issue.Identifier,
			IssueTitle:      spec.Issue.Title,
		}
	}
	if spec.OnEvent != nil {
		onEvent := spec.OnEvent
		ls.ExtraObservers = []func(store.Event){
			func(evt store.Event) { onEvent(string(evt.Type)) },
		}
	}
	r.logger.Info("dispatch: routing run %s through the shared launch authority (ADR-046)", spec.RunID)
	return r.launcher.LaunchAndWait(ctx, ls)
}

// stringifyVars flattens the dispatcher's map[string]any bot-args into the
// LaunchSpec's map[string]string. Dispatcher vars are string bot-args in
// practice; a non-string value is rendered with %v so it still reaches the
// bot rather than being dropped.
func stringifyVars(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

// dispatchViaServiceEnabled reports whether the ADR-046 convergence route
// is switched on. Default OFF — the direct-engine path is unchanged unless
// an operator opts in with ITERION_DISPATCH_VIA_SERVICE=1 (or true/on/yes).
func dispatchViaServiceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ITERION_DISPATCH_VIA_SERVICE"))) {
	case "1", "true", "on", "yes":
		return true
	}
	if b, err := strconv.ParseBool(os.Getenv("ITERION_DISPATCH_VIA_SERVICE")); err == nil {
		return b
	}
	return false
}

// bundleName is the dispatched bundle's declared id, or "" for a plain .bot.
// It outranks the path because a `.botz` lives in a content-hash cache slot,
// whose name changes with every edit to the bundle.
func (r *EngineRunner) bundleName() string {
	if r.bundle == nil {
		return ""
	}
	return r.bundle.Manifest.Name
}
