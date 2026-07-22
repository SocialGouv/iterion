package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Run executes the workflow. It creates a run, walks the graph from the
// entry node, and returns when a terminal node is reached, a human pause
// is hit (ErrRunPaused), or an error occurs.
//
// Two entry shapes are accepted:
//
//   - **Direct** (CLI / single-process): no doc exists yet, CreateRun
//     inserts a fresh row.
//   - **Cloud pickup** (runner pool): the cloudpublisher already
//     persisted the run with status=queued before publishing on
//     JetStream. The runner calls Run with the same runID, expecting
//     us to claim the existing row and transition it to running. A
//     plain CreateRun would error with "already exists".
//
// LoadRun + transition is attempted first; if no doc exists we fall
// back to CreateRun. Any other status (running, finished, …) is a
// programming error — refuse to clobber state.
func (e *Engine) Run(ctx context.Context, runID string, inputs map[string]any) (err error) {
	run, err := e.runResolveDoc(ctx, runID, inputs)
	if err != nil {
		return err
	}

	run, err = e.runPromoteAttachments(ctx, runID, run)
	if err != nil {
		return err
	}

	// Default workDir to process cwd if not set explicitly.
	if e.workDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			e.workDir = cwd
		}
	}

	// Enum gate: every launch surface (CLI --var, HTTP launch, dispatcher
	// bot_args, preset overlay, cloud pickup) funnels its var values into
	// run.Inputs, so this single check rejects any enum-constrained var
	// value outside its declared set — before a worktree or sandbox is
	// spun up for a doomed run.
	if err := e.validateVarEnums(run.Inputs); err != nil {
		e.markFailedBestEffort(ctx, runID, "var validation", err)
		return fmt.Errorf("runtime: var validation: %w", err)
	}

	// Worktree setup stays inline: the finalizeOnExit defer must
	// capture the named return `err`, and the defer installation
	// is the meaningful side effect — extracting it would require
	// returning a deferred-callable that the caller invokes, which
	// is less clear than keeping the block here.
	var worktreeCleanup func()
	var wtCtx worktreeContext
	worktreeActive := false
	if e.workflow.Worktree == "auto" {
		// Workspace isolation is the IR default (ir.defaultWorktreeMode),
		// so any `iterion run` against a non-git workspace would otherwise
		// hard-fail with "not a git repository". Degrade gracefully to
		// in-place: this is the documented contract — auto is best-effort
		// isolation, never a precondition. Many e2e/examples and ad-hoc
		// runs against scratch dirs rely on this. The explicit opt-out
		// path is `worktree: none`.
		if e.store.Root() == "" {
			// A store with no filesystem root (the cloud Mongo store) has
			// nowhere durable to host a per-run worktree: setupWorktree
			// derives the worktree home from store.Root(), so "" would
			// anchor it in the process cwd. Decisive on a cloud runner,
			// the workspace is a per-run clone recycled between queue
			// deliveries — a worktree's gitdir lives inside that clone's
			// .git, so the re-clone on resume severs the linkage and every
			// git command in the workspace fails with "not a git
			// repository". The per-run clone on an ephemeral runner is
			// already isolation; run in place so git keeps working across
			// deliveries.
			if e.logger != nil {
				e.logger.Info("runtime: store has no filesystem root — running in place in %s (worktree isolation skipped: an ephemeral per-run workspace cannot host a durable worktree)", e.workDir)
			}
		} else if !workspaceIsGitRepo(e.workDir) {
			if e.logger != nil {
				e.logger.Warn("runtime: workspace %s is not a git repository — running in-place (set `worktree: none` to silence this)", e.workDir)
			}
		} else if !workspaceHasCommits(e.workDir) {
			// An empty repository (unborn HEAD — a freshly created forge
			// repo right after clone) can't anchor a worktree. Degrade
			// in-place: the bot's first commit lands on the unborn default
			// branch directly, and its own push publishes it.
			if e.logger != nil {
				e.logger.Warn("runtime: workspace %s has no commits yet — running in-place (worktree needs a HEAD to anchor on)", e.workDir)
			}
		} else {
			wtc, cleanup, wtErr := setupWorktree(e.store.Root(), runID, e.workDir, e.logger)
			if wtErr != nil {
				e.markFailedBestEffort(ctx, runID, "worktree setup", wtErr)
				return fmt.Errorf("runtime: worktree setup: %w", wtErr)
			}
			e.workDir = wtc.wtPath
			worktreeCleanup = cleanup
			wtCtx = wtc
			worktreeActive = true
			// Cover every exit path from this point on. Without this defer,
			// failures in subsequent setup would return without finalize and
			// leak the worktree dir + orphan any commits the partial run
			// produced. finalizeOnExit short-circuits on error (preserves
			// worktree for inspection, skips FF) so a single defer covers
			// both happy and error paths uniformly.
			defer func() {
				e.finalizeOnExit(ctx, runID, &wtCtx, worktreeCleanup, err)
			}()
		}
	}

	if err := e.runPersistWorkspace(ctx, runID, run, worktreeActive, wtCtx); err != nil {
		return err
	}

	// Sandbox lifecycle: when the workflow opts in, start a long-lived
	// container that hosts every delegate invocation for this run.
	repoRoot := wtCtx.repoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	// Persist on the engine so resolveVars's `${PROJECT_MEMORY_DIR}`
	// expansion (and any other repo-rooted lookup) doesn't have to
	// re-derive it. runPersistWorkspace already wrote `run.RepoRoot`
	// to the store; this is the in-memory mirror for the live run.
	e.repoRoot = repoRoot
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot, wtCtx.gitDir, inputs)
	if sbErr != nil {
		e.markFailedBestEffort(ctx, runID, "sandbox start", sbErr)
		return fmt.Errorf("runtime: sandbox: %w", sbErr)
	}
	defer sandboxCleanup()

	// Carry the run's named-loop iteration bounds on run_started so the
	// runview snapshot can render a run-level "real loops" indicator
	// (e.g. review_loop 48/50) — the current counter comes from each
	// node_started's iteration_path, the bound (max) from here. Literal
	// caps only; expression / unbounded caps emit 0 (max unknown).
	if err := e.emit(ctx, runID, store.EventRunStarted, "", loopBoundsPayload(e.workflow)); err != nil {
		e.markFailedBestEffort(ctx, runID, "emit run_started", err)
		return fmt.Errorf("runtime: emit run_started: %w", err)
	}

	rs := e.runInitState(ctx, runID, inputs)

	loopErr := e.execLoop(ctx, rs, e.workflow.Entry)
	e.evictRunSessions(runID, loopErr)

	return loopErr
}

