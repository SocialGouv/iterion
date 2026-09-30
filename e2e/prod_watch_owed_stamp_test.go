package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryResolvedIssuesWaitingIsACut: with exactly max_tracked open
// issues, the open ones take every read and a resolved one (reopened by hand:
// ongoing, in neither list) waits — the cut is said, never a silent starvation;
// one open issue more and the open ones take turns, the resolved one still
// waiting.
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
	if want := "2 tracked issues for max_tracked 2 reads a tick: they take every read, and 1 resolved issue(s) wait"; ps["tracked_cut"] != true || said != want {
		t.Fatalf("2 open tracked issues, max_tracked 2, a resolved one waiting: want the cut said %q, got cut=%v read %v, walk %q",
			want, ps["tracked_cut"], ps["tracked_ids"], said)
	}
	h.sentry.put(&pwSentryIssue{ID: "33", ShortID: strp("P-33"), Title: "x", Level: "error", FirstProcessed: time.Now(), LastSeen: time.Now(), Count: 1})
	sentryTick(t, h, wf) // 33 is new: alerted, tracked from the next tick
	time.Sleep(1100 * time.Millisecond)
	o = sentryTick(t, h, wf)
	said = fmt.Sprint(o["poll_sentry"]["walk"].(map[string]any)["tracked_cut"])
	if want := "3 tracked issues for max_tracked 2 reads a tick: they are read in turns, and 1 resolved issue(s) wait"; said != want {
		t.Fatalf("3 open tracked issues, max_tracked 2, a resolved one waiting: want %q, got %q", want, said)
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
// every fresher one until real time catches up, its id sorting after theirs: the
// stamp gives it its turn, not the id order.
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
	real := idle("tzzz", "ERROR the real template that went quiet "+long)
	real["note_owed_at"] = time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00") // a runner 3 days ahead
	incidents := map[string]any{"loki:tzzz": real}
	for tick := 0; tick < 3; tick++ {
		for k := 0; k < 60; k++ {
			id := fmt.Sprintf("ta%02d%02d", tick, k)
			incidents["loki:"+id] = idle(id, "ERROR minted "+id+" "+long)
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents), nil)
		if err != nil {
			t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
		}
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		if incidents["loki:tzzz"].(map[string]any)["quiet_noted"] == true {
			return
		}
		time.Sleep(1100 * time.Millisecond)
	}
	t.Fatalf("an idle note owed under a stamp 3 days ahead was never named in 3 ticks (60 fresher idle notes a tick)")
}

// TestProdWatch_ANoteLongerThanTheMessageGoesOutInParts: a note whose labels
// render longer than the message (a fold label escaping into 5x its length:
// `&` renders `&amp;`) goes out in parts — each within max_message_chars and
// naming its own members, every member named once; of a note holding members
// back for the next ticks, only the last part says how many.
func TestProdWatch_ANoteLongerThanTheMessageGoesOutInParts(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	idle := map[string]any{}
	for k := 0; k < 80; k++ {
		id := fmt.Sprintf("q%03d", k)
		idle["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": fmt.Sprintf("Q%02d %s", k, strings.Repeat("y", 110)), "detail_key": "loki_detail",
			"fields": map[string]any{}, "first_seen": old, "last_seen": old, "count": 3, "alerted": true, "last_notified": old, "quiet_noted": false}
	}
	facts := pwMintedTemplates(60, func(k int) string { return fmt.Sprintf("E%02d %s", k, strings.Repeat("y", 110)) })
	for _, c := range []struct {
		name    string
		signals map[string]any
		state   map[string]any
		held    bool
	}{
		{"facts", map[string]any{"templates": facts, "leak": []any{}}, pwLokiState(map[string]any{}), false},
		{"held", map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(idle), true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			out, stderr, err := pwDecide(t, wf, h, c.signals, c.state, map[string]any{"max_message_chars": 4000})
			if err != nil {
				t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
			}
			labels := map[string]any{"folded_detail": "{n} more (R&D): {names} " + strings.Repeat("&", 360),
				"folded_detail_more": "{n} more (R&D): {names} — {more} held " + strings.Repeat("&", 350)}
			nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
				"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
				"labels": labels, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
				"dry_run": true, "max_message_chars": 4000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
			if err != nil {
				t.Fatalf("notify: %v %s", err, lastN(nerr, 400))
			}
			var parts []string
			for _, m := range nout["messages"].([]any) {
				text := m.(map[string]any)["text"].(string)
				if n := len([]rune(text)); n > 4000 {
					t.Fatalf("a message of %d characters, over max_message_chars 4000", n)
				}
				if strings.Contains(text, "more (R&amp;D)") {
					parts = append(parts, text)
				}
			}
			notes := 0
			all := strings.Join(parts, "\n")
			for _, a := range out["alerts"].([]any) {
				names := pwMembers(a)
				if len(names) > 0 {
					notes++
				}
				for _, name := range names {
					if n := strings.Count(all, name); n != 1 {
						t.Fatalf("member %q is named %d time(s) across the parts", name, n)
					}
				}
			}
			if notes == 0 || len(parts) <= notes {
				t.Fatalf("want the notes in parts: %d note(s), %d message(s)", notes, len(parts))
			}
			for i, p := range parts {
				if says := strings.Contains(p, " held &amp;"); says != (c.held && i == len(parts)-1) {
					t.Fatalf("part %d of %d says the held members: %v (a note holding members back says it in its last part only)", i+1, len(parts), says)
				}
			}
		})
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
