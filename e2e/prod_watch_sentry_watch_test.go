package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func sentryArmedAgo(t *testing.T, h *pwHarness, ago time.Duration) {
	t.Helper()
	st := h.state(t)
	st["cursors"].(map[string]any)["sentry"].(map[string]any)["armed_at"] = time.Now().Add(-ago).UTC().Format(time.RFC3339)
	h.setState(t, st)
}

func sentryEditRecord(t *testing.T, h *pwHarness, id string, f func(rec map[string]any)) {
	t.Helper()
	st := h.state(t)
	f(st["incidents"].(map[string]any)["sentry:"+id].(map[string]any))
	h.setState(t, st)
}

func sentryShift(t *testing.T, v any, d time.Duration) any {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		return v
	}
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("shift %q: %v", s, err)
	}
	return tt.Add(d).UTC().Format(time.RFC3339)
}

// TestProdWatch_SentryHistoryNamesSurviveTheCoverageBudget: every transition
// the catch-up floor makes history after the arming is named in a coverage
// note: the ones the note's 600-character budget leaves out are carried in the
// state until a note says them.
func TestProdWatch_SentryHistoryNamesSurviveTheCoverageBudget(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	now := time.Now()
	var ids []string
	for k := 0; k < 7; k++ {
		id := fmt.Sprint(701 + k)
		ids = append(ids, "P-"+id)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
			Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-25 * time.Hour)}}})
	}
	n := len(h.bodies())
	var alerts []string
	for k := 0; k < 3; k++ {
		alerts = append(alerts, sentryAlerts(sentryTick(t, h, wf))...)
	}
	channel := strings.Join(h.bodies()[n:], "\n")
	var missing []string
	for _, id := range ids {
		if !strings.Contains(channel, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("history transitions never named in the channel (3 ticks): %v (alerts %v)\nchannel:\n%s", missing, alerts, channel)
	}
}

// TestProdWatch_SentryPendingTransitionIsNotSaidUnposted: a pending transition
// re-dated past the floor posts from its record, and is never also said to be
// history.
func TestProdWatch_SentryPendingTransitionIsNotSaidUnposted(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	h.alertCap.Store(1)
	now := time.Now()
	at := now.Add(-24*time.Hour + 10*time.Second)
	h.sentry.put(&pwSentryIssue{ID: "40", ShortID: strp("P-40"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: at}}})
	h.sentry.put(&pwSentryIssue{ID: "90", ShortID: strp("P-90"), Title: "boom", Level: "fatal", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-90:high" {
		t.Fatalf("setup: %v", got)
	}
	if rec := sentryIncident(t, h, "40"); rec["pending"] != "regressed" {
		t.Fatalf("setup: the regression is not pending: %v", rec)
	}
	time.Sleep(time.Until(at.Add(24*time.Hour + 4*time.Second)))
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	channel := strings.Join(h.bodies()[n:], "\n")
	if !anyPrefix(sentryAlerts(o), "regressed:P-40") {
		t.Fatalf("the pending regression did not post from its record: %v\n%s", sentryAlerts(o), channel)
	}
	for _, line := range strings.Split(channel, "\n") {
		if strings.Contains(line, "P-40") && strings.Contains(line, "recorded as history") {
			t.Fatalf("P-40 posted REGRESSED and the same tick's coverage note says it was recorded as history, not posted:\n%s", channel)
		}
	}
}