// runResolveDoc loads-or-creates the run doc, transitioning a queued
// row to running for cloud-pickup or rejecting an already-terminal
// run, then stamps any engine-level metadata (workflow hash, file
// path, parent run, run name, merge strategy, auto-merge, preset, bundle hash)
// onto the persisted record.
func (e *Engine) runResolveDoc(ctx context.Context, runID string, inputs map[string]any) (*store.Run, error) {
	var run *store.Run
	if existing, loadErr := e.store.LoadRun(ctx, runID); loadErr == nil {
		// Pickup path: the doc already exists.
		//   - queued: cloudpublisher pre-created the row before
		//     publishing on JetStream; transition to running here.
		//   - running: either FilesystemRunStore.CreateRun (which
		//     creates with running) was invoked first by a CLI or
		//     test setup, or a sibling runner is already executing
		//     this run. The contention case is supposed to be guarded
		//     at the queue level (NATS distributed lock), not here.
		// Other statuses (finished/failed/cancelled/paused/failed_resumable)
		// are terminal or resume-only and must not be silently restarted.
		switch existing.Status {
		case store.RunStatusQueued:
			if err := e.store.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
				return nil, fmt.Errorf("runtime: pickup transition: %w", err)
			}
			existing.Status = store.RunStatusRunning
		case store.RunStatusRunning:
			// Already running — assume legitimate claim.
		default:
			return nil, fmt.Errorf("runtime: run %s already in status %s, refusing to restart", runID, existing.Status)
		}
		if len(inputs) > 0 {
			existing.Inputs = inputs
		}
		run = existing
	} else {
		// Direct path: no doc yet, create one. CreateRun is strict
		// (InsertOne) so a parallel pickup would lose this race —
		// acceptable: the only callers here are the CLI and tests, both
		// single-writer.
		created, err := e.store.CreateRun(ctx, runID, e.workflow.Name, inputs)
		if err != nil {
			return nil, fmt.Errorf("runtime: create run: %w", err)
		}
		run = created
	}
	if e.workflowHash != "" || e.filePath != "" || e.parentRunID != "" || e.parentNodeID != "" || e.runName != "" || e.mergeStrategy != "" || e.autoMerge || e.preset != "" || e.bundle != nil || e.source != nil || e.callbackURL != "" || len(e.modelOverrides) > 0 || e.workflow.Budget != nil {
		if e.workflowHash != "" {
			run.WorkflowHash = e.workflowHash
		}
		if e.filePath != "" {
			run.FilePath = e.filePath
		}
		if e.parentRunID != "" {
			run.ParentRunID = e.parentRunID
		}
		if e.parentNodeID != "" {
			run.ParentNodeID = e.parentNodeID
		}
		if e.runName != "" {
			run.Name = e.runName
		}
		if e.mergeStrategy != "" {
			run.MergeStrategy = store.MergeStrategy(e.mergeStrategy)
		}
		run.AutoMerge = e.autoMerge
		if e.preset != "" {
			run.Preset = e.preset
		}
		// Persist the workflow-declared tool-permission mode so the studio
		// RunHeader can badge a gated run (off|ask|deny). This is the bot's
		// declared posture; a run-level --permission override refines it per
		// node but isn't reflected here.
		run.PermissionMode = e.workflow.Permission
		// Guard on len>0 so a resume (which never re-supplies overrides)
		// preserves the value persisted at the original launch instead of
		// clobbering it with nil.
		if len(e.modelOverrides) > 0 {
			run.ModelOverrides = e.modelOverrides
		}
		// Persist the EFFECTIVE budget caps (after CLI/recipe overrides and,
		// in cloud, the platform ceiling clamp — both mutate wf.Budget
		// before the engine runs) so the studio Overview draws budget meters
		// with a denominator. A resume that raises a cap re-parses the
		// budget, so overwriting is correct; the non-nil guard preserves a
		// prior snapshot if a --force resume dropped the budget: block.
		if b := snapshotBudgetForPersist(e.workflow.Budget); b != nil {
			run.Budget = b
		}
		if e.bundle != nil {
			run.BundleHash = e.bundle.Hash
			run.BundlePath = e.bundle.SourcePath
			if e.bundle.Manifest != nil {
				run.BundleName = e.bundle.Manifest.Name
				run.BundleDisplayName = e.bundle.Manifest.DisplayName
			}
		}
		if e.source != nil {
			// Copy so the engine's option pointer can't later be mutated
			// through the run record.
			src := *e.source
			run.Source = &src
		}
		if e.callbackURL != "" {
			run.CallbackURL = e.callbackURL
			run.CallbackToken = e.callbackToken
			run.CallbackAnswerNode = e.callbackAnswerNode
		}
		if err := e.store.SaveRun(ctx, run); err != nil {
			return nil, fmt.Errorf("runtime: save run metadata: %w", err)
		}
	}
	return run, nil
}

