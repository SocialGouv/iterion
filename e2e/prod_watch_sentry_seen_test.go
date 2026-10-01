package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryAsksForTheSeenStats: every issue list and by-id read asks
// with collapse=lifetime,filtered and an empty groupStatsPeriod — collapse=stats
// strips count, userCount, firstSeen and lastSeen on Sentry 24.11.1 (its own
// test_collapse_stats, and a live 24.11.1 answers so; the fake does the same):
// the lane would never see an event. A probe lane keeps the tick reported
// whatever the Sentry lane says.
func TestProdWatch_SentryAsksForTheSeenStats(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "501", ShortID: strp("P-501"), Title: "x", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	seen := time.Now().Add(time.Second)
	h.sentry.edit("501", func(i *pwSentryIssue) { i.LastSeen = seen; i.Count = 7 })
	o := sentryTick(t, h, wf)
	calls := append(h.sentry.callsTo("list"), h.sentry.callsTo("tracked")...)
	for _, c := range calls {
		if strings.Join(c.Q["collapse"], ",") != "lifetime,filtered" || !c.Q.Has("groupStatsPeriod") || c.Q.Get("groupStatsPeriod") != "" {
			t.Fatalf("an issues call without the seen stats' parameters: %v", c.Q)
		}
	}
	if len(h.sentry.callsTo("tracked")) == 0 || fmt.Sprint(o["poll_sentry"]["errors"]) != "[]" {
		t.Fatalf("setup: the alerted issue was not read by id without error: %d by-id call(s), errors %v",
			len(h.sentry.callsTo("tracked")), o["poll_sentry"]["errors"])
	}
	rec := sentryIncident(t, h, "501")
	got, err := time.Parse(time.RFC3339, fmt.Sprint(rec["sentry_last_seen"]))
	if err != nil || !got.Equal(seen.Truncate(time.Second)) {
		t.Fatalf("the event seen by id was not read: sentry_last_seen=%v, want %v", rec["sentry_last_seen"], seen.UTC().Truncate(time.Second))
	}
}

// TestProdWatch_SentryAnAnswerWithoutSeenStatsIsALaneError: a server answering
// issues without their seen stats (a collapse that strips them, a version that
// no longer sends them) leaves the lane blind to events — said as a lane error,
// never read as issues without events (no idle note).
func TestProdWatch_SentryAnAnswerWithoutSeenStatsIsALaneError(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "61", ShortID: strp("P-61"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	ageIncidents(t, h, 49*time.Hour, "61")
	h.sentry.mu.Lock()
	h.sentry.stripSeen = true
	h.sentry.mu.Unlock()
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); len(got) != 0 {
		t.Fatalf("answers without seen stats were read as issues without events: %v", got)
	}
	if errs := fmt.Sprint(o["poll_sentry"]["errors"]); !strings.Contains(errs, "carries no seen stats") {
		t.Fatalf("the stripped answers were not a lane error: %s", errs)
	}
}
