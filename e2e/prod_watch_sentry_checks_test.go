package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// sentryNoteReasons is the tick's coverage note ("" when none was due).
func sentryNoteReasons(o map[string]map[string]any) string {
	ss, _ := o["decide"]["stale_sources"].([]any)
	for _, s := range ss {
		m := s.(map[string]any)
		if m["source"] == "coverage" {
			return fmt.Sprint(m["reasons"])
		}
	}
	return ""
}

func sentryWalkLine(o map[string]map[string]any) string {
	w, _ := o["poll_sentry"]["walk"].(map[string]any)
	return fmt.Sprintf("checks=%v deferred=%v cut=%v truncated=%v", w["activity_checks"], w["rechecks_deferred"], w["checks_cut"], o["poll_sentry"]["truncated"])
}

// sentryTickCoverage reads the coverage the tick ledger recorded.
func sentryTickCoverage(t *testing.T, o map[string]map[string]any) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprint(o["decide"]["tick_file"]))
	if err != nil {
		t.Fatalf("tick file: %v", err)
	}
	if strings.Contains(string(b), `"coverage": "full"`) {
		return "full"
	}
	return "partial"
}

// sentryReopenedByHand puts issues a human unresolved through the API: Sentry
// 24.11.1 gives them the substatus REGRESSED with a set_unresolved activity
// and no set_regression — a transition no check can date.
func sentryReopenedByHand(h *pwHarness, ids ...string) {
	now := time.Now()
	for _, id := range ids {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "reopened by hand", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(),
			Acts: []pwSentryAct{{Type: "set_resolved", At: now.Add(-2 * time.Hour)}, {Type: "set_unresolved", At: now.Add(-time.Hour)}}})
	}
}

// TestProdWatch_SentryAnUndatableTransitionTakesItsTurn: a transition checked
// once without a date waits its turn among the re-checks: as many of them as
// max_transition_checks never hold a known issue's second regression back.
func TestProdWatch_SentryAnUndatableTransitionTakesItsTurn(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, reopened := range []bool{false, true} {
		reopened := reopened
		t.Run(fmt.Sprint("reopened=", reopened), func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 2 }))
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "10", ShortID: strp("P-10"), Title: "x", FirstProcessed: now, LastSeen: now})
			if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-10:medium" {
				t.Fatalf("setup new: %v", got)
			}
			h.sentry.edit("10", func(i *pwSentryIssue) { i.Status = "resolved"; i.FirstProcessed = now.Add(-3 * time.Hour) })
			if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-10:low" {
				t.Fatalf("setup resolved: %v", got)
			}
			t1 := time.Now().Add(time.Second)
			h.sentry.edit("10", func(i *pwSentryIssue) {
				i.Status = "unresolved"
				i.Substatus = strp("regressed")
				i.LastSeen = t1
				i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
			})
			if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:P-10:medium" {
				t.Fatalf("setup first regression: %v", got)
			}
			if reopened {
				sentryReopenedByHand(h, "20", "21")
			}
			sentryTick(t, h, wf)
			h.sentry.edit("10", func(i *pwSentryIssue) { i.Status = "resolved" })
			if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-10:low" {
				t.Fatalf("setup second resolution: %v", got)
			}
			t2 := time.Now().Add(time.Second)
			h.sentry.edit("10", func(i *pwSentryIssue) {
				i.Status = "unresolved"
				i.Substatus = strp("regressed")
				i.LastSeen = t2
				i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t2})
			})
			var seq []string
			posted := false
			for k := 0; k < 4 && !posted; k++ {
				o := sentryTick(t, h, wf)
				a := sentryAlerts(o)
				posted = anyPrefix(a, "regressed:P-10")
				seq = append(seq, fmt.Sprintf("tick%d %v %s", k, a, sentryWalkLine(o)))
			}
			if !posted {
				t.Fatalf("P-10's second regression never posted over 4 ticks (two undatable transitions checked first every tick):\n%s",
					strings.Join(seq, "\n"))
			}
		})
	}
}

// TestProdWatch_SentryUndatableTransitionsLeaveRoomForANewRegression: busier
// undatable transitions are not checked ahead of a regression of an issue the
// lane never knew, every tick: that regression is dated and posts.
func TestProdWatch_SentryUndatableTransitionsLeaveRoomForANewRegression(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 2 }))
	sentryTick(t, h, wf)
	sentryReopenedByHand(h, "20", "21")
	sentryTick(t, h, wf)
	now := time.Now()
	treg := now.Add(-10 * time.Second)
	h.sentry.put(&pwSentryIssue{ID: "30", ShortID: strp("P-30"), Title: "regressed", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: treg,
		Acts: []pwSentryAct{{Type: "set_resolved", At: now.Add(-3 * 24 * time.Hour)}, {Type: "set_regression", At: treg}}})
	var seq []string
	for k := 0; k < 3; k++ {
		for _, id := range []string{"20", "21"} {
			h.sentry.edit(id, func(i *pwSentryIssue) { i.LastSeen = time.Now() }) // still firing, busier than P-30
		}
		o := sentryTick(t, h, wf)
		a := sentryAlerts(o)
		seq = append(seq, fmt.Sprintf("tick%d %v %s", k, a, sentryWalkLine(o)))
		if anyPrefix(a, "regressed:P-30") {
			return
		}
	}
	t.Fatalf("P-30 regressed while the lane watched and was never dated (undatable transitions checked first every tick):\n%s",
		strings.Join(seq, "\n"))
}

