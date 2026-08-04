package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

var rewindOpts struct {
	runID     string
	nodeID    string
	auto      bool
	keepFiles bool
	file      string
	storeDir  string
}

var rewindCmd = &cobra.Command{
	Use:   "rewind",
	Short: "Re-anchor a run on an earlier node and invalidate everything downstream (same run id)",
	Long: `Rewind moves an existing run's checkpoint back to an already-executed node
and drops the outputs of every node downstream of it, so the next resume
re-executes from there instead of replaying the whole workflow.

This is the loop for iterating on a bot's configuration: run it, see a node
misbehave, fix the prompt/schema/edges in the .bot, rewind to that node, and
resume — the upstream nodes you already paid for are kept.

  # edit main.bot, then let iterion locate the edit:
  iterion rewind --run-id RUN --auto
  iterion resume --run-id RUN --force

  # or target the pivot yourself:
  iterion rewind --run-id RUN --node implement

Rewind mutates THIS run — same id, same name, same lineage. Use 'iterion fork'
instead when you want an alternative future in a new run with the original
left intact.

Downstream means forward-reachable from the pivot minus anything that can
reach it back, so a loop's earlier stages survive for {{loop.*.previous_output}}.
Budget accounting and loop counters are NOT refunded (raise them on resume with
--max-cost-usd / --max-iterations).

For a run with an isolated worktree (worktree: auto) the workspace is also
restored to the state the pivot started from — a docs or code bot's real
product is its files, not its output map. The restore is a revert: the prior
tree is committed on top of HEAD and the current state is banked under a
backup ref first, so nothing becomes unreachable. --keep-files opts out, and a
run without a worktree says so rather than silently leaving the files.

External effects (board cards, forge comments, pushed commits, already-launched
subbot runs) are never undone.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if rewindOpts.runID == "" {
			return fmt.Errorf("--run-id is required")
		}
		if rewindOpts.nodeID == "" && !rewindOpts.auto {
			return fmt.Errorf("--node is required (or --auto to derive it from your edit)")
		}
		ctx := cmd.Context()
		storeRoot := rewindOpts.storeDir
		if storeRoot == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("resolve cwd: %w", err)
			}
			storeRoot = store.ResolveStoreDir(cwd, "")
		}
		absStore, err := filepath.Abs(storeRoot)
		if err != nil {
			return fmt.Errorf("resolve store dir: %w", err)
		}
		svc, err := runview.NewService(absStore)
		if err != nil {
			return fmt.Errorf("open service: %w", err)
		}
		result, err := svc.Rewind(ctx, runview.RewindSpec{
			RunID:      rewindOpts.runID,
			NodeID:     rewindOpts.nodeID,
			Auto:       rewindOpts.auto,
			KeepFiles:  rewindOpts.keepFiles,
			SourcePath: rewindOpts.file,
		})
		if err != nil {
			return fmt.Errorf("rewind: %w", err)
		}
		p := newPrinter()
		p.JSON(result)
		if result.AutoTargeted {
			for _, c := range result.Changes {
				fmt.Fprintf(os.Stderr, "  changed: %s\n", c.String())
			}
		}
		if result.PromotedFrom != "" {
			fmt.Fprintf(os.Stderr, "  %q is inside a fan-out — anchored on its router %q so every branch replays\n",
				result.PromotedFrom, result.NodeID)
		}
		fmt.Fprintf(os.Stderr, "rewound %s to %q (dropped: %s)\n",
			result.RunID, result.NodeID, strings.Join(result.DroppedNodes, ", "))
		if f := result.Files; f != nil {
			if f.Reverted {
				fmt.Fprintf(os.Stderr, "  workspace reverted to the state %q started from (backup: %s)\n", result.NodeID, f.BackupRef)
			} else if f.SkipReason != "" {
				fmt.Fprintf(os.Stderr, "  WARNING: files NOT reverted — %s\n", f.SkipReason)
			}
		}
		for _, id := range result.OrphanedChildRuns {
			fmt.Fprintf(os.Stderr, "  released child run %s (still alive — cancel it if it is burning budget)\n", id)
		}
		fmt.Fprintf(os.Stderr, "resume with: iterion resume --run-id %s%s\n",
			result.RunID, forceHintFor(rewindOpts.file))
		return nil
	},
}

// forceHintFor nudges toward --force, since the overwhelmingly common
// reason to rewind is that the .bot was (or is about to be) edited, and
// resume refuses a source-hash mismatch without it.
func forceHintFor(file string) string {
	if file != "" {
		return " --file " + file + " --force"
	}
	return " --force   # --force needed once the .bot source has changed"
}

func init() {
	f := rewindCmd.Flags()
	f.StringVar(&rewindOpts.runID, "run-id", "", "Run to rewind (mutated in place)")
	f.StringVar(&rewindOpts.nodeID, "node", "", "Pivot node id (the node the run re-executes from)")
	f.BoolVar(&rewindOpts.auto, "auto", false, "Derive the pivot by diffing your edited .bot against the source this run executed")
	f.BoolVar(&rewindOpts.keepFiles, "keep-files", false, "Do not restore the workspace to the state the pivot started from")
	f.StringVar(&rewindOpts.file, "file", "", "Workflow source to read the graph from (default: the run's persisted source)")
	f.StringVar(&rewindOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	mustMarkRequired(rewindCmd, "run-id")
	rootCmd.AddCommand(rewindCmd)
}
