package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func sentryPendingCount(t *testing.T, h *pwHarness) int {
	t.Helper()
	n := 0
	for fp, r := range h.state(t)["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "sentry:") && r.(map[string]any)["pending"] != nil {
			n++
		}
	}
	return n
}

// sentryFolds lists the tick's Sentry fold notes (kind:members).
func sentryFolds(o map[string]map[string]any) []string {
	var got []string
	for _, a := range o["decide"]["alerts"].([]any) {
		m := a.(map[string]any)
		if m["kind"] == "sentry" && m["detail_key"] == "folded_detail" {
			got = append(got, fmt.Sprint(m["state"], ":", len(m["members"].([]any))))
		}
	}
	return got
}

func sentryJunk(h *pwHarness, from, n int) {
	for k := 0; k < n; k++ {
		id := fmt.Sprint(from + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("JUNK-" + id), Title: "junk", Level: "fatal",
			FirstProcessed: time.Now(), LastSeen: time.Now(), Count: 1})
	}
}

// TestProdWatch_SentryAFloodIsFoldedNotQueued: anyone holding the public DSN
// can create issues by the hundred at fatal: the lane posts max_alerts_per_lane
// of them one by one and names the others in ONE note, which says them —
// nothing queues behind, a real issue arriving mid-flood is named the tick it
// arrives, not days later, and an issue named in the note is backlog: it
// comes back as no follow-up of its own.
func TestProdWatch_SentryAFloodIsFoldedNotQueued(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 12)
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	got := sentryAlerts(o)
	single := 0
	for _, a := range got {
		if strings.HasPrefix(a, "new:JUNK-") {
			single++
		}
	}
	if folds := sentryFolds(o); single != 5 || strings.Join(folds, " ") != "new:7" {
		t.Fatalf("12 new issues, max_alerts_per_lane 5: want 5 alerts one by one and 1 note of 7, got %v and %v", got, folds)
	}
	body := strings.Join(h.bodies()[n:], "\n")
	for k := 0; k < 12; k++ {
		if id := fmt.Sprint("JUNK-", 5001+k); !strings.Contains(body, id) {
			t.Fatalf("%s was neither posted nor named in the note:\n%s", id, body)
		}
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("the note said them, and %d are still pending", p)
	}
	var backlog []string
	for k := 0; k < 12; k++ {
		rec := sentryIncident(t, h, fmt.Sprint(5001+k))
		switch {
		case rec["backlog"] == true && rec["alerted"] != true:
			backlog = append(backlog, fmt.Sprint(5001+k))
		case rec["alerted"] != true:
			t.Fatalf("JUNK-%d, posted or named, is recorded as neither: %v", 5001+k, rec)
		}
	}
	if len(backlog) != 7 {
		t.Fatalf("the 7 issues named in the note: want them backlog, got %v", backlog)
	}
	sentryJunk(h, 6001, 12)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "the real outage", Level: "error",
		FirstProcessed: now, LastSeen: now, Count: 50})
	n = len(h.bodies())
	sentryTick(t, h, wf)
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "REAL-4242") {
		t.Fatalf("a real issue arriving mid-flood was neither posted nor named the tick it arrived:\n%s", body)
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("a flood built a queue of %d pending alerts", p)
	}
}

// TestProdWatch_SentryAFoldPostsTheMostSevereOneByOne: past max_alerts_per_lane
// the most severe new issues are the ones posted one by one.
func TestProdWatch_SentryAFoldPostsTheMostSevereOneByOne(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.setMaxPerLane(3)
	now := time.Now()
	for k := 0; k < 10; k++ {
		level := "error"
		if k >= 7 {
			level = "fatal" // the last ids: never first by id or arrival
		}
		id := fmt.Sprint(8001 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("S-" + id), Title: "x", Level: level, FirstProcessed: now, LastSeen: now, Count: 1})
	}
	got := sentryAlerts(sentryTick(t, h, wf))
	var single []string
	for _, a := range got {
		if strings.HasPrefix(a, "new:S-") {
			single = append(single, a)
		}
	}
	if strings.Join(single, " ") != "new:S-8008:high new:S-8009:high new:S-8010:high" {
		t.Fatalf("max_alerts_per_lane 3 among 3 fatal and 7 error issues: want the fatal ones one by one, got %v", got)
	}
}

