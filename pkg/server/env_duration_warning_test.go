package server

import (
	"bytes"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestTheRunSocketDurationKnobsNameAnUnhonouredValueAtBoot: an operator who
// states an unparsable or non-positive duration is told at boot that the
// default is used — the value is never silently replaced.
func TestTheRunSocketDurationKnobsNameAnUnhonouredValueAtBoot(t *testing.T) {
	t.Setenv("ITERION_RUN_WS_MAX_LIFETIME", "2 hours")
	t.Setenv("ITERION_RUN_BROWSER_STANDING_PERIOD", "-5m")
	t.Setenv("ITERION_RUN_SHELL_WATCH_PERIOD", "") // unset: nothing to state
	var buf bytes.Buffer
	New(Config{}, iterlog.New(iterlog.LevelWarn, &buf))
	out := buf.String()
	for _, want := range []string{
		"ITERION_RUN_WS_MAX_LIFETIME",
		"ITERION_RUN_BROWSER_STANDING_PERIOD",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("the boot log named no warning for %s:\n%s", want, out)
		}
	}
	if bytes.Contains([]byte(out), []byte("ITERION_RUN_SHELL_WATCH_PERIOD")) {
		t.Fatalf("the boot log named a knob the operator never set:\n%s", out)
	}

	// A valid value stays unnamed.
	buf.Reset()
	t.Setenv("ITERION_RUN_WS_MAX_LIFETIME", "2h")
	New(Config{}, iterlog.New(iterlog.LevelWarn, &buf))
	out = buf.String()
	if bytes.Contains([]byte(out), []byte("ITERION_RUN_WS_MAX_LIFETIME")) {
		t.Fatalf("a valid override was warned about:\n%s", out)
	}
}
