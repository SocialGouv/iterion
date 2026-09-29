package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Multi-tick lifecycle witnesses of the Sentry lane, each written from a
// defect an adversarial round reproduced (and seen red on it).

func sentryWithProbe(h *pwHarness, mod func(s map[string]any)) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		sentryOnly(h, mod)(cfg)
		cfg["probes"] = []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}}
	}
}

func ageIncidents(t *testing.T, h *pwHarness, d time.Duration, ids ...string) {
	t.Helper()
	st := h.state(t)
	for _, id := range ids {
		st["incidents"].(map[string]any)["sentry:"+id].(map[string]any)["last_seen"] = time.Now().Add(-d).UTC().Format(time.RFC3339)
	}
	h.setState(t, st)
}

// TestProdWatch_SentryResolvedNoteCutByTheCapRefires: a "resolved" note the
// per-run cap cuts is re-emitted the next tick from the recorded status (the
// issue is no longer read by id once recorded closed).
func TestProdWatch_SentryResolvedNoteCutByTheCapRefires(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "11", ShortID: strp("P-11"), Title: "a", FirstProcessed: now, LastSeen: now, Count: 1})
	h.sentry.put(&pwSentryIssue{ID: "12", ShortID: strp("P-12"), Title: "b", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	h.sentry.edit("11", func(i *pwSentryIssue) { i.Status = "resolved" })
	h.sentry.edit("12", func(i *pwSentryIssue) { i.Status = "resolved" })
	h.alertCap.Store(1)
	first := sentryAlerts(sentryTick(t, h, wf))
	second := sentryAlerts(sentryTick(t, h, wf))
	third := sentryAlerts(sentryTick(t, h, wf))
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] || !strings.HasPrefix(second[0], "resolved:") || len(third) != 0 {
		t.Fatalf("two resolved notes under cap 1: %v, %v, %v — want one each, then nothing", first, second, third)
	}
}

// TestProdWatch_SentryIdentityTypoThenRevertReplaysNothing: a failed re-arming
// (an environment typo) drops the old memory AND its cursor, so reverting the
// config is a silent bootstrap — never a replay against an empty memory.
func TestProdWatch_SentryIdentityTypoThenRevertReplaysNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "21", ShortID: strp("P-21"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	h.sentry.put(&pwSentryIssue{ID: "22", ShortID: strp("P-22"), Title: "r", Substatus: strp("regressed"), FirstProcessed: now.Add(-9 * 24 * time.Hour),
		LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 2 {
		t.Fatalf("setup: %v", got)
	}
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["environment"] = "prepod" }))
	if o := sentryTick(t, h, wf); o["poll_sentry"]["ok"] != false {
		t.Fatalf("the typo did not fail the lane: %v", o["poll_sentry"])
	}
	h.writeConfig(t, sentryWithProbe(h, nil))
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("reverting a failed identity change replayed alerts: %v", got)
	}
}

