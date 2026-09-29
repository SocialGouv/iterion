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

func sentryCursor(t *testing.T, h *pwHarness) map[string]any {
	t.Helper()
	cur, _ := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)
	return cur
}

// TestProdWatch_SentryLeakCutByTheCapIsReEmitted: a Sentry leak alert the
// per-run cap cuts is re-emitted from its record until it posts, once — the
// issue that carried it is not sighted again, so nothing else would bring it
// back, while the overflow note promised it.
func TestProdWatch_SentryLeakCutByTheCapIsReEmitted(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "51", ShortID: strp("P-51"), Title: "boom", Level: "fatal", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "52", ShortID: strp("P-52"), Title: "UserNotFound: helene.zq7rtx@qz9mail.fr", FirstProcessed: now, LastSeen: now})
	var seq []string
	for k := 0; k < 4; k++ {
		seq = append(seq, strings.Join(sentryAlerts(sentryTick(t, h, wf)), " "))
	}
	if want := []string{"new:P-51:high", "new:leak-email:high", "new:P-52:medium", ""}; !eqStrings(seq, want) {
		t.Fatalf("cap 1: want %q, got %q", want, seq)
	}
}

// TestProdWatch_SentryCatchUpFloorBoundsTransitions: a transition dated before
// the catch-up floor is history, not news — whether the lane was off for days
// (its cursor and its arming far behind) or a lowered level floor admits
// issues the lane never knew.
func TestProdWatch_SentryCatchUpFloorBoundsTransitions(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	t.Run("a cursor twenty days old", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		sentryTick(t, h, wf)
		st := h.state(t)
		cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
		old := time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339)
		cur["since"], cur["armed_at"], cur["at"] = old, old, old
		h.setState(t, st)
		now := time.Now()
		for k := 0; k < 5; k++ {
			id := fmt.Sprint(900 + k)
			// 2 to 6 days ago: still regressed (Sentry turns it ongoing after 7 days).
			h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "old regression", Substatus: strp("regressed"),
				FirstProcessed: now.Add(-60 * 24 * time.Hour), LastSeen: now.Add(-time.Hour),
				Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Duration(2+k) * 24 * time.Hour)}}})
		}
		o := sentryTick(t, h, wf)
		walk, _ := o["poll_sentry"]["walk"].(map[string]any)
		if got := sentryAlerts(o); len(got) != 0 || walk["gap"] != true {
			t.Fatalf("a cursor 20 days old: regressions 2-6 days old posted as news (%v), or no gap declared (%v)", got, walk["gap"])
		}
		if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
			t.Fatalf("the history was not recorded: the next tick posted %v", got)
		}
	})
	t.Run("a lowered level floor", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, nil))
		sentryTick(t, h, wf)
		st := h.state(t)
		st["cursors"].(map[string]any)["sentry"].(map[string]any)["armed_at"] = time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
		h.setState(t, st)
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "55", ShortID: strp("P-55"), Title: "w", Level: "warning", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Hour),
			Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-3 * 24 * time.Hour)}}})
		h.sentry.put(&pwSentryIssue{ID: "56", ShortID: strp("P-56"), Title: "w", Level: "warning", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute),
			Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-time.Hour)}}})
		if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
			t.Fatalf("setup: warnings below min_level error posted: %v", got)
		}
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["min_level"] = "warning" }))
		if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"regressed:P-56:low"}) {
			t.Fatalf("lowering min_level: want only the regression of the last hour, got %v", got)
		}
	})
}

// TestProdWatch_SentryIdentityChangeDropsAnUnarmedBootstrap: an identity change
// during a bootstrap that never armed (no cursor yet) drops what that
// bootstrap recorded — or an issue id shared across environments would never
// post NEW in the new one.
func TestProdWatch_SentryIdentityChangeDropsAnUnarmedBootstrap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.sentry.mu.Lock()
	h.sentry.envs["staging"] = true
	h.sentry.pageSize = 1
	h.sentry.mu.Unlock()
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "71", ShortID: strp("P-71"), Title: "x", FirstProcessed: now.Add(-5 * time.Minute), LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "72", ShortID: strp("P-72"), Title: "y", FirstProcessed: now.Add(-10 * time.Minute), LastSeen: now})
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_issues"] = 1 }))
	sentryTick(t, h, wf) // preprod bootstrap, cut: records 71, does not arm
	if sentryCursor(t, h) != nil || sentryIncident(t, h, "71") == nil {
		t.Fatalf("setup: the cut bootstrap armed (%v) or recorded nothing", sentryCursor(t, h))
	}
	h.sentry.mu.Lock()
	h.sentry.pageSize = 100
	h.sentry.mu.Unlock()
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["environment"] = "staging" }))
	sentryTick(t, h, wf) // staging bootstrap: empty, complete, arms
	if cur := sentryCursor(t, h); cur == nil || cur["identity"].(map[string]any)["environment"] != "staging" {
		t.Fatalf("setup: staging did not arm: %v", cur)
	}
	if sentryIncident(t, h, "71") != nil {
		t.Fatal("the unarmed preprod bootstrap's record survived the switch to staging")
	}
	// Issue 71's first staging event, after the arming: NEW in staging.
	h.sentry.edit("71", func(i *pwSentryIssue) { i.Env = "staging"; i.FirstProcessed = time.Now(); i.LastSeen = time.Now() })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:P-71:medium" {
		t.Fatalf("an issue first seen in the new environment after its arming did not post NEW: %v", got)
	}
}

