package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryAFloodsMembersAreReadAfterTheIssuesSaidOneByOne: the
// issues named in a flood's notes are followed too, but read by id after the
// issues the channel heard of one by one — past max_tracked, a flood's members
// take turns among themselves, never against a real issue's escalation.
func TestProdWatch_SentryAFloodsMembersAreReadAfterTheIssuesSaidOneByOne(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 4 }))
	h.setMaxPerLane(1)
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "real", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "new:REAL-4242:medium") {
		t.Fatalf("setup: %v", got)
	}
	// Out of the new list's window from now on: read by id only.
	h.sentry.edit("4242", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	for k := 0; k < 3; k++ {
		sentryJunk(h, 10001+100*k, 6) // one posted one by one, five named in a note, every tick
		if k == 2 {
			h.sentry.edit("4242", func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = time.Now().Add(time.Second) })
		}
		n := len(h.bodies())
		o := sentryTick(t, h, wf)
		if k == 2 && (!strings.Contains(strings.Join(h.bodies()[n:], "\n"), "REAL-4242") || sentryIncident(t, h, "4242")["announced_severity"] != "high") {
			t.Fatalf("the real issue escalated to fatal behind 10 flood members, max_tracked 4: not said the tick it fired (read %v)",
				o["plan"]["sentry"].(map[string]any)["tracked_ids"])
		}
	}
}

// TestProdWatch_SentryAGoneIssueIsReadLast: an issue asked by id and absent
// from the answer is gone — deleted or merged, the lookup filters on nothing
// else — and read after every other, with reads to spare, counting for no
// cut: deleting a flood in Sentry frees the reads it held. An answer carrying
// it again (one empty answer is not trusted for good) clears it.
func TestProdWatch_SentryAGoneIssueIsReadLast(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"61", "62"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now})
	}
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	delete(h.sentry.issues, "61")
	h.sentry.mu.Unlock()
	read := func(o map[string]map[string]any) string {
		return fmt.Sprint(o["plan"]["sentry"].(map[string]any)["tracked_ids"])
	}
	if o := sentryTick(t, h, wf); read(o) != "[61]" {
		t.Fatalf("setup: never read, 61 goes first: %v", read(o))
	}
	for k := 0; k < 3; k++ {
		time.Sleep(1100 * time.Millisecond) // tracked_read_at has a one-second resolution
		o := sentryTick(t, h, wf)
		if read(o) != "[62]" || o["plan"]["sentry"].(map[string]any)["tracked_cut"] != false {
			t.Fatalf("tick %d: 61 gone, 62 open, max_tracked 1: want 62 read every tick and no cut, got %v cut=%v",
				k, read(o), o["plan"]["sentry"].(map[string]any)["tracked_cut"])
		}
	}
	h.sentry.put(&pwSentryIssue{ID: "61", ShortID: strp("P-61"), Title: "x", FirstProcessed: now, LastSeen: now})
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 2 }))
	sentryTick(t, h, wf)
	if rec := sentryIncident(t, h, "61"); rec["gone"] != nil {
		t.Fatalf("61 answered again and is still taken for gone: %v", rec)
	}
}

// TestProdWatch_SentryAGoneIssueStillGetsItsIdleNoteLater: gone and read last —
// here not asked at all, the one read a tick going to an open issue — an
// alerted issue still gets its idle note once quiet_after_hours pass: being
// gone is a fact about it on every tick.
func TestProdWatch_SentryAGoneIssueStillGetsItsIdleNoteLater(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"61", "62"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now})
	}
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	delete(h.sentry.issues, "61")
	h.sentry.mu.Unlock()
	if o := sentryTick(t, h, wf); len(sentryAlerts(o)) != 0 || fmt.Sprint(o["plan"]["sentry"].(map[string]any)["tracked_ids"]) != "[61]" {
		t.Fatalf("setup: the tick asking 61 found it gone: %v %v", sentryAlerts(o), o["plan"]["sentry"].(map[string]any)["tracked_ids"])
	}
	ageIncidents(t, h, 49*time.Hour, "61")
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); strings.Join(got, " ") != "quiet:P-61:low" || fmt.Sprint(o["plan"]["sentry"].(map[string]any)["tracked_ids"]) != "[62]" {
		t.Fatalf("an issue gone from Sentry, not asked this tick, idle 49 h: want its idle note alone, got %v (read %v)",
			got, o["plan"]["sentry"].(map[string]any)["tracked_ids"])
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
