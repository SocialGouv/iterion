package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryATrackedCutHidesNoLaterLoss: a tracked set over
// max_tracked is said in its own coverage note — a loss that comes after it
// (a list stopped at max_issues) is still said, never folded into a note
// already posted.
func TestProdWatch_SentryATrackedCutHidesNoLaterLoss(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"21", "22"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "tracked issues for max_tracked 1") {
		t.Fatalf("setup: the tracked cut was not said:\n%s", body)
	}
	h.sentry.mu.Lock()
	h.sentry.pageSize = 1
	h.sentry.mu.Unlock()
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1; s["max_issues"] = 1 }))
	for _, id := range []string{"31", "32", "33"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("N-" + id), Title: "x", FirstProcessed: time.Now(), LastSeen: time.Now(), Count: 1})
	}
	n = len(h.bodies())
	o := sentryTick(t, h, wf)
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "stopped at max_issues") {
		t.Fatalf("the new-issue list stopped at max_issues after a tracked cut, and no coverage note says it (partial %v):\n%s",
			o["poll_sentry"]["walk"].(map[string]any)["partial"], body)
	}
}

// TestProdWatch_SentryATrackedSetOverMaxTrackedIsAPartialWalk: more followed
// issues than max_tracked reads a tick take turns, and the walk says it is
// partial — the coverage note names the cause — never a silent delay.
func TestProdWatch_SentryATrackedSetOverMaxTrackedIsAPartialWalk(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"21", "22"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now})
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 2 {
		t.Fatalf("setup: %v", got)
	}
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "2 tracked issues for max_tracked 1 reads a tick") {
		t.Fatalf("two followed issues, max_tracked 1: the partial walk is not said:\n%s", body)
	}
}

// TestProdWatch_SentryUnresolvedInTheUIWithinItsFirstHourSaysReopened: a
// resolved issue unresolved from Sentry's UI (substatus ongoing) is in no
// transition list — within its first hour the new-issue list shows it open,
// read by id or not.
func TestProdWatch_SentryUnresolvedInTheUIWithinItsFirstHourSaysReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "80", ShortID: strp("P-80"), Title: "x", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-80:medium" {
		t.Fatalf("setup new: %v", got)
	}
	h.sentry.edit("80", func(i *pwSentryIssue) {
		i.Status = "resolved"
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_resolved", At: time.Now()})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-80:low" {
		t.Fatalf("setup closing note: %v", got)
	}
	// No read by id from here: only the new-issue list can show it open.
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 0 }))
	h.sentry.edit("80", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_unresolved", At: time.Now()})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reopened:P-80:medium" {
		t.Fatalf("resolved (said), unresolved in the UI within its first hour, max_tracked 0: want reopened:P-80:medium, got %v", got)
	}
}
