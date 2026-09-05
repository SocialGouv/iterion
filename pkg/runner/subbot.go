package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// maxSubbotDepth bounds nested subbot recursion on a pod, as runview does
// in-process: a child that (directly or through its own children) reaches
// its parent again would otherwise recurse until the pod dies.
const maxSubbotDepth = 8

type subbotDepthKey struct{}

// resolveSubbotSource locates the child .bot a `subbot` node names, for a
// run executing on a pod.
//
// A child is declared relative to its PARENT's bundle — `../golden-master/extend.bot`
// from bots/modernize/main.bot. Two places can hold it on a pod, tried in
// order: beside the parent, when the parent is a baked catalogue bot; and
// the baked catalogue itself, when the parent was materialised from a stored
// bundle (team-authored or platform override) into a directory that holds no
// sibling — the sibling is the deployment's, not the tenant's. A child found
// in neither is a typed error naming both places: a bot declaring a subbot
// it cannot reach must fail at the node, in words, not at the pod.
//
// The name is a RELATIVE path into one of those two places and nothing else:
// an absolute path, or a `..` chain that climbs out of the parent's bundle
// collection and out of every catalogue root, is refused — a subbot names a
// bundle, not a file on the pod.
func resolveSubbotSource(source, parentDir string, botsPaths []string) (string, error) {
	if filepath.IsAbs(source) {
		return "", fmt.Errorf("subbot source %q: an absolute path is not served on a pod — name the child relative to its parent bundle", source)
	}
	roots := make([]string, 0, 1+len(botsPaths))
	if parentDir != "" {
		// The parent's bundle COLLECTION, not the bundle itself: a sibling
		// bundle (`../golden-master/…`) is the designed shape of a child.
		roots = append(roots, filepath.Dir(filepath.Clean(parentDir)))
	}
	for _, bp := range botsPaths {
		if bp != "" {
			roots = append(roots, filepath.Clean(bp))
		}
	}
	contained := func(p string) bool {
		for _, root := range roots {
			rel, err := filepath.Rel(root, p)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	beside := "(no parent bundle directory)"
	if parentDir != "" {
		beside = filepath.Clean(filepath.Join(parentDir, source))
		if _, err := os.Stat(beside); err == nil {
			if !contained(beside) {
				return "", fmt.Errorf("subbot source %q: resolves to %s, outside the parent's bundle collection and every catalogue root %v — a child is named relative to its parent bundle, not by a path across the pod", source, beside, botsPaths)
			}
			return beside, nil
		}
	}
	// Catalogue fallback: `../<slug>/<file>` names the baked bundle <slug>.
	parts := strings.Split(filepath.ToSlash(filepath.Clean(source)), "/")
	i := 0
	for i < len(parts) && parts[i] == ".." {
		i++
	}
	if i < len(parts)-1 && len(botsPaths) > 0 {
		slug, file := parts[i], filepath.Join(parts[i+1:]...)
		// The catalogue's layout first (<bots>/<slug>/<file>), then the
		// registry's tolerant name match (kebab/snake/case) for a slug spelled
		// differently from its directory. A <file> that climbs back out of
		// the bundle is refused by the same containment.
		for _, bp := range botsPaths {
			cand := filepath.Clean(filepath.Join(bp, slug, file))
			if _, err := os.Stat(cand); err == nil && contained(cand) {
				return cand, nil
			}
		}
		if mainFile, err := botregistry.ResolveBotPath(slug, botsPaths); err == nil {
			cand := filepath.Clean(filepath.Join(filepath.Dir(mainFile), file))
			if _, err := os.Stat(cand); err == nil && contained(cand) {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("subbot source %q: not found beside the parent (%s) nor in the baked catalogue %v — a child bot must ship with its parent or in the catalogue the pod carries", source, beside, botsPaths)
}

// subbotRunnerFor builds the runtime.SubbotRunner a pod's engine invokes for
// `subbot` nodes. Before this the runner wired none: every `subbot` node on a
// cloud run died with "no SubbotRunner is wired" — the CLI and studio paths
// were covered, the one surface that runs unattended was not, so a net's
// extension and re-anchor subbots, and a supervisor's per-lot child, existed
// locally only. Mirrors runview.subbotRunnerFor: resolve the child .bot,
// compile it, build a child executor under the SAME message (credentials,
// sandbox, model pins travel), run it in the parent's EFFECTIVE workdir
// (the per-run worktree, when the parent swapped to one), and hand back the
// child's terminal output. Children carry the parent linkage so the studio
// folds them into the parent's card, re-attach across a pod restart through
// the same records runview keeps, and are charged like any attempt.
func (r *Runner) subbotRunnerFor(msg *queue.RunMessage, parentDir, workDir string, runLogger *iterlog.Logger) runtime.SubbotRunner {
	if runLogger == nil {
		runLogger = r.cfg.Logger
	}
	return func(ctx context.Context, req runtime.SubbotRequest) (map[string]any, error) {
		depth, _ := ctx.Value(subbotDepthKey{}).(int)
		if depth >= maxSubbotDepth {
			return nil, fmt.Errorf("subbot recursion too deep (>%d) at %q — possible cycle", maxSubbotDepth, req.Source)
		}
		// Every store call below is tenant-scoped; the parent's ctx carries
		// the identity, and re-stamping it costs nothing where its absence
		// panics deep inside SaveRun.
		ctx = store.WithIdentity(ctx, msg.TenantID, msg.OwnerID)

		// Re-attach across a pod restart through the records runview keeps
		// (ADR-084). A child left `running` by a pod that died mid-child is
		// parked on until the orphan sweeper flips it — no pod resumes a
		// child by itself, it rides its parent's delivery — after which the
		// next parent resume spawns a FRESH child; the child's own
		// checkpoint is not resumed. Same contract as in-process.
		if out, aerr, handled := runview.ReattachSubbotChild(ctx, r.cfg.Store, req, runLogger); handled {
			return out, aerr
		}

		childPath, err := resolveSubbotSource(req.Source, parentDir, r.cfg.BotsPaths)
		if err != nil {
			return nil, err
		}
		childWf, hash, err := runview.CompileWorkflowWithHash(childPath)
		if err != nil {
			return nil, fmt.Errorf("compile child %q: %w", req.Source, err)
		}
		// The deployment's multitenant ceiling (ITERION_CLOUD_MAX_*) clamps
		// the child exactly as executeRun clamps the parent: a tenant bot
		// could otherwise declare its spend in a child and leave the cap
		// behind. The parent's own launch overrides are NOT replayed — a
		// child budgets itself from its .bot, as it does in-process.
		applyCloudBudgetCeiling(childWf, runLogger)
		childRunID, err := store.GenerateRunID()
		if err != nil {
			return nil, err
		}
		runview.RecordSubbotChild(ctx, r.cfg.Store, req, childRunID, runLogger)

		// The child runs under the parent's message — same credentials, same
		// sandbox decision, same pins — as its own run: the doc the engine
		// creates below is the child's, linked to the parent.
		child := *msg
		child.RunID = childRunID
		child.ParentRunID = msg.RunID
		child.BotID = runview.ResolveBotID("", runview.BundleNameForPath(childPath), childPath)
		child.WorkflowHash = hash
		child.Vars = req.Vars
		child.Resume = nil
		child.BotBundle = nil

		// The child's own run.log — the studio's per-node Logs tab reads the
		// CHILD's persisted log, and the parent's logger would fold every
		// line into the parent's. Same shape as the parent's writer.
		childLogger := runLogger
		if ls := store.AsRunLogStore(r.cfg.Store); ls != nil {
			idCtx := store.WithIdentity(context.Background(), msg.TenantID, msg.OwnerID)
			seed, serr := ls.RunLogSize(idCtx, childRunID)
			if serr != nil {
				runLogger.Warn("subbot %s: seed log offset: %v — starting at 0", childRunID, serr)
			}
			w := newRunLogWriter(idCtx, ls, childRunID, seed, r.cfg.Logger)
			defer func() { _ = w.Close() }()
			r.registerLogWriter(childRunID, w)
			defer r.unregisterLogWriter(childRunID)
			childLogger = r.cfg.Logger.WithWriter(io.MultiWriter(r.cfg.Logger.Writer(), w))
		}

		childExec, childUsage, err := r.buildExecutor(ctx, &child, childWf, childLogger, nil)
		if err != nil {
			return nil, err
		}
		defer r.recordOrgSpend(ctx, &child, childUsage)

		childWorkDir := req.WorkDir
		if childWorkDir == "" {
			childWorkDir = workDir
		}
		var (
			lastMu sync.Mutex
			last   map[string]any
		)
		// Sandbox-run observer: the mid-run credential refreshers write
		// rotated tokens THROUGH into the child's container, and file
		// secrets refresh — a child that outlives a token (a per-lot child
		// of a supervisor runs for hours) would otherwise push with a dead
		// one, exactly the parent's #99.
		sbObsCtx, stopSbObs := context.WithCancel(ctx)
		defer stopSbObs()
		defer r.unregisterSandboxRun(childRunID)

		opts := []runtime.EngineOption{
			runtime.WithLogger(childLogger),
			runtime.WithWorkflowHash(hash),
			runtime.WithFilePath(childPath),
			runtime.WithWorkDir(childWorkDir),
			runtime.WithSandboxRunObserver(r.sandboxRunObserver(sbObsCtx, childRunID, msg.TenantID, r.sandboxFileSecretRefs(ctx, childWf))),
			runtime.WithSandboxDefault(r.cfg.SandboxDefault),
			runtime.WithSandboxDefaultImage(msg.SandboxImage),
			runtime.WithSandboxHostStateDefault(r.cfg.SandboxHostState),
			runtime.WithSandboxOverride(r.cfg.SandboxOverride),
			runtime.WithLoopBudgetGuard(msg.LoopBudgetGuard),
			runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
			runtime.WithParentRunID(req.ParentRunID),
			runtime.WithParentNodeID(req.NodeID),
			// Recursive wiring: a child that declares subbot nodes resolves
			// its own children relative to ITS directory.
			runtime.WithSubbotRunner(r.subbotRunnerFor(&child, filepath.Dir(childPath), childWorkDir, childLogger)),
			runtime.WithEventObserver(childUsage.observe),
			runtime.WithOnNodeFinished(func(runID, nodeID string, out map[string]any) {
				if out != nil {
					lastMu.Lock()
					last = out
					lastMu.Unlock()
				}
			}),
		}
		// The child executes in the PARENT's sandbox when the parent has one:
		// on a copy-based driver a pod of its own would be a second copy of
		// the workspace, and its commits would die with that pod.
		if req.ParentSandbox != nil {
			opts = append(opts, runtime.WithSharedSandbox(req.ParentSandbox))
		}
		// The child's own bundle: its skills and devbox tools, exactly as a
		// local `iterion run bots/<child>` would provision them.
		if b, berr := bundle.OpenDir(filepath.Dir(childPath)); berr == nil {
			opts = append(opts, runtime.WithBundle(b))
		} else {
			runLogger.Warn("subbot %s: bundle open %s: %v (skills not mirrored, devbox tools not provisioned)", req.Source, filepath.Dir(childPath), berr)
		}
		// Plugin/library skills the LAUNCHING instance resolved: the pod's
		// iterion home is empty, so without the payload the child would
		// silently find only the compiled-in builtins.
		if msg.Contributions != nil {
			opts = append(opts, runtime.WithContributions(contributionsFromWire(msg.Contributions)))
		}

		childEng := runtime.New(childWf, r.cfg.Store, childExec, opts...)
		r.registerRunEngine(childRunID, childEng)
		defer r.unregisterRunEngine(childRunID)

		childCtx, childCancel := context.WithCancelCause(context.WithValue(ctx, subbotDepthKey{}, depth+1))
		childLock, err := r.cfg.Store.LockRun(childCtx, childRunID)
		if err != nil {
			childCancel(nil)
			closeExecutor(childExec)
			return nil, fmt.Errorf("subbot %s: acquire run lock: %w", childRunID, err)
		}
		// The child's lease is the orphan sweeper's liveness signal, exactly
		// like the parent's: on the cloud store LockRun is a NATS-KV entry
		// with a 60 s TTL that only a refresher keeps alive. The parent's
		// heartbeat refreshes the PARENT's lease; without one of its own the
		// child reads dead at T+60 s, the sweeper flips it failed_resumable,
		// and the reaper deletes its sandbox pod mid-flight.
		hbDone := make(chan struct{})
		errtrack.Go("runner.subbot.heartbeat", func() { r.childLeaseHeartbeat(childCtx, childCancel, childLock, hbDone) })
		runErr := func() error {
			defer func() { _ = childLock.Unlock() }()
			defer func() {
				childCancel(nil)
				<-hbDone
			}()
			return childEng.Run(childCtx, childRunID, req.Vars)
		}()
		closeExecutor(childExec)
		if runErr != nil {
			// A human gate inside the child pauses the CHILD run; that is not
			// a parent failure. Park until the operator answers and the child
			// reaches a terminal state, then pick up its output.
			if errors.Is(runErr, runtime.ErrRunPaused) || errors.Is(runErr, runtime.ErrRunPausedOperator) {
				// Parked on the PARENT's ctx: the child's was cancelled the
				// moment its engine returned (that cancel is the heartbeat's
				// stop signal), and a park on it returns at once with
				// `context canceled` — a gate meant to wait for a human read
				// as a parent failure 20 ms in.
				out, aerr := runview.AwaitSubbotTerminal(ctx, r.cfg.Store, childRunID, runLogger)
				if aerr == nil {
					runview.ClearSubbotChild(ctx, r.cfg.Store, req)
				}
				return out, aerr
			}
			// Leave the re-attach record for the resume path.
			return nil, runErr
		}
		runview.ClearSubbotChild(ctx, r.cfg.Store, req)
		lastMu.Lock()
		defer lastMu.Unlock()
		return last, nil
	}
}

func closeExecutor(exec runtime.NodeExecutor) {
	if c, ok := any(exec).(io.Closer); ok {
		_ = c.Close()
	}
}

// childLeaseHeartbeat refreshes a subbot child's run lease while its engine
// runs — the child half of Runner.heartbeat, without the JetStream delivery
// (a child has none: it rides its parent's). A refresh failure cancels the
// child with ErrRunInterrupted so it unwinds to failed_resumable before the
// lease lapses, rather than letting a sibling pod's redelivery of the parent
// find two writers on the child's state.
func (r *Runner) childLeaseHeartbeat(ctx context.Context, cancel context.CancelCauseFunc, lock store.RunLock, done chan<- struct{}) {
	defer close(done)
	natsLock, ok := lock.(*natsq.Lock)
	if !ok {
		return // no-op lock or non-NATS provider — nothing to refresh
	}
	interval := r.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := natsLock.Refresh(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				if r.cfg.Metrics != nil {
					r.cfg.Metrics.RunnerHeartbeatErrors.Inc()
				}
				r.cfg.Logger.Error("runner: subbot child lease refresh failed: %v — cancelling the child (resumable) to avoid split-brain", err)
				cancel(runtime.ErrRunInterrupted)
				return
			}
		}
	}
}
