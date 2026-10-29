package model

import (
	"flag"
	"runtime"
	"strconv"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// modelParallelCap bounds how many of this package's tests may run at once.
//
// This package is not subprocess-bound the way `bots` is (18 time.Sleep call
// sites and 15 files using exec.Command, out of 215 test files): its 1051 s
// under -race is compile and CPU, so parallelism is the right lever and the
// cap is about -race RESIDENCY, not about starving children.
//
// MEASURED on a 32-thread workstation, 2026-10-29, over the whole package
// under -race (`go test -race -mod=vendor -count=1 -parallel N`, readings in
// fleet/logs/vagues50-race-calibration.log):
//
//	-parallel 1	657.8 s	1.37 GB peak RSS
//	-parallel 8	328.4 s	2.12 GB
//	-parallel 32	280.0 s	2.26 GB	(= GOMAXPROCS here)
//
// 8 takes 87% of the achievable speedup (329 s of 378 s) for 92% of the
// residency 32 costs, and residency is what binds on CI rather than on a
// workstation: the `race` leg runs `go test $pkgs` with no -p, so up to
// GOMAXPROCS package BINARIES share one runner, each of which would otherwise
// widen to GOMAXPROCS tests of its own. ITERION_MODEL_PARALLEL overrides it,
// and the campaign that grows this package's t.Parallel() population
// recalibrates it.
const modelParallelCap = 8

// ITERION_MODEL_PARALLEL overrides the cap. TestMain runs before flag.Parse
// (the testing package documents this contract), so capParallelism always
// sees the flag at its registered default and caps it; an explicit
// `-parallel` then wins because the parse inside m.Run overwrites whatever
// the cap set.
const modelParallelEnv = "ITERION_MODEL_PARALLEL"

// capParallelism lowers `-parallel` to modelParallelCap when the operator left
// it at its default. Called from TestMain (modelspecs_merge_test.go), so it
// lands before m.Run builds the test context that reads the flag.
func capParallelism() { testparallel.Cap(modelParallelEnv, modelParallelCap) }

// TestTheParallelCapBindsThisPackage pins the three answers of the cap where
// it is wired: it lowers an untouched default, it leaves an explicit
// `-parallel` alone, and the env var replaces the number. Without it the cap
// could silently become a no-op and this package would go back to
// oversubscribing the runner under -race, which is what it exists to prevent.
//
// Serial: it mutates the process-wide `test.parallel` flag.
func TestTheParallelCapBindsThisPackage(t *testing.T) {
	f := flag.Lookup("test.parallel")
	if f == nil {
		t.Fatal("test.parallel is gone — capParallelism is a no-op and the cap no longer applies")
	}
	restore := f.Value.String()
	t.Cleanup(func() {
		if err := f.Value.Set(restore); err != nil {
			t.Fatalf("restore test.parallel=%s: %v", restore, err)
		}
	})
	set := func(v string) {
		t.Helper()
		if err := f.Value.Set(v); err != nil {
			t.Fatalf("set test.parallel=%s: %v", v, err)
		}
	}
	dflt := strconv.Itoa(runtime.GOMAXPROCS(0))

	set(dflt)
	capParallelism()
	want := dflt
	if runtime.GOMAXPROCS(0) > modelParallelCap {
		want = strconv.Itoa(modelParallelCap)
	}
	if got := f.Value.String(); got != want {
		t.Errorf("default %s capped to %s, want %s", dflt, got, want)
	}

	explicit := strconv.Itoa(runtime.GOMAXPROCS(0) + 7)
	set(explicit)
	capParallelism()
	if got := f.Value.String(); got != explicit {
		t.Errorf("explicit -parallel %s became %s — the cap overrode an operator's choice", explicit, got)
	}

	t.Setenv(modelParallelEnv, "3")
	set(dflt)
	capParallelism()
	want = "3"
	if runtime.GOMAXPROCS(0) <= 3 {
		want = dflt
	}
	if got := f.Value.String(); got != want {
		t.Errorf("%s=3 gave %s, want %s", modelParallelEnv, got, want)
	}
}
