package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// forceStaleStaleAfter is the minimum time since the last events.jsonl
// flush before `--force-stale` will promote a status=running run.
// Mirrors `runview.reconcileOrphans`' implicit policy (which relies on
// flock as the liveness probe instead of mtime) — kept conservative
// enough to avoid stepping on a live engine in a brief silent stretch
// (LLM reasoning turns can run 30-60s without an event), short enough
// to recover immediately after a watchexec restart instead of waiting
// the 15min the runs LIST filter uses for staleness display.
const forceStaleStaleAfter = 60 * time.Second

// ResumeOptions holds the configuration for the resume command.
type ResumeOptions struct {
	RunID       string
	StoreDir    string
	AnswersFile string            // path to JSON answers file
	Answers     map[string]string // --answer key=value overrides
	LogLevel    string            // log level (default: "info", env: ITERION_LOG_LEVEL)
	Force       bool              // allow resume despite workflow hash change
	// ForceStale auto-promotes a status=running run to failed_resumable
	// IFF its events.jsonl mtime is older than forceStaleStaleAfter.
	// The server-boot sweep (pkg/store.PromoteStaleOrphans) covers the
	// common case automatically; this flag is the operator's escape
	// hatch when they spot an orphan in a long-running session, or want
	// to bypass waiting for the next server restart.
	ForceStale bool
	Executor   runtime.NodeExecutor
	// Background marks this invocation as a managed-runner subprocess
	// spawned by the studio server. The CLI writes a .pid file so the
	// server can detect liveness across its own restart.
	Background bool
	// Budget carries CLI overrides for the workflow's budget: block,
	// mirroring `iterion run`. The documented "budget exceeded → raise the
	// budget + resume" recovery relies on this. Non-zero wins; zero
	// inherits. See applyBudgetOverrides.
	Budget BudgetOverrides
	// Permission overrides the tool-permission gate on resume. Adding an
	// allow rule here is how an operator authorizes a call the run paused
	// on (`permission: ask`): the rule lands in the resolved policy, the
	// agent re-issues the call, and it proceeds. See docs/permissions.md.
	Permission      string
	PermissionAllow []string
	PermissionAsk   []string
	PermissionDeny  []string
	// AutoMemory re-states the run-level auto-memory (MEMORY.md) override on
	// resume. Run-level overrides are not persisted on the run, so a resume
	// that says nothing falls back to the workflow's own value — which for a
	// bot whose DSL says `on` silently un-does a hermetic `--auto-memory off`
	// launch.
	AutoMemory string
	// ModelFor / BackendFor re-apply the launch-time per-node/-group model+
	// backend overrides on resume (repeatable --model / --backend,
	// "selector=value" or bare "value"). Resume does NOT persist the original
	// run's overrides, so a run launched with e.g. `--model 'judge=...'` falls
	// back to the node's DSL model unless the same flags are passed again here.
	// Parsed via model.ParseModelOverrides in buildResumeExecutor.
	ModelFor   []string
	BackendFor []string
	// Fallback re-applies the launch-time run-level fallback route
	// (`--fallback <backend>:<model>`). Resume does not persist launch
	// rules, and this feature's whole scenario — a long run outliving a
	// quota window — is exactly the one that resumes, so a route that
	// silently stopped applying would kill the run on the second
	// closure. See ADR-087.
	Fallback string
	// AutoResume is the bounded run-level auto-resume budget N
	// (`--auto-resume`, env ITERION_AUTO_RESUME; default 0 = off). Mirrors
	// RunOptions.AutoResume so `iterion resume` can itself keep re-driving a
	// transient-failing run without a human in the loop. See auto_resume.go.
	AutoResume int
	// Retry is the per-run retry override (pkg/retrypolicy), the highest
	// layer of the chain. Zero value inherits ITERION_RETRY_* then the
	// package defaults.
	Retry retrypolicy.Policy
}

