package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryACapToggleForgesNoEscalation: announced at critical, a
// lowered max_severity re-caps the record; the cap restored, the next event
// at the same level is no escalation — the channel already heard critical.
func TestProdWatch_SentryACapToggleForgesNoEscalation(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	crit := func(s map[string]any) {
		s["max_severity"] = "critical"
		s["severity"] = map[string]any{"fatal": "critical"}
	}
	lowered := func(s map[string]any) { s["severity"] = map[string]any{"fatal": "critical"} }
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, crit))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "52", ShortID: strp("P-52"), Title: "x", Level: "fatal", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-52:critical" {
		t.Fatalf("setup: %v", got)
	}
	h.sentry.edit("52", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	h.writeConfig(t, sentryOnly(h, lowered))
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("setup: lowering the cap posted %v", got)
	}
	if rec := sentryIncident(t, h, "52"); rec["severity"] != "high" {
		t.Fatalf("setup: the lowered cap did not re-cap the record: %v", rec["severity"])
	}
	h.writeConfig(t, sentryOnly(h, crit))
	h.sentry.edit("52", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Second) }) // one more event, still fatal
	if got := sentryAlerts(sentryTick(t, h, wf)); anyPrefix(got, "escalated:P-52") {
		t.Fatalf("announced critical, never announced lower: the cap toggle forged ESCALATED: %v", got)
	}
}

// TestProdWatch_SentryARecappedPendingEscalationIsDropped: a pending
// escalation a lowered max_severity brings back to the severity already
// announced is no news: it is dropped, never posted as ESCALATED.
func TestProdWatch_SentryARecappedPendingEscalationIsDropped(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	crit := func(s map[string]any) {
		s["max_severity"] = "critical"
		s["severity"] = map[string]any{"fatal": "critical"}
	}
	lowered := func(s map[string]any) {
		s["max_severity"] = "medium"
		s["severity"] = map[string]any{"fatal": "critical"}
	}
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, crit))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "51", ShortID: strp("P-51"), Title: "x", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-51:medium" {
		t.Fatalf("setup: %v", got)
	}
	h.alertCap.Store(1)
	h.sentry.edit("51", func(i *pwSentryIssue) {
		i.Level = "fatal"
		i.LastSeen = time.Now().Add(time.Second)
		i.FirstProcessed = now.Add(-3 * time.Hour)
	})
	h.sentry.put(&pwSentryIssue{ID: "49", ShortID: strp("P-49"), Title: "y", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
	sentryTick(t, h, wf)
	if rec := sentryIncident(t, h, "51"); rec["pending"] != "escalated" {
		t.Fatalf("setup: the escalation is not pending: %v", rec)
	}
	h.alertCap.Store(0)
	h.writeConfig(t, sentryOnly(h, lowered))
	if got := sentryAlerts(sentryTick(t, h, wf)); anyPrefix(got, "escalated:P-51") {
		t.Fatalf("P-51 was announced at medium; capped at medium its pending escalation still posted: %v", got)
	}
	if rec := sentryIncident(t, h, "51"); rec["pending"] != nil {
		t.Fatalf("the escalation that is no news any more is still pending: %v", rec["pending"])
	}
}

// TestProdWatch_SentryAnArchiveEndingByItselfSaysReopened: an issue archived
// for a while (a Sentry snooze with `until`) is reopened by Sentry as ONGOING
// at the first event past it, with no transition: the channel, whose last
// word was "archived in Sentry", hears it is open again.
func TestProdWatch_SentryAnArchiveEndingByItselfSaysReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "60", ShortID: strp("P-60"), Title: "x", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-60:medium" {
		t.Fatalf("setup: %v", got)
	}
	h.sentry.edit("60", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = "ignored"
		i.Substatus = strp("archived_until_condition_met")
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-60:low" {
		t.Fatalf("setup archived note: %v", got)
	}
	at := time.Now().Add(time.Second)
	h.sentry.edit("60", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.LastSeen = at
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_unresolved", At: at})
	})
	n := len(h.bodies())
	got := sentryAlerts(sentryTick(t, h, wf))
	if strings.Join(got, " ") != "reopened:P-60:medium" {
		t.Fatalf("an archived issue Sentry reopened as ongoing, firing: %v (want reopened:P-60:medium)", got)
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "OPEN AGAIN IN SENTRY") {
		t.Fatalf("the reopening does not say so:\n%s", body)
	}
	h.sentry.edit("60", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(2 * time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("the reopening was said, and the next sighting said it again: %v", got)
	}
}