// TestProdWatch_SentryCutBootstrapNeverArms: a bootstrap cut by the deadline
// or by the page cap records what it read as backlog and does NOT arm — armed
// on a cut read, the next tick's overlap re-reads the unread part and posts it
// as new.
func TestProdWatch_SentryCutBootstrapNeverArms(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["deadline_secs"] = 10 }))
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "31", ShortID: strp("P-31"), Title: "old a", FirstProcessed: now.Add(-20 * time.Minute), LastSeen: now.Add(-time.Minute)})
		h.sentry.put(&pwSentryIssue{ID: "32", ShortID: strp("P-32"), Title: "old b", FirstProcessed: now.Add(-25 * time.Minute), LastSeen: now.Add(-time.Minute)})
		h.sentry.mu.Lock()
		h.sentry.delay = 11 * time.Second
		h.sentry.mu.Unlock()
		sentryTick(t, h, wf)
		if cur := h.state(t)["cursors"].(map[string]any)["sentry"]; cur != nil {
			t.Fatalf("a bootstrap cut by the deadline armed: %v", cur)
		}
		h.sentry.mu.Lock()
		h.sentry.delay = 0
		h.sentry.mu.Unlock()
		if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
			t.Fatalf("pre-arming issues posted as NEW after a deadline-cut bootstrap: %v", got)
		}
	})
	t.Run("page cap", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_issues"] = 1 }))
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "33", ShortID: strp("P-33"), Title: "old a", FirstProcessed: now.Add(-20 * time.Minute), LastSeen: now.Add(-time.Minute)})
		h.sentry.put(&pwSentryIssue{ID: "34", ShortID: strp("P-34"), Title: "old b", FirstProcessed: now.Add(-25 * time.Minute), LastSeen: now.Add(-time.Minute)})
		h.sentry.mu.Lock()
		h.sentry.pageSize = 1
		h.sentry.mu.Unlock()
		sentryTick(t, h, wf)
		if cur := h.state(t)["cursors"].(map[string]any)["sentry"]; cur != nil {
			t.Fatalf("a page-capped bootstrap armed: %v", cur)
		}
		if rec := sentryIncident(t, h, "33"); rec == nil || rec["backlog"] != true {
			t.Fatalf("the page the cut bootstrap read is not backlog: %v", rec)
		}
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_issues"] = 300 }))
		if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
			t.Fatalf("an issue the cut bootstrap left unread posted as NEW: %v", got)
		}
		if cur := h.state(t)["cursors"].(map[string]any)["sentry"]; cur == nil {
			t.Fatal("the complete bootstrap did not arm")
		}
	})
}

// TestProdWatch_SentryClosedNotesNameTheStatus: resolved, archived and deleted
// issues each get the note of their own status.
func TestProdWatch_SentryClosedNotesNameTheStatus(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for status, want := range map[string]string{"resolved": "RESOLVED IN SENTRY", "ignored": "ARCHIVED IN SENTRY", "pending_deletion": "DELETED OR MERGED IN SENTRY"} {
		status, want := status, want
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "41", ShortID: strp("P-41"), Title: "x", FirstProcessed: now, LastSeen: now})
			sentryTick(t, h, wf)
			h.sentry.edit("41", func(i *pwSentryIssue) { i.Status = status })
			n := len(h.bodies())
			if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 1 {
				t.Fatalf("closed %s: %v", status, got)
			}
			if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, want) {
				t.Fatalf("status %s: want %q in\n%s", status, want, body)
			}
		})
	}
}

// TestProdWatch_SentryPendingGoesOutBeforeTheResolvedNote: a pending alert of
// an issue resolved before it posts goes out first, the resolved note after —
// neither drops the other (a pending NEW, and a pending regression resolved
// again).
func TestProdWatch_SentryPendingGoesOutBeforeTheResolvedNote(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	t.Run("pending new", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		sentryTick(t, h, wf)
		h.alertCap.Store(1)
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "51", ShortID: strp("P-51"), Title: "a", Level: "fatal", FirstProcessed: now, LastSeen: now})
		h.sentry.put(&pwSentryIssue{ID: "52", ShortID: strp("P-52"), Title: "b", FirstProcessed: now, LastSeen: now})
		sentryTick(t, h, wf)
		h.sentry.edit("52", func(i *pwSentryIssue) { i.Status = "resolved" })
		var seq []string
		for k := 0; k < 3; k++ {
			seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
		}
		if strings.Join(seq, " ") != "new:P-52:medium resolved:P-52:low" {
			t.Fatalf("pending new then resolved: %v", seq)
		}
	})
	t.Run("pending regression", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		sentryTick(t, h, wf)
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "111", ShortID: strp("P-111"), Title: "x", FirstProcessed: now, LastSeen: now})
		sentryTick(t, h, wf)
		h.sentry.edit("111", func(i *pwSentryIssue) { i.Status = "resolved" })
		sentryTick(t, h, wf)
		h.alertCap.Store(1)
		t1 := time.Now().Add(time.Second)
		h.sentry.edit("111", func(i *pwSentryIssue) {
			i.Status = "unresolved"
			i.Substatus = strp("regressed")
			i.LastSeen = t1
			i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
		})
		h.sentry.put(&pwSentryIssue{ID: "112", ShortID: strp("P-112"), Title: "boom", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
		sentryTick(t, h, wf) // the fatal new issue takes the slot, the regression is pending
		h.sentry.edit("111", func(i *pwSentryIssue) { i.Status = "resolved" })
		var seq []string
		for k := 0; k < 3; k++ {
			seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
		}
		if strings.Join(seq, " ") != "regressed:P-111:medium resolved:P-111:low" {
			t.Fatalf("pending regression then resolved: %v", seq)
		}
	})
}

