package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "Manage runs on the local store",
	Long: `Manage runs persisted under <store-dir>/runs/.

Subcommands:
  prune   Delete old runs (retention for schedule/dispatcher-driven runs)
`,
}

var runsPruneOpts struct {
	storeDir  string
	olderThan string
	keepLast  int
	statuses  string
	dryRun    bool
}

var runsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete old runs matching status + age filters",
	Long: `Delete runs from the local store that are older than a duration
and whose status is prunable (finished / failed / cancelled by default;
opt in to failed_resumable explicitly).

The local run store has no built-in retention: every ` + "`iterion run`" + `,
scheduled bot, or dispatcher-launched workflow persists forever under
<store-dir>/runs/<run_id>/. Pair this command with recurring bots to
cap disk usage.

Only run directories under <store-dir>/runs/ are removed. The command
never touches <store-dir>/worktrees/ or any other subtree of the store.

Examples:
  iterion runs prune                                        # default: delete finished/failed/cancelled older than 30 days
  iterion runs prune --dry-run                              # preview what would be deleted; delete nothing
  iterion runs prune --older-than 168h                      # keep the last week
  iterion runs prune --keep-last 100                        # always keep the 100 most recent matching runs
  iterion runs prune --status finished                      # only finished runs (skip failed / cancelled)
  iterion runs prune --status finished,failed,cancelled,failed_resumable  # explicit opt-in to failed_resumable
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		olderThan, err := time.ParseDuration(runsPruneOpts.olderThan)
		if err != nil {
			return cli.UserInputError(fmt.Errorf("--older-than: %w", err))
		}
		var statuses []string
		if s := strings.TrimSpace(runsPruneOpts.statuses); s != "" {
			statuses = strings.Split(s, ",")
		}
		return cli.RunPrune(cli.PruneOptions{
			StoreDir:  runsPruneOpts.storeDir,
			OlderThan: olderThan,
			KeepLast:  runsPruneOpts.keepLast,
			Statuses:  statuses,
			DryRun:    runsPruneOpts.dryRun,
		}, newPrinter())
	},
}

func init() {
	f := runsPruneCmd.Flags()
	f.StringVar(&runsPruneOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	f.StringVar(&runsPruneOpts.olderThan, "older-than", "720h", "Prune runs older than this Go duration (e.g. 168h = 7d, 720h = 30d)")
	f.IntVar(&runsPruneOpts.keepLast, "keep-last", 0, "Always keep the N most recent matching runs (default 0 = keep none extra)")
	f.StringVar(&runsPruneOpts.statuses, "status", "", "Comma-separated statuses to prune (default: finished,failed,cancelled; allowed additionally: failed_resumable)")
	f.BoolVar(&runsPruneOpts.dryRun, "dry-run", false, "List runs that would be pruned without deleting them")

	runsCmd.AddCommand(runsPruneCmd)
	rootCmd.AddCommand(runsCmd)
}
