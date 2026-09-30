package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sentryHistoryIssues puts n issues that regressed 25 h ago — past the
// catch-up floor, after an arming 30 h old: history, named in the notes.
func sentryHistoryIssues(h *pwHarness, base, n int) []string {
	now := time.Now()
	var ids []string
	for k := 0; k < n; k++ {
		id := fmt.Sprint(base + k)
		ids = append(ids, "P-"+id)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Duration(k+1) * time.Second),
			Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-25 * time.Hour)}}})
	}
	return ids
}

// TestProdWatch_SentryCarriedHistoryYieldsToALaneError: carried history comes
// after the losses in the coverage note: a lane error (the token refused)
// is named on its first tick, whatever history is carried.
func TestProdWatch_SentryCarriedHistoryYieldsToALaneError(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["max_transition_checks"] = 40 }))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	sentryHistoryIssues(h, 701, 12)
	sentryTick(t, h, wf)
	if un, _ := h.state(t)["sentry_history_unsaid"].([]any); len(un) < 3 {
		t.Fatalf("setup: want three or more carried names, got %v", un)
	}
	h.sentry.failNext("project", 401)
	o := sentryTick(t, h, wf)
	if r := sentryNoteReasons(o); !strings.Contains(r, "CredentialRefused") {
		t.Fatalf("the token refused this tick, and the coverage note names carried history instead: %q", r)
	}
}

// TestProdWatch_SentryHistoryPastTheCarryIsCounted: past the carry's 100
// names, the oldest go — counted, and the coverage note says how many ahead
// of the names, whatever losses take part of its budget (an activity
// lookup failing here).
func TestProdWatch_SentryHistoryPastTheCarryIsCounted(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 200; s["max_issues"] = 200 }))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	ids := sentryHistoryIssues(h, 701, 130)
	h.sentry.failNext("activities", 500, 500) // one transition stays undated: 129 history
	o := sentryTick(t, h, wf)
	reasons := sentryNoteReasons(o)
	if !strings.Contains(reasons, "sentry transition: HTTPError") {
		t.Fatalf("setup: the failed lookup is not in the note: %q", reasons)
	}
	carried := fmt.Sprint(h.state(t)["sentry_history_unsaid"])
	named := 0
	for _, id := range ids {
		if strings.Contains(reasons, id+" regressed") || strings.Contains(carried, id+"@") {
			named++
		}
	}
	m := regexp.MustCompile(`sentry: (\d+) more transition\(s\) recorded as history`).FindStringSubmatch(reasons)
	if m == nil || named != 100 {
		t.Fatalf("129 history transitions: %d named or carried (want 100), and the note must count the others: %q", named, reasons)
	}
	if n, _ := strconv.Atoi(m[1]); named+n != 129 {
		t.Fatalf("the note counts %d past the carry, %d are named or carried: %d lost uncounted", n, named, 129-named-n)
	}
	if d, left := h.state(t)["sentry_history_dropped"]; left {
		t.Fatalf("the count was said, and is still carried: %v", d)
	}
}

// TestProdWatch_SentrySaidHistoryLeavesTheCarry: once every carried name was
// said, the carry is empty and the tick's coverage is full again.
func TestProdWatch_SentrySaidHistoryLeavesTheCarry(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	sentryHistoryIssues(h, 701, 7)
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	o := sentryTick(t, h, wf)
	un, _ := h.state(t)["sentry_history_unsaid"].([]any)
	if cov := sentryTickCoverage(t, o); len(un) != 0 || cov != "full" {
		t.Fatalf("every name said by the third tick; the fourth still carries %d (%v), coverage %q", len(un), un, cov)
	}
}

// TestProdWatch_SentryIdentityChangeDropsCarriedHistory: another project or
// environment drops the old identity's carried names.
func TestProdWatch_SentryIdentityChangeDropsCarriedHistory(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	sentryHistoryIssues(h, 701, 7)
	sentryTick(t, h, wf)
	if un, _ := h.state(t)["sentry_history_unsaid"].([]any); len(un) == 0 {
		t.Fatalf("setup: nothing carried")
	}
	st := h.state(t)
	st["sentry_history_dropped"] = 7 // as if past the carry
	h.setState(t, st)
	h.sentry.envs["staging"] = true
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["environment"] = "staging" }))
	var said []string
	for k := 0; k < 2; k++ {
		said = append(said, sentryNoteReasons(sentryTick(t, h, wf)))
	}
	if j := strings.Join(said, " | "); strings.Contains(j, "P-70") || strings.Contains(j, "7 more transition(s)") {
		t.Fatalf("after the identity change, notes still speak of the old environment's history: %s", j)
	}
}

// TestProdWatch_SentryHistoryNoteSaysWhen: the history note says when each
// transition happened.
func TestProdWatch_SentryHistoryNoteSaysWhen(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryArmedAgo(t, h, 30*time.Hour)
	now := time.Now()
	at := now.Add(-25 * time.Hour)
	h.sentry.put(&pwSentryIssue{ID: "701", ShortID: strp("P-701"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
		Acts: []pwSentryAct{{Type: "set_regression", At: at}}})
	r := sentryNoteReasons(sentryTick(t, h, wf))
	if want := "P-701 regressed or escalated at " + at.UTC().Format("2006-01-02T15:04"); !strings.Contains(r, want) {
		t.Fatalf("the history note does not say when (%q): %q", want, r)
	}
}

// TestProdWatch_SentryForeignHistoryCarryIsRefusedByName: plan refuses a
// history carry it did not write — another type, an entry of another shape
// (one longer than the note's budget would never be said, and keep the
// coverage partial for ever), a count that is not one — by name, never a
// traceback.
func TestProdWatch_SentryForeignHistoryCarryIsRefusedByName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, c := range map[string]struct {
		key string
		v   any
	}{
		"an int":                    {"sentry_history_unsaid", 5},
		"a string":                  {"sentry_history_unsaid", "P-1@2026-09-29T10:00"},
		"an entry past the budget":  {"sentry_history_unsaid", []any{"P-1@" + strings.Repeat("9", 700)}},
		"an entry with a space":     {"sentry_history_unsaid", []any{"P 1@2026-09-29T10:00"}},
		"an entry without its date": {"sentry_history_unsaid", []any{"P-1"}},
		"a negative dropped count":  {"sentry_history_dropped", -1},
		"a dropped count as text":   {"sentry_history_dropped", "3"},
	} {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			h.setState(t, map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{}, "health": map[string]any{},
				"incidents": map[string]any{}, c.key: c.v})
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
			if err == nil || !strings.Contains(stderr, c.key) || strings.Contains(stderr, "Traceback") {
				t.Fatalf("a foreign %s (%s) was not refused by name: err=%v %s", c.key, name, err, stderr)
			}
		})
	}
}
