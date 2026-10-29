package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestResolveWaitDelay_AnswersTheSiteDefaultUnlessTheOperatorSaysOtherwise
// pins the whole decision table of the one override, including what it says
// when it refuses a value. Each case gets its own warn-once and sink, so the
// warning is a fact under test and not a side effect of case order.
//
// Mutations seen red: drop the `raw == ""` arm (an unset variable answers 0
// and every site loses its bound); drop the `d < 0` arm (`-1s` is kept, and
// exec's timer fires at once — the opposite of widening); return `def`
// instead of 0 for "off" (the escape hatch stops existing); drop the
// warnOnce.Do around the malformed arm (a refused override says nothing).
func TestResolveWaitDelay_AnswersTheSiteDefaultUnlessTheOperatorSaysOtherwise(t *testing.T) {
	const def = 2 * time.Second
	for _, tc := range []struct {
		raw   string
		want  time.Duration
		says  []string
		quiet bool
	}{
		{raw: "", want: def, quiet: true},
		{raw: "5s", want: 5 * time.Second, quiet: true},
		{raw: "  1h  ", want: time.Hour, quiet: true},
		{raw: "500ms", want: 500 * time.Millisecond, quiet: true},
		{raw: "0", want: 0, says: []string{WaitDelayEnv, `"0"`, "UNBOUNDED"}},
		{raw: "off", want: 0, says: []string{"UNBOUNDED"}},
		{raw: "None", want: 0, says: []string{"UNBOUNDED"}},
		{raw: "unbounded", want: 0, says: []string{"UNBOUNDED"}},
		{raw: "banana", want: def, says: []string{WaitDelayEnv, `"banana"`, "2s"}},
		{raw: "-1s", want: def, says: []string{`"-1s"`, "2s"}},
		// Go refuses a bare number ("missing unit"), so the operator who
		// meant five seconds gets the default and a line saying so.
		{raw: "5", want: def, says: []string{`"5"`, "500ms, 10s, 1h"}},
	} {
		t.Run(fmt.Sprintf("%s=%q", WaitDelayEnv, tc.raw), func(t *testing.T) {
			var warn strings.Builder
			got := resolveWaitDelay(tc.raw, def, &sync.Once{}, &warn)
			if got != tc.want {
				t.Errorf("resolveWaitDelay(%q, %s) = %s, want %s", tc.raw, def, got, tc.want)
			}
			switch {
			case tc.quiet && warn.Len() != 0:
				t.Errorf("%q is a value the resolver takes, and it warned anyway: %q", tc.raw, warn.String())
			case !tc.quiet && warn.Len() == 0:
				t.Errorf("%q was answered %s with nothing said — an override that never took, silently", tc.raw, got)
			}
			for _, want := range tc.says {
				if !strings.Contains(warn.String(), want) {
					t.Errorf("the warning for %q does not name %q: %q", tc.raw, want, warn.String())
				}
			}
		})
	}
}

// A refused override is a fact about the process, not about the call: one
// line, however many commands it then starts.
//
// Mutation seen red: call the Fprintf outside warnOnce.Do — the second
// resolution warns again.
func TestResolveWaitDelay_SaysItOncePerProcess(t *testing.T) {
	var warn strings.Builder
	once := &sync.Once{}
	resolveWaitDelay("banana", time.Second, once, &warn)
	first := warn.String()
	resolveWaitDelay("banana", time.Second, once, &warn)
	if warn.String() != first {
		t.Errorf("the second resolution warned again:\nfirst: %q\nthen:  %q", first, warn.String())
	}
	if strings.Count(warn.String(), WaitDelayEnv) != 1 {
		t.Errorf("want exactly one line naming %s, got %q", WaitDelayEnv, warn.String())
	}
}

// SetWaitDelay REPLACES what the Cmd carries, and hands the applied bound
// back: the call sites quote it in the log or the error that reports an
// abandoned drain, and a message quoting the site's default lies as soon as
// an operator overrides it.
//
// Mutations seen red: return `def` instead of `d` (the message names a bound
// that never ran); guard the assignment with `if cmd.WaitDelay == 0` (the
// bound a driver chose survives the one the call site owns).
func TestSetWaitDelay_ReplacesTheCmdsBoundAndReportsWhatItApplied(t *testing.T) {
	t.Setenv(WaitDelayEnv, "250ms")
	cmd := exec.Command("true")
	cmd.WaitDelay = time.Hour
	switch applied := SetWaitDelay(cmd, 2*time.Second); {
	case applied != 250*time.Millisecond:
		t.Errorf("SetWaitDelay reported %s, want the operator's 250ms", applied)
	case cmd.WaitDelay != 250*time.Millisecond:
		t.Errorf("cmd.WaitDelay = %s, want the operator's 250ms over the hour the Cmd carried", cmd.WaitDelay)
	}

	t.Setenv(WaitDelayEnv, "")
	plain := exec.Command("true")
	if applied := SetWaitDelay(plain, 2*time.Second); applied != 2*time.Second || plain.WaitDelay != 2*time.Second {
		t.Errorf("with no override: applied %s, cmd %s — want this site's 2s default", applied, plain.WaitDelay)
	}
}

