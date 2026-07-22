package runview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// maxSubbotDepth bounds nested subbot recursion so a child that (directly or
// transitively) runs itself fails cleanly instead of overflowing. Mirrors
// pkg/cli's guard — the two runners must agree so a bot behaves identically
// when launched from the CLI and from the studio.
const maxSubbotDepth = 8

type subbotDepthKey struct{}

// manageSubbotChild registers an in-process subbot child run with the run
// Manager so a studio Cancel/Pause targeting the CHILD's run id acts on it
// WHILE it executes its first pass — not only once it has paused on a human
// gate (before this, the child engine ran unregistered, so Cancel(childID) /
// Pause(childID) returned ErrRunNotActive and silently no-op'd mid-flight;
// only parent-ctx cancellation propagated). The pipeline board promises "act
// on any node of the tree", so the child must be individually controllable.
//
// Returns the cancellable ctx the child engine must run under, the engine
// options carrying the operator-pause signal, and a release func that MUST be
// called once the child engine's ACTIVE pass returns — BEFORE parking on
// AwaitSubbotTerminal. Releasing hands the run id back so an external
// `iterion resume` of a paused child can re-register the same id in its own
// manager (keeping it registered during the park would make that Register
// fail with "already registered"); the persisted run doc is the handoff.
//
// Degrades safely: if the manager is stopped (server shutdown) or the id is
// somehow already registered, it falls back to the parent ctx unmanaged with
// a warning — the child still runs and parent-ctx cancellation still
// propagates, exactly as before this wiring.
func manageSubbotChild(mgr *Manager, parent context.Context, childRunID string, logger *iterlog.Logger) (context.Context, []runtime.EngineOption, func()) {
	ctx, err := mgr.Register(parent, childRunID)
	if err != nil {
		if logger != nil {
			logger.Warn("subbot child %s: manager register failed (%v) — running unmanaged; cancel/pause ride the parent ctx", childRunID, err)
		}
		return parent, nil, func() {}
	}
	var opts []runtime.EngineOption
	if pauseCh, perr := mgr.PauseSignal(childRunID); perr == nil {
		opts = append(opts, runtime.WithPauseSignal(pauseCh))
	}
	var once sync.Once
	release := func() { once.Do(func() { mgr.Deregister(childRunID) }) }
	return ctx, opts, release
}

// subbotRunnerFor builds the runtime.SubbotRunner wired into every in-process
// engine the service spawns (Launch AND Resume paths). Before this, a bot
// declaring `subbot` nodes could only run through the CLI: the studio's
// in-process engine had no runner and died with "no SubbotRunner is wired".
//
// It mirrors pkg/cli's subbotRunnerForCLI: compile the child .bot (resolved
// relative to the parent's directory, falling back to the service workDir for
// inline-source parents), build a child executor sharing the service's store /
// secrets / board wiring, run it with the resolved `with:` data as inputs, and
// return the child's terminal-node output. Children inherit the service's
// engine options (broker publish, alerting, board MCP) so their run consoles
// are live in the studio, plus the parent linkage via WithParentRunID — which
// is what folds them into the parent's pipeline-board card.
func (s *Service) subbotRunnerFor(parentPath string, runLogger *iterlog.Logger) runtime.SubbotRunner {
	if runLogger == nil {
		runLogger = s.logger
	}
	base := s.workDir
	if parentPath != "" {
		base = filepath.Dir(parentPath)
	}
	return func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		depth, _ := ctx.Value(subbotDepthKey{}).(int)
		if depth >= maxSubbotDepth {
			return nil, fmt.Errorf("subbot recursion too deep (>%d) at %q — possible cycle", maxSubbotDepth, req.Source)
		}

		childPath := req.Source
		if !filepath.IsAbs(childPath) {
			childPath = filepath.Join(base, childPath)
		}
		childWf, hash, err := CompileWorkflowWithHash(childPath)
		if err != nil {
			return nil, fmt.Errorf("compile child %q: %w", req.Source, err)
		}
		childRunID, err := store.GenerateRunID()
		if err != nil {
			return nil, err
		}

		// Register the child with the run Manager so studio Cancel/Pause on the
		// child's run id act on it mid-flight. managedCtx cancels when
		// Manager.Cancel(childRunID) fires; releaseChild deregisters it once the
		// active pass returns (before any park below).
		managedCtx, pauseOpts, releaseChild := manageSubbotChild(s.manager, ctx, childRunID, runLogger)

		childExec, err := BuildExecutor(ExecutorSpec{
			Ctx:           managedCtx,
			Workflow:      childWf,
			Store:         s.store,
			RunID:         childRunID,
			Logger:        runLogger,
			StoreDir:      s.storeDir,
			Inbox:         s.inboxBinder(),
			AsyncAsk:      s.asyncAskBinder(),
			BoardRegister: s.boardRegister,
			LocalSecrets:  s.localSecrets,
			LocalSealer:   s.localSealer,
		})
		if err != nil {
			releaseChild()
			return nil, err
		}

		// Capture the child's terminal-node output (the last node before Done)
		// as the subbot's result, composing with the service's watch-stamping
		// hook (WithOnNodeFinished is single-slot, so engineOptions' default
		// would otherwise be lost). The callback fires concurrently when the
		// child fans out parallel branches, so the capture is mutex-guarded.
		var (
			lastMu sync.Mutex
			last   map[string]any
		)
		opts := s.engineOptions(runLogger, hash, childPath, "", finalizationOpts{}, launchExtras{})
		opts = append(opts,
			runtime.WithParentRunID(req.ParentRunID),
			runtime.WithParentNodeID(req.NodeID),
			// Recursive wiring so a child that itself declares subbot nodes can
			// run them (grandchild sources resolve relative to the CHILD's dir);
			// the ctx-carried depth keeps the recursion bounded.
			runtime.WithSubbotRunner(s.subbotRunnerFor(childPath, runLogger)),
			runtime.WithOnNodeFinished(func(runID, nodeID string, out map[string]any) {
				if out != nil {
					lastMu.Lock()
					last = out
					lastMu.Unlock()
				}
				s.stampWatchedFromOutput(runID, nodeID, out)
			}),
		)
		// The operator-pause signal (if the manager registered the child) so
		// Pause(childRunID) checkpoints the child at its next safe boundary.
		opts = append(opts, pauseOpts...)
		childEng := runtime.New(childWf, s.store, childExec, opts...)
		childCtx := context.WithValue(managedCtx, subbotDepthKey{}, depth+1)
		runErr := childEng.Run(childCtx, childRunID, req.Vars)
		// Release the manager handle now the active pass is done: a paused
		// child is resumed EXTERNALLY (which re-registers the id in its own
		// manager), so keeping it here would both block that Register and
		// wrongly hold the id past the point this engine owns it.
		releaseChild()
		// Close promptly — BEFORE a potentially hours-long human wait below —
		// so per-child MCP servers / board-store watchers don't accumulate
		// under parallel fan-out (the inotify-instance exhaustion #197 fixed).
		if c, ok := any(childExec).(io.Closer); ok {
			_ = c.Close()
		}
		if runErr != nil {
			// A human gate inside the child pauses the CHILD run (its doc is
			// paused_waiting_human with a checkpoint + interaction); that is not
			// a parent failure. Park this branch until the operator answers the
			// child's review (pipeline-board sidebar / `iterion resume`) and the
			// child reaches a terminal state, then pick up its output.
			if errors.Is(runErr, runtime.ErrRunPaused) || errors.Is(runErr, runtime.ErrRunPausedOperator) {
				return AwaitSubbotTerminal(childCtx, s.store, childRunID, runLogger)
			}
			return nil, runErr
		}
		return last, nil
	}
}