// TestProdWatch_SentryGapOnlyBelowTheFloor: only a cursor itself below the
// catch-up floor is a gap — the overlap below it was read by the previous
// tick. A cursor 23.5 h old (floor 24 h) declares none, and the lane stays
// observed: a Sentry leak idle for 49 h gets its quiet note.
func TestProdWatch_SentryGapOnlyBelowTheFloor(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "66", ShortID: strp("P-66"), Title: "UserNotFound: helene.zq7rtx@qz9mail.fr", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); !strings.Contains(strings.Join(got, " "), "new:leak-email") {
		t.Fatalf("setup: %v", got)
	}
	st := h.state(t)
	st["incidents"].(map[string]any)["sentry_leak:email"].(map[string]any)["last_seen"] = time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	cur := st["cursors"].(map[string]any)["sentry"].(map[string]any)
	c := time.Now().Add(-23*time.Hour - 30*time.Minute).UTC().Format(time.RFC3339)
	cur["since"], cur["at"] = c, c
	h.setState(t, st)
	o := sentryTick(t, h, wf)
	walk, _ := o["poll_sentry"]["walk"].(map[string]any)
	if walk["gap"] == true || o["poll_sentry"]["truncated"] == true {
		t.Fatalf("a cursor 23.5 h old (floor 24 h) declared a gap or a partial walk: %v", walk["partial"])
	}
	if got := sentryAlerts(o); !strings.Contains(strings.Join(got, " "), "quiet:leak-email") {
		t.Fatalf("a Sentry leak idle 49 h got no quiet note: %v", got)
	}
}

// TestProdWatch_SentryTrackedSetTakesTurns: over max_tracked the alerted
// incidents read by id take turns, least recently read first — the least
// recently sighted one is read again and its resolution noted while busier
// incidents keep being sighted.
func TestProdWatch_SentryTrackedSetTakesTurns(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 2 }))
	sentryTick(t, h, wf)
	// The quiet one has the LARGEST id: neither the recency of its sightings
	// nor the id order can give it a slot — only its turn.
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "95", ShortID: strp("P-95"), Title: "a", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	time.Sleep(1100 * time.Millisecond)
	h.sentry.put(&pwSentryIssue{ID: "31", ShortID: strp("P-31"), Title: "b", FirstProcessed: time.Now(), LastSeen: time.Now()})
	h.sentry.put(&pwSentryIssue{ID: "38", ShortID: strp("P-38"), Title: "c", FirstProcessed: time.Now(), LastSeen: time.Now()})
	sentryTick(t, h, wf)
	h.sentry.edit("95", func(i *pwSentryIssue) { i.Status = "resolved" })
	var all []string
	for k := 0; k < 4; k++ {
		time.Sleep(1100 * time.Millisecond) // tracked_read_at has a one-second resolution
		for _, id := range []string{"31", "38"} {
			h.sentry.edit(id, func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Duration(k+1) * time.Second) })
		}
		all = append(all, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if strings.Join(all, " ") != "resolved:P-95:low" {
		t.Fatalf("over max_tracked, the least recently read issue must be read again and its resolution noted once: %v", all)
	}
}

