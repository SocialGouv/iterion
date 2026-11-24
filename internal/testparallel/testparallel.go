// Package testparallel bounds how wide a test binary runs its parallel tests,
// from TestMain.
//
// Several packages of this tree cap `-parallel` for the same reason: their
// tests either hold real subprocesses (throwaway git repos, sandboxed tool
// nodes, python harnesses) or multiply their residency under -race, so letting
// `-parallel` follow GOMAXPROCS on a 32-thread runner starves the node instead
// of using it. The cap lives here so the next package joining the campaign
// implements the seam rather than copying twenty lines of flag handling — the
// third copy is where a recopied guard drifts.
package testparallel

import (
	"flag"
	"os"
	"runtime"
	"strconv"
)

// Cap lowers the `test.parallel` flag to limit when the operator left it at
// its registered default. env names an environment variable that replaces
// limit; an unusable value there panics rather than falling back to a number
// nobody asked for.
//
// The ordering that makes this work: TestMain runs BEFORE flag.Parse (the
// testing package documents this contract), so Cap always sees the flag at its
// registered default and caps it; an explicit `-parallel` then wins because
// the parse inside m.Run overwrites whatever the cap set. Two consequences
// worth knowing: the default-value guard below never discriminates today, and
// a value equal to GOMAXPROCS is indistinguishable from the default — if a Go
// release ever parses the flag before TestMain, revisit every caller's cap
// before trusting it.
func Cap(env string, limit int) {
	if limit < 1 {
		panic("testparallel.Cap: want a positive limit, got " + strconv.Itoa(limit))
	}
	f := flag.Lookup("test.parallel")
	if f == nil {
		return
	}
	if v := os.Getenv(env); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			// An unusable value is a mistake worth seeing, not a silent
			// fallback to a number the operator did not ask for.
			panic(env + ": want a positive integer, got " + strconv.Quote(v))
		}
		limit = n
	}
	// Unreachable discrimination today: TestMain runs before flag.Parse, so
	// the flag is always at its registered default here. The check keeps the
	// function honest if that ordering ever changes — see the doc above.
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
