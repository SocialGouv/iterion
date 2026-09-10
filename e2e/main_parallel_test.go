package e2e

import (
	"flag"
	"runtime"
	"strconv"
	"testing"
)

// TestCapParallelism pins the three answers of the cap: it lowers an
// untouched default, it leaves an explicit `-parallel` alone, and the env
// var replaces the number. Without this the cap could silently become a
// no-op (a Go release renaming the flag, say) and the suite would go back to
// oversubscribing the machine — slower AND flaky, which is what it exists to
// prevent.
//
// Serial: it mutates the process-wide `test.parallel` flag.
func TestCapParallelism(t *testing.T) {
	f := flag.Lookup("test.parallel")
	if f == nil {
		t.Fatal("test.parallel is gone — capParallelism is a no-op and the cap no longer applies")
	}
	restore := f.Value.String()
	t.Cleanup(func() {
		if err := f.Value.Set(restore); err != nil {
			t.Fatalf("restore test.parallel: %v", err)
		}
	})
	dflt := strconv.Itoa(runtime.GOMAXPROCS(0))

	set := func(v string) {
		t.Helper()
		if err := f.Value.Set(v); err != nil {
			t.Fatalf("set test.parallel=%s: %v", v, err)
		}
	}

	// The default gets capped — unless the machine is already smaller than
	// the cap, in which case the value is left where it is.
	set(dflt)
	capParallelism()
	want := dflt
	if runtime.GOMAXPROCS(0) > e2eParallelCap {
		want = strconv.Itoa(e2eParallelCap)
	}
	if got := f.Value.String(); got != want {
		t.Errorf("default %s capped to %s, want %s", dflt, got, want)
	}

	// An explicit choice is not overridden, even a large one.
	explicit := strconv.Itoa(runtime.GOMAXPROCS(0) + 7)
	set(explicit)
	capParallelism()
	if got := f.Value.String(); got != explicit {
		t.Errorf("explicit -parallel %s became %s — the cap overrode an operator's choice", explicit, got)
	}

	// The env var replaces the cap for the default case.
	t.Setenv(e2eParallelEnv, "3")
	set(dflt)
	capParallelism()
	want = "3"
	if runtime.GOMAXPROCS(0) <= 3 {
		want = dflt
	}
	if got := f.Value.String(); got != want {
		t.Errorf("%s=3 gave %s, want %s", e2eParallelEnv, got, want)
	}
}