// TestProdWatch_SentryAnIssueNamedInAFoldIsBacklog: an issue named in a fold
// note is followed in Sentry, not here: firing at a higher level later, it
// posts no escalation of its own — the one posted one by one does.
func TestProdWatch_SentryAnIssueNamedInAFoldIsBacklog(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.setMaxPerLane(1)
	now := time.Now()
	for k := 0; k < 4; k++ {
		id := fmt.Sprint(9001 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("F-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	o := sentryTick(t, h, wf)
	var posted string
	for _, a := range sentryAlerts(o) {
		if strings.HasPrefix(a, "new:F-") {
			posted = strings.Split(a, ":")[1][len("F-"):]
		}
	}
	if posted == "" || strings.Join(sentryFolds(o), " ") != "new:3" {
		t.Fatalf("setup: want one alert and a note of 3, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	for k := 0; k < 4; k++ {
		id := fmt.Sprint(9001 + k)
		h.sentry.edit(id, func(i *pwSentryIssue) {
			i.Level = "fatal"
			i.FirstProcessed = now.Add(-3 * time.Hour)
			i.LastSeen = time.Now().Add(time.Second)
		})
	}
	o = sentryTick(t, h, wf)
	got := sentryAlerts(o)
	if !anyPrefix(got, "escalated:F-"+posted) {
		t.Fatalf("setup: the issue posted one by one did not escalate: %v", got)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "escalated:F-") && !strings.HasPrefix(a, "escalated:F-"+posted) {
			t.Fatalf("an issue named in a fold came back as an escalation of its own: %v", got)
		}
	}
	if folds := sentryFolds(o); len(folds) != 0 {
		t.Fatalf("the issues named in a fold came back as a fold of follow-ups: %v", folds)
	}
}

// TestProdWatch_AFoldCutByTheCapKeepsItsMembersPending: a note of folded
// alerts the per-run cap cuts stands for them: each stays pending, and the
// next tick folds and posts them.
func TestProdWatch_AFoldCutByTheCapKeepsItsMembersPending(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.setMaxPerLane(1)
	h.alertCap.Store(1)
	sentryJunk(h, 7001, 4)
	got := sentryAlerts(sentryTick(t, h, wf))
	if len(got) != 1 || !strings.HasPrefix(got[0], "new:JUNK-") {
		t.Fatalf("setup: cap 1, max_alerts_per_lane 1: want the one alert and the note cut, got %v", got)
	}
	if p := sentryPendingCount(t, h); p != 3 {
		t.Fatalf("the cut note's 3 members: want them pending, %d are", p)
	}
	first := got[0]
	h.alertCap.Store(0)
	n := len(h.bodies())
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies()[n:], "\n")
	for k := 0; k < 4; k++ {
		if id := fmt.Sprint("JUNK-", 7001+k); !strings.Contains(first, id+":") && !strings.Contains(body, id) {
			t.Fatalf("pending member %s of the cut note did not come back:\n%s", id, body)
		}
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("after the note posted, %d members are still pending", p)
	}
}

// TestProdWatch_ALogTemplateFloodIsFolded: log lines carry user input — an
// attacker can mint error templates; the Loki lane posts max_alerts_per_lane
// new ones one by one and names the others in one note, which marks them
// as said.
func TestProdWatch_ALogTemplateFloodIsFolded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var tpls []any
	for k := 0; k < 8; k++ {
		id := fmt.Sprint("t", k)
		tpls = append(tpls, map[string]any{"template_id": id, "template": fmt.Sprint("ERROR minted pattern ", k), "count": 3, "count_live": 3,
			"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
			"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"})
	}
	state := map[string]any{"version": 1, "generation": 1, "incidents": map[string]any{}, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": "100"}}}}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, state, nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	single, folded := 0, map[string]any(nil)
	for _, a := range out["alerts"].([]any) {
		m := a.(map[string]any)
		switch {
		case m["detail_key"] == "folded_detail":
			folded = m
		case m["kind"] == "loki" && m["state"] == "new":
			single++
		}
	}
	if single != 5 || folded == nil {
		t.Fatalf("8 new templates, max_alerts_per_lane 5: want 5 alerts and 1 note, got %d and %v", single, folded)
	}
	if n := folded["fields"].(map[string]any)["n"]; fmt.Sprint(n) != "3" {
		t.Fatalf("the note counts %v, want 3", n)
	}
	var st map[string]any
	b, err := os.ReadFile(fmt.Sprint(out["state_next_file"]))
	if err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	said := 0
	for fp, r := range st["incidents"].(map[string]any) {
		rec := r.(map[string]any)
		if strings.HasPrefix(fp, "loki:") && rec["alerted"] == true && rec["pending"] == nil {
			said++
		}
	}
	if said != 8 {
		t.Fatalf("8 templates, 5 posted and 3 in the note: %d are marked said", said)
	}
}

// TestProdWatch_ANegativeMaxPerLaneIsRefused: max_alerts_per_lane below 0 means
// nothing — refused by name, never a slice cut from the wrong end.
func TestProdWatch_ANegativeMaxPerLaneIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	state := map[string]any{"version": 1, "generation": 1, "incidents": map[string]any{}, "health": map[string]any{}, "cursors": map[string]any{}}
	_, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, state, map[string]any{"max_alerts_per_lane": -1})
	if err == nil || !strings.Contains(stderr, "max_alerts_per_lane") || strings.Contains(stderr, "Traceback") {
		t.Fatalf("max_alerts_per_lane -1 was not refused by name: %v %s", err, stderr)
	}
}