// TestProdWatch_SentryPostedTransitionReArmsTheClosingNote: a transition posted
// without a sighting (the substatus is project-wide, the event came from
// another environment) re-arms the closing note a previous resolution
// consumed — the next resolution is noted.
func TestProdWatch_SentryPostedTransitionReArmsTheClosingNote(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "81", ShortID: strp("P-81"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("81", func(i *pwSentryIssue) { i.Status = "resolved" })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-81:low" {
		t.Fatalf("setup resolved: %v", got)
	}
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("81", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("regressed")
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1}) // lastSeen in the watched environment unchanged
	})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:P-81:medium" {
		t.Fatalf("setup regression: %v", got)
	}
	h.sentry.edit("81", func(i *pwSentryIssue) { i.Status = "resolved" })
	var seq []string
	for k := 0; k < 2; k++ {
		seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if strings.Join(seq, " ") != "resolved:P-81:low" {
		t.Fatalf("the regression's resolution was not noted once: %v", seq)
	}
}

// TestProdWatch_SentryArchivedIssueStaysTracked: an archived issue Sentry
// reopens as ongoing (a snooze expiring, an unarchive) is still read by id
// after its "archived" note — its new events are sightings again.
func TestProdWatch_SentryArchivedIssueStaysTracked(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "91", ShortID: strp("P-91"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	// Hours later (out of the new-issue window), archived.
	h.sentry.edit("91", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour); i.Status = "ignored" })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-91:low" {
		t.Fatalf("setup archived note: %v", got)
	}
	h.sentry.edit("91", func(i *pwSentryIssue) {
		i.Status = "unresolved"
		i.Substatus = strp("ongoing")
		i.LastSeen = time.Now().Add(time.Second)
		i.Count += 40
	})
	st := h.state(t)
	st["incidents"].(map[string]any)["sentry:91"].(map[string]any)["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reminder:P-91:medium" {
		t.Fatalf("a reopened archived issue firing again (last notified 25 h ago) was not read as a sighting: %v", got)
	}
}

// TestProdWatch_SentryUnarmedLaneSaysSoAndIsNotHealthy: while more issues are
// first seen per overlap window than max_issues, the bootstrap never arms and
// the lane posts nothing — a fatal NEW issue included. The channel is told
// (NOT ARMED) and the lane's health is not stamped.
func TestProdWatch_SentryUnarmedLaneSaysSoAndIsNotHealthy(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_issues"] = 1 }))
	h.sentry.mu.Lock()
	h.sentry.pageSize = 1
	h.sentry.mu.Unlock()
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "11", ShortID: strp("P-11"), Title: "a", FirstProcessed: now.Add(-5 * time.Minute), LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "12", ShortID: strp("P-12"), Title: "b", FirstProcessed: now.Add(-6 * time.Minute), LastSeen: now})
	for k := 0; k < 2; k++ {
		sentryTick(t, h, wf)
	}
	h.sentry.put(&pwSentryIssue{ID: "13", ShortID: strp("P-13"), Title: "fatal thing", Level: "fatal", FirstProcessed: time.Now(), LastSeen: time.Now()})
	o := sentryTick(t, h, wf)
	if cur := sentryCursor(t, h); cur != nil {
		t.Fatalf("setup: the cut bootstrap armed: %v", cur)
	}
	if got := sentryAlerts(o); len(got) != 0 {
		t.Fatalf("an unarmed lane posted: %v", got)
	}
	healths, _ := h.state(t)["health"].(map[string]any)
	health, _ := healths["sentry"].(map[string]any)
	if health["last_ok"] != nil {
		t.Fatalf("an unarmed lane was stamped healthy: %v", health)
	}
	if body := strings.Join(h.bodies(), "\n"); !strings.Contains(body, "NOT ARMED") {
		t.Fatalf("the channel was never told the lane is not armed:\n%s", body)
	}
}

// TestProdWatch_SentryArmingAndSinceFollowTheNewList: the lane arms, and its
// cursor advances, once the new-issue list was read whole — an activity
// lookup failing, once or every tick, holds neither.
func TestProdWatch_SentryArmingAndSinceFollowTheNewList(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	t.Run("since advances past a failed activity lookup", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryWithProbe(h, nil))
		sentryTick(t, h, wf)
		since1 := sentryCursor(t, h)["since"]
		time.Sleep(1100 * time.Millisecond)
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "21", ShortID: strp("P-21"), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-9 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
		h.sentry.put(&pwSentryIssue{ID: "22", ShortID: strp("P-22"), Title: "n", FirstProcessed: now, LastSeen: now})
		h.sentry.failNext("activities", 500, 500)
		o := sentryTick(t, h, wf)
		walk, _ := o["poll_sentry"]["walk"].(map[string]any)
		if walk["new_complete"] != true || o["poll_sentry"]["ok"] != false {
			t.Fatalf("setup: want a complete new list and a failed activity lookup: %v", o["poll_sentry"])
		}
		if since2 := sentryCursor(t, h)["since"]; since2 == since1 {
			t.Fatalf("the new-issue list was read whole but since held at %v", since1)
		}
	})
	t.Run("a bootstrap arms while an activity lookup keeps failing", func(t *testing.T) {
		t.Parallel()
		h := newPWHarness(t)
		h.writeConfig(t, sentryWithProbe(h, nil))
		now := time.Now()
		h.sentry.put(&pwSentryIssue{ID: "41", ShortID: strp("P-41"), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-9 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(-3 * time.Hour)}}})
		var all []string
		for k := 0; k < 3; k++ {
			h.sentry.failNext("activities", 500, 500)
			all = append(all, sentryAlerts(sentryTick(t, h, wf))...)
			if k == 0 && sentryCursor(t, h) == nil {
				t.Fatal("a bootstrap whose new-issue list was read whole did not arm: one activity lookup failed")
			}
		}
		if len(all) != 0 {
			t.Fatalf("the pre-arming regression posted after the arming: %v", all)
		}
	})
}