// The knob end to end, on a real process, in the shape every call site has:
// a child that exits 0 at once while a descendant keeps writing to the pipes
// it inherited. The site's own bound abandons the copy and loses the tail;
// the operator's override keeps it. This is what #2469 is about — a bound
// with no escape hatch silently truncating what a container last said.
//
// Mutation seen red: make SetWaitDelay ignore the environment (return def) —
// the override subtest loses the holder's last line exactly like the default
// one.
func TestTheOverrideKeepsTheTailADescendantWritesLate(t *testing.T) {
	// The holder outlives the child by this much. Comfortably over the
	// site default below, so the default truly cuts; short enough that the
	// override's subtest is seconds, not minutes.
	const late = 900 * time.Millisecond
	const siteDefault = 150 * time.Millisecond
	// Written by the DESCENDANT after the child is gone: finding it proves
	// the drain outlived the child, not that the child was slow.
	const tail = "the-last-line-of-the-drain"
	script := fmt.Sprintf("( sleep %.1f; printf '%%s\\n' %q >&2 ) &\nexit 0", late.Seconds(), tail)

	for _, tc := range []struct {
		name     string
		override string
		wantTail bool
	}{
		{name: "the site default abandons the drain", override: "", wantTail: false},
		{name: "the operator's override waits for it", override: "1h", wantTail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(WaitDelayEnv, tc.override)
			var stderr bytes.Buffer
			cmd := exec.Command("sh", "-c", script)
			cmd.Stderr = &stderr
			applied := SetWaitDelay(cmd, siteDefault)
			start := time.Now()
			err := cmd.Run()
			elapsed := time.Since(start)

			switch {
			case tc.wantTail:
				if err != nil {
					t.Fatalf("the crossing failed under a %s bound: %v", applied, err)
				}
				if elapsed < late {
					t.Fatalf("Wait returned after %s, before the descendant's write at %s — the bound was not the one applied", elapsed, late)
				}
			default:
				if !DrainCutShort(err) {
					t.Fatalf("a %s bound on a drain held for %s did not cut it: err = %v (%s)", applied, late, err, elapsed)
				}
				if elapsed >= late {
					t.Fatalf("Wait returned after %s — it waited for the descendant instead of abandoning the drain at %s", elapsed, applied)
				}
			}
			if got := strings.Contains(stderr.String(), tail); got != tc.wantTail {
				t.Errorf("tail %q present = %v, want %v (bound %s, stderr %q)", tail, got, tc.wantTail, applied, stderr.String())
			}
		})
	}
}

// What makes tolerating the sentinel safe at a call site whose verdict is the
// exit status: os/exec reports ErrWaitDelay ONLY for a command that otherwise
// succeeded, so a failing command holding its pipes comes back as the
// ExitError and nothing swallows it. Asserted against the real producer, not
// a hand-made sentinel.
//
// Mutation seen red: make DrainCutShort answer true for any non-nil error —
// the failing child's exit 3 reads as an abandoned drain.
func TestDrainCutShort_IsTheCutAloneAndNeverAFailure(t *testing.T) {
	t.Setenv(WaitDelayEnv, "")
	holder := "( sleep 0.9; printf 'x\\n' >&2 ) &\n"

	var ok, bad bytes.Buffer
	exited0 := exec.Command("sh", "-c", holder+"exit 0")
	exited0.Stderr = &ok
	SetWaitDelay(exited0, 150*time.Millisecond)
	cut := exited0.Run()

	exited3 := exec.Command("sh", "-c", holder+"exit 3")
	exited3.Stderr = &bad
	SetWaitDelay(exited3, 150*time.Millisecond)
	failed := exited3.Run()

	if !DrainCutShort(cut) {
		t.Errorf("a child that exited 0 with its pipes held: err = %v, want the drain-cut sentinel", cut)
	}
	if DrainCutShort(failed) {
		t.Errorf("a child that exited 3 with its pipes held read as a cut drain: %v — tolerating it would hide the failure", failed)
	}
	var exit *exec.ExitError
	if !errors.As(failed, &exit) || exit.ExitCode() != 3 {
		t.Errorf("want the exit status 3 to win over the expired bound, got %v", failed)
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "the sentinel", err: exec.ErrWaitDelay, want: true},
		{name: "wrapped", err: fmt.Errorf("hook foo: %w (after 2s)", exec.ErrWaitDelay), want: true},
		{name: "another failure", err: errors.New("signal: killed"), want: false},
	} {
		if got := DrainCutShort(tc.err); got != tc.want {
			t.Errorf("DrainCutShort(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every site's report of an abandoned drain ends with the SAME clause, and
// this package is the only place that spells the variable: the five messages
// an operator can meet name the bound that ran and the one way to widen it.
//
// Mutation seen red: drop WaitDelayEnv from the hint (or quote `def` instead
// of `applied`) — the clause stops naming what an operator must act on.
func TestWaitDelayHint_NamesTheAppliedBoundAndTheOverride(t *testing.T) {
	hint := WaitDelayHint(250 * time.Millisecond)
	for _, want := range []string{"250ms", WaitDelayEnv, "to wait longer"} {
		if !strings.Contains(hint, want) {
			t.Errorf("WaitDelayHint does not name %q: %q", want, hint)
		}
	}
}

// The first subtest above arms the bound BEFORE Start on purpose: exec reads
// the field in Start to decide whether to arm its context watcher. This pins
// the ordering the call sites owe, from the one place that can state it.
func TestSetWaitDelayIsArmedBeforeStart(t *testing.T) {
	t.Setenv(WaitDelayEnv, "")
	cmd := exec.Command("sh", "-c", "exit 0")
	SetWaitDelay(cmd, time.Second)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	before := cmd.WaitDelay
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if before != time.Second {
		t.Errorf("cmd.WaitDelay at Start = %s, want the second the site armed", before)
	}
}
