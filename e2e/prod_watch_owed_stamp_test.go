package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryResolvedIssuesWaitingIsACut: with exactly max_tracked open
// issues, the open ones take every read and a resolved one (reopened by hand:
// ongoing, in neither list) waits — the cut is said, never a silent starvation.
func TestProdWatch_SentryResolvedIssuesWaitingIsACut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 2 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"31", "32", "40"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 3 {
		t.Fatalf("setup new: %v", got)
	}
	for _, id := range []string{"31", "32", "40"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	h.sentry.edit("40", func(i *pwSentryIssue) { i.Status = "resolved" })
	for k := 0; k < 8 && sentryIncident(t, h, "40")["closed_said"] != true; k++ {
		time.Sleep(1100 * time.Millisecond)
		sentryTick(t, h, wf)
	}
	if sentryIncident(t, h, "40")["closed_said"] != true {
		t.Fatalf("setup: the closing note of 40 was never said")
	}
	h.sentry.edit("40", func(i *pwSentryIssue) { i.Status = "unresolved"; i.Substatus = strp("ongoing") })
	time.Sleep(1100 * time.Millisecond)
	o := sentryTick(t, h, wf)
	ps := o["plan"]["sentry"].(map[string]any)
	// The coverage note itself went out once already (it is deduplicated): the walk says the cut every tick.
	said := fmt.Sprint(o["poll_sentry"]["walk"].(map[string]any)["tracked_cut"])
	if ps["tracked_cut"] != true || !strings.Contains(said, "1 resolved issue(s) wait") {
		t.Fatalf("2 open tracked issues, max_tracked 2, a resolved one waiting: want the cut said, got cut=%v read %v, walk %q",
			ps["tracked_cut"], ps["tracked_ids"], said)
	}
}

// TestProdWatch_SentryAllReadIsNoCut: exactly max_tracked open issues and nothing
// waiting — every one is read each tick: no cut is said.
func TestProdWatch_SentryAllReadIsNoCut(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 2 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"31", "32"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	if o := sentryTick(t, h, wf); o["plan"]["sentry"].(map[string]any)["tracked_cut"] != false {
		t.Fatalf("2 open tracked issues, max_tracked 2, nothing waiting: a cut was said (%v)", o["poll_sentry"]["walk"].(map[string]any)["tracked_cut"])
	}
}

// TestProdWatch_SentryAFutureReadStampGoesFirst: a read stamp a clock running
// ahead wrote is taken for none — the issue is read first, the read replacing
// the stamp, instead of looking freshest for ever.
func TestProdWatch_SentryAFutureReadStampGoesFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"21", "22"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	for _, id := range []string{"21", "22"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	sentryEditRecord(t, h, "21", func(r map[string]any) {
		r["tracked_read_at"] = time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00") // a runner 3 days ahead
	})
	read := map[string]bool{}
	for k := 0; k < 3; k++ {
		time.Sleep(1100 * time.Millisecond)
		o := sentryTick(t, h, wf)
		for _, x := range o["plan"]["sentry"].(map[string]any)["tracked_ids"].([]any) {
			read[fmt.Sprint(x)] = true
		}
	}
	if !read["21"] {
		t.Fatalf("an issue stamped read 3 days ahead was never read in 3 ticks (max_tracked 1): %v", read)
	}
}

// TestProdWatch_ANoteOwedStampLivesUntilSaid: a note owed since tick 0 that the
// owed queue itself cannot absorb at tick 1 still goes before notes first owed
// at tick 1 (the stamp keeps its first value), and its stamp is dropped once
// said — 60 minted idle templates a tick, sorting before the real one.
func TestProdWatch_ANoteOwedStampLivesUntilSaid(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	idle := func(id, title string) map[string]any {
		return map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": title, "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": old, "last_seen": old, "count": 3, "alerted": true, "last_notified": old, "quiet_noted": false}
	}
	long := strings.Repeat("x", 100)
	incidents := map[string]any{"loki:tzz": idle("tzz", "ERROR the real template that went quiet "+long)}
	for tick := 0; tick < 5; tick++ {
		for k := 0; k < 60; k++ {
			id := fmt.Sprintf("ta%02d%02d", tick, k)
			incidents["loki:"+id] = idle(id, "ERROR minted "+id+" "+long)
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents), nil)
		if err != nil {
			t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
		}
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		rec := incidents["loki:tzz"].(map[string]any)
		if rec["quiet_noted"] == true {
			if tick > 2 {
				t.Fatalf("the real template's idle note came at tick %d: owed since tick 0, it goes by tick 2", tick)
			}
			if rec["note_owed_at"] != nil {
				t.Fatalf("said at tick %d, the real template still carries its owed stamp %v", tick, rec["note_owed_at"])
			}
			return
		}
		time.Sleep(1100 * time.Millisecond) // the owed stamp has a one-second resolution
	}
	t.Fatalf("a real template idle 49 h, owed since tick 0, was never named in 5 ticks")
}