// TestProdWatch_SentryBusyRegressionsNeverStarveANewOne: already-dated
// regressed issues that keep receiving events are re-checked round-robin
// AFTER the undated ones, and a deferred re-check is not a coverage loss — a
// fresh regression is dated and posted, and the lane stays complete.
func TestProdWatch_SentryBusyRegressionsNeverStarveANewOne(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 2 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"75", "76"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "busy", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	}
	sentryTick(t, h, wf)
	reg := time.Now().Add(time.Second)
	h.sentry.put(&pwSentryIssue{ID: "77", ShortID: strp("P-77"), Title: "quiet one", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: reg, Acts: []pwSentryAct{{Type: "set_regression", At: reg}}})
	var all []string
	truncated := false
	for k := 0; k < 3; k++ {
		for _, id := range []string{"75", "76"} {
			h.sentry.edit(id, func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Duration(10+k) * time.Second) })
		}
		o := sentryTick(t, h, wf)
		all = append(all, sentryAlerts(o)...)
		if o["poll_sentry"]["truncated"] == true {
			truncated = true
		}
	}
	if !strings.Contains(strings.Join(all, " "), "regressed:P-77") {
		t.Fatalf("a regression after the arming was never posted in 3 ticks: %v", all)
	}
	if truncated {
		t.Fatal("deferred re-checks of dated regressions made the walk partial")
	}
}

// TestProdWatch_SentryPreArmingTransitionsAreDatedOnce: regressions dated
// before the arming are recorded as history whatever the record, so they are
// not re-dated every tick (three of them under a cap of one settle).
func TestProdWatch_SentryPreArmingTransitionsAreDatedOnce(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	now := time.Now()
	for k, id := range []string{"61", "62", "63"} {
		reg := now.Add(-3*time.Hour - time.Duration(k)*time.Minute)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: reg, Acts: []pwSentryAct{{Type: "set_regression", At: reg}}})
	}
	var last map[string]map[string]any
	for k := 0; k < 5; k++ {
		last = sentryTick(t, h, wf)
		if got := sentryAlerts(last); len(got) != 0 {
			t.Fatalf("a pre-arming regression posted: %v", got)
		}
	}
	// Settled: every one is dated (history), the re-checks take turns under the
	// cap and are no coverage loss.
	before := len(h.sentry.callsTo("activities"))
	last = sentryTick(t, h, wf)
	if after := len(h.sentry.callsTo("activities")); after-before > 1 || last["poll_sentry"]["truncated"] == true {
		t.Fatalf("settled pre-arming regressions: %d activity calls under a cap of 1, truncated=%v", after-before, last["poll_sentry"]["truncated"])
	}
	for _, id := range []string{"61", "62", "63"} {
		if rec := sentryIncident(t, h, id); rec == nil || rec["transition_at"] == nil {
			t.Fatalf("pre-arming regression %s never recorded as history: %v", id, rec)
		}
	}
}

// TestProdWatch_SentryStaleCursorOpensAtTheCatchupFloor: a cursor older than
// max_catchup_hours (the lane turned off for weeks) opens at the floor and
// declares the gap — never a flood of days-old issues posted as NEW.
func TestProdWatch_SentryStaleCursorOpensAtTheCatchupFloor(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	sentryTick(t, h, wf)
	st := h.state(t)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	old := time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339)
	cur["since"], cur["armed_at"], cur["at"] = old, old, old
	h.setState(t, st)
	now := time.Now()
	for k := 0; k < 25; k++ {
		id := fmt.Sprint(800 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "backlog " + id,
			FirstProcessed: now.Add(-time.Duration(11+k) * 24 * time.Hour / 10), LastSeen: now}) // 1.1 to 3.5 days old
	}
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); len(got) != 0 {
		t.Fatalf("a stale cursor posted days-old issues as NEW: %d (%v)", len(got), got)
	}
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "max\\_catchup\\_hours") && !strings.Contains(body, "max_catchup_hours") {
		t.Fatalf("the gap was not declared in the coverage note:\n%s", body)
	}
}