// RunResumeWithFile resumes a paused run using a workflow file and answers.
// iterFile is optional: when empty, the run's persisted FilePath (recorded at
// launch — for inline launches, this is the server's inline-source cache
// path) is used. This lets the CLI resume an inline-launched run without
// the caller re-supplying the source.
func RunResumeWithFile(ctx context.Context, iterFile string, opts ResumeOptions, p *Printer) error {
	if opts.RunID == "" {
		return fmt.Errorf("--run-id is required")
	}

	level, err := iterlog.ResolveLevel(opts.LogLevel, "ITERION_LOG_LEVEL")
	if err != nil {
		return err
	}
	logger := iterlog.New(level, os.Stderr)

	// Same anchor as run: a run must be resumable from the directory it was
	// launched in, whether or not --file names the bot.
	storeDir := runStoreDir(iterFile, opts.StoreDir)
	if !runStoreHas(storeDir, opts.RunID) {
		if legacy := legacyRunStoreDir(iterFile, opts.StoreDir, storeDir, opts.RunID); legacy != "" {
			p.Line("run %s lives in the pre-upgrade per-bot store %s — resuming there; "+
				"pass --store-dir to pin it explicitly", opts.RunID, legacy)
			storeDir = legacy
		}
	}

	// Tee log output to run.log so the studio's Logs panel sees
	// output for resumed runs. Resume re-uses the same file via
	// O_APPEND so the original run.log + resume sessions stack into
	// one timeline, matching what the daemon-launched path produces.
	logger, logCloser := teeRunLog(logger, level, storeDir, opts.RunID)
	if logCloser != nil {
		defer logCloser.Close()
	}

	s, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	r, err := s.LoadRun(ctx, opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot load run: %w", err)
	}

	// Fall back to the FilePath persisted at launch when --file was omitted.
	// Inline-launched runs have this pointing at the server's inline-source
	// cache, so resume replays the exact bytes the run was launched with.
	if iterFile == "" {
		iterFile = r.FilePath
		if iterFile == "" {
			return fmt.Errorf("--file is required: run %q has no persisted workflow path", opts.RunID)
		}
	}

	resumingFromFailure := false
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		// OK — requires answers
	case store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		resumingFromFailure = true
	case store.RunStatusRunning:
		// Status=running on a run whose engine actually died is the
		// classic orphan case. The server-boot sweep handles most of
		// these automatically; --force-stale lets the operator unstick
		// the rest mid-session when they see an orphan. Gate on
		// events.jsonl mtime so we don't step on a live engine that's
		// just briefly silent.
		if !opts.ForceStale {
			return fmt.Errorf("run %q cannot be resumed (status: %s); pass --force-stale if you're certain the engine has died", opts.RunID, r.Status)
		}
		evPath := filepath.Join(storeDir, "runs", opts.RunID, "events.jsonl")
		if st, statErr := os.Stat(evPath); statErr == nil {
			if age := time.Since(st.ModTime()); age < forceStaleStaleAfter {
				return fmt.Errorf("run %q has events.jsonl flushed %s ago (< %s) — engine may still be alive; refusing to force-stale resume. Wait, or use `iterion inspect` to confirm the engine is gone", opts.RunID, age.Truncate(time.Second), forceStaleStaleAfter)
			}
		}
		if changed, casErr := s.UpdateRunStatusIf(ctx, opts.RunID, store.RunStatusFailedResumable, "engine subprocess died abnormally; status auto-promoted via `iterion resume --force-stale`", []store.RunStatus{store.RunStatusRunning}); casErr != nil {
			return fmt.Errorf("force-stale promote: %w", casErr)
		} else if !changed {
			// Reload — something else changed it between LoadRun and now.
			r, err = s.LoadRun(ctx, opts.RunID)
			if err != nil {
				return fmt.Errorf("reload after CAS: %w", err)
			}
		} else {
			r.Status = store.RunStatusFailedResumable
		}
		resumingFromFailure = true
		logger.Warn("resume %s: force-stale active (status auto-promoted from running)", opts.RunID)
	default:
		return fmt.Errorf("run %q cannot be resumed (status: %s)", opts.RunID, r.Status)
	}

	answers, err := buildResumeAnswers(opts, resumingFromFailure)
	if err != nil {
		return err
	}

	pausedNode := ""
	if r.Checkpoint != nil {
		pausedNode = r.Checkpoint.NodeID
	}

	wf, wfHash, iterFile, bundleHandle, bundleCleanup, err := resumeOpenWorkflow(r, iterFile)
	// Install cleanup BEFORE the error check: resumeOpenWorkflow returns a
	// live cleanup (the .botz temp-dir remover) even on a bundle compile
	// error, so returning on err without deferring it leaks the extracted dir.
	if bundleCleanup != nil {
		defer func() {
			if cerr := bundleCleanup(); cerr != nil {
				logger.Warn("bundle cleanup: %v", cerr)
			}
		}()
	}
	if err != nil {
		return err
	}

	// `--answer field=@./file.mp3` attaches a local file to the gate, the
	// CLI counterpart of the studio's upload widget. Done after the
	// compile so the '@' convention can be scoped to the fields the
	// paused node actually declares as `file` — an ordinary answer that
	// happens to start with '@' (a chat mention, an npm scope, a `@v1.2`
	// ref) must reach the workflow verbatim, as it always has. Needs the
	// store handle and the paused node id to name the attachment the same
	// way the HTTP path does.
	if err := resolveFileAnswerFlags(ctx, s, opts.RunID, pausedNode, fileAnswerFields(wf, pausedNode), answers); err != nil {
		return err
	}

	// CLI budget overrides — applied before buildResumeExecutor (which
	// snapshots Budget), same seam as the run path. Lets an operator raise
	// a cap and resume a budget-exceeded run without editing the .bot.
	if err := opts.Budget.Validate(); err != nil {
		return UserInputError(err)
	}
	applyBudgetOverrides(wf, opts.Budget)

	executor, err := buildResumeExecutor(opts, wf, s, storeDir, logger, r)
	if err != nil {
		return err
	}

	resumeOpts := []runtime.EngineOption{}
	// Keep capturing across a resume: stopping here would leave a hole in
	// the workspace history exactly where the operator is iterating.
	if tracker := runview.WorkspaceTrackerFor(storeDir); tracker != nil {
		resumeOpts = append(resumeOpts, runtime.WithWorkspaceTracker(tracker))
	}
	eng := runtime.New(wf, s, executor, append(resumeOpts,
		runtime.WithLogger(logger),
		runtime.WithWorkflowHash(wfHash),
		runtime.WithFilePath(iterFile),
		runtime.WithForceResume(opts.Force),
		// Sandbox-by-default: resumed runs re-resolve their sandbox with
		// the same global default as `iterion run`.
		runtime.WithSandboxDefault(runtime.ResolveGlobalSandboxDefault()),
		runtime.WithBundle(bundleHandle),
		runtime.WithPreset(r.Preset),
		// Wire the subbot runner, mirroring the run path (run.go). Without
		// it, ANY resumed run whose remaining graph contains a subbot node
		// died with "no SubbotRunner is wired" — runs with subbots were
		// unresumable as a class. The RunOptions below carries the resume
		// flags that flow into child executors (permission gate, model/
		// backend overrides, stub executor for tests).
		runtime.WithSubbotRunner(subbotRunnerForCLI(iterFile, storeDir, s, logger, RunOptions{
			StoreDir:        storeDir,
			LogLevel:        opts.LogLevel,
			NoInteractive:   opts.Background,
			Background:      opts.Background,
			Executor:        opts.Executor,
			Permission:      opts.Permission,
			PermissionAllow: opts.PermissionAllow,
			PermissionAsk:   opts.PermissionAsk,
			PermissionDeny:  opts.PermissionDeny,
			AutoMemory:      opts.AutoMemory,
			ModelFor:        opts.ModelFor,
			BackendFor:      opts.BackendFor,
			Fallback:        opts.Fallback,
		})),
	)...)

	// Acquire exclusive run lock to prevent concurrent processes.
	// Use the SIGINT-aware ctx so a contended lock can still be
	// interrupted by Ctrl-C rather than blocking forever.
	lock, err := s.LockRun(ctx, opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot acquire run lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	// Managed-runner mode: the studio server writes the .pid file on
	// our behalf at spawn time, so we only need to remove it on exit.
	if opts.Background {
		defer func() {
			if rmErr := s.RemovePIDFile(opts.RunID); rmErr != nil {
				logger.Warn("background: remove .pid: %v", rmErr)
			}
		}()
	}

	// Re-check run status under the lock to prevent a TOCTOU race
	// against a concurrent process that flipped the run to terminal.
	r, err = s.LoadRun(ctx, opts.RunID)
	if err != nil {
		return fmt.Errorf("cannot reload run: %w", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman &&
		r.Status != store.RunStatusFailedResumable &&
		r.Status != store.RunStatusCancelled &&
		r.Status != store.RunStatusPausedOperator {
		return fmt.Errorf("run %q can no longer be resumed (status: %s)", opts.RunID, r.Status)
	}

	if p.Format == OutputHuman {
		p.Header("Resume: " + opts.RunID)
		if r.Name != "" {
			p.KV("Name", r.Name)
		}
		p.KV("Workflow", wf.Name)
		if r.Checkpoint != nil {
			p.KV("Node", r.Checkpoint.NodeID)
		}
		if resumingFromFailure {
			p.KV("Resuming from", "failed (re-executing failed node)")
			if r.Error != "" {
				p.KV("Previous error", r.Error)
			}
		}
		p.KV("Log Level", level.String())
		p.Blank()
	}

	err = eng.Resume(ctx, opts.RunID, answers)
	err = autoResumeLoop(ctx, eng, s, opts.RunID, resolveAutoResume(opts.AutoResume, opts.Budget, opts.Retry), err, logger)
	return reportResumeOutcome(p, s, opts.RunID, err, map[string]any{
		"run_id":   opts.RunID,
		"workflow": wf.Name,
	})
}

// ParseAnswerFlags parses a slice of "key=value" strings into a map.
func ParseAnswerFlags(flags []string) (map[string]string, error) {
	return parseKVPairs[string](flags, kvOpts[string]{
		errFmt: "invalid --answer format %q (expected key=value)",
	})
}