// TestProdWatch_SentryBootstrapDatesUncheckedTransitionsAtTheArming: a
// regression the bootstrap READ but left undated (the check cap) is dated at
// the arming — history, like the one it did date — never posted afterwards
// because the cap happened to reach another issue first.
func TestProdWatch_SentryBootstrapDatesUncheckedTransitionsAtTheArming(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	now := time.Now()
	reg := now.Add(-10 * time.Minute)
	for _, id := range []string{"61", "62"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now.Add(-time.Minute), Acts: []pwSentryAct{{Type: "set_regression", At: reg}}})
	}
	sentryTick(t, h, wf) // bootstrap: dates one, the cap leaves the other undated; arms
	if sentryCursor(t, h) == nil {
		t.Fatal("setup: the bootstrap did not arm")
	}
	var seq []string
	for k := 0; k < 3; k++ {
		seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if len(seq) != 0 {
		t.Fatalf("a regression the bootstrap read posted after the arming: %v", seq)
	}
}

// TestProdWatch_SentryPendingGoesAheadOfFreshAlerts: a Sentry alert the cap
// cut goes out before this tick's fresh alerts of the same rank — one fresh
// issue per tick must not hold a pending regression back for ever.
func TestProdWatch_SentryPendingGoesAheadOfFreshAlerts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(1)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "9", ShortID: strp("P-9"), Title: "real regression", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	var seq []string
	for k := 0; k < 3; k++ {
		id := fmt.Sprint(10 + k) // "10" sorts before "9"
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "fresh", FirstProcessed: time.Now(), LastSeen: time.Now()})
		seq = append(seq, strings.Join(sentryAlerts(sentryTick(t, h, wf)), " "))
	}
	if want := []string{"new:P-10:medium", "regressed:P-9:medium", "new:P-11:medium"}; !eqStrings(seq, want) {
		t.Fatalf("cap 1, one fresh same-rank issue per tick: want %q, got %q", want, seq)
	}
}

// TestProdWatch_SentryRetentionKeepsATransitioningIssue: an issue still in the
// regressed list keeps its record (and its dated transition) whatever its
// age — forgotten, the same regression would be re-dated and posted again.
func TestProdWatch_SentryRetentionKeepsATransitioningIssue(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "77", ShortID: strp("P-77"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:P-77:medium" {
		t.Fatalf("setup: %v", got)
	}
	// Aged past forget_after_days (14 in the harness): an operator's shorter
	// retention than the issue's listing life.
	ageIncidents(t, h, 15*24*time.Hour, "77")
	var seq []string
	for k := 0; k < 3; k++ {
		seq = append(seq, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if strings.Contains(strings.Join(seq, " "), "regressed:P-77") || sentryIncident(t, h, "77") == nil {
		t.Fatalf("retention forgot an issue still in the regressed list (record kept: %v): %v", sentryIncident(t, h, "77") != nil, seq)
	}
}

// TestProdWatch_SentryReprocessingIsOpen: an alerted issue being reprocessed is
// still open — no closing note, still read by id — and its later resolution
// gets the note.
func TestProdWatch_SentryReprocessingIsOpen(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "3401", ShortID: strp("P-3401"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("3401", func(i *pwSentryIssue) { i.Status = "reprocessing" })
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("an issue being reprocessed was noted closed: %v", got)
	}
	h.sentry.edit("3401", func(i *pwSentryIssue) { i.Status = "resolved" })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "resolved:P-3401:low" {
		t.Fatalf("the resolution after the reprocessing was not noted: %v", got)
	}
}

// TestProdWatch_SentryCountLabelNamesItsScope: the lists count events over
// their 14 days, the by-id answer over the issue's whole life — a NEW alert
// says "in 14 days", a reminder read by id only says "in total".
func TestProdWatch_SentryCountLabelNamesItsScope(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "3301", ShortID: strp("P-3301"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 7})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if b := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(b, "event(s) in 14 days") {
		t.Fatalf("the NEW alert does not name the lists' 14 days:\n%s", b)
	}
	// Hours later, out of the new-issue window: read by id only.
	h.sentry.edit("3301", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.LastSeen = time.Now().Add(time.Second)
		i.Count = 90
	})
	st := h.state(t)
	st["incidents"].(map[string]any)["sentry:3301"].(map[string]any)["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	n = len(h.bodies())
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reminder:P-3301:medium" {
		t.Fatalf("setup: want a reminder, got %v", got)
	}
	if b := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(b, "event(s) in total") {
		t.Fatalf("a count read by id does not say \"in total\":\n%s", b)
	}
}

