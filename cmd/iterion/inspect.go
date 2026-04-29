package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var inspectOpts struct {
	runID    string
	storeDir string
	events   bool
	full     bool
}

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect runs and state",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunInspect(cli.InspectOptions{
			RunID:    inspectOpts.runID,
			StoreDir: inspectOpts.storeDir,
			Events:   inspectOpts.events,
			Full:     inspectOpts.full,
		}, newPrinter())
	},
}

func init() {
	f := inspectCmd.Flags()
	f.StringVar(&inspectOpts.runID, "run-id", "", "Run to inspect (omit to list all)")
	f.StringVar(&inspectOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	f.BoolVar(&inspectOpts.events, "events", false, "Show event log")
	f.BoolVar(&inspectOpts.full, "full", false, "Show all details")
	rootCmd.AddCommand(inspectCmd)
}
