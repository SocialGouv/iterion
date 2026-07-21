package main

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var runOpts struct {
	recipe              string
	preset              string
	runID               string
	storeDir            string
	timeout             time.Duration
	logLevel            string
	noInteractive       bool
	skipMCPHealth       bool
	varFlags            []string
	modelFor            []string
	backendFor          []string
	background          bool
	mergeInto           string
	branchName          string
	mergeStrategy       string
	autoMerge           bool
	sandbox             string
	sandboxDefaultImage string
	sandboxHostState    string
	compress            string
	permission          string
	permissionAllow     []string
	permissionAsk       []string
	permissionDeny      []string
	reviewMode          string
	maxCostUSD          float64
	maxTokens           int
	maxDuration         string
	maxIterations       int
	maxParallelBranches int
	autoResume          int
}

var runCmd = &cobra.Command{
	Use:   "run <file.bot|file.botz|bundle-dir>",
	Short: "Execute a workflow",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.RunOptions{
			File:                args[0],
			Recipe:              runOpts.recipe,
			Preset:              runOpts.preset,
			RunID:               runOpts.runID,
			StoreDir:            runOpts.storeDir,
			Timeout:             runOpts.timeout,
			LogLevel:            runOpts.logLevel,
			NoInteractive:       runOpts.noInteractive,
			SkipMCPHealth:       runOpts.skipMCPHealth,
			Background:          runOpts.background,
			MergeInto:           runOpts.mergeInto,
			BranchName:          runOpts.branchName,
			MergeStrategy:       runOpts.mergeStrategy,
			AutoMerge:           runOpts.autoMerge,
			Sandbox:             runOpts.sandbox,
			SandboxDefaultImage: runOpts.sandboxDefaultImage,
			SandboxHostState:    runOpts.sandboxHostState,
			Compress:            runOpts.compress,
			Permission:          runOpts.permission,
			PermissionAllow:     runOpts.permissionAllow,
			PermissionAsk:       runOpts.permissionAsk,
			PermissionDeny:      runOpts.permissionDeny,
			ReviewMode:          runOpts.reviewMode,
			ModelFor:            runOpts.modelFor,
			BackendFor:          runOpts.backendFor,
			AutoResume:          runOpts.autoResume,
			Budget: cli.BudgetOverrides{
				MaxCostUSD:          runOpts.maxCostUSD,
				MaxTokens:           runOpts.maxTokens,
				MaxDuration:         runOpts.maxDuration,
				MaxIterations:       runOpts.maxIterations,
				MaxParallelBranches: runOpts.maxParallelBranches,
			},
		}
		if len(runOpts.varFlags) > 0 {
			vars, err := cli.ParseVarFlags(runOpts.varFlags)
			if err != nil {
				return err
			}
			opts.Vars = vars
		}
		return cli.RunRun(cmd.Context(), opts, newPrinter())
	},
}

