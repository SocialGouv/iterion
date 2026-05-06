package main

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var runOpts struct {
	recipe        string
	runID         string
	storeDir      string
	timeout       time.Duration
	logLevel      string
	noInteractive bool
	varFlags      []string
	background    bool
	mergeInto     string
	branchName    string
	mergeStrategy string
	autoMerge     bool
	sandbox       string
}

var runCmd = &cobra.Command{
	Use:   "run <file.iter>",
	Short: "Execute a workflow",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.RunOptions{
			File:          args[0],
			Recipe:        runOpts.recipe,
			RunID:         runOpts.runID,
			StoreDir:      runOpts.storeDir,
			Timeout:       runOpts.timeout,
			LogLevel:      runOpts.logLevel,
			NoInteractive: runOpts.noInteractive,
			Background:    runOpts.background,
			MergeInto:     runOpts.mergeInto,
			BranchName:    runOpts.branchName,
			MergeStrategy: runOpts.mergeStrategy,
			AutoMerge:     runOpts.autoMerge,
			Sandbox:       runOpts.sandbox,
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
	f.StringVar(&runOpts.runID, "run-id", "", "Explicit run ID")
	f.StringVar(&runOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	f.DurationVar(&runOpts.timeout, "timeout", 0, "Maximum run duration (e.g. 30s, 5m, 1h)")
	f.StringVar(&runOpts.logLevel, "log-level", "", "Log verbosity: error, warn, info, debug, trace")
	f.BoolVar(&runOpts.noInteractive, "no-interactive", false, "Don't prompt on TTY; exit on human pause")
	f.BoolVar(&runOpts.background, "background", false, "Internal: managed-runner mode for the editor server (writes .pid, suppresses interactive prompts)")
	_ = f.MarkHidden("background")
	f.StringVar(&runOpts.mergeInto, "merge-into", "", "For worktree:auto runs, branch to merge into after the run (\"\"/\"current\"=current branch, \"none\"=skip, or a branch name)")
	f.StringVar(&runOpts.branchName, "branch-name", "", "For worktree:auto runs, override the storage branch name (default iterion/run/<friendly>)")
	f.StringVar(&runOpts.mergeStrategy, "merge-strategy", "", "For worktree:auto runs, how to land commits when --auto-merge is on: \"squash\" (default) collapses all run commits into one, \"merge\" fast-forwards (preserves history)")
	f.BoolVar(&runOpts.autoMerge, "auto-merge", true, "For worktree:auto runs, apply --merge-strategy at the end of the run (CLI default true preserves prior behaviour; the editor sets false by default to defer the merge to a UI action)")
	f.StringVar(&runOpts.sandbox, "sandbox", "", "Run-level sandbox override: \"none\" (force off), \"auto\" (read .devcontainer/devcontainer.json). Empty inherits ITERION_SANDBOX_DEFAULT then the workflow's own sandbox: block. See pkg/sandbox.")
	rootCmd.AddCommand(runCmd)
}