// TestProdWatch_SentryAStaleWatchStampDoesNotHoldTheNextEpisode: out of a
// transition list read whole, an issue's watch stamp ends: the next episode,
// watched from the minute it happened, keeps its exemption from the floor.
func TestProdWatch_SentryAStaleWatchStampDoesNotHoldTheNextEpisode(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 5*24*time.Hour)
	now := time.Now()
	// Episode 1: watched undated (the lookup fails once).
	h.sentry.put(&pwSentryIssue{ID: "33", ShortID: strp("P-33"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Minute)}}})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	if sentryIncident(t, h, "33")["transition_seen_at"] == nil {
		t.Fatalf("setup: episode 1 not stamped: %v", sentryIncident(t, h, "33"))
	}
	// That was four days ago.
	sentryEditRecord(t, h, "33", func(rec map[string]any) {
		rec["transition_seen_at"] = now.Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})
	h.sentry.edit("33", func(i *pwSentryIssue) {
		i.Acts = []pwSentryAct{{Type: "set_regression", At: now.Add(-4*24*time.Hour - 5*time.Minute)}}
		i.Status = "resolved" // resolved in Sentry; the lane does not read it (backlog, untracked)
	})
	sentryTick(t, h, wf)
	// Episode 2: regressed a minute ago, watched undated (the lookup fails).
	h.sentry.edit("33", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("regressed")
		i.LastSeen = time.Now()
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: time.Now().Add(-time.Minute)})
	})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	// 25 h pass: every stored date moves back 25 h, the lookup works again.
	shift := -25 * time.Hour
	stamp := sentryIncident(t, h, "33")["transition_seen_at"]
	sentryEditRecord(t, h, "33", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "first_seen", "last_seen", "transition_checked_at", "tracked_read_at"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
	})
	st := h.state(t)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	cur["armed_at"] = sentryShift(t, cur["armed_at"], shift)
	h.setState(t, st)
	h.sentry.edit("33", func(i *pwSentryIssue) {
		i.Acts[len(i.Acts)-1].At = i.Acts[len(i.Acts)-1].At.Add(shift)
		i.LastSeen = i.LastSeen.Add(shift)
	})
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "regressed:P-33") {
		t.Fatalf("episode 2 was watched from a minute after it happened, dated past the floor, and did not post (stale stamp %v from "+
			"episode 1): alerts %v, channel:\n%s", stamp, sentryAlerts(o), strings.Join(h.bodies()[n:], "\n"))
	}
}

// TestProdWatch_SentryBackAfterAnUnseenClosureIsSpared: a transition back in
// the list after a closure the lane never read (an untracked issue resolved and
// regressed between two reads) is watched from its re-entry, and posts however
// late its date comes.
func TestProdWatch_SentryBackAfterAnUnseenClosureIsSpared(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "34", ShortID: strp("P-34"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-2 * 24 * time.Hour),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-2 * 24 * time.Hour)}}})
	sentryTick(t, h, wf) // bootstrap: P-34 backlog, its regression history
	if rec := sentryIncident(t, h, "34"); rec == nil || rec["transition_at"] == nil {
		t.Fatalf("setup: the bootstrap did not date P-34: %v", rec)
	}
	h.sentry.edit("34", func(i *pwSentryIssue) { i.Status = "resolved" })
	sentryTick(t, h, wf) // not read: untracked
	h.sentry.edit("34", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("regressed")
		i.LastSeen = time.Now()
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: time.Now().Add(-time.Minute)})
	})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf) // back in the list; its re-check fails
	stamp := sentryIncident(t, h, "34")["transition_seen_at"]
	shift := -25 * time.Hour
	sentryEditRecord(t, h, "34", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "first_seen", "last_seen", "transition_checked_at", "tracked_read_at"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
	})
	st := h.state(t)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	cur["armed_at"] = sentryShift(t, cur["armed_at"], shift)
	h.setState(t, st)
	h.sentry.edit("34", func(i *pwSentryIssue) {
		i.Acts[len(i.Acts)-1].At = i.Acts[len(i.Acts)-1].At.Add(shift)
		i.LastSeen = i.LastSeen.Add(shift)
	})
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "regressed:P-34") {
		t.Fatalf("a regression watched in the list from a minute after it happened (back after a closure the lane did not read) "+
			"became history once dated past the floor (stamp %v): alerts %v, channel:\n%s", stamp, sentryAlerts(o), strings.Join(h.bodies()[n:], "\n"))
	}
}

// TestProdWatch_SentryLoweredMaxSeverityCapsKnownIncidents: a lowered
// max_severity caps what the lane already recorded — a known issue's
// regression, reminder and pending escalation included (a level is
// attacker-writable: critical is an opt-in).
func TestProdWatch_SentryLoweredMaxSeverityCapsKnownIncidents(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	crit := func(s map[string]any) {
		s["max_severity"] = "critical"
		s["severity"] = map[string]any{"fatal": "critical"}
	}
	lowered := func(s map[string]any) { s["severity"] = map[string]any{"fatal": "critical"} } // max_severity back to its default, high
	t.Run("a regression of a known issue", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, crit))
		sentryTick(t, h, wf)
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "50", ShortID: strp("P-50"), Title: "x", Level: "fatal", FirstProcessed: now, LastSeen: now})
		if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-50:critical" {
			t.Fatalf("setup: %v", got)
		}
		h.writeConfig(t, sentryOnly(h, lowered))
		h.sentry.edit("50", func(i *pwSentryIssue) { i.Status = "resolved"; i.FirstProcessed = now.Add(-3 * time.Hour) })
		sentryTick(t, h, wf)
		t1 := time.Now().Add(time.Second)
		h.sentry.edit("50", func(i *pwSentryIssue) {
			i.Status = "unresolved"
			i.Substatus = strp("regressed")
			i.LastSeen = t1
			i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
		})
		if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:P-50:high" {
			t.Fatalf("max_severity lowered to high: a known issue's regression must be capped at high, got %v", got)
		}
	})
	t.Run("a pending escalation", func(t *testing.T) {
		t.Parallel()
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
		h.writeConfig(t, sentryOnly(h, lowered))
		got := sentryAlerts(sentryTick(t, h, wf))
		if !anyPrefix(got, "escalated:P-51") || anyPrefix(got, "escalated:P-51:critical") {
			t.Fatalf("max_severity lowered to high: the pending escalation must post capped at high, got %v", got)
		}
	})
}

