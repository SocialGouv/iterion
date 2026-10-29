package bots

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// botsParallelCap bounds how many of this package's tests may run at once.
//
// These tests are not CPU-bound in the ordinary sense: most of them wait on
// real subprocesses (throwaway git repos, python harnesses extracted from
// compiled IR and inline sh, the pinned ultra11y node engine), and -race
// multiplies the memory each one holds. Oversubscribing the node starves
// those children and inflates residency. 8 is the width the sibling e2e
// suite converged on for the same shape of load; the campaign that grows
// this package's t.Parallel() population recalibrates it. The cap already
// binds the parallel instances that exist today (the delivery-probe fault
// matrix), which pay a slower matrix until that calibration lands.
const botsParallelCap = 8

// ITERION_BOTS_PARALLEL overrides the cap.
//
// The ordering that makes this work: TestMain runs BEFORE flag.Parse (the
// testing package documents this contract), so capParallelism always sees
// the flag at its registered default and caps it; an explicit `-parallel`
// then wins because the parse inside m.Run overwrites whatever the cap set.
// Two consequences worth knowing: the default-value guard inside
// capParallelism never discriminates today, and a value equal to GOMAXPROCS
// is indistinguishable from the default — if a Go release ever parses the
// flag before TestMain, revisit this cap before trusting it.
const botsParallelEnv = "ITERION_BOTS_PARALLEL"

// The git-exec tests in this package (dep-update-guard, branch-improve
// gate, campaign gate commands, docs-refresh scope…) create throwaway
// repos and commit in them. The operator's global/system git config must
// not leak into those repos: commit.gpgsign=true hangs every test commit
// on a pinentry that has no TTY, which reads as a 600s package timeout
// with no failing test named. CI never sees it (runners sign nothing) —
// only contributors do.
func TestMain(m *testing.M) {
	capParallelism()
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(m.Run())
}

// capParallelism lowers `-parallel` to botsParallelCap when the operator left
// it at its default. Called from TestMain, so it lands before m.Run builds
// the test context that reads the flag.
func capParallelism() { testparallel.Cap(botsParallelEnv, botsParallelCap) }
