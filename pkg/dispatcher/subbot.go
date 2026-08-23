package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// maxSubbotDepth bounds nested subbot recursion so a child that (directly or
// transitively) runs itself fails cleanly instead of overflowing. Mirrors the
// guard in pkg/cli and pkg/runview — the three runners must agree so a bot
// behaves identically whichever surface launched its parent.
const maxSubbotDepth = 8

type subbotDepthKey struct{}

// subbotRunnerForDispatch builds the runtime.SubbotRunner wired into the
// dispatcher's in-process engine.
//
// Without it, EVERY `subbot` node of a dispatched bot died with
// `no SubbotRunner is wired`: the CLI (pkg/cli/run.go, pkg/cli/resume.go) and
// the studio (pkg/runview/service_launch.go) each wired one, the dispatcher's
// direct engine path never did. The ADR-046 route that would have borrowed the
// studio's is inert — WithRunLauncher has no non-test caller, so `r.launcher`
// is always nil and ITERION_DISPATCH_VIA_SERVICE can't switch it on. The
// symptom hid well: a ticket's FIRST dispatch died at its first subbot, and an
// operator resuming from the CLI got a working runner, so the bot looked
// merely flaky rather than structurally unable to run under the dispatcher.
// Its retries were no escape either — they re-enter this same engine.
//
// It mirrors subbotRunnerForCLI (same store, same re-attach records, same
// pause-parking) rather than the studio's, because the dispatcher has no
// per-run control plane to register a child with: its only control signal is
// the parent ctx, which childCtx descends from. What it does NOT borrow from
// the CLI is the working directory — the CLI's cwd already IS the workspace,
// whereas here the daemon's cwd is the host repo, so workDir must be threaded
// explicitly or the child resolves relative paths (a bot's `.venv/bin/python`)
// against the wrong tree.
func subbotRunnerForDispatch(parentPath, storeDir, workDir string, s store.RunStore, sealer secrets.Sealer, logger *iterlog.Logger) runtime.SubbotRunner {
	parentDir := filepath.Dir(parentPath)
	return func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		depth, _ := ctx.Value(subbotDepthKey{}).(int)
		if depth >= maxSubbotDepth {
			return nil, fmt.Errorf("subbot recursion too deep (>%d) at %q — possible cycle", maxSubbotDepth, req.Source)
		}

		// Re-attach to an in-flight/finished child from a prior (interrupted)
		// execution of this subbot node before spawning a fresh one — the
		// dispatcher resumes a failed run on its own retry path, so this is
		// the difference between picking a paid child back up and paying for
		// it twice.
		if out, aerr, handled := runview.ReattachSubbotChild(ctx, s, req, logger); handled {
			return out, aerr
		}

		childPath := req.Source
		if !filepath.IsAbs(childPath) {
			childPath = filepath.Join(parentDir, childPath)
		}
		childWf, hash, err := runview.CompileWorkflowWithHash(childPath)
		if err != nil {
			return nil, fmt.Errorf("compile child %q: %w", req.Source, err)
		}
		childRunID, err := store.GenerateRunID()
		if err != nil {
			return nil, err
		}
		// Record the child on the parent BEFORE running it, so a restart while
		// parked below re-attaches instead of spawning fresh.
		runview.RecordSubbotChild(ctx, s, req, childRunID, logger)

		// Hold the CHILD's run flock for its active pass. runview's orphan reaper
		// uses LockRun as its liveness probe, and a dispatcher-spawned child is
		// neither in runview's Manager (the studio's runner registers one; there
		// is nothing here to register with) nor a detached-.pid runner — so
		// without this a reconcile tick reads an unlocked `running` child as dead
		// and flips it to failed_resumable WHILE its engine and delegate
		// subprocesses are still working. Observed on the first dispatched run
		// that reached a subbot: the child was reaped 2.5 minutes in and only
		// survived because its human-gate write landed 16 seconds later and won.
		// A long child — a Blender render, a video generation — has no such luck.
		// Released as soon as the active pass returns (see below), never held
		// across the park.
		var releaseLock func()
		if lock, lerr := s.LockRun(ctx, childRunID); lerr == nil {
			releaseLock = func() { _ = lock.Unlock() }
		} else {
			releaseLock = func() {}
			logger.Warn("subbot child %s: run lock unavailable (%v) — the orphan reaper may misjudge it as dead mid-flight", childRunID, lerr)
		}

		execSpec := runview.ExecutorSpec{
			Ctx:      ctx,
			Workflow: childWf,
			Store:    s,
			RunID:    childRunID,
			Logger:   logger,
			StoreDir: storeDir,
			// A subbot is a DIFFERENT bot from its parent, so it keys its own
			// bot-scoped memory — derived from the CHILD's path, exactly as the
			// CLI and studio runners do. Without it the executor falls back to
			// the child workflow's name and the same subbot ends up with two
			// memory spaces depending on which surface launched the parent.
			BotID: runview.ResolveBotID("", runview.BundleNameForPath(childPath), childPath),
		}
		// Local secret injection, gated on the CHILD's own declaration: a
		// child that declares `secrets:` needs them resolved even when the
		// parent declared none, and a secretless child never touches the store.
		if len(childWf.Secrets) > 0 {
			lstore, lerr := secrets.LocalStoreForProject(storeDir)
			if lerr != nil {
				releaseLock()
				return nil, fmt.Errorf("engine runner: local secrets store for child %q: %w", req.Source, lerr)
			}
			execSpec.LocalSecrets = lstore
			execSpec.LocalSealer = sealer
		}
		childExec, err := runview.BuildExecutor(execSpec)
		if err != nil {
			releaseLock()
			return nil, err
		}

		// Capture the child's terminal-node output (the last node before Done)
		// as the subbot's result. The callback fires concurrently when the
		// child fans out parallel branches, so the capture is mutex-guarded.
		var (
			lastMu sync.Mutex
			last   map[string]any
		)
		opts := []runtime.EngineOption{
			runtime.WithLogger(logger),
			runtime.WithWorkflowHash(hash),
			runtime.WithFilePath(childPath),
			runtime.WithParentRunID(req.ParentRunID),
			runtime.WithParentNodeID(req.NodeID),
			// Recursive wiring so a child that itself declares subbot nodes can
			// run them (grandchild sources resolve relative to the CHILD's
			// dir); the ctx-carried depth keeps the recursion bounded.
			runtime.WithSubbotRunner(subbotRunnerForDispatch(childPath, storeDir, workDir, s, sealer, logger)),
			runtime.WithOnNodeFinished(func(_, _ string, out map[string]any) {
				if out != nil {
					lastMu.Lock()
					last = out
					lastMu.Unlock()
				}
			}),
		}
		if workDir != "" {
			opts = append(opts, runtime.WithWorkDir(workDir))
		}
		childEng := runtime.New(childWf, s, childExec, opts...)
		childCtx := context.WithValue(ctx, subbotDepthKey{}, depth+1)
		runErr := childEng.Run(childCtx, childRunID, req.Vars)
		// Release the flock the moment the ACTIVE pass returns — before the park
		// below, which can last hours. A parked child is paused, so the reaper
		// leaves it alone, and an external `iterion resume` of it must be able to
		// take the lock itself. Holding it across the park would block that
		// resume — the very thing the park is waiting for.
		releaseLock()
		// Close promptly — BEFORE a potentially hours-long human wait below — so
		// per-child MCP servers / board-store watchers don't accumulate under
		// parallel fan-out (the inotify-instance exhaustion #197 fixed for the
		// studio). A `defer` here would hold all N of them open for the whole
		// park, and this path is the fan-out-heavy one: a chapter bot dispatches
		// N identity subbots at once and every one of them parks on its gate.
		if c, ok := any(childExec).(io.Closer); ok {
			_ = c.Close()
		}
		if runErr != nil {
			// A human gate inside the child pauses the CHILD run; that is not a
			// parent failure. Park this subbot node until the operator answers
			// the child's review and it reaches a terminal state, then pick up
			// its output from the store.
			if errors.Is(runErr, runtime.ErrRunPaused) || errors.Is(runErr, runtime.ErrRunPausedOperator) {
				out, aerr := runview.AwaitSubbotTerminal(childCtx, s, childRunID, logger)
				if aerr == nil {
					// Consumed — tidy the record. On error (shutdown mid-park or
					// a failed child) LEAVE it for the resume-time re-attach.
					runview.ClearSubbotChild(ctx, s, req)
				}
				return out, aerr
			}
			// Non-pause error: leave the re-attach record for the resume path.
			return nil, runErr
		}
		runview.ClearSubbotChild(ctx, s, req)
		return last, nil
	}
}
