package dispatcher

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// EngineRunner is the production Runner: each Dispatch compiles a
// fresh executor for the requested RunID and drives the iterion
// runtime engine until completion or cancellation.
//
// The workflow source can be a plain `.iter` / `.bot` file, a `.botz`
// archive, or an unpacked bundle directory. Bundles are opened once
// at NewEngineRunner — the bundle handle is shared across dispatches,
// then released via Close() when the dispatcher shuts down.
type EngineRunner struct {
	workflow     *ir.Workflow
	workflowPath string
	workflowHash string
	bundle       *bundle.Bundle // nil for plain .iter/.bot
	bundleClean  func() error   // no-op when bundle is nil
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

// Close releases any resources tied to the workflow source — in
// particular, removes the extraction directory of a `.botz` archive.
// Safe to call multiple times.
func (r *EngineRunner) Close() error {
	if r.bundleClean == nil {
		return nil
	}
	clean := r.bundleClean
	r.bundleClean = func() error { return nil }
	return clean()
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
		filepath.Join(spec.StoreDir, "runs", spec.RunID),
	)
	if logCloser != nil {
		defer func() {
			if cerr := logCloser.Close(); cerr != nil {
				r.logger.Warn("engine runner: close run.log: %v", cerr)
			}
		}()
	}

	exec, err := runview.BuildExecutor(runview.ExecutorSpec{
		Ctx:      ctx,
		Workflow: r.workflow,
		Store:    s,
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
		// Per-issue workspace becomes the runtime workDir so ${PROJECT_DIR}
		// in bot var defaults expands to the dispatcher's isolated
		// worktree path, not the daemon's cwd (= host repo). Without
		// this, doc-align's `workspace_dir: "${PROJECT_DIR}"` resolved
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
	}
	if r.bundle != nil {
		opts = append(opts, runtime.WithBundle(r.bundle))
	}
	eng := runtime.New(r.workflow, s, exec, opts...)

	return eng.Run(ctx, spec.RunID, spec.Vars)
}