// runPromoteAttachments materialises bundle attachment defaults, then
// runs the optional attachmentPromote callback, then reloads the run
// so the caller's next SaveRun does not clobber Run.Attachments (the
// promote writes directly to the store; the in-memory copy is now
// stale). On error, marks the run failed best-effort before returning.
func (e *Engine) runPromoteAttachments(ctx context.Context, runID string, run *store.Run) (*store.Run, error) {
	if err := promoteBundleAttachmentDefaults(ctx, e.store, runID, e.workflow, e.bundle, e.logger); err != nil {
		e.markFailedBestEffort(ctx, runID, "bundle attachment defaults", err)
		return nil, fmt.Errorf("runtime: bundle attachment defaults: %w", err)
	}
	if e.attachmentPromote != nil {
		if err := e.attachmentPromote(ctx, runID); err != nil {
			e.markFailedBestEffort(ctx, runID, "attachment promote", err)
			return nil, fmt.Errorf("runtime: promote attachments: %w", err)
		}
	}
	if reloaded, err := e.store.LoadRun(ctx, runID); err == nil {
		return reloaded, nil
	}
	return run, nil
}

// runPersistWorkspace persists the resolved workDir + worktree baseline
// onto the run record, pushes workDir into the executor (when the
// concrete executor implements SetWorkDir), and mirrors any bundle
// skills into the workspace's .claude/skills/ directory.
func (e *Engine) runPersistWorkspace(ctx context.Context, runID string, run *store.Run, worktreeActive bool, wtCtx worktreeContext) error {
	if e.workDir != "" {
		run.WorkDir = e.workDir
		// run.Worktree reflects whether the runtime actually set up an
		// isolated git worktree for this run — not just whether the
		// workflow declared `worktree: auto`. With auto being the IR
		// default, a non-git workspace degrades to in-place and must
		// honestly report Worktree=false so downstream consumers
		// (resume, finalize, FilesPanel) don't chase a phantom path.
		run.Worktree = worktreeActive
		if worktreeActive {
			run.RepoRoot = wtCtx.repoRoot
			run.BaseCommit = wtCtx.originalTip
		} else if worktreeRoot := gitlib.FindRepoRoot(e.workDir); worktreeRoot != "" && e.workDirDelegated {
			// workDir is a git working tree that the runtime didn't set
			// up itself. Only promote this to a managed-worktree baseline
			// when the workspace was DELEGATED to the engine (WithWorkDir —
			// dispatcher-seeded per-issue worktrees, studio-bound dirs) AND
			// is already isolated from the operator's main checkout. Both
			// gates matter:
			//   - An explicit `worktree: none` run launched from the main
			//     checkout is intentionally in-place — stamping Worktree=true
			//     would make resume/review-gate finalization reconstruct a
			//     worktree context against the user's checkout and
			//     potentially branch/merge/clean it.
			//   - A defaulted-CWD run from inside a FOREIGN linked worktree
			//     (a Claude Code session worktree, an operator's manual
			//     `git worktree add`) is equally the operator's own place:
			//     without the workDirDelegated gate, closing such a run
			//     would create an iterion/run/* branch there and best-effort
			//     FF the operator's checked-out branch onto its HEAD.
			mainRepoRoot := gitlib.FindMainRepoRoot(e.workDir)
			if mainRepoRoot != "" && mainRepoRoot != worktreeRoot {
				if head, herr := gitlib.RevParseHead(e.workDir); herr == nil && head != "" {
					run.RepoRoot = mainRepoRoot
					run.BaseCommit = head
					run.Worktree = true
				}
			}
		}
		if err := e.store.SaveRun(ctx, run); err != nil {
			e.markFailedBestEffort(ctx, runID, "save work dir", err)
			return fmt.Errorf("runtime: save work dir: %w", err)
		}
	}
	// Push workDir into the executor so backend subprocesses (claude_code,
	// codex) and tool nodes see it. Type-assert because NodeExecutor is a
	// minimal interface; only ClawExecutor implements SetWorkDir.
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}
	// Push repoRoot (when known) so memory specs with `project_root: true`
	// resolve against the operator's main checkout instead of the per-run
	// workspace. Same minimal-interface pattern as SetWorkDir.
	if s, ok := e.executor.(repoRootSetter); ok {
		s.SetRepoRoot(run.RepoRoot)
	}
	// Refresh the orchestrator-facing bot catalog from the live manifests
	// (display_name / description / when_to_use / triggers / enabled +
	// the workspace overlay) BEFORE mirroring, so an edited bot or a
	// catalog toggle reaches Nexie on her next run. Writes into the
	// whats-next bundle SOURCE skills dir; the mirror below then refreshes
	// the workspace copy via its marker logic. Best-effort + no-op unless
	// this workspace ships the catalog template — a failure must never
	// abort the run (the mirror falls back to the on-disk catalog).
	if _, err := botregistry.RegenerateWhatsNextCatalog(e.workDir); err != nil {
		if e.logger != nil {
			e.logger.Warn("bot catalog regen: %v", err)
		}
	}
	// Bundle skill mirroring: when a .botz backs this run, copy the
	// bundle's skills/ entries into <workDir>/.claude/skills/ so both
	// claude_code's native skill lookup and the claw `skill` tool
	// discover them transparently. Workspace files always win on
	// collision (see runtime/bundle.go for the rule).
	if err := mirrorBundleSkills(e.workDir, e.bundle, e.logger); err != nil {
		e.markFailedBestEffort(ctx, runID, "bundle skills", err)
		return fmt.Errorf("runtime: bundle skills: %w", err)
	}
	// Mirror markdown contributions (skills / commands / agents) from enabled plugins
	// after the bundle skills so a same-named bundle/workspace file
	// wins on collision. Best-effort: a plugin must not fail the run.
	if err := mirrorPluginContributions(e.workDir, e.contributions, e.logger); err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin contributions: %v", err)
	}
	if err := mergePluginHooks(e.workDir, e.logger); err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin hooks: %v", err)
	}
	// Skill-library skills referenced by the workflow (DSL `skills:`), mirrored
	// LAST so a same-named bundle/plugin/workspace file wins on collision
	// (precedence: bundle > plugin > library > hand-authored — ADR-059). The
	// returned name→description map feeds every LLM node's "## Skills" hint.
	e.applyLibrarySkills()
	e.applyPresetFocus()
	return nil
}

