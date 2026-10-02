package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// sentryAlertedThenClosed posts a new issue, then its closing note (status
// "ignored" for an archive, "resolved" for a resolution).
func sentryAlertedThenClosed(t *testing.T, h *pwHarness, wf *ir.Workflow, id, status string) {
	t.Helper()
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-"+id+":medium" {
		t.Fatalf("setup new: %v", got)
	}
	h.sentry.edit(id, func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = status
		if status == "ignored" {
			i.Substatus = strp("archived_until_condition_met")
		}
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_" + status, At: time.Now()})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-"+id+":low" {
		t.Fatalf("setup closing note: %v", got)
	}
}

// TestProdWatch_SentryAnArchiveReadOpenBeforeItsNextEventSaysReopened: Sentry
// reopens an issue archived for a while at the snooze's expiry, with no event:
// the first read showing it open says so, event or not.
func TestProdWatch_SentryAnArchiveReadOpenBeforeItsNextEventSaysReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryAlertedThenClosed(t, h, wf, "60", "ignored")
	h.sentry.edit("60", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reopened:P-60:medium" {
		t.Fatalf("archived (said), read open with no event: want reopened:P-60:medium, got %v", got)
	}
	h.sentry.edit("60", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("the reopening was said, and its next event said it again: %v", got)
	}
}

// TestProdWatch_SentryAResolvedIssueReopenedByHandSaysReopened: an operator
// unresolves a resolved issue through the API — REGRESSED with no date, read
// off the transition list (a resolved issue is not read by id) — and the
// channel hears it is open again.
func TestProdWatch_SentryAResolvedIssueReopenedByHandSaysReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, substatus := range map[string]string{"through the API": "regressed", "from the issue page or in bulk": "ongoing"} {
		name, substatus := name, substatus
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			sentryTick(t, h, wf)
			sentryAlertedThenClosed(t, h, wf, "70", "resolved")
			h.sentry.edit("70", func(i *pwSentryIssue) {
				i.Status = "unresolved"
				i.Substatus = strp(substatus)
				i.LastSeen = time.Now().Add(time.Second)
				i.Acts = append(i.Acts, pwSentryAct{Type: "set_unresolved", At: time.Now()})
			})
			// The tick that reads it in the transition list says it — not the
			// next one, once the list's status got it read by id.
			if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "reopened:P-70") {
				t.Fatalf("resolved (said), unresolved by hand %s: not said open again the tick it was read open: %v", name, got)
			}
		})
	}
}

// TestProdWatch_SentryASeverityReachedWhileArchivedIsSaidOnReopen: a severity
// reached while the issue was archived is said when it is read open again,
// even when the reopening event is back at the lower level.
func TestProdWatch_SentryASeverityReachedWhileArchivedIsSaidOnReopen(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryAlertedThenClosed(t, h, wf, "30", "ignored")
	h.sentry.edit("30", func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = time.Now().Add(time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("setup: an archived issue's event posted: %v", got)
	}
	h.sentry.edit("30", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.Level = "error"
		i.LastSeen = time.Now().Add(2 * time.Second)
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reopened:P-30:high" {
		t.Fatalf("fatal while archived, reopened on an error event: want reopened:P-30:high, got %v", got)
	}
}

// TestProdWatch_SentryAnIdleNotedOpenIssueIsNotSaidReopened: an OPEN issue
// whose last word was the idle note, firing again, was never closed in
// Sentry: no "open again".
func TestProdWatch_SentryAnIdleNotedOpenIssueIsNotSaidReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1401", ShortID: strp("PROJ-140"), Title: "idle soon", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("1401", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	sentryEditRecord(t, h, "1401", func(r map[string]any) {
		r["last_seen"] = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
		r["last_notified"] = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"quiet:PROJ-140:low"}) {
		t.Fatalf("setup idle note: %v", got)
	}
	h.sentry.edit("1401", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); anyPrefix(got, "reopened:") {
		t.Fatalf("an open issue idle-noted, firing again, was said open again in Sentry: %v", got)
	}
}

// TestProdWatch_SentryARecordWithoutAnnouncedSeverityIsSeeded: a record
// written before announced_severity existed takes its recorded severity as
// what the channel heard, before any raise: an escalation while archived —
// its closing note cut, so no reopening — is said as ESCALATED on the next
// sighting read open.
func TestProdWatch_SentryARecordWithoutAnnouncedSeverityIsSeeded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "31", ShortID: strp("P-31"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	sentryEditRecord(t, h, "31", func(r map[string]any) { delete(r, "announced_severity") })
	h.sentry.edit("31", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.Status = "ignored"
		i.Substatus = strp("archived_forever")
		i.Level = "fatal"
		i.LastSeen = time.Now().Add(time.Second)
	})
	h.alertCap.Store(1)
	h.sentry.put(&pwSentryIssue{ID: "32", ShortID: strp("P-32"), Title: "y", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
	if got := sentryAlerts(sentryTick(t, h, wf)); anyPrefix(got, "resolved:P-31") {
		t.Fatalf("setup: the closing note should have been cut by the cap: %v", got)
	}
	h.alertCap.Store(0)
	h.sentry.edit("31", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.LastSeen = time.Now().Add(2 * time.Second)
	})
	var seq []string
	for k := 0; k < 2; k++ {
		seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if !anyPrefix(seq, "escalated:P-31:high") {
		t.Fatalf("medium when said, fatal while archived with its note cut, read open again firing: want escalated:P-31:high, got %v", seq)
	}
}

// TestProdWatch_SentryACarriedDroppedCountIsSaidLater: a dropped-history
// count carried unsaid is added to, never replaced: the next note says it.
func TestProdWatch_SentryACarriedDroppedCountIsSaidLater(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	st := h.state(t)
	st["sentry_history_dropped"] = 7
	h.setState(t, st)
	if r := sentryNoteReasons(sentryTick(t, h, wf)); !strings.Contains(r, "sentry: 7 more transition(s)") {
		t.Fatalf("a carried count of 7 dropped names was never said: %q", r)
	}
}

// TestProdWatch_SentryALossOfTheWalkComesBeforeCarriedHistory: a loss of the
// walk (a malformed issue skipped) is named on its first tick, whatever
// history names are carried.
func TestProdWatch_SentryALossOfTheWalkComesBeforeCarriedHistory(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 40 }))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	sentryHistoryIssues(h, 701, 16)
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4401", ShortID: strp("P-4401"), Title: "boom", FirstProcessed: now, LastSeen: now, Count: 3,
		Raw: map[string]string{"id": `"not-a-number"`}})
	if r := sentryNoteReasons(sentryTick(t, h, wf)); !strings.Contains(r, "malformed") {
		t.Fatalf("a malformed issue skipped (a loss) was not named on its first tick (carried history first?): %q", r)
	}
}
