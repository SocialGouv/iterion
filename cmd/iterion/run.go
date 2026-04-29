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
	rootCmd.AddCommand(runCmd)
}
