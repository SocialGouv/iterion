package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var resumeOpts struct {
	runID       string
	file        string
	storeDir    string
	answersFile string
	answerFlags []string
	logLevel    string
	force       bool
	forceStale  bool
	background  bool

	autoMemory      string
	loopBudgetGuard string
	supervisors     string
	repoDevbox      string
	permission      string
	permissionAllow []string
	permissionAsk   []string
	permissionDeny  []string
	modelFor        []string
	backendFor      []string
	fallback        string
	effortFor       []string

	// Sandbox and worktree-finalization overrides on resume — empty
	// means "inherit the launch's persisted decision", non-empty
	// replaces it on purpose. Same doctrine as --max-*: the record
	// carries the launch's ask, the flag layers over it. See #1435/#1366.
	sandbox             string
	sandboxDefaultImage string
	sandboxHostState    string
	mergeInto           string
	branchName          string
	mergeStrategy       string
	// autoMergeSet mirrors AutoMerge=bool but expresses "unset" (the
	// default) as false-false so we can tell inherit apart from an
	// explicit --auto-merge=false. cobra's BoolVar cannot do that; we
	// read the flag's presence via cmd.Flags().Changed instead.
	autoMerge bool

	maxCostUSD          float64
	maxTokens           int
	maxDuration         string
	maxIterations       int
	maxParallelBranches int
	unlimitedWorkflow   bool
	autoResume          int
}

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume a paused or failed run",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.ResumeOptions{
			RunID:       resumeOpts.runID,
			StoreDir:    resumeOpts.storeDir,
			AnswersFile: resumeOpts.answersFile,
			LogLevel:    resumeOpts.logLevel,
			Force:       resumeOpts.force,
			ForceStale:  resumeOpts.forceStale,
			Background:  resumeOpts.background,

			AutoMemory:          resumeOpts.autoMemory,
			LoopBudgetGuard:     resumeOpts.loopBudgetGuard,
			Supervisors:         resumeOpts.supervisors,
			RepoDevbox:          resumeOpts.repoDevbox,
			Permission:          resumeOpts.permission,
			PermissionAllow:     resumeOpts.permissionAllow,
			PermissionAsk:       resumeOpts.permissionAsk,
			PermissionDeny:      resumeOpts.permissionDeny,
			ModelFor:            resumeOpts.modelFor,
			BackendFor:          resumeOpts.backendFor,
			Fallback:            resumeOpts.fallback,
			EffortFor:           resumeOpts.effortFor,
			AutoResume:          resumeOpts.autoResume,
			Sandbox:             resumeOpts.sandbox,
			SandboxDefaultImage: resumeOpts.sandboxDefaultImage,
			SandboxHostState:    resumeOpts.sandboxHostState,
			MergeInto:           resumeOpts.mergeInto,
			BranchName:          resumeOpts.branchName,
			MergeStrategy:       resumeOpts.mergeStrategy,
			Budget: cli.BudgetOverrides{
				MaxCostUSD:          resumeOpts.maxCostUSD,
				MaxTokens:           resumeOpts.maxTokens,
				MaxDuration:         resumeOpts.maxDuration,
				MaxIterations:       resumeOpts.maxIterations,
				MaxParallelBranches: resumeOpts.maxParallelBranches,
				UnlimitedWorkflow:   resumeOpts.unlimitedWorkflow,
			},
		}
		if len(resumeOpts.answerFlags) > 0 {
			answers, err := cli.ParseAnswerFlags(resumeOpts.answerFlags)
			if err != nil {
				return err
			}
			opts.Answers = answers
		}
		// cobra cannot express "flag not set" for a bool, so read
		// the presence of --auto-merge explicitly. A resume that
		// omits it inherits the launch's persisted AutoMerge.
		if cmd.Flags().Changed("auto-merge") {
			v := resumeOpts.autoMerge
			opts.AutoMerge = &v
		}
		return cli.RunResumeWithFile(cmd.Context(), resumeOpts.file, opts, newPrinter())
	},
}