// TestProdWatch_SentryLaneThatNeverAnsweredIsNotHealthy: a tick where no Sentry
// query answered (the deadline cut them all) does not stamp the lane healthy,
// so the staleness note can fire; alone, it refuses the tick naming why.
func TestProdWatch_SentryLaneThatNeverAnsweredIsNotHealthy(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	t.Run("health", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["deadline_secs"] = 10 }))
		sentryTick(t, h, wf)
		st := h.state(t)
		old := time.Now().Add(-7 * time.Hour).UTC().Format(time.RFC3339)
		st["health"].(map[string]any)["sentry"].(map[string]any)["last_ok"] = old
		h.setState(t, st)
		h.sentry.mu.Lock()
		h.sentry.delay = 11 * time.Second
		h.sentry.mu.Unlock()
		o := sentryTick(t, h, wf)
		if last := h.state(t)["health"].(map[string]any)["sentry"].(map[string]any)["last_ok"]; last != old {
			t.Fatalf("a tick where no Sentry query answered stamped the lane healthy (last_ok %v)", last)
		}
		if s := fmt.Sprint(o["decide"]["stale_sources"]); !strings.Contains(s, "sentry") {
			t.Fatalf("no staleness note for a lane silent 7 h: %v", s)
		}
	})
	t.Run("alone", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["deadline_secs"] = 10 }))
		sentryTick(t, h, wf)
		h.sentry.mu.Lock()
		h.sentry.delay = 11 * time.Second
		h.sentry.mu.Unlock()
		vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
		secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
		plan, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		s, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_sentry").Script, map[string]any{"sentry": plan["sentry"],
			"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		leak, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{"raw_file": filepath.Join(h.scratch, "none"),
			"per_query": map[string]any{}, "sentry_file": s["raw_file"], "sentry_issues": s["issues"], "app": plan["app"], "scratch_dir": h.scratch}, vars, secrets))
		if err != nil {
			t.Fatal(err)
		}
		_, stderr, err := pwDecide(t, wf, h, map[string]any{}, nil, map[string]any{"signals_file": leak["signals_file"],
			"lanes": plan["lanes"], "sentry": plan["sentry"], "sentry_ok": s["ok"], "sentry_truncated": s["truncated"],
			"sentry_errors": s["errors"], "sentry_walk": s["walk"], "sentry_issues": s["issues"], "loki_per_query": map[string]any{}})
		if err == nil || !strings.Contains(stderr, "deadline") {
			t.Fatalf("a Sentry-only tick cut by the deadline was not refused naming why: err=%v %s", err, stderr)
		}
	})
}

// TestProdWatch_SentryKnownIssueRegressionPassesTheLevelFloor: the regression of
// an issue the lane knows posts even when its latest event is below min_level
// (the floor decides what the lane STARTS tracking).
func TestProdWatch_SentryKnownIssueRegressionPassesTheLevelFloor(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "91", ShortID: strp("P-91"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("91", func(i *pwSentryIssue) { i.Status = "resolved" })
	sentryTick(t, h, wf)
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("91", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("regressed")
		i.Level = "warning"
		i.LastSeen = t1
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:P-91:medium" {
		t.Fatalf("the regression of a known issue below the level floor: %v", got)
	}
}

// TestProdWatch_SentryMinLevelChangeKeepsPending: min_level is not part of the
// lane identity — tightening it (the documented flood remedy) re-arms nothing
// and drops no pending alert.
func TestProdWatch_SentryMinLevelChangeKeepsPending(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "131", ShortID: strp("P-131"), Title: "a", Level: "fatal", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "132", ShortID: strp("P-132"), Title: "b", Level: "fatal", FirstProcessed: now, LastSeen: now})
	first := sentryAlerts(sentryTick(t, h, wf))
	armed := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)["armed_at"]
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["min_level"] = "fatal" }))
	second := sentryAlerts(sentryTick(t, h, wf))
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] {
		t.Fatalf("pending across a min_level change: %v then %v", first, second)
	}
	if again := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)["armed_at"]; again != armed {
		t.Fatalf("a min_level change re-armed the lane: armed_at %v -> %v", armed, again)
	}
}

