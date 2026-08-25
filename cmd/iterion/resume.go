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

	maxCostUSD          float64
	maxTokens           int
	maxDuration         string
	maxIterations       int
	maxParallelBranches int
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

			AutoMemory:      resumeOpts.autoMemory,
			LoopBudgetGuard: resumeOpts.loopBudgetGuard,
			Supervisors:     resumeOpts.supervisors,
			RepoDevbox:      resumeOpts.repoDevbox,
			Permission:      resumeOpts.permission,
			PermissionAllow: resumeOpts.permissionAllow,
			PermissionAsk:   resumeOpts.permissionAsk,
			PermissionDeny:  resumeOpts.permissionDeny,
			ModelFor:        resumeOpts.modelFor,
			BackendFor:      resumeOpts.backendFor,
			Fallback:        resumeOpts.fallback,
			AutoResume:      resumeOpts.autoResume,
			Budget: cli.BudgetOverrides{
				MaxCostUSD:          resumeOpts.maxCostUSD,
				MaxTokens:           resumeOpts.maxTokens,
				MaxDuration:         resumeOpts.maxDuration,
				MaxIterations:       resumeOpts.maxIterations,
				MaxParallelBranches: resumeOpts.maxParallelBranches,
			},
		}
		if len(resumeOpts.answerFlags) > 0 {
			answers, err := cli.ParseAnswerFlags(resumeOpts.answerFlags)
			if err != nil {
				return err
			}
			opts.Answers = answers
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
	f.StringArrayVar(&resumeOpts.modelFor, "model", nil, "Re-apply a per-node/-group model override on resume (repeatable): \"selector=model\" or a bare \"model\". Resume does NOT persist the launch overrides, so pass the same --model flags used at run to keep e.g. reviewers on their chosen model. Selector = node id, id glob (reviewer_*), or kind (agent|judge).")
	f.StringVar(&resumeOpts.fallback, "fallback", "", "Re-apply the run-level fallback route on resume: \"<backend>:<model>\". Resume does NOT persist launch rules, and a long run outliving a quota window is exactly the case that resumes — pass the same --fallback used at run or the route stops applying silently.")
	f.StringArrayVar(&resumeOpts.backendFor, "backend", nil, "Re-apply a per-node/-group backend override on resume (repeatable): \"selector=backend\" or a bare \"backend\" (claw|claude_code|codex|pi|kimi|grok). Same selector syntax as --model.")
	registerBudgetFlags(f, &resumeOpts.maxCostUSD, &resumeOpts.maxTokens, &resumeOpts.maxDuration, &resumeOpts.maxIterations, &resumeOpts.maxParallelBranches)
	registerAutoResumeFlag(f, &resumeOpts.autoResume)
	mustMarkRequired(resumeCmd, "run-id")
	rootCmd.AddCommand(resumeCmd)
}
