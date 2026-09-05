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

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
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
func resolveSubbotSource(source, parentDir string, botsPaths []string) (string, error) {
	if filepath.IsAbs(source) {
		if _, err := os.Stat(source); err == nil {
			return source, nil
		}
		return "", fmt.Errorf("subbot source %q: not found", source)
	}
	beside := "(no parent bundle directory)"
	if parentDir != "" {
		beside = filepath.Join(parentDir, source)
		if _, err := os.Stat(beside); err == nil {
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
		// differently from its directory.
		for _, bp := range botsPaths {
			cand := filepath.Join(bp, slug, file)
			if _, err := os.Stat(cand); err == nil {
				return cand, nil
			}
		}
		if mainFile, err := botregistry.ResolveBotPath(slug, botsPaths); err == nil {
			cand := filepath.Join(filepath.Dir(mainFile), file)
			if _, err := os.Stat(cand); err == nil {
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

		childExec, childUsage, err := r.buildExecutor(ctx, &child, childWf, runLogger, nil)
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
		opts := []runtime.EngineOption{
			runtime.WithLogger(runLogger),
			runtime.WithWorkflowHash(hash),
			runtime.WithWorkDir(childWorkDir),
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
			runtime.WithSubbotRunner(r.subbotRunnerFor(&child, filepath.Dir(childPath), childWorkDir, runLogger)),
			runtime.WithEventObserver(childUsage.observe),
			runtime.WithOnNodeFinished(func(runID, nodeID string, out map[string]any) {
				if out != nil {
					lastMu.Lock()
					last = out
					lastMu.Unlock()
				}
			}),
		}
		// The child's own bundle: its skills and devbox tools, exactly as a
		// local `iterion run bots/<child>` would provision them.
		if b, berr := bundle.OpenDir(filepath.Dir(childPath)); berr == nil {
			opts = append(opts, runtime.WithBundle(b))
		} else {
			runLogger.Warn("subbot %s: bundle open %s: %v (skills not mirrored, devbox tools not provisioned)", req.Source, filepath.Dir(childPath), berr)
		}

		childEng := runtime.New(childWf, r.cfg.Store, childExec, opts...)
		r.registerRunEngine(childRunID, childEng)
		defer r.unregisterRunEngine(childRunID)

		childCtx := context.WithValue(ctx, subbotDepthKey{}, depth+1)
		childLock, err := r.cfg.Store.LockRun(childCtx, childRunID)
		if err != nil {
			closeExecutor(childExec)
			return nil, fmt.Errorf("subbot %s: acquire run lock: %w", childRunID, err)
		}
		runErr := func() error {
			defer func() { _ = childLock.Unlock() }()
			return childEng.Run(childCtx, childRunID, req.Vars)
		}()
		closeExecutor(childExec)
		if runErr != nil {
			// A human gate inside the child pauses the CHILD run; that is not
			// a parent failure. Park until the operator answers and the child
			// reaches a terminal state, then pick up its output.
			if errors.Is(runErr, runtime.ErrRunPaused) || errors.Is(runErr, runtime.ErrRunPausedOperator) {
				out, aerr := runview.AwaitSubbotTerminal(childCtx, r.cfg.Store, childRunID, runLogger)
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