// subbotAwaitPollInterval is how often AwaitSubbotTerminal re-reads the child
// run while parked on a human gate. Store reads are cheap (one run.json), and
// a human answer arrives on human timescales — 1s keeps the parent responsive
// without hammering the store.
const subbotAwaitPollInterval = time.Second

// AwaitSubbotTerminal parks a subbot node whose in-process child engine
// returned a pause (human gate / operator pause) until the child run reaches a
// terminal state, then reconstructs the child's terminal output from the
// store. The child is resumed EXTERNALLY — the pipeline-board sidebar answer
// or `iterion resume --run-id <child>` claims the paused run doc and drives a
// fresh engine to completion in another goroutine/process; this waiter only
// observes the store.
//
// Terminal semantics: finished → output; failed / failed_resumable /
// cancelled → error (the parent branch fails; resuming the PARENT re-runs the
// subbot node with a fresh child). A child that pauses again after a resume
// (several human gates) simply keeps this waiter parked — each new pause
// surfaces on the pipeline board like the first.
//
// Shared by the studio's in-process runner (subbotRunnerFor) and pkg/cli's
// subbotRunnerForCLI so both surfaces behave identically.
func AwaitSubbotTerminal(ctx context.Context, rs store.RunStore, childRunID string, logger *iterlog.Logger) (map[string]any, error) {
	if logger != nil {
		logger.Info("subbot child run %s paused for human input — answer it from the pipeline board (or `iterion resume --run-id %s`); the parent continues when the child finishes", childRunID, childRunID)
	}
	ticker := time.NewTicker(subbotAwaitPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		run, err := rs.LoadRun(ctx, childRunID)
		if err != nil {
			return nil, fmt.Errorf("subbot child %s: load run: %w", childRunID, err)
		}
		switch run.Status {
		case store.RunStatusFinished:
			return subbotTerminalOutput(ctx, rs, run), nil
		case store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
			msg := run.Error
			if msg == "" {
				msg = string(run.Status)
			}
			return nil, fmt.Errorf("subbot child run %s ended %s after its pause: %s", childRunID, run.Status, msg)
		}
		// queued / running / paused_* → keep waiting.
	}
}

// subbotTerminalOutput reconstructs a finished child's terminal-node output
// from the store. The in-process capture (WithOnNodeFinished) is unavailable
// once the child was resumed in another engine, and the run checkpoint is
// cleared on finish — the durable record is events.jsonl: every node emits
// node_finished with its (secret-scrubbed) output payload, so the LAST one is
// exactly what the in-process capture would have held. Falls back to the
// latest-written `publish:` artifact when the event payload is absent (legacy
// events). A child with neither returns an empty map — the subbot's declared
// output schema will flag missing fields if that matters.
func subbotTerminalOutput(ctx context.Context, rs store.RunStore, run *store.Run) map[string]any {
	var lastOutput map[string]any
	_ = rs.ScanEvents(ctx, run.ID, func(e *store.Event) bool {
		if e.Type == store.EventNodeFinished && e.NodeID != "" {
			if out, ok := e.Data["output"].(map[string]any); ok && out != nil {
				lastOutput = out
			}
		}
		return true
	})
	if lastOutput != nil {
		return lastOutput
	}
	var best *store.Artifact
	for nodeID := range run.ArtifactIndex {
		art, err := rs.LoadLatestArtifact(ctx, run.ID, nodeID)
		if err != nil || art == nil || len(art.Data) == 0 {
			continue
		}
		if best == nil || art.WrittenAt.After(best.WrittenAt) {
			best = art
		}
	}
	if best != nil {
		return best.Data
	}
	return map[string]any{}
}