// TestProdWatch_SentryAMissingLinkHeaderIsNeverAWholeRead: a list answer with
// no Link header (a proxy stripping it) is never taken for the last page:
// the walk says why, the cursor stays, and the page it could not reach is
// read once the header is back.
func TestProdWatch_SentryAMissingLinkHeaderIsNeverAWholeRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	h.sentry.pageSize = 2
	h.sentry.noLink = true
	h.sentry.mu.Unlock()
	since := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)["since"]
	now := time.Now()
	for k, id := range []string{"81", "82", "83"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "n", FirstProcessed: now.Add(-time.Duration(k) * time.Second), LastSeen: now})
	}
	o := sentryTick(t, h, wf)
	w := o["poll_sentry"]["walk"].(map[string]any)
	if w["new_complete"] != false {
		t.Fatalf("a page with no Link header was taken for the last one: %v", w)
	}
	if !strings.Contains(fmt.Sprint(o["poll_sentry"]["errors"]), "Link header") {
		t.Fatalf("the walk does not say the Link header is missing: %v", o["poll_sentry"]["errors"])
	}
	if got := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)["since"]; got != since {
		t.Fatalf("the cursor moved past the page it could not reach: %v -> %v", since, got)
	}
	h.sentry.mu.Lock()
	h.sentry.noLink = false
	h.sentry.mu.Unlock()
	if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "new:P-83") {
		t.Fatalf("the page behind the missing Link header was never read once it came back: %v", got)
	}
}

// TestProdWatch_SentryAStampSurvivesACutTransitionList: a watch stamp survives
// a tick whose transition list was CUT (max_issues) with the issue on the
// unread page: the transition, dated past the floor later, still posts.
func TestProdWatch_SentryAStampSurvivesACutTransitionList(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["max_issues"] = 1 }))
	h.sentry.pageSize = 1
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 5*24*time.Hour)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "36", ShortID: strp("P-36"), Title: "x", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Minute)}}})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	if sentryIncident(t, h, "36")["transition_seen_at"] == nil {
		t.Fatalf("setup: P-36 not stamped: %v", sentryIncident(t, h, "36"))
	}
	h.sentry.put(&pwSentryIssue{ID: "37", ShortID: strp("P-37"), Title: "y", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: time.Now(),
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-3 * 24 * time.Hour)}}})
	o := sentryTick(t, h, wf)
	if o["poll_sentry"]["walk"].(map[string]any)["transition_complete"] != false {
		t.Fatalf("setup: the transition list was read whole: %v", o["poll_sentry"]["walk"])
	}
	h.sentry.edit("37", func(i *pwSentryIssue) { i.Status = "resolved" })
	shift := -25 * time.Hour
	sentryEditRecord(t, h, "36", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "first_seen", "last_seen", "transition_checked_at", "tracked_read_at"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
	})
	st := h.state(t)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	cur["armed_at"] = sentryShift(t, cur["armed_at"], shift)
	h.setState(t, st)
	h.sentry.edit("36", func(i *pwSentryIssue) {
		i.Acts[0].At = i.Acts[0].At.Add(shift)
		i.LastSeen = i.LastSeen.Add(shift)
	})
	o = sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "regressed:P-36") {
		t.Fatalf("P-36 was watched from a minute after its regression; a tick whose list was cut before it ended the watch, and the "+
			"regression dated past the floor became history: %v note=%q", sentryAlerts(o), sentryNoteReasons(o))
	}
}

// TestProdWatch_SentryACheckedRecheckEndsTheStamp: a re-check that succeeds
// with no new date ends the watch stamp, so a later transition of the same
// issue (still in the list: an escalation) gets a fresh stamp and its
// exemption from the floor.
func TestProdWatch_SentryACheckedRecheckEndsTheStamp(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 5*24*time.Hour)
	now := time.Now()
	t1 := now.Add(-time.Minute)
	h.sentry.put(&pwSentryIssue{ID: "38", ShortID: strp("P-38"), Title: "x", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: t1, Acts: []pwSentryAct{{Type: "set_regression", At: t1}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "regressed:P-38") {
		t.Fatalf("setup regression: %v", got)
	}
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	if sentryIncident(t, h, "38")["transition_seen_at"] == nil {
		t.Fatalf("setup: not stamped on a failed re-check")
	}
	sentryEditRecord(t, h, "38", func(rec map[string]any) {
		rec["transition_seen_at"] = now.Add(-4 * 24 * time.Hour).UTC().Format(time.RFC3339)
	})
	sentryTick(t, h, wf) // the re-check succeeds, no new date
	t2 := time.Now().Add(-30 * time.Second)
	h.sentry.edit("38", func(i *pwSentryIssue) {
		i.Substatus = strp("escalating")
		i.LastSeen = time.Now()
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_escalating", At: t2})
	})
	h.sentry.failNext("activities", 500, 500)
	sentryTick(t, h, wf)
	shift := -25 * time.Hour
	sentryEditRecord(t, h, "38", func(rec map[string]any) {
		for _, f := range []string{"transition_seen_at", "first_seen", "last_seen", "transition_checked_at", "tracked_read_at", "last_notified", "transition_at", "sentry_last_seen"} {
			rec[f] = sentryShift(t, rec[f], shift)
		}
	})
	st := h.state(t)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	cur["armed_at"] = sentryShift(t, cur["armed_at"], shift)
	h.setState(t, st)
	h.sentry.edit("38", func(i *pwSentryIssue) {
		for k := range i.Acts {
			i.Acts[k].At = i.Acts[k].At.Add(shift)
		}
		i.LastSeen = i.LastSeen.Add(shift)
	})
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "escalating:P-38") {
		t.Fatalf("the escalation watched from 30 s after it happened became history (a stale stamp): %v note=%q", sentryAlerts(o), sentryNoteReasons(o))
	}
}