func init() {
	f := resumeCmd.Flags()
	f.StringVar(&resumeOpts.runID, "run-id", "", "Run to resume")
	f.StringVar(&resumeOpts.file, "file", "", "Workflow file (.bot) or bundle (.botz); defaults to the path persisted at launch")
	f.StringVar(&resumeOpts.storeDir, "store-dir", "", "Store directory override (default: managed project store)")
	f.StringVar(&resumeOpts.answersFile, "answers-file", "", "JSON file with answers")
	f.StringArrayVar(&resumeOpts.answerFlags, "answer", nil, "Set answer (key=value, repeatable)")
	f.StringVar(&resumeOpts.logLevel, "log-level", "", "Log verbosity: error, warn, info, debug, trace")
	f.BoolVar(&resumeOpts.force, "force", false, "Resume even if workflow source has changed")
	f.BoolVar(&resumeOpts.forceStale, "force-stale", false, "Resume a status=running run whose engine has died (requires events.jsonl mtime ≥ 60s — server boot does this automatically)")
	f.BoolVar(&resumeOpts.background, "background", false, "Internal: managed-runner mode for the studio server (writes .pid, suppresses interactive prompts)")
	_ = f.MarkHidden("background")
	f.StringVar(&resumeOpts.autoMemory, "auto-memory", "", "backend auto-memory (MEMORY.md) override on resume: on|off. Empty inherits the workflow/node auto_memory: DSL then ITERION_AUTO_MEMORY — NOT the original launch, which is not persisted, so re-state it to keep a hermetic run hermetic. See docs/memory-and-knowledge.md.")
	f.StringVar(&resumeOpts.repoDevbox, "repo-devbox", "", "install the target repository's devbox.json on resume: on|off. Empty inherits the workflow repo_devbox: DSL then ITERION_REPO_DEVBOX — NOT the original launch, which is not persisted. See docs/dsl.md.")
	f.StringVar(&resumeOpts.loopBudgetGuard, "loop-budget-guard", "", "loop back-edge affordability guard on resume: on|off. Empty inherits the workflow loop_budget_guard: DSL then ITERION_LOOP_BUDGET_GUARD — NOT the original launch, which is not persisted. See docs/dsl.md.")
	f.StringVar(&resumeOpts.supervisors, "supervisors", "", "spawn DSL-declared supervisors on resume: on|off (not persisted from launch; empty inherits ITERION_SUPERVISORS). See docs/supervisors.md.")
	f.StringVar(&resumeOpts.permission, "permission", "", "tool-permission gate override on resume: off|ask|deny (empty inherits the workflow/ITERION_PERMISSION). See docs/permissions.md.")
	f.StringArrayVar(&resumeOpts.permissionAllow, "permission-allow", nil, "permission allow rule (repeatable), e.g. 'Bash(go build:*)'. Authorize an action the run paused on, then it proceeds on resume.")
	f.StringArrayVar(&resumeOpts.permissionAsk, "permission-ask", nil, "permission ask rule (repeatable): matching calls pause for approval.")
	f.StringArrayVar(&resumeOpts.permissionDeny, "permission-deny", nil, "permission deny rule (repeatable): matching calls are always blocked.")
	f.StringArrayVar(&resumeOpts.modelFor, "model", nil, "Override the model on resume (repeatable): \"selector=model\" or a bare \"model\". A resume already INHERITS what the run was launched with (read back off the run document); this layers over it per field, so overriding the effort alone keeps the launched model. Selector = node id, id glob (reviewer_*), or kind (agent|judge).")
	f.StringVar(&resumeOpts.fallback, "fallback", "", "Re-apply the run-level fallback route on resume: \"<backend>:<model>\". Resume does NOT persist launch rules, and a long run outliving a quota window is exactly the case that resumes — pass the same --fallback used at run or the route stops applying silently.")
	f.StringArrayVar(&resumeOpts.backendFor, "backend", nil, "Re-apply a per-node/-group backend override on resume (repeatable): \"selector=backend\" or a bare \"backend\" (claw|claude_code|pi|kimi|grok; codex is legacy). Same selector syntax as --model.")
	f.StringArrayVar(&resumeOpts.effortFor, "effort-for", nil, "Re-apply a per-node/-group reasoning_effort override on resume (repeatable): \"selector=effort\" or a bare \"effort\" (low|medium|high|xhigh|max|ultracode). Same selector syntax as --model.")
	f.StringVar(&resumeOpts.sandbox, "sandbox", "", "sandbox override on resume: \"none\" (force off), \"auto\" (read .devcontainer/devcontainer.json). Empty inherits the launch-time --sandbox choice persisted on the run (a run launched with --sandbox none refuses docker on resume too); non-empty replaces it on purpose. See docs/sandbox.md.")
	f.StringVar(&resumeOpts.sandboxDefaultImage, "sandbox-default-image", "", "sandbox-default-image override on resume. Empty inherits the launch's persisted value; non-empty replaces it. See --sandbox.")
	f.StringVar(&resumeOpts.sandboxHostState, "sandbox-host-state", "", "sandbox host_state override on resume: \"auto\" | \"none\". Empty inherits the launch's persisted value; non-empty replaces it. See docs/sandbox.md.")
	f.StringVar(&resumeOpts.mergeInto, "merge-into", "", "worktree-finalization --merge-into override on resume: \"\"/\"current\", \"none\", or a branch name. Empty inherits the launch's persisted target; non-empty replaces it. See docs/dsl.md#worktree.")
	f.StringVar(&resumeOpts.branchName, "branch-name", "", "worktree-finalization storage-branch override on resume. Empty inherits the launch's persisted value; non-empty replaces it.")
	f.StringVar(&resumeOpts.mergeStrategy, "merge-strategy", "", "worktree-finalization merge-strategy override on resume: \"squash\" | \"merge\". Empty inherits the launch's persisted value.")
	f.BoolVar(&resumeOpts.autoMerge, "auto-merge", false, "worktree-finalization auto-merge override on resume. Omit to inherit the launch's persisted intent; pass --auto-merge=false to defer the merge to the UI, --auto-merge=true to run it synchronously.")
	registerBudgetFlags(f, &resumeOpts.maxCostUSD, &resumeOpts.maxTokens, &resumeOpts.maxDuration, &resumeOpts.maxIterations, &resumeOpts.maxParallelBranches, &resumeOpts.unlimitedWorkflow)
	registerAutoResumeFlag(f, &resumeOpts.autoResume)
	mustMarkRequired(resumeCmd, "run-id")
	rootCmd.AddCommand(resumeCmd)
}