// TestProdWatch_SentrySinceHoldsOnAFailedNewList: an issue first processed
// while the new-issue list failed is read on the next tick (with no overlap to
// hide a cursor that moved on).
func TestProdWatch_SentrySinceHoldsOnAFailedNewList(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["overlap_minutes"] = 0 }))
	sentryTick(t, h, wf)
	time.Sleep(1100 * time.Millisecond)
	h.sentry.put(&pwSentryIssue{ID: "401", ShortID: strp("P-401"), Title: "x", FirstProcessed: time.Now(), LastSeen: time.Now()})
	time.Sleep(1100 * time.Millisecond)
	h.sentry.failNext("list", 500, 500)
	sentryTick(t, h, wf)
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 1 {
		t.Fatalf("an issue first processed while the new list failed was lost: %v", got)
	}
}

// TestProdWatch_SentryResolvedIsTheOnlyNote: after the resolved note, an idle
// period brings no second note.
func TestProdWatch_SentryResolvedIsTheOnlyNote(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "701", ShortID: strp("P-701"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("701", func(i *pwSentryIssue) { i.Status = "resolved" })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 1 {
		t.Fatalf("resolved: %v", got)
	}
	ageIncidents(t, h, 49*time.Hour, "701")
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a second note followed the resolved note: %v", got)
	}
}

// TestProdWatch_SentryPendingOfAVanishedIssueStillGoesOut: a pending alert
// whose issue is no longer read (deleted, merged) is re-emitted from its short
// id and link.
func TestProdWatch_SentryPendingOfAVanishedIssueStillGoesOut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "291", ShortID: strp("P-291"), Title: "a", Level: "fatal", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "292", ShortID: strp("P-292"), Title: "b", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	delete(h.sentry.issues, "292")
	h.sentry.mu.Unlock()
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-292:medium" {
		t.Fatalf("the pending alert of a vanished issue: %v", got)
	}
}

// TestProdWatch_SentryRetentionForgetsTheIdleKeepsThePending: an alerted issue
// merely re-read by id for forget_after_days gets its note and leaves the
// tracked set; a pending one stays and goes out.
func TestProdWatch_SentryRetentionForgetsTheIdleKeepsThePending(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "41", ShortID: strp("P-41"), Title: "alerted", Level: "fatal", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "42", ShortID: strp("P-42"), Title: "pending", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.alertCap.Store(0)
	ageIncidents(t, h, 15*24*time.Hour, "41", "42")
	got := sentryAlerts(sentryTick(t, h, wf))
	if strings.Join(got, " ") != "new:P-42:medium quiet:P-41:low" {
		t.Fatalf("retention tick: %v", got)
	}
	if sentryIncident(t, h, "41") != nil {
		t.Fatal("an alerted issue idle for forget_after_days is still tracked")
	}
	if sentryIncident(t, h, "42") == nil {
		t.Fatal("a pending incident was forgotten")
	}
}

// TestProdWatch_SentryTransitionDeferredTwicePostsOnce and the quiet note cut
// by the cap: held behaviours kept as witnesses.
func TestProdWatch_SentryTransitionDeferredTwicePostsOnce(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "60", ShortID: strp("P-60"), Title: "r", Substatus: strp("regressed"), FirstProcessed: now.Add(-9 * 24 * time.Hour),
		LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	var seq []string
	for k := 0; k < 2; k++ {
		id := fmt.Sprint(61 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "f", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
		seq = append(seq, strings.Join(sentryAlerts(sentryTick(t, h, wf)), " "))
	}
	seq = append(seq, strings.Join(sentryAlerts(sentryTick(t, h, wf)), " "), strings.Join(sentryAlerts(sentryTick(t, h, wf)), " "))
	if seq[2] != "regressed:P-60:medium" || seq[3] != "" {
		t.Fatalf("a transition deferred twice: %q", seq)
	}
}
