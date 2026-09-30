package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryAFreshFloodNeverStarvesAnOldRead: every issue read this
// tick — by id or in a list — joins the back of the by-id rotation, so a flood
// of fresh issues, as many a tick as max_tracked, never takes the reads of an
// older issue: its escalation is said the tick it is read.
func TestProdWatch_SentryAFreshFloodNeverStarvesAnOldRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 3 }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "real", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	h.sentry.edit("4242", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	for k := 0; k < 3; k++ {
		sentryJunk(h, 10001+100*k, 3) // three fresh issues a tick, max_tracked 3
		if k == 2 {
			h.sentry.edit("4242", func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = time.Now().Add(time.Second) })
		}
		time.Sleep(1100 * time.Millisecond) // tracked_read_at has a one-second resolution
		n := len(h.bodies())
		o := sentryTick(t, h, wf)
		if k == 2 && !strings.Contains(strings.Join(h.bodies()[n:], "\n"), "REAL-4242") {
			t.Fatalf("an older issue escalated behind three fresh issues a tick, max_tracked 3: not said the tick it fired (read %v)",
				o["plan"]["sentry"].(map[string]any)["tracked_ids"])
		}
	}
}

// TestProdWatch_SentryResolvedIssuesMakeNoWalkCut: resolved, noted issues are
// read in their turn for a reopening by hand, best effort — they do not make
// the walk cut for weeks, and max_tracked 0 (reads off) cuts nothing.
func TestProdWatch_SentryResolvedIssuesMakeNoWalkCut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, maxTracked := range map[string]int{"three resolved, max_tracked 2": 2, "reads off, max_tracked 0": 0} {
		name, maxTracked := name, maxTracked
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = maxTracked }))
			sentryTick(t, h, wf)
			now := time.Now()
			for _, id := range []string{"31", "32", "33"} {
				h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
			}
			sentryTick(t, h, wf)
			if maxTracked > 0 {
				for _, id := range []string{"31", "32", "33"} {
					h.sentry.edit(id, func(i *pwSentryIssue) { i.Status = "resolved"; i.FirstProcessed = now.Add(-3 * time.Hour) })
				}
				for k := 0; k < 3; k++ {
					time.Sleep(1100 * time.Millisecond)
					sentryTick(t, h, wf) // two reads a tick: the three closing notes come in turn
				}
			}
			o := sentryTick(t, h, wf)
			if cut := o["plan"]["sentry"].(map[string]any)["tracked_cut"]; cut != false {
				t.Fatalf("%s: the walk is cut (%v): resolved issues, or reads turned off, are no cut", name, o["poll_sentry"]["walk"])
			}
		})
	}
}

// TestProdWatch_SentryClosingsFoldByWhatTheySay: a closing note folds by what it
// says (resolved, archived, or gone otherwise) — status words a server or a
// proxy invents open no group each, so twelve closings are five notes one by
// one and one note of seven, whatever their words.
func TestProdWatch_SentryClosingsFoldByWhatTheySay(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 12)
	sentryTick(t, h, wf)
	for k := 0; k < 12; k++ {
		k := k
		h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) { i.Status = fmt.Sprintf("hostile_word_%c", 'a'+k) })
	}
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if got := strings.Join(sentryFolds(o), " "); got != "resolved:7" || len(h.bodies())-n != 6 {
		t.Fatalf("twelve closings with twelve status words: want 5 notes one by one and one note of 7, got %v and %d message(s)", got, len(h.bodies())-n)
	}
}
