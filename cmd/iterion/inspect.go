package main

import (
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var inspectOpts struct {
	runID       string
	storeDir    string
	events      bool
	full        bool
	node        string
	branch      string
	iteration   int
	executionID string
	section     string
	logTail     int
	listNodes   bool
}

var inspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect runs and state",
	Long: `Inspect runs, individual node executions, and their associated
events / LLM trace / tool calls / artifacts / interactions / log slice.

Examples:
  iterion inspect                                       # list all runs
  iterion inspect --run-id RUN                          # run-level summary
  iterion inspect --run-id RUN --events                 # run summary + events
  iterion inspect --run-id RUN --list-nodes             # one row per execution
  iterion inspect --run-id RUN --node analyze           # full node report
  iterion inspect --run-id RUN --node analyze --section trace
  iterion inspect --run-id RUN --exec exec:main:analyze:0`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := cli.InspectOptions{
			RunID:       inspectOpts.runID,
			StoreDir:    inspectOpts.storeDir,
			Events:      inspectOpts.events,
			Full:        inspectOpts.full,
			Node:        inspectOpts.node,
			Branch:      inspectOpts.branch,
			ExecutionID: inspectOpts.executionID,
			Section:     cli.InspectSection(inspectOpts.section),
			LogTail:     inspectOpts.logTail,
			ListNodes:   inspectOpts.listNodes,
		}
		if cmd.Flags().Changed("iteration") {
			v := inspectOpts.iteration
			opts.Iteration = &v
		}
		return cli.RunInspect(opts, newPrinter())
	},
}

func init() {
	f := inspectCmd.Flags()
	f.StringVar(&inspectOpts.runID, "run-id", "", "Run to inspect (omit to list all)")
	f.StringVar(&inspectOpts.storeDir, "store-dir", "", "Store directory (default: .iterion)")
	f.BoolVar(&inspectOpts.events, "events", false, "Show event log (run-level mode)")
	f.BoolVar(&inspectOpts.full, "full", false, "Show all details (run-level mode)")

	// Per-node selection — additive, mutually validated in RunInspect.
	f.StringVar(&inspectOpts.node, "node", "", "Focus on a specific IR node (returns a node-scoped report)")
	f.StringVar(&inspectOpts.branch, "branch", "", "Optional branch_id when --node is ambiguous (default: 'main')")
	f.IntVar(&inspectOpts.iteration, "iteration", 0, "0-based loop iteration of --node (-1 = latest started)")
	f.StringVar(&inspectOpts.executionID, "exec", "", "Execution ID (exec:<branch>:<node>:<iter>); alternative to --node")
	f.StringVar(&inspectOpts.section, "section", "", "Restrict node report to one section: summary|events|trace|tools|artifacts|interactions|log|all")
	f.IntVar(&inspectOpts.logTail, "log-tail", 0, "Cap the log slice in bytes (0 = uncapped)")
	f.BoolVar(&inspectOpts.listNodes, "list-nodes", false, "List node executions (one row per branch × iteration)")

	rootCmd.AddCommand(inspectCmd)
}
