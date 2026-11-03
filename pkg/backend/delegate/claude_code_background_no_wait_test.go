//go:build !windows

package delegate

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// This decorator records the value passed to the real timer's Reset, at the
// operation itself. Sampling the lifecycle's chosen duration separately would
// miss a select loop that accidentally armed a different duration.
type observedBackgroundTimer struct {
	backgroundSessionTimer
	resets int
	last   time.Duration
}

func (t *observedBackgroundTimer) Reset(d time.Duration) bool {
	t.resets++
	t.last = d
	return t.backgroundSessionTimer.Reset(d)
}

func noWaitScriptSleep(t *testing.T, line string) (time.Duration, bool) {
	t.Helper()
	seconds, ok := strings.CutPrefix(line, "@sleep ")
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(seconds + "s")
	if err != nil || d <= 0 {
		t.Fatalf("no-wait witness: invalid pacing %q", line)
	}
	return d, true
}

// backgroundBeforeEnd feeds the no-wait witness's actual CLI lines to the
// production tracker and lifecycle at explicit times. It stops BEFORE the
// named end line: that decision, rather than process startup or scheduling,
// is the assertion's subject. This is not a session driver: interactive
// directives are refused, and every earlier result must keep reading without
// sending a message. The real runBgSession remains the integration witness.
func backgroundBeforeEnd(t *testing.T, script []string, end string) *lifecycleAt {
	t.Helper()
	stop := -1
	for i, line := range script {
		if line == end {
			if stop >= 0 {
				t.Fatal("no-wait witness: end line is not unique")
			}
			stop = i
		}
	}
	if stop < 0 {
		t.Fatal("no-wait witness: end line is absent")
	}
	h := newLifecycleAt(t, resolveBackgroundLifecycleConfig(), 0)
	h.at = time.Unix(1_700_000_000, 0)
	for _, line := range script[:stop] {
		if d, ok := noWaitScriptSleep(t, line); ok {
			h.at = h.at.Add(d)
			continue
		}
		if strings.HasPrefix(line, "@") {
			t.Fatalf("no-wait witness cannot drive interactive directive %q", line)
		}
		m, err := claudesdk.UnmarshalMessage([]byte(line))
		if err != nil {
			t.Fatalf("no-wait witness: parse %q: %v", line, err)
		}
		h.tr.observe(m)
		if rm, ok := m.(*claudesdk.ResultMessage); ok {
			h.l.addResult(rm)
			if act, msg := h.l.atClose(h.at, rm); act != bgReenter || msg != "" {
				t.Fatalf("no-wait witness: earlier result chose action %v, message %q before the named end", act, msg)
			}
			// runSession re-enters only after consuming the result's close.
			h.l.reentries++
		}
	}
	return h
}

// assertBackgroundReturnsAtResult proves that the named result closes the
// lifecycle without waiting for another turn, idle, notification or timer.
// Call after runBgSession, while its pinned environment still applies.
func assertBackgroundReturnsAtResult(t *testing.T, script []string, result string) {
	t.Helper()
	h := backgroundBeforeEnd(t, script, result)
	m, err := claudesdk.UnmarshalMessage([]byte(result))
	if err != nil {
		t.Fatal(err)
	}
	rm, ok := m.(*claudesdk.ResultMessage)
	if !ok {
		t.Fatal("no-wait witness: end line is not a result")
	}
	h.tr.observe(rm)
	h.l.addResult(rm)
	if act, msg := h.l.atClose(h.at, rm); act != bgReturn || msg != "" {
		t.Fatalf("result did not end the session: action=%v message=%q", act, msg)
	}
}

// assertBackgroundEndsAtIdle proves that the named idle needs only its own
// settle window, even before any later line in the script arrives. In
// particular, a nested notification or a grace must not govern this end.
func assertBackgroundEndsAtIdle(t *testing.T, script []string, run bgRun) {
	t.Helper()
	h := backgroundBeforeEnd(t, script, lnIdle())
	if run.bgTimerResets == 0 || run.lastBgTimerDelay > h.l.cfg.idleSettle {
		t.Fatalf("CLI session armed %v as its final background timer (%d resets), want at most its settle window %v", run.lastBgTimerDelay, run.bgTimerResets, h.l.cfg.idleSettle)
	}
	// The fixture must leave this idle undisturbed for its settle window.
	// Merely stopping the replay at an idle followed by running would prove
	// a state the real CLI never lets settle.
	var quiet time.Duration
	for _, line := range script[slices.Index(script, lnIdle())+1:] {
		if line == "@drain" {
			quiet = h.l.cfg.idleSettle
			break
		}
		if d, ok := noWaitScriptSleep(t, line); ok {
			quiet += d
			if quiet >= h.l.cfg.idleSettle {
				break
			}
			continue
		}
		t.Fatalf("no-wait witness: %q disturbs the idle before its settle window", line)
	}
	if quiet < h.l.cfg.idleSettle {
		t.Fatal("no-wait witness: script does not keep the idle open through its settle window")
	}
	h.feed(lnIdle())
	d, kind, suspended := h.l.timer(h.at, false, false)
	if kind != bgTimerSettle || d != h.l.cfg.idleSettle || !suspended {
		t.Fatalf("idle chose timer=%v delay=%v suspended=%v, want its settle window %v", kind, d, suspended, h.l.cfg.idleSettle)
	}
	h.at = h.at.Add(d)
	if left, kind, _ := h.l.timer(h.at, false, false); kind != bgTimerSettle || left != 0 {
		t.Fatalf("idle did not keep its settle deadline: timer=%v left=%v", kind, left)
	}
	if h.tr.cliMovedOn() || !h.tr.view().settled {
		t.Fatal("idle moved on before its settle timer fired")
	}
	if msg := h.l.settle(h.at); msg != "" || !h.l.ended {
		t.Fatalf("settled idle did not end the session: message=%q ended=%v", msg, h.l.ended)
	}
}