// applyLibrarySkills resolves and mirrors the workflow's skill-library
// references, then pushes the resolved name→description hints into the executor
// so each LLM node renders its "## Skills" section. Best-effort: a mirror
// failure is logged but never fails the run (the DSL reference is soft). Only
// ClawExecutor implements SetSkillHints.
func (e *Engine) applyLibrarySkills() {
	hints, err := mirrorLibrarySkills(e.workDir, e.store.Root(), e.workflow, e.contributions, e.logger)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("runtime: library skills: %v", err)
		}
		return
	}
	if len(hints) == 0 {
		return
	}
	type skillHintSetter interface{ SetSkillHints(map[string]string) }
	if s, ok := e.executor.(skillHintSetter); ok {
		s.SetSkillHints(hints)
	}
}

// applyPresetFocus wires the selected preset's launch-time bias into the
// run. It first folds the bundle's file-based presets into the workflow as
// a backstop for paths that compiled without the bundle (studio in-process,
// cloud runner), then pushes the selected preset's prompt fragment + skill
// hints into the executor so every LLM node's system prompt gains a
// "## Focus" section (see delegate.Task.BuildSystemPrompt). Var-only presets
// (no prompt, no skills) are a no-op here — their overrides already flowed
// through the launch wiring. The executor focus is best-effort: only
// ClawExecutor implements SetPresetFocus.
func (e *Engine) applyPresetFocus() {
	MergeBundlePresets(e.workflow, e.bundle, e.logger)
	if e.preset == "" {
		return
	}
	p, ok := e.workflow.Presets[e.preset]
	if !ok || (p.Prompt == "" && len(p.Skills) == 0) {
		return
	}
	type presetFocusSetter interface {
		SetPresetFocus(prompt string, skills []string)
	}
	if s, ok := e.executor.(presetFocusSetter); ok {
		s.SetPresetFocus(p.Prompt, p.Skills)
	}
}

