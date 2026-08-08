package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

var rewindOpts struct {
	runID        string
	nodeID       string
	auto         bool
	keepFiles    bool
	restoreScope string
	listSnaps    bool
	restoreID    string
	file         string
	storeDir     string
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

The workspace is also restored to the state the pivot started from — a docs or
code bot's real product is its files, not its output map. A run with an
isolated worktree (worktree: auto) is reverted through git; an in-place run —
the default shape, where the workspace is YOUR live checkout — is reverted
through iterion's own workspace versioning.

--restore-scope sets how much of it comes back:

  produced  only the paths this run recorded changing after the pivot started.
            The default for an in-place run: a rewind is launched right after
            you edit files, so putting the whole tree back would revert your
            own work — including the edit the rewind exists to test.
  full      every versioned path in the snapshot. The default for a worktree
            run, whose tree iterion owns. ("Versioned", not "the whole disk":
            ignored, protected and never-captured paths are untouched.)
            It is also the ONLY breadth available there: git reverts the whole
            tree or none of it, so "produced" is refused rather than widened.
  none      leave the workspace alone; replay the node against the tree as it
            stands. (--keep-files is the older spelling of this.)

Under "produced", two things iterion cannot attribute are REPORTED rather than
guessed at: paths it overwrote that had changed after the run stopped recording
(almost always yours), and paths it left in place that changed since the run's
last recorded boundary — which may be the partial output of a node that died
before that boundary was written, or may be your editor.

Either way the state you had is banked first, so nothing becomes unreachable:
recover it with --list-snapshots / --restore-snapshot in place, or with the
printed backup ref in a worktree. When no boundary was recorded for the pivot,
or nothing is recorded as having changed after it, the rewind says so rather
than silently leaving — or silently reverting — the files.

External effects (board cards, forge comments, pushed commits, already-launched
subbot runs) are never undone.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if rewindOpts.runID == "" {
			return fmt.Errorf("--run-id is required")
		}
		if rewindOpts.listSnaps || rewindOpts.restoreID != "" {
			return runWorkspaceSnapshotCmd(cmd, rewindOpts.runID)
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
		scope, err := runview.ParseRestoreScope(rewindOpts.restoreScope)
		if err != nil {
			return err
		}
		result, err := svc.Rewind(ctx, runview.RewindSpec{
			RunID:        rewindOpts.runID,
			NodeID:       rewindOpts.nodeID,
			Auto:         rewindOpts.auto,
			KeepFiles:    rewindOpts.keepFiles,
			RestoreScope: scope,
			SourcePath:   rewindOpts.file,
		})
		if err != nil {
			return fmt.Errorf("rewind: %w", err)
		}
		p := newPrinter()
		if jsonOutput {
			// Gated, like the snapshot listing below: the result now
			// carries the restore's per-path accounting, so an ungated
			// dump puts a workspace listing on stdout and scrolls the
			// warnings — the part that matters — off the screen.
			p.JSON(result)
		}
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
		printRewindFiles(os.Stderr, result)
		for _, id := range result.OrphanedChildRuns {
			fmt.Fprintf(os.Stderr, "  released child run %s (still alive — cancel it if it is burning budget)\n", id)
		}
		fmt.Fprintf(os.Stderr, "resume with: iterion resume --run-id %s%s\n",
			result.RunID, forceHintFor(rewindOpts.file))
		return nil
	},
}

// printRewindFiles reports the workspace half of a rewind.
//
// The ticket that produced the scoping work asked for exactly this: a
// rewind writes to the operator's live checkout, so it must say how many
// paths it touched and which — a one-line "workspace reverted" is not
// enough to notice that 38 files just moved. The two warning sets go
// LAST, closest to the prompt, and are capped harder than the inventory:
// above a handful of paths people read counts, not lists, and the part
// that must survive the scroll is what was taken.
func printRewindFiles(w io.Writer, result *runview.RewindResult) {
	f := result.Files
	if f == nil {
		return
	}
	if !f.Reverted {
		if f.SkipReason != "" {
			fmt.Fprintf(w, "  workspace NOT restored — %s\n", f.SkipReason)
		}
		printRewindLeftInPlace(w, f)
		return
	}
	fmt.Fprintf(w, "  workspace reverted to the state %q started from (backup: %s)\n", result.NodeID, f.BackupRef)
	if f.Scope == string(runview.RestoreScopeProduced) {
		fmt.Fprintf(w, "    scope: %d path(s) this run recorded changing after %q started\n", f.ScopeCount, result.NodeID)
	} else if f.Scope != "" {
		fmt.Fprintf(w, "    scope: %s — every versioned path in the snapshot\n", f.Scope)
	}
	if r := f.Restored; r != nil {
		fmt.Fprintf(w, "    %d written, %d deleted, %d unchanged\n", r.Written, r.Deleted, r.Unchanged)
		if len(r.WrittenPaths) > 0 {
			fmt.Fprintf(w, "      written: %s\n", joinCapped(r.WrittenPaths, r.Written, 8))
		}
		if len(r.DeletedPaths) > 0 {
			fmt.Fprintf(w, "      deleted: %s\n", joinCapped(r.DeletedPaths, r.Deleted, 8))
		}
	}
	if f.CoverageGap != "" {
		// A revert that ran but could not put everything back is not a
		// clean success — say so on the same screen.
		fmt.Fprintf(w, "    WARNING: %s\n", f.CoverageGap)
	}
	if f.OverwrittenCount > 0 {
		fmt.Fprintf(w, "  WARNING: %d path(s) had changed after this run last recorded its workspace and were overwritten:\n", f.OverwrittenCount)
		fmt.Fprintf(w, "      %s\n", joinCapped(f.Overwritten, f.OverwrittenCount, 5))
		fmt.Fprintf(w, "    recover them with: iterion rewind --run-id %s --restore-snapshot %s\n", result.RunID, f.BackupRef)
	}
	printRewindLeftInPlace(w, f)
}

