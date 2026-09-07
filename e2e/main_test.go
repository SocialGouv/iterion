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

// ITERION_E2E_PARALLEL overrides the cap; an explicit `-parallel` on the
// command line wins over both (the cap only applies when the flag still
// holds its GOMAXPROCS default, i.e. nobody chose).
const e2eParallelEnv = "ITERION_E2E_PARALLEL"

func TestMain(m *testing.M) {
	capParallelism()
	os.Exit(m.Run())
}

// capParallelism lowers `-parallel` to e2eParallelCap when the operator left
// it at its default. Called from TestMain, so it lands before m.Run builds
// the test context that reads the flag.
func capParallelism() {
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
	// Only when the flag still carries its default: `-parallel N` is an
	// explicit choice and stays untouched.
	if f.Value.String() != strconv.Itoa(runtime.GOMAXPROCS(0)) {
		return
	}
	if current, err := strconv.Atoi(f.Value.String()); err == nil && current <= limit {
		return
	}
	if err := f.Value.Set(strconv.Itoa(limit)); err != nil {
		panic("cap test.parallel: " + err.Error())
	}
}