func init() {
	f := runCmd.Flags()
	f.StringArrayVar(&runOpts.varFlags, "var", nil, "Set workflow variable (key=value, repeatable)")
	f.StringVar(&runOpts.recipe, "recipe", "", "Recipe JSON file")
	f.StringVar(&runOpts.preset, "preset", "", "Apply a named in-source preset (presets: block) before --var overrides")
	f.StringVar(&runOpts.runID, "run-id", "", "Explicit run ID")
	f.StringVar(&runOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the workflow project)")
	f.DurationVar(&runOpts.timeout, "timeout", 0, "Maximum run duration (e.g. 30s, 5m, 1h)")
	f.StringVar(&runOpts.logLevel, "log-level", "", "Log verbosity: error, warn, info, debug, trace")
	f.BoolVar(&runOpts.noInteractive, "no-interactive", false, "Don't prompt on TTY; exit on human pause")
	f.BoolVar(&runOpts.skipMCPHealth, "skip-mcp-health", false, "Don't abort the run when an MCP server fails its startup health-check — log the failure as a warning and continue. Also enabled by ITERION_SKIP_MCP_HEALTH=1. Use when a declared MCP server (e.g. an HTTP-OAuth one) is unreachable/unauthorized in this environment but the run does not depend on it.")
	f.BoolVar(&runOpts.background, "background", false, "Internal: managed-runner mode for the studio server (writes .pid, suppresses interactive prompts)")
	_ = f.MarkHidden("background")
	f.StringVar(&runOpts.mergeInto, "merge-into", "", "For worktree:auto runs, branch to merge into after the run (\"\"/\"current\"=current branch, \"none\"=skip, or a branch name)")
	f.StringVar(&runOpts.branchName, "branch-name", "", "For worktree:auto runs, override the storage branch name (default iterion/run/<friendly>)")
	f.StringVar(&runOpts.mergeStrategy, "merge-strategy", "", "For worktree:auto runs, how to land commits when --auto-merge is on: \"squash\" (default) collapses all run commits into one, \"merge\" fast-forwards (preserves history)")
	f.BoolVar(&runOpts.autoMerge, "auto-merge", true, "For worktree:auto runs, apply --merge-strategy at the end of the run (CLI default true preserves prior behaviour; the studio sets false by default to defer the merge to a UI action)")
	f.StringVar(&runOpts.sandbox, "sandbox", "", "Run-level sandbox override: \"none\" (force off), \"auto\" (read .devcontainer/devcontainer.json). Empty inherits ITERION_SANDBOX_DEFAULT then the workflow's own sandbox: block. See pkg/sandbox.")
	f.StringVar(&runOpts.sandboxDefaultImage, "sandbox-default-image", "", "Image ref used by sandbox: auto when no .devcontainer/devcontainer.json is found (env: ITERION_SANDBOX_DEFAULT_IMAGE; built-in: ghcr.io/socialgouv/iterion-sandbox-slim:<iterion-version>)")
	f.StringVar(&runOpts.sandboxHostState, "sandbox-host-state", "", "Bind host ~/.iterion and ~/.claude into the sandbox so persistent memory survives across runs: \"auto\" (default) | \"none\". Empty inherits ITERION_SANDBOX_HOST_STATE then the built-in default \"auto\". Use \"none\" on multi-tenant/cloud runners to avoid leaking host OAuth credentials. See docs/sandbox.md.")
	f.StringVar(&runOpts.compress, "compress", "", "command-output compression via the active rewriter plugin chain (rtk by default): \"on\" rewrites agent shell commands to their compact form (e.g. \"rtk <cmd>\"), \"ultra\" requests the densest output, \"off\" disables. Empty inherits the workflow/node compress: DSL then ITERION_COMPRESS. Needs an enabled rewriter plugin whose binary is on PATH. See docs/plugins.md.")
	f.StringVar(&runOpts.permission, "permission", "", "tool-permission gate (anti-prompt-injection): \"ask\" pauses for human approval on any tool not allow-listed, \"deny\" hard-blocks it (headless), \"off\" disables. Empty inherits the workflow/node permission: DSL then ITERION_PERMISSION. See docs/permissions.md.")
	f.StringArrayVar(&runOpts.permissionAllow, "permission-allow", nil, "permission allow rule (repeatable), Claude-Code syntax e.g. 'Bash(go test:*)', 'Read(**)', 'Edit(pkg/**)'. Auto-approved without prompting. Additive to the workflow allow: list.")
	f.StringArrayVar(&runOpts.permissionAsk, "permission-ask", nil, "permission ask rule (repeatable): matching calls always pause for approval. Additive to the workflow ask: list.")
	f.StringArrayVar(&runOpts.permissionDeny, "permission-deny", nil, "permission deny rule (repeatable): matching calls are always blocked, even in ask mode. Additive to the workflow deny: list.")
	f.StringVar(&runOpts.reviewMode, "review-mode", "", "For bots that opt into the mono/dual review-loop topology by declaring a review_mode var (ADR-052): \"mono\" (one model family, ~half the calls), \"dual\" (alternate two families), or \"auto\" (default: dual when two provider families are detected, else mono). The shipped catalog no longer declares it (ADR-058 v2 campaigns); the surface stays for third-party or future reviewer-loop bots. No-op otherwise.")
	f.StringArrayVar(&runOpts.modelFor, "model", nil, "Per-node/-group model override (repeatable): \"selector=model\" or a bare \"model\" for every LLM node. Selector = node id (reviewer_claude), id glob (reviewer_*, fix_*), or node kind (agent|judge). Wins over the node's DSL model:. E.g. --model 'reviewer_*=anthropic/claude-fable-5' --model 'fix_*=claude-sonnet-5'. Composes with --review-mode.")
	f.StringArrayVar(&runOpts.backendFor, "backend", nil, "Per-node/-group backend override (repeatable): \"selector=backend\" or a bare \"backend\" for every LLM node (claw|claude_code|kimi|grok; codex is legacy). Same selector syntax as --model; wins over the node's DSL backend:.")
	registerBudgetFlags(f, &runOpts.maxCostUSD, &runOpts.maxTokens, &runOpts.maxDuration, &runOpts.maxIterations, &runOpts.maxParallelBranches)
	registerAutoResumeFlag(f, &runOpts.autoResume)
	rootCmd.AddCommand(runCmd)
}

// registerBudgetFlags wires the at-run budget-override flags onto a command's
// flag set. Shared by `run` and `resume` so both expose the same overrides
// (the documented "raise the cap + resume" recovery needs them on resume too).
// Each flag's zero value means "inherit the workflow/recipe budget"; a
// non-zero value overrides that dimension (see cli.applyBudgetOverrides).
// registerAutoResumeFlag wires the bounded run-level auto-resume flag onto a
// command's flag set. Shared by `run` and `resume` (the latter can itself
// keep re-driving a transient-failing run). 0 = off (env: ITERION_AUTO_RESUME).
func registerAutoResumeFlag(f *pflag.FlagSet, n *int) {
	f.IntVar(n, "auto-resume", 0, "Auto-resume a failed_resumable run with a retryable cause (transient backend error, budget/timeout with a raised --max-* cap, rate-limit) up to N times with capped exponential backoff (0 = off; env: ITERION_AUTO_RESUME). Respects the forfait usage cap (ITERION_FORFAIT_CAP_PCT).")
}

func registerBudgetFlags(f *pflag.FlagSet, cost *float64, tokens *int, duration *string, iterations, parallel *int) {
	f.Float64Var(cost, "max-cost-usd", 0, "Override the workflow budget's max_cost_usd (USD; 0 = inherit the bot's budget)")
	f.IntVar(tokens, "max-tokens", 0, "Override the workflow budget's max_tokens (0 = inherit)")
	f.StringVar(duration, "max-duration", "", "Override the workflow budget's max_duration, e.g. 30m, 2h (empty = inherit)")
	f.IntVar(iterations, "max-iterations", 0, "Override the workflow budget's max_iterations (0 = inherit)")
	f.IntVar(parallel, "max-parallel-branches", 0, "Override the workflow budget's max_parallel_branches (0 = inherit)")
}