func printRewindLeftInPlace(w io.Writer, f *runview.FileRevertResult) {
	if f.LeftInPlaceCount == 0 {
		return
	}
	fmt.Fprintf(w, "  NOTE: %d path(s) changed since this run last recorded its workspace and were left in place —\n", f.LeftInPlaceCount)
	fmt.Fprintf(w, "        iterion cannot tell a failed node's partial output from your own edits, so it reports them:\n")
	fmt.Fprintf(w, "      %s\n", joinCapped(f.LeftInPlace, f.LeftInPlaceCount, 5))
}

// joinCapped renders at most n of paths, naming the remainder from the
// exact total rather than from the (already capped) slice length.
func joinCapped(paths []string, total, n int) string {
	if len(paths) > n {
		paths = paths[:n]
	}
	out := strings.Join(paths, ", ")
	if rest := total - len(paths); rest > 0 {
		out += fmt.Sprintf(" (+%d more)", rest)
	}
	return out
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
	f.StringVar(&rewindOpts.restoreScope, "restore-scope", "",
		"How much of the workspace to put back: none | produced | full (default: produced in place, full in a worktree)")
	f.BoolVar(&rewindOpts.keepFiles, "keep-files", false, "Deprecated alias for --restore-scope none")
	_ = f.MarkDeprecated("keep-files", "use --restore-scope none")
	f.BoolVar(&rewindOpts.listSnaps, "list-snapshots", false, "List the recoverable workspace states of this run, newest first")
	f.StringVar(&rewindOpts.restoreID, "restore-snapshot", "", "Put the workspace back to a snapshot id (undoes a rewind's file restore)")
	f.StringVar(&rewindOpts.file, "file", "", "Workflow source to read the graph from (default: the run's persisted source)")
	f.StringVar(&rewindOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	mustMarkRequired(rewindCmd, "run-id")
	rootCmd.AddCommand(rewindCmd)
}

// runWorkspaceSnapshotCmd serves --list-snapshots / --restore-snapshot.
//
// A rewind banks the workspace before restoring it, but until now nothing
// could consume the id it printed: the bytes were on disk and out of
// reach. These are the way back for an operator whose own edits were
// swept up by a restore.
func runWorkspaceSnapshotCmd(cmd *cobra.Command, runID string) error {
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
	p := newPrinter()

	if rewindOpts.restoreID != "" {
		report, err := svc.RestoreWorkspaceSnapshot(cmd.Context(), runID, rewindOpts.restoreID)
		if err != nil {
			return fmt.Errorf("restore snapshot: %w", err)
		}
		if jsonOutput {
			p.JSON(report)
		}
		fmt.Fprintf(os.Stderr, "workspace restored to %s: %d written, %d deleted, %d unchanged\n",
			rewindOpts.restoreID, report.Written, report.Deleted, report.Unchanged)
		// Deliberately unscoped, and the asymmetry has to be said out
		// loud. A rewind narrows itself to what the run recorded; this
		// puts the WHOLE snapshot back, because "recover my workspace"
		// cannot be served by a partial answer. The current state was
		// banked first, so this is itself undoable — but an operator who
		// just read "12 paths restored" will not assume this one is
		// bigger unless told.
		fmt.Fprintf(os.Stderr,
			"  note: --restore-snapshot puts the ENTIRE snapshot back, not only what a rewind touched;\n"+
				"        the state you had a moment ago was banked first (see --list-snapshots)\n")
		return nil
	}

	snaps, err := svc.ListWorkspaceSnapshots(cmd.Context(), runID)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	if jsonOutput {
		// Every Entry (path + hash + mode + size) of every snapshot in the
		// chain — on a real repo that is ~10k paths per capture times the
		// number of node boundaries, i.e. hundreds of MB. Machine
		// consumers only; the human summary goes to stderr below. Printer
		// .JSON writes to stdout regardless of Format, so the gate has to
		// be here, as every other command in the tree does it.
		p.JSON(snaps)
	}
	// Print EVERY label a snapshot carries, not just the one its manifest
	// was created under. Capture dedupes against an identical parent and
	// returns the parent, so a rewind's bank routinely lands as a second
	// label on an existing snapshot — and listing only the manifest label
	// then shows rows named pre:/post: and not one saying rewind-backup,
	// leaving an operator in a panic unable to tell which id is their
	// bank. That bank is the whole recovery story.
	byID := map[string][]string{}
	if tr := svc.WorkspaceTracker(); tr != nil {
		for label, id := range tr.Labels(runID) {
			byID[id] = append(byID[id], label)
		}
		for _, labels := range byID {
			sort.Strings(labels)
		}
	}
	for _, s := range snaps {
		label := s.Label
		if all := byID[s.ID]; len(all) > 0 {
			label = strings.Join(all, " ")
		}
		fmt.Fprintf(os.Stderr, "%s  %-40s  %d files  %s\n",
			s.ID, label, len(s.Entries), s.CreatedAt.Format("15:04:05"))
	}
	if len(snaps) == 0 {
		fmt.Fprintf(os.Stderr, "no workspace snapshots for %s\n", runID)
	}
	return nil
}