// TestProdWatch_ALogTemplateReminderFloodIsFolded: minted templates come back
// as follow-ups too — reminders of eight recurring templates post
// max_alerts_per_lane of them one by one and name the others in one note of
// their kind, which stamps them reminded.
func TestProdWatch_ALogTemplateReminderFloodIsFolded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var tpls []any
	incidents := map[string]any{}
	old := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	for k := 0; k < 8; k++ {
		id := fmt.Sprint("t", k)
		tpls = append(tpls, map[string]any{"template_id": id, "template": fmt.Sprint("ERROR minted pattern ", k), "count": 3, "count_live": 3,
			"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
			"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"})
		incidents["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": fmt.Sprint("ERROR minted pattern ", k), "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": old, "last_seen": old, "count": 3, "alerted": true, "last_notified": old, "quiet_noted": false}
	}
	state := map[string]any{"version": 1, "generation": 1, "incidents": incidents, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": "100"}}}}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, state, nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	single, folded := 0, map[string]any(nil)
	for _, a := range out["alerts"].([]any) {
		m := a.(map[string]any)
		switch {
		case m["detail_key"] == "folded_detail":
			folded = m
		case m["kind"] == "loki" && m["state"] == "reminder":
			single++
		}
	}
	if single != 5 || folded == nil || folded["state"] != "reminder" {
		t.Fatalf("8 templates due a reminder, max_alerts_per_lane 5: want 5 reminders and 1 reminder note, got %d and %v", single, folded)
	}
	var st map[string]any
	b, err := os.ReadFile(fmt.Sprint(out["state_next_file"]))
	if err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	for fp, r := range st["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "loki:") && r.(map[string]any)["last_notified"] == old {
			t.Fatalf("%s, reminded or named in the note, is not stamped reminded", fp)
		}
	}
}

// TestProdWatch_AFoldOfEscalationsCutByTheCapIsReSelected: a template's
// escalation is re-derived from the present, so a fold of escalations the cap
// cuts puts each member back at its previous severity — the same rule
// re-selects it next tick instead of the escalation being lost.
func TestProdWatch_AFoldOfEscalationsCutByTheCapIsReSelected(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var tpls []any
	incidents := map[string]any{}
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for k := 0; k < 3; k++ {
		id := fmt.Sprint("e", k)
		tpls = append(tpls, map[string]any{"template_id": id, "template": fmt.Sprint("ERROR spiking pattern ", k), "count": 60, "count_live": 60,
			"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
			"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"})
		incidents["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": fmt.Sprint("ERROR spiking pattern ", k), "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": recent, "last_seen": recent, "count": 3, "alerted": true, "last_notified": recent, "quiet_noted": false}
	}
	state := map[string]any{"version": 1, "generation": 1, "incidents": incidents, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": "100"}}}}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, state,
		map[string]any{"max_alerts": 1, "max_alerts_per_lane": 1})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	if n := len(out["alerts"].([]any)); n != 1 {
		t.Fatalf("setup: cap 1: want one alert posted, got %v", out["alerts"])
	}
	var st map[string]any
	b, err := os.ReadFile(fmt.Sprint(out["state_next_file"]))
	if err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("the staged state: %v", err)
	}
	high := 0
	for fp, r := range st["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "loki:e") && r.(map[string]any)["severity"] == "high" {
			high++
		}
	}
	if high != 1 {
		t.Fatalf("one escalation posted, two folded and cut: want only the posted one at high, %d are", high)
	}
}