// TestProdWatch_SentryEscalationWhileArchivedIsSaidOnReopen: an archived
// issue's events raise nothing silently: reopened, its first sighting says
// it is open again, at the higher level.
func TestProdWatch_SentryEscalationWhileArchivedIsSaidOnReopen(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "30", ShortID: strp("P-30"), Title: "x", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-30:medium" {
		t.Fatalf("setup: %v", got)
	}
	h.sentry.edit("30", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = "ignored"
		i.Substatus = strp("archived_forever")
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-30:low" {
		t.Fatalf("setup archived note: %v", got)
	}
	h.sentry.edit("30", func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = time.Now().Add(time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("setup: an archived issue's event posted: %v", got)
	}
	// The operator unarchives it; it keeps firing at fatal.
	h.sentry.edit("30", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.LastSeen = time.Now().Add(2 * time.Second)
	})
	var seq []string
	for k := 0; k < 2; k++ {
		seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
		h.sentry.edit("30", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(3 * time.Second) })
	}
	if !anyPrefix(seq, "reopened:P-30:high") {
		t.Fatalf("reopened and firing at fatal (last announced medium), not said open again at high: %v (severity %v)", seq, sentryIncident(t, h, "30")["severity"])
	}
}

// TestProdWatch_SentryLateTransitionSaysWhenItHappened: a transition's alert
// carries its date: posted late (a watched transition, up to the seven days
// Sentry keeps a regressed substatus), it does not read as fresh.
func TestProdWatch_SentryLateTransitionSaysWhenItHappened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 10*24*time.Hour)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "7", ShortID: strp("P-7"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Minute)}}})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	// Six days of failing lookups later.
	shift := -6 * 24 * time.Hour
	sentryEditRecord(t, h, "7", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "last_seen", "transition_checked_at", "tracked_read_at"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
		rec["first_seen"] = now.Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339) // known for weeks
	})
	h.sentry.edit("7", func(i *pwSentryIssue) { i.Acts[0].At = i.Acts[0].At.Add(shift) })
	day := now.Add(-time.Minute).Add(shift).UTC().Format("2006-01-02")
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "regressed:P-7") {
		t.Fatalf("setup: the watched regression did not post: %v", sentryAlerts(o))
	}
	body := strings.Join(h.bodies()[n:], "\n")
	if !strings.Contains(body, day) {
		t.Fatalf("a regression six days old posted with no date of the regression (%s) in the message:\n%s", day, body)
	}
}

// TestProdWatch_SentryADateAtTheCalendarEdgeIsTakenWhole: a date at the
// calendar edge whose offset leaves the calendar is unknown, never an
// OverflowError losing the page and the rest of the walk.
func TestProdWatch_SentryADateAtTheCalendarEdgeIsTakenWhole(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4401", ShortID: strp("P-4401"), Title: "boom", FirstProcessed: now, LastSeen: now, Count: 3,
		Raw: map[string]string{"lastSeen": `"9999-12-31T23:59:59-14:00"`}})
	h.sentry.put(&pwSentryIssue{ID: "4402", ShortID: strp("P-4402"), Title: "bam", FirstProcessed: now.Add(-time.Second), LastSeen: now, Count: 3})
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); strings.Join(got, " ") != "new:P-4401:medium new:P-4402:medium" || o["poll_sentry"]["ok"] != true {
		t.Fatalf("want both issues read whole and posted: %v, errors %v", got, o["poll_sentry"]["errors"])
	}
}