// TestProdWatch_SentryAReopeningCutByTheCapStaysPending: the reopening does
// not recur by itself: cut by the per-run cap, it stays pending and posts on
// the next tick.
func TestProdWatch_SentryAReopeningCutByTheCapStaysPending(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "61", ShortID: strp("P-61"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("61", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = "ignored"
		i.Substatus = strp("archived_until_condition_met")
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-61:low" {
		t.Fatalf("setup archived note: %v", got)
	}
	h.alertCap.Store(1)
	h.sentry.edit("61", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.LastSeen = time.Now().Add(time.Second)
	})
	h.sentry.put(&pwSentryIssue{ID: "62", ShortID: strp("P-62"), Title: "y", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-62:high" {
		t.Fatalf("setup: want the fatal new issue in the one slot, got %v", got)
	}
	if rec := sentryIncident(t, h, "61"); rec["pending"] != "reopened" {
		t.Fatalf("the reopening cut by the cap is not pending: %v", rec["pending"])
	}
	h.alertCap.Store(0)
	if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "reopened:P-61") {
		t.Fatalf("the pending reopening did not post the next tick: %v", got)
	}
}

// TestProdWatch_SentryPendingEscalationIsNotSaidUnposted: a pending escalation
// re-dated past the floor posts from its record — with its date — and is
// never also named as history.
func TestProdWatch_SentryPendingEscalationIsNotSaidUnposted(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	h.alertCap.Store(1)
	now := time.Now()
	at := now.Add(-24*time.Hour + 10*time.Second)
	h.sentry.put(&pwSentryIssue{ID: "41", ShortID: strp("P-41"), Title: "e", Substatus: strp("escalating"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_escalating", At: at}}})
	h.sentry.put(&pwSentryIssue{ID: "90", ShortID: strp("P-90"), Title: "boom", Level: "fatal", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-90:high" {
		t.Fatalf("setup: %v", got)
	}
	if rec := sentryIncident(t, h, "41"); rec["pending"] != "escalating" {
		t.Fatalf("setup: the escalation is not pending: %v", rec)
	}
	time.Sleep(time.Until(at.Add(24*time.Hour + 4*time.Second)))
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	channel := strings.Join(h.bodies()[n:], "\n")
	if !anyPrefix(sentryAlerts(o), "escalating:P-41") {
		t.Fatalf("the pending escalation did not post from its record: %v\n%s", sentryAlerts(o), channel)
	}
	for _, line := range strings.Split(channel, "\n") {
		if strings.Contains(line, "P-41") && strings.Contains(line, "recorded as history") {
			t.Fatalf("P-41 posted ESCALATING and the same tick's note says it was recorded as history, not posted:\n%s", channel)
		}
	}
	if day := at.UTC().Format("2006-01-02T15:04"); !strings.Contains(channel, day) {
		t.Fatalf("the pending escalation posted from its record without its date (%s):\n%s", day, channel)
	}
}

// TestProdWatch_SentryLateEscalationSaysWhenItHappened: an escalation posted
// late (watched while its date was unknown) says when it happened.
func TestProdWatch_SentryLateEscalationSaysWhenItHappened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 10*24*time.Hour)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "8", ShortID: strp("P-8"), Title: "e", Substatus: strp("escalating"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_escalating", At: now.Add(-time.Minute)}}})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	shift := -6 * 24 * time.Hour
	sentryEditRecord(t, h, "8", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "last_seen", "transition_checked_at", "tracked_read_at"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
		rec["first_seen"] = now.Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})
	h.sentry.edit("8", func(i *pwSentryIssue) { i.Acts[0].At = i.Acts[0].At.Add(shift) })
	day := now.Add(-time.Minute).Add(shift).UTC().Format("2006-01-02")
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "escalating:P-8") {
		t.Fatalf("setup: the watched escalation did not post: %v", sentryAlerts(o))
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, day) {
		t.Fatalf("an escalation six days old posted with no date (%s):\n%s", day, body)
	}
}
