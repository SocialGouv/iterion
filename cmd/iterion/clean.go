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

Not age, and not run status alone — where the commits live:

  orphan            the directory is no longer a git worktree
  landed-elsewhere  HEAD is contained by a ref other than its own branch
  own-branch        HEAD is contained only by the branch it checked out
  unlanded          no ref contains HEAD

TWO GUARDS NO LEVEL LIFTS

  1. A run that is not terminal keeps its worktree. Checked against run
     status, never mtime: a run can spend hours in one agent turn without
     touching its worktree, and age alone would call it abandoned.
  2. An 'unlanded' worktree is never deleted. Its commits would survive
     only in the reflog, which expires. Recovering or discarding that work
     is a decision for the operator and for git, not for a sweep.

<store-dir>/worktrees/.state and any other dot-prefixed entry is left
alone: it holds gate state shared across runs, not one run's checkout.

LEVELS (cumulative)

  conservative  orphan + landed-elsewhere with a clean tree. Nothing that
                could be work is touched. This is the default.
  moderate      + landed-elsewhere carrying uncommitted files. The commits
                survive on the other ref; uncommitted files do not.
  aggressive    + own-branch. No commit is lost — the branch is a ref in
                the parent repository and outlives the directory — but
                nothing else references that work yet.

Dry run by default: the command prints what it would delete and deletes
nothing until --apply. After deleting it runs 'git worktree prune' in each
affected repository, so no stale registration is left behind.

Examples:
  iterion clean                                  # dry run, conservative, this project
  iterion clean --apply                          # execute it
  iterion clean --all-projects                   # every project store under the iterion home
  iterion clean --older-than 720h --apply        # only worktrees idle 30 days or more
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
	f.StringVar(&cleanOpts.olderThan, "older-than", "168h", "Only worktrees untouched for at least this Go duration (168h = 7d)")
	f.BoolVar(&cleanOpts.apply, "apply", false, "Actually delete; without it the command is a dry run")
	f.BoolVar(&cleanOpts.allProjects, "all-projects", false, "Sweep every project store under the iterion data dir, not just this project's")
	f.IntVar(&cleanOpts.keepLast, "keep-last", 0, "Always keep the N most recent otherwise-eligible worktrees")
	f.BoolVar(&cleanOpts.withRuns, "with-runs", false, "Also delete the run record paired with each deleted worktree")

	rootCmd.AddCommand(cleanCmd)
}