// TestProdWatch_AFutureOwedStampGoesFirst: an owed stamp a clock running ahead
// wrote is taken for the oldest — the note goes first instead of waiting behind
// every fresher one until real time catches up.
func TestProdWatch_AFutureOwedStampGoesFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	idle := func(id, title string) map[string]any {
		return map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": title, "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": old, "last_seen": old, "count": 3, "alerted": true, "last_notified": old, "quiet_noted": false}
	}
	long := strings.Repeat("x", 100)
	real := idle("t000", "ERROR the real template that went quiet "+long)
	real["note_owed_at"] = time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00") // a runner 3 days ahead
	incidents := map[string]any{"loki:t000": real}
	for tick := 0; tick < 3; tick++ {
		for k := 0; k < 60; k++ {
			id := fmt.Sprintf("tz%02d%02d", tick, k)
			incidents["loki:"+id] = idle(id, "ERROR minted "+id+" "+long)
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents), nil)
		if err != nil {
			t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
		}
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		if incidents["loki:t000"].(map[string]any)["quiet_noted"] == true {
			return
		}
		time.Sleep(1100 * time.Millisecond)
	}
	t.Fatalf("an idle note owed under a stamp 3 days ahead was never named in 3 ticks (60 fresher idle notes a tick)")
}

// TestProdWatch_ANoteLongerThanTheMessageIsRefused: a note stamps as said every
// member it names — one its clip would cut (here a fold label escaping into 5×
// its length: `&` renders `&amp;`) is refused by name, never delivered short.
func TestProdWatch_ANoteLongerThanTheMessageIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(60, func(k int) string { return fmt.Sprintf("E%02d %s", k, strings.Repeat("y", 110)) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 4000})
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	label := "{n} more (R&D): {names} " + strings.Repeat("&", 360)
	_, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{"folded_detail": label}, "app": map[string]any{"name": "demo"},
		"release": "", "release_known": false, "dry_run": true, "max_message_chars": 4000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err == nil || !strings.Contains(nerr, "its names would be cut") || strings.Contains(nerr, "Traceback") {
		t.Fatalf("a note longer than max_message_chars was not refused by name: %v %s", err, lastN(nerr, 300))
	}
}

// TestProdWatch_SentryByIdUsersAreNeverShown: a by-id read counts the issue's
// whole life in every environment, its users only the environment's over the
// retention — an operator label keeping {users} there shows `?`, never numbers
// of two scopes side by side.
func TestProdWatch_SentryByIdUsersAreNeverShown(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		cfg["labels"] = map[string]any{"sentry_detail_total": "{level} · {count} event(s) in total · {users} user(s)"}
	})
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "71", ShortID: strp("P-71"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 3, Users: 9})
	sentryTick(t, h, wf)
	h.sentry.edit("71", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.LastSeen = time.Now().Add(time.Second)
	})
	sentryEditRecord(t, h, "71", func(r map[string]any) {
		r["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies()[n:], "\n")
	if !strings.Contains(body, "in total") || !strings.Contains(body, "`?` user(s)") {
		t.Fatalf("a by-id detail showed users next to a whole-life count:\n%s", body)
	}
}

// TestProdWatch_SentryAnEnvironmentNameIsQuotedTwiceInItsPath: Sentry unquotes the
// environment segment of its path once more after the server did — a name
// holding `%41` is quoted twice, so the check reads the environment the lists
// filter on (quoted once, it would read `euA`).
func TestProdWatch_SentryAnEnvironmentNameIsQuotedTwiceInItsPath(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.sentry.mu.Lock()
	h.sentry.envs["eu%41"] = true
	h.sentry.mu.Unlock()
	h.writeConfig(t, sentryWithProbe(h, func(s map[string]any) { s["environment"] = "eu%41" }))
	o := sentryTick(t, h, wf)
	if errs := fmt.Sprint(o["poll_sentry"]["errors"]); errs != "[]" {
		t.Fatalf("environment `eu%%41`: the check did not find it: %s", errs)
	}
}
