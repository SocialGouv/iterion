package testparallel

import (
	"flag"
	"runtime"
	"strconv"
	"testing"
)

// pinFlag hands back a setter for `test.parallel` and restores the flag when
// the test ends. Fatal, not Skip, when the flag is gone: a Cap that silently
// becomes a no-op is exactly the regression this package exists to prevent.
func pinFlag(t *testing.T) func(string) {
	t.Helper()
	f := flag.Lookup("test.parallel")
	if f == nil {
		t.Fatal("test.parallel is gone — Cap is a no-op and no caller's cap applies any more")
	}
	restore := f.Value.String()
	t.Cleanup(func() {
		if err := f.Value.Set(restore); err != nil {
			t.Fatalf("restore test.parallel=%s: %v", restore, err)
		}
	})
	return func(v string) {
		t.Helper()
		if err := f.Value.Set(v); err != nil {
			t.Fatalf("set test.parallel=%s: %v", v, err)
		}
	}
}

func parallelFlag(t *testing.T) string {
	t.Helper()
	return flag.Lookup("test.parallel").Value.String()
}

// Serial: it mutates the process-wide `test.parallel` flag.
func TestCapLowersAnUntouchedDefault(t *testing.T) {
	set := pinFlag(t)
	dflt := strconv.Itoa(runtime.GOMAXPROCS(0))
	set(dflt)

	Cap("ITERION_TESTPARALLEL_UNSET_IN_THIS_TEST", 4)

	want := dflt
	if runtime.GOMAXPROCS(0) > 4 {
		want = "4"
	}
	if got := parallelFlag(t); got != want {
		t.Errorf("default %s capped to %s, want %s", dflt, got, want)
	}
}

// Serial: it mutates the process-wide `test.parallel` flag.
func TestCapLeavesAnExplicitChoiceAlone(t *testing.T) {
	set := pinFlag(t)
	explicit := strconv.Itoa(runtime.GOMAXPROCS(0) + 7)
	set(explicit)

	Cap("ITERION_TESTPARALLEL_UNSET_IN_THIS_TEST", 4)

	if got := parallelFlag(t); got != explicit {
		t.Errorf("explicit -parallel %s became %s — the cap overrode an operator's choice", explicit, got)
	}
}

// Serial: it mutates the process-wide `test.parallel` flag and an env name.
func TestCapReadsItsEnvOverride(t *testing.T) {
	const env = "ITERION_TESTPARALLEL_OVERRIDE"
	set := pinFlag(t)
	t.Setenv(env, "3")
	dflt := strconv.Itoa(runtime.GOMAXPROCS(0))
	set(dflt)

	Cap(env, 16)

	want := "3"
	if runtime.GOMAXPROCS(0) <= 3 {
		want = dflt
	}
	if got := parallelFlag(t); got != want {
		t.Errorf("%s=3 gave %s, want %s", env, got, want)
	}
}

// An override nobody can honour is a mistake worth seeing: the operator asked
// for a width, and silently running at another one is the fallback this tree
// refuses.
//
// Serial: it mutates the process-wide `test.parallel` flag and an env name.
func TestCapPanicsOnAnUnusableOverride(t *testing.T) {
	const env = "ITERION_TESTPARALLEL_OVERRIDE"
	for _, bad := range []string{"abc", "0", "-1", "3.5", " 4"} {
		t.Run(bad, func(t *testing.T) {
			set := pinFlag(t)
			t.Setenv(env, bad)
			set(strconv.Itoa(runtime.GOMAXPROCS(0)))

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s=%q was accepted; the cap ran at a width nobody asked for", env, bad)
				}
				msg, _ := r.(string)
				if want := env + ": want a positive integer, got " + strconv.Quote(bad); msg != want {
					t.Errorf("panic = %q, want %q", msg, want)
				}
			}()
			Cap(env, 4)
		})
	}
}

// A caller's own mistake is just as loud: a non-positive cap would widen
// nothing and hide the typo.
func TestCapRefusesANonPositiveLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("Cap accepted limit=%d", limit)
				}
				msg, _ := r.(string)
				if want := "testparallel.Cap: want a positive limit, got " + strconv.Itoa(limit); msg != want {
					t.Errorf("panic = %q, want %q", msg, want)
				}
			}()
			Cap("ITERION_TESTPARALLEL_UNSET_IN_THIS_TEST", limit)
		})
	}
}
