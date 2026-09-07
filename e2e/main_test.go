package e2e

import (
	"flag"
	"os"
	"runtime"
	"strconv"
	"testing"
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
// 8 is where the curve flattens. CI's 4-vCPU runner never reaches the cap
// (GOMAXPROCS = 4 there), so this only bites on a developer's or a
// self-hosted machine.
const e2eParallelCap = 8

// ITERION_E2E_PARALLEL replaces the cap's NUMBER; an explicit `-parallel` on
// the command line wins over both. See capParallelism for WHY it wins — it is
// the flag-parsing order, not a test this code performs.
//
// It only ever lowers: capParallelism leaves a value already at or under the
// limit alone, so ITERION_E2E_PARALLEL=32 on a 16-core box still runs 16. Use
// `-parallel 32` to go above what the machine offers — that is the knob that
// raises, and this one is a ceiling.
const e2eParallelEnv = "ITERION_E2E_PARALLEL"

func TestMain(m *testing.M) {
	capParallelism()
	os.Exit(m.Run())
}

// capParallelismSawParsedFlags records whether flag.Parse had already run
// when capParallelism did. It is the load-bearing assumption of the whole
// mechanism, so TestCapParallelism asserts it rather than trusting it.
var capParallelismSawParsedFlags bool

// capParallelism lowers `-parallel` to e2eParallelCap when the operator left
// it at its default.
//
// It runs from TestMain, i.e. BEFORE testing calls flag.Parse inside m.Run
// (measured: `flag.Parsed()` is false here and test.parallel still reads
// GOMAXPROCS, whatever the command line says). So this does not override an
// explicit choice — it rewrites the DEFAULT, and a `-parallel N` on the
// command line is applied afterwards by that Parse and wins on its own.
//
// Which is why the "did the operator choose?" test below is gated on
// flag.Parsed(). Ungated it was worse than useless: pre-parse it compared the
// default against GOMAXPROCS and could only be equal, so it never fired as
// documented — except in the one case where the two disagree, GOMAXPROCS
// moving between testing.Init and here (Go 1.25+ updates it from the cgroup
// limit at runtime), where it silently SKIPPED the cap it exists to apply.
// Gated, it is what still holds if a future release ever parses earlier.
func capParallelism() {
	capParallelismSawParsedFlags = flag.Parsed()
	f := flag.Lookup("test.parallel")
	if f == nil {
		return
	}
	limit := e2eParallelCap
	if v := os.Getenv(e2eParallelEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			// An unusable value is a mistake worth seeing, not a silent
			// fallback to a number the operator did not ask for.
			panic(e2eParallelEnv + ": want a positive integer, got " + strconv.Quote(v))
		}
		limit = n
	}
	// Post-parse only: a value that is no longer the GOMAXPROCS default is
	// an operator's explicit `-parallel` and stays untouched. Pre-parse
	// (the production path) there is nothing to read yet, so skip it.
	if flag.Parsed() && f.Value.String() != strconv.Itoa(runtime.GOMAXPROCS(0)) {
		return
	}
	if current, err := strconv.Atoi(f.Value.String()); err == nil && current <= limit {
		return
	}
	if err := f.Value.Set(strconv.Itoa(limit)); err != nil {
		panic("cap test.parallel: " + err.Error())
	}
}