// runInitState constructs the per-run runState, resolves vars,
// loads attachments, caches the worktree flag (so per-node snapshot
// decisions don't re-read run.json N times), and pushes the resolved
// vars back into the executor — PROJECT_DIR-aware expansion may have
// changed values from what the caller originally seeded.
func (e *Engine) runInitState(ctx context.Context, runID string, inputs map[string]any) *runState {
	rs := e.newRunState(runID, inputs)
	rs.ctx = ctx
	rs.vars = e.resolveVars(inputs)
	rs.attachments = e.loadAttachmentInfos(ctx, runID)
	if r, err := e.store.LoadRun(ctx, runID); err == nil && r != nil {
		rs.isWorktree = r.Worktree
	}
	if sv, ok := e.executor.(varsSetter); ok {
		sv.SetVars(rs.vars)
	}
	return rs
}

// finalizeOnExit applies the worktree-finalization step at the end of a
// run. Called from Run() (which captures wtCtx during setupWorktree)
// and from both resume paths (which reconstruct wtCtx from the
// persisted run record). Persistence + cleanup are best-effort: a save
// failure logs but never fails the run, since the work has completed.
//
// Without this on the resume paths, a `worktree: auto` run that paused
// and resumed via CLI ended with no final_branch / final_commit
// persisted, the worktree dir leaked, and the run's commits were
// reachable only via reflog (eligible for `git gc` after ~30 days) —
// see F-RT-1 in docs/reviews/codebase-2026-05-17.md.
func (e *Engine) finalizeOnExit(ctx context.Context, runID string, wtCtx *worktreeContext, cleanup func(), loopErr error) {
	if wtCtx == nil {
		return
	}
	if loopErr != nil {
		if e.logger != nil {
			e.logger.Info("runtime: worktree preserved for inspection: %s", e.workDir)
		}
		return
	}
	// Idempotency guard for the review-&-merge gate: a review gate
	// (interaction: review) finalizes the worktree DURING the human pause —
	// it squash-merges (merge_status=merged) or, for merge_into: none / no
	// commits, records merge_status=skipped. MergeStatus is the canonical
	// "finalize already happened" signal (the run-end finalizeWorktree only
	// ever sets it from empty), so its presence here means the gate already
	// owns this run's finalize. Re-running finalizeWorktree would create a
	// duplicate storage branch and possibly re-merge — skip it, just clean up.
	if r, err := e.store.LoadRun(ctx, runID); err == nil && r.MergeStatus != "" {
		// Staleness check: the recorded finalize (review gate, or an
		// orphan-recovery pass) captured FinalCommit at ITS moment. If the
		// worktree HEAD has since moved, later committed work exists that the
		// recorded finalize never promoted — skipping here would delete the
		// worktree and strand those commits on the per-node GC-guard refs
		// (observed: a mid-flight recovered finalize marked the run
		// finalized, the true completion skipped, and the delivery had to be
		// recovered from refs/iterion/runs/*). Re-finalize at the true tip
		// instead; the storage branch gets a numeric suffix on collision.
		if head := readHEAD(wtCtx.wtPath); head != "" && r.FinalCommit != "" && head != r.FinalCommit {
			if e.logger != nil {
				e.logger.Warn("runtime: finalize: recorded finalize (%s @ %.9s) is stale — worktree HEAD moved to %.9s; re-finalizing",
					r.MergeStatus, r.FinalCommit, head)
			}
		} else {
			if e.logger != nil {
				e.logger.Info("runtime: finalize: run already finalized at review gate (status=%s, branch=%s); skipping",
					r.MergeStatus, r.FinalBranch)
			}
			// The gate finalized COMMITS, but post-gate work may sit
			// uncommitted in the worktree — removing it would destroy that
			// work silently. Preserve instead; the operator recovers via the
			// studio commit-and-finalize action.
			if clean, cleanErr := workdirIsClean(wtCtx.wtPath); cleanErr == nil && !clean {
				if e.logger != nil {
					e.logger.Warn("runtime: finalize: worktree has uncommitted changes after review-gate finalize — preserving %s for inspection", wtCtx.wtPath)
				}
				return
			}
			if cleanup != nil {
				cleanup()
			}
			return
		}
	}
	finRes := finalizeWorktree(*wtCtx, finalizeOptions{
		runName:       e.runName,
		runID:         runID,
		branchName:    e.branchName,
		mergeInto:     e.mergeInto,
		mergeStrategy: e.mergeStrategy,
		autoMerge:     e.autoMerge,
	}, e.logger)
	if finRes.FinalCommit != "" || finRes.FinalBranch != "" || finRes.MergedInto != "" || finRes.MergeStatus != "" || finRes.FinalBranchError != "" {
		if r2, err := e.store.LoadRun(ctx, runID); err == nil {
			r2.FinalCommit = finRes.FinalCommit
			r2.FinalBranch = finRes.FinalBranch
			r2.FinalBranchError = finRes.FinalBranchError
			r2.MergedInto = finRes.MergedInto
			r2.MergeStatus = store.MergeStatus(finRes.MergeStatus)
			r2.MergedCommit = finRes.MergedCommit
			if e.mergeStrategy != "" {
				r2.MergeStrategy = store.MergeStrategy(e.mergeStrategy)
			}
			r2.AutoMerge = e.autoMerge
			if saveErr := e.store.SaveRun(ctx, r2); saveErr != nil && e.logger != nil {
				e.logger.Warn("runtime: persist finalization metadata: %v", saveErr)
			}
		}
	}
	if finRes.FinalBranchError != "" {
		if err := e.emit(ctx, runID, store.EventWorktreeBranchFailed, "", map[string]any{
			"sha":    finRes.FinalCommit,
			"reason": finRes.FinalBranchError,
		}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit worktree_branch_failed event for %s: %v", runID, err)
		}
	}
	if finRes.PreserveWorktree {
		// finalize could not bank the worktree's uncommitted changes —
		// removing it now would silently destroy finished work.
		if e.logger != nil {
			e.logger.Warn("runtime: worktree preserved (unbanked uncommitted changes): %s", e.workDir)
		}
		return
	}
	if cleanup != nil {
		cleanup()
	}
}

