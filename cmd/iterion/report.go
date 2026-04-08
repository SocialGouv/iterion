package main

import (
	"github.com/SocialGouv/iterion/cli"
	"github.com/spf13/cobra"
)

var reportOpts struct {
	runID    string
	storeDir string
	output   string
}

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate run report",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunReport(cli.ReportOptions{
			RunID:    reportOpts.runID,
			StoreDir: reportOpts.storeDir,
			Output:   reportOpts.output,
		}, newPrinter())
	},
}

func init() {
	f := reportCmd.Flags()
	f.StringVar(&reportOpts.runID, "run-id", "", "Run to report on")
	f.StringVar(&reportOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	f.StringVar(&reportOpts.output, "output", "", "Output file (default: store/runs/<id>/report.md)")
	mustMarkRequired(reportCmd, "run-id")
	rootCmd.AddCommand(reportCmd)
}
