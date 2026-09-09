package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var reliabilityCmd = &cobra.Command{
	Use:   "reliability",
	Short: "Inspect and roll back the workflow-reliability rollout",
	Long: `Read-only operator surface for the staged workflow-reliability rollout
(docs/workflow-reliability-1006.md).

Subcommands:
  report     Resolved rollout mode + a fleet baseline, or one run's compatibility report
  rollback   Print the non-destructive rollback plan
`,
}

var reliabilityReportOpts struct {
	storeDir string
	runID    string
}

var reliabilityReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Show the resolved rollout mode and a pilot baseline",
	Long: `Print the rollout mode the launch surfaces actually apply for this
environment (ITERION_RELIABILITY_MODE, falling back to the older
ITERION_EXECUTION_CONTEXT_POLICY), the retry-circuit settings, and either a
fleet baseline over the store's runs or one run's compatibility report.

This is step 1 of the pilot procedure — capture it BEFORE switching modes and
again after, and compare. Nothing here writes: a report never upgrades a
legacy run's policy.

Examples:
  iterion reliability report                       # fleet baseline
  iterion reliability report --run-id <id>         # one run's compatibility
  iterion reliability report --json                # machine-readable
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunReliabilityReport(cli.ReliabilityOptions{
			StoreDir: reliabilityReportOpts.storeDir,
			RunID:    reliabilityReportOpts.runID,
		}, newPrinter())
	},
}

var reliabilityRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Print the non-destructive rollback plan",
	Long: `Print the rollback contract: which variable to set, and what evidence to
keep. It prints — it does not mutate. The variables are read where the launch
surfaces run (a pod spec, a service unit, a shell), which a one-shot CLI
process cannot reach.
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cli.RunReliabilityRollback(newPrinter())
	},
}

func init() {
	f := reliabilityReportCmd.Flags()
	f.StringVar(&reliabilityReportOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	f.StringVar(&reliabilityReportOpts.runID, "run-id", "", "Report on a single run instead of the fleet baseline")

	reliabilityCmd.AddCommand(reliabilityReportCmd)
	reliabilityCmd.AddCommand(reliabilityRollbackCmd)
	rootCmd.AddCommand(reliabilityCmd)
}