// reconstructWorktreeContext rebuilds a worktreeContext from a persisted
// run record on the resume path. The original setupWorktree-time
// `originalBranch` is not in `r.*` — re-read it from the live repo so
// finalizeWorktree can attempt the FF when the operator hasn't switched
// branches since launch. Returns nil when the run isn't a worktree run
// or when the persisted paths are empty.
func (e *Engine) reconstructWorktreeContext(r *store.Run) *worktreeContext {
	if r == nil || !r.Worktree || r.WorkDir == "" || r.RepoRoot == "" {
		return nil
	}
	// Skip if the worktree directory is gone (already finalized, or
	// operator removed it manually). finalizeWorktree handles a missing
	// dir gracefully, but skipping here avoids an unnecessary git call.
	if _, err := os.Stat(r.WorkDir); err != nil {
		return nil
	}
	originalBranch := ""
	brCmd, brCancel := gitCmd("-C", r.RepoRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
	if out, brErr := brCmd.Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	brCancel()
	return &worktreeContext{
		repoRoot:       r.RepoRoot,
		wtPath:         r.WorkDir,
		gitDir:         resolveWorktreeGitDir(r.RepoRoot, r.WorkDir),
		originalBranch: originalBranch,
		originalTip:    r.BaseCommit,
	}
}

// evictRunSessions clears any per-node session state still held by
// the executor for runID, except when the run is paused (human input
// awaited) — in which case Resume will pick up the same sessions.
func (e *Engine) evictRunSessions(runID string, loopErr error) {
	if errors.Is(loopErr, ErrRunPaused) {
		return
	}
	if ev, ok := e.executor.(interface{ EvictRun(string) }); ok {
		ev.EvictRun(runID)
	}
}
