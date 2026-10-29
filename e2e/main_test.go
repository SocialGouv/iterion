package e2e

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/proctest"
	"github.com/SocialGouv/iterion/internal/testparallel"
)

// e2eParallelCap bounds how many of this package's tests run at once.
//
// These tests are not CPU-bound: nearly every one waits on a background loop
// (a dispatcher poll, an engine branch scheduler, a subprocess) to reach a
// state within a wall-clock budget. Oversubscribing the machine starves those
// loops, so past a point MORE parallelism is both slower and flaky. Measured
// on a 32-core box, 2026-09-07, over the whole package:
//
//	serial               425.6 s   green
//	-parallel 8          100.7 s   green
//	-parallel 32 (= GOMAXPROCS)  204.7 s   two await-answers rows timed out
//
// 8 is where the curve flattens. A GitHub-hosted runner (GOMAXPROCS = 4)
// stays under it; the organisation's self-hosted runners, which run `test`
// and `race` with no CPU limit, see all their node's CPUs (8 or 16 measured)
// and run at the cap.
const e2eParallelCap = 8

// ITERION_E2E_PARALLEL overrides the cap. TestMain runs before flag.Parse
// (the testing package documents this contract), so capParallelism always
// sees the flag at its registered default and caps it; an explicit
// `-parallel` then wins because the parse inside m.Run overwrites whatever
// the cap set.
const e2eParallelEnv = "ITERION_E2E_PARALLEL"

// The suite runs out of the developer's own checkout, so an engine built
// without WithWorkDir hands `worktree: auto` (the IR default) that repository
// as its source: `git worktree add` registers the run's worktree THERE while
// the checkout lives under t.TempDir() and is deleted on return. Measured at
// 1 773 dead entries / 2.5 GB after two days of parallel agent work (#870),
// and 66 more per full `go test ./...`. gittest.NoWorktreeLeaks fails the
// package when that count moves, and names the test that moved it.
// hometest keeps the suite off the operator's iterion home, and the
// directory it owns doubles as processScratch.
func TestMain(m *testing.M) {
	capParallelism()
	os.Exit(hometest.Isolate(func() int {
		processScratch = os.Getenv("ITERION_HOME")
		return proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) })
	}))
}

// capParallelism lowers `-parallel` to e2eParallelCap when the operator left
// it at its default. Called from TestMain, so it lands before m.Run builds
// the test context that reads the flag.
func capParallelism() { testparallel.Cap(e2eParallelEnv, e2eParallelCap) }
