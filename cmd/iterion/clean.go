package main

import (
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/spf13/cobra"
)

var cleanOpts struct {
	storeDir    string
	level       string
	olderThan   string
	apply       bool
	allProjects bool
	keepLast    int
	withRuns    bool
}

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Reclaim disk by deleting run worktrees whose work has landed",
	Long: `Delete per-run git worktrees from the local store once their work is
safely reachable somewhere else.

A ` + "`worktree: auto`" + ` run executes in its own checkout under
<store-dir>/worktrees/<run-id>/. A successful run promotes its commits
onto a branch and removes that directory; a failed or interrupted one
deliberately leaves it behind for inspection — and never comes back to
collect it. Those leftovers are where a long-lived store's disk goes.
` + "`iterion runs prune`" + ` does not help: it only ever touches
<store-dir>/runs/.

WHAT DECIDES SAFETY

Not age, and not run status alone — what git can PROVE about the commits:

  merged       a ref whose tip is NOT this HEAD contains this HEAD, so
               another line of work was built on top of these commits
  own-branch   refs contain HEAD but every one points exactly AT it: they
               are labels keeping the commits alive, not work built upon
  unlanded     no ref contains HEAD, or git could not answer
  orphan       git cannot account for the directory at all
  nested-repo  the tree holds a repository of its own

Refs under refs/iterion/ — the per-run checkpoints iterion writes itself —
are consulted, because they do hold a run's commits alive; but they are
that run's own bookkeeping and are reaped with it, so containment by one of
them never means merged. Annotated tags are compared on the commit they
peel to: %(objectname) is the tag object's id and never the commit's, so a
release tag sitting on a HEAD would otherwise read as work built upon it.

Every git answer is refused unless git is talking about THIS directory.
Asked about a directory merely nested inside some repository — and the
project-local <repo>/.iterion/ store puts the whole worktree pool inside
the operator's checkout — git walks up and answers for the enclosing
repository. Its HEAD, its clean status and its refs would read as a
landed, clean worktree and delete whatever the directory actually held.

THREE GUARDS NO LEVEL LIFTS

  1. A run that is not terminal keeps its worktree. Checked against run
     status, never mtime: a run can spend hours in one agent turn without
     touching its worktree, and age alone would call it abandoned. The
     sweep takes the same per-run lock 'iterion run' and 'iterion resume'
     hold for a run's lifetime, and holds it across the deletion — the
     window a status re-read alone would leave open is not an instant but
     the whole removal, which on a real worktree runs for seconds.
  2. An 'unlanded' worktree is never deleted. Its commits would survive
     only in the reflog, which expires. Recovering or discarding that work
     is a decision for the operator and for git, not for a sweep.
  3. A worktree holding a repository of its own is never deleted — an
     initialised submodule, or a plain clone dropped inside it (a vendored
     checkout, a dependency's source kept beside the code that uses it).
     Its objects live under the directory, so containment in the outer
     repository proves nothing about them, and being gitignored the tree
     still reads clean. A submodule merely DECLARED and never initialised
     does not trigger this: 'git worktree add' never populates submodules,
     so that is their normal state and there is nothing there to lose.

Git must also be usable before any verdict is formed: a git missing from a
cron PATH, or a malformed config, would otherwise make every directory
unclassifiable at once. A git that answers but fails on one directory
yields 'unlanded', never 'orphan'.

What iterion mirrors into the worktree at run start does not count as the
run's uncommitted work: .claude/skills/, .claude/commands/, .claude/agents/,
.claude/.iterion-managed/ and .claude/settings.json — and nothing else.
Anything a run puts elsewhere under .claude/ is the run's, and reads as work.

Immediately before a deletion the whole verdict is derived again, because a
sweep runs for tens of seconds and the classification is a photograph. A
worktree whose HEAD moved, whose tree turned dirty, or which gained a
repository of its own is spared — asking only about the working tree would
miss a commit, which leaves it clean.

<store-dir>/worktrees/.state and any other dot-prefixed entry is left
alone: it holds gate state shared across runs, not one run's checkout.

LEVELS (cumulative)

  conservative  merged, with a clean tree. Git proves the commits are
                recoverable and nothing is uncommitted. This is the default.
  moderate      + merged carrying uncommitted files. The commits survive
                on the other ref; uncommitted files do not.
  aggressive    + own-branch, where no commit is lost (the ref is in the
                parent repository and outlives the directory) but nothing
                has adopted the work yet; and + orphan, where git cannot
                say what the directory holds — a checkout whose parent
                repository moved looks exactly like a stale leftover.

Gitignored content is deleted at every level: in a run worktree it is the
build output this command exists to reclaim. The count of gitignored paths
is reported per worktree, so it is visible before --apply.

Dry run by default: the command prints what it would delete and deletes
nothing until --apply. A deletion that fails does not abort the sweep: the
remaining worktrees are still processed and the report still printed,
because an aborted sweep strands what it already deleted with no record.

After each successful deletion the worktree's own registration is dropped
from the parent repository. 'git worktree prune' is deliberately NOT used:
it sweeps the whole repository and would also drop the registration of any
worktree merely absent at that instant — an operator's checkout on an
unmounted volume — discarding its index and its staged work.

--keep-last applies per store, so under --all-projects it keeps N of each
project's worktrees rather than N across the whole machine.

Examples:
  iterion clean                                  # dry run, conservative, this project
  iterion clean --apply                          # execute it
  iterion clean --all-projects                   # every project store under the iterion home
  iterion clean --older-than 720h --apply        # root dir untouched for 30 days (default 168h)
  iterion clean --level moderate --apply         # also drop uncommitted files of landed runs
  iterion clean --keep-last 10 --apply           # always leave the 10 most recent
  iterion clean --with-runs --apply              # delete each worktree's run record too
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		olderThan, err := time.ParseDuration(cleanOpts.olderThan)
		if err != nil {
			return cli.UserInputError(fmt.Errorf("--older-than: %w", err))
		}
		return cli.RunClean(cli.CleanOptions{
			StoreDir:    cleanOpts.storeDir,
			Level:       cli.CleanLevel(cleanOpts.level),
			OlderThan:   olderThan,
			Apply:       cleanOpts.apply,
			AllProjects: cleanOpts.allProjects,
			KeepLast:    cleanOpts.keepLast,
			WithRuns:    cleanOpts.withRuns,
		}, newPrinter())
	},
}

func init() {
	f := cleanCmd.Flags()
	f.StringVar(&cleanOpts.storeDir, "store-dir", "", "Store directory override (default: managed store for the working directory)")
	f.StringVar(&cleanOpts.level, "level", "conservative", "How much to reclaim: conservative, moderate, aggressive")
	f.StringVar(&cleanOpts.olderThan, "older-than", "168h", "Only worktrees whose root directory has not changed for at least this Go duration (168h = 7d)")
	f.BoolVar(&cleanOpts.apply, "apply", false, "Actually delete; without it the command is a dry run")
	f.BoolVar(&cleanOpts.allProjects, "all-projects", false, "Sweep every project store under the iterion data dir, not just this project's")
	f.IntVar(&cleanOpts.keepLast, "keep-last", 0, "Always keep the N most recent otherwise-eligible worktrees")
	f.BoolVar(&cleanOpts.withRuns, "with-runs", false, "Also delete the run record paired with each deleted worktree")

	rootCmd.AddCommand(cleanCmd)
}