// TestProdWatch_SentryBaseURLIsComparedAsTheHostItNames: a cosmetic edit of the
// base URL (host case, the scheme's default port, a trailing slash) names the
// same Sentry — the same identity, the same link prefix — and never re-arms
// the lane (which drops every incident and pending alert).
func TestProdWatch_SentryBaseURLIsComparedAsTheHostItNames(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	plan := func(base string) map[string]any {
		t.Helper()
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["base_url"] = base }))
		vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
			"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
		if err != nil {
			t.Fatalf("plan %q: %v %s", base, err, stderr)
		}
		return out["sentry"].(map[string]any)
	}
	a := plan("https://sentry.example")
	for _, base := range []string{"https://Sentry.EXAMPLE:443/", "https://SENTRY.example"} {
		b := plan(base)
		if fmt.Sprint(a["identity"]) != fmt.Sprint(b["identity"]) || a["identity_key"] != b["identity_key"] || a["link_prefix"] != b["link_prefix"] {
			t.Fatalf("%q does not name the Sentry of https://sentry.example: %v / %v", base, b["identity"], a["identity"])
		}
	}
	if c := plan("http://sentry.example:80"); c["link_prefix"] != "http://sentry.example/organizations/org/issues/" {
		t.Fatalf("http's default port was kept: %v", c["link_prefix"])
	}
	if d := plan("https://sentry.example:8443"); d["link_prefix"] != "https://sentry.example:8443/organizations/org/issues/" {
		t.Fatalf("a non-default port was dropped: %v", d["link_prefix"])
	}
}

// TestProdWatch_SentryLeakTakesItsTurnInTheRank: inside one rank each alert
// kind takes its turn — a burst of Sentry issues cannot push a Sentry leak
// alert of the same rank past the cap.
func TestProdWatch_SentryLeakTakesItsTurnInTheRank(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(2)
	now := time.Now()
	for _, id := range []string{"61", "62", "63"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "boom", Level: "fatal", FirstProcessed: now, LastSeen: now})
	}
	h.sentry.put(&pwSentryIssue{ID: "64", ShortID: strp("P-64"), Title: "UserNotFound: helene.zq7rtx@qz9mail.fr", FirstProcessed: now, LastSeen: now})
	if got := sentryAlerts(sentryTick(t, h, wf)); !eqStrings(got, []string{"new:P-61:high", "new:leak-email:high"}) {
		t.Fatalf("cap 2, three fatal issues and a leak of the same rank: want one issue and the leak, got %v", got)
	}
}

// TestProdWatch_SentryGoneIssueTakesItsTurn: a tracked issue asked by id and
// absent from the answer (deleted, merged) was read — it goes to the back of
// the line like any other, never holding a max_tracked slot every tick.
func TestProdWatch_SentryGoneIssueTakesItsTurn(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "21", ShortID: strp("P-21"), Title: "a", FirstProcessed: now, LastSeen: now})
	h.sentry.put(&pwSentryIssue{ID: "95", ShortID: strp("P-95"), Title: "b", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.mu.Lock()
	delete(h.sentry.issues, "21")
	h.sentry.mu.Unlock()
	h.sentry.edit("95", func(i *pwSentryIssue) { i.Status = "resolved" })
	var all []string
	for k := 0; k < 3; k++ {
		time.Sleep(1100 * time.Millisecond) // tracked_read_at has a one-second resolution
		all = append(all, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if strings.Join(all, " ") != "resolved:P-95:low" {
		t.Fatalf("max_tracked 1: an issue gone from Sentry held the slot, the other's resolution was never read: %v", all)
	}
}
