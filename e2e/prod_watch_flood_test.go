package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// sentryFolds lists the tick's Sentry fold notes (state:members).
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

// sentrySingles counts the tick's Sentry alerts posted one by one in a state
// for a short-id prefix.
func sentrySingles(o map[string]map[string]any, state, prefix string) int {
	n := 0
	for _, a := range sentryAlerts(o) {
		if strings.HasPrefix(a, state+":"+prefix) {
			n++
		}
	}
	return n
}

func sentryJunk(h *pwHarness, from, n int) {
	for k := 0; k < n; k++ {
		id := fmt.Sprint(from + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("JUNK-" + id), Title: "junk", Level: "fatal",
			FirstProcessed: time.Now(), LastSeen: time.Now(), Count: 1})
	}
}

// pwAlertLogFps is every fingerprint the committed alert log holds.
func pwAlertLogFps(t *testing.T, h *pwHarness) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "alertlog.jsonl"))
	if err != nil {
		t.Fatalf("alert log: %v", err)
	}
	fps := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			fps[fmt.Sprint(m["fp"])] = true
		}
	}
	return fps
}

// TestProdWatch_SentryAFloodIsFoldedNotQueued: anyone holding the public DSN
// can create issues by the hundred at fatal: the lane posts max_alerts_per_lane
// of them one by one and names the others in a note, which says them — each
// recorded as said, in the alert log one by one, nothing queued — and a real
// issue arriving mid-flood is said the tick it arrives.
func TestProdWatch_SentryAFloodIsFoldedNotQueued(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 12)
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if single, folds := sentrySingles(o, "new", "JUNK-"), sentryFolds(o); single != 5 || strings.Join(folds, " ") != "new:7" {
		t.Fatalf("12 new issues, max_alerts_per_lane 5: want 5 alerts one by one and 1 note of 7, got %v and %v", sentryAlerts(o), folds)
	}
	body := strings.Join(h.bodies()[n:], "\n")
	logged := pwAlertLogFps(t, h)
	for k := 0; k < 12; k++ {
		id := fmt.Sprint(5001 + k)
		if !strings.Contains(body, "JUNK-"+id) {
			t.Fatalf("JUNK-%s was neither posted nor named in the note:\n%s", id, body)
		}
		if rec := sentryIncident(t, h, id); rec["alerted"] != true || rec["pending"] != nil {
			t.Fatalf("JUNK-%s, said, is not recorded as said: %v", id, rec)
		}
		if !logged["sentry:"+id] {
			t.Fatalf("JUNK-%s, said, is not in the alert log", id)
		}
	}
	sentryJunk(h, 6001, 12)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "the real outage", Level: "error",
		FirstProcessed: now, LastSeen: now, Count: 50})
	n = len(h.bodies())
	sentryTick(t, h, wf)
	if body := strings.Join(h.bodies()[n:], "\n"); !strings.Contains(body, "REAL-4242") {
		t.Fatalf("a real issue arriving mid-flood was not said the tick it arrived:\n%s", body)
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("a flood built a queue of %d pending alerts", p)
	}
}

// TestProdWatch_SentryAFoldGroupsBySeverity: a real issue at a severity the
// flood does not use is posted one by one, whatever the flood.
func TestProdWatch_SentryAFoldGroupsBySeverity(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.setMaxPerLane(3)
	sentryJunk(h, 8001, 10)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "8999", ShortID: strp("REAL-8999"), Title: "real", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	o := sentryTick(t, h, wf)
	if !anyPrefix(sentryAlerts(o), "new:REAL-8999:medium") {
		t.Fatalf("an error-level issue among ten fatal junk issues was not posted one by one: %v %v", sentryAlerts(o), sentryFolds(o))
	}
	if single := sentrySingles(o, "new", "JUNK-"); single != 3 {
		t.Fatalf("max_alerts_per_lane 3: want 3 junk alerts one by one, got %d", single)
	}
}

// TestProdWatch_SentryFollowUpsOfAFoldFoldToo: issues named in a note are
// followed like any other; their follow-ups — here an escalation each — fold
// the same way, so a flood cannot come back as a flood.
func TestProdWatch_SentryFollowUpsOfAFoldFoldToo(t *testing.T) {
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
	if o := sentryTick(t, h, wf); sentrySingles(o, "new", "F-") != 1 || strings.Join(sentryFolds(o), " ") != "new:3" {
		t.Fatalf("setup: want one alert and a note of 3, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	for k := 0; k < 4; k++ {
		h.sentry.edit(fmt.Sprint(9001+k), func(i *pwSentryIssue) {
			i.Level = "fatal"
			i.FirstProcessed = now.Add(-3 * time.Hour)
			i.LastSeen = time.Now().Add(time.Second)
		})
	}
	o := sentryTick(t, h, wf)
	if single, folds := sentrySingles(o, "escalated", "F-"), sentryFolds(o); single != 1 || strings.Join(folds, " ") != "escalated:3" {
		t.Fatalf("four escalations at max_alerts_per_lane 1: want one alert and a note of 3, got %v %v", sentryAlerts(o), folds)
	}
}

// TestProdWatch_AFoldNoteIsNeverCutByTheCap: the per-run cap bounds the
// alerts posted one by one; a note of folded alerts is never cut — its
// members would otherwise queue as pending behind the next tick's flood.
func TestProdWatch_AFoldNoteIsNeverCutByTheCap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.setMaxPerLane(1)
	h.alertCap.Store(1)
	sentryJunk(h, 7001, 4)
	o := sentryTick(t, h, wf)
	if sentrySingles(o, "new", "JUNK-") != 1 || strings.Join(sentryFolds(o), " ") != "new:3" {
		t.Fatalf("cap 1, max_alerts_per_lane 1: want the one alert and its note of 3, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("the note was posted, and %d members are pending", p)
	}
}

// TestProdWatch_SentryAFoldNamesEveryMember: a note names every member it
// stands for — however many, within the message budget, as many notes as
// needed —, and a real issue at another level is posted one by one.
func TestProdWatch_SentryAFoldNamesEveryMember(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, n := range map[string]int{"30 junk issues": 30, "the new list's whole page, 199": 199} {
		name, n := name, n
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			sentryTick(t, h, wf)
			sentryJunk(h, 5001, n)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "the real outage", Level: "error",
				FirstProcessed: now, LastSeen: now, Count: 50})
			nb := len(h.bodies())
			o := sentryTick(t, h, wf)
			body := strings.Join(h.bodies()[nb:], "\n")
			var missing []string
			for k := 0; k < n; k++ {
				if id := fmt.Sprint("JUNK-", 5001+k); !strings.Contains(body, id) {
					missing = append(missing, id)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("%d members said nowhere in the channel (%v…), notes %v", len(missing), missing[:min(len(missing), 5)], sentryFolds(o))
			}
			if !anyPrefix(sentryAlerts(o), "new:REAL-4242:medium") {
				t.Fatalf("the error-level issue among fatal junk was not posted one by one: %v", sentryAlerts(o))
			}
			if p := sentryPendingCount(t, h); p != 0 {
				t.Fatalf("%d members pending after their notes posted", p)
			}
		})
	}
}

// TestProdWatch_SentryATransitionFloodIsFolded: the fold covers every kind of
// alert of the lane — a flood that regresses (bulk-resolved junk re-sent) or
// escalates (Sentry's own spike detection) posts max_alerts_per_lane of them
// one by one and names the rest; each member keeps its transition date, so
// the next tick posts it again neither one by one nor in a note.
func TestProdWatch_SentryATransitionFloodIsFolded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for _, kind := range []string{"regressed", "escalating"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 60 }))
			sentryTick(t, h, wf)
			sentryJunk(h, 5001, 40)
			sentryTick(t, h, wf)
			now := time.Now()
			if kind == "regressed" {
				for k := 0; k < 40; k++ {
					h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) {
						i.Status = "resolved"
						i.FirstProcessed = now.Add(-3 * time.Hour)
						i.Acts = append(i.Acts, pwSentryAct{Type: "set_resolved", At: time.Now()})
					})
				}
				if o := sentryTick(t, h, wf); sentrySingles(o, "resolved", "JUNK-") > h.maxPerLane() {
					t.Fatalf("40 closing notes posted %d one by one", sentrySingles(o, "resolved", "JUNK-"))
				}
			}
			at := time.Now().Add(time.Second)
			for k := 0; k < 40; k++ {
				h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) {
					i.Status = "unresolved"
					i.Substatus = strp(kind)
					i.FirstProcessed = now.Add(-3 * time.Hour)
					i.LastSeen = at
					act := map[string]string{"regressed": "set_regression", "escalating": "set_escalating"}[kind]
					i.Acts = append(i.Acts, pwSentryAct{Type: act, At: at})
				})
			}
			nb := len(h.bodies())
			o := sentryTick(t, h, wf)
			if single := sentrySingles(o, kind, "JUNK-"); single > h.maxPerLane() {
				t.Fatalf("%d %s alerts of minted junk posted one by one (max_alerts_per_lane %d)", single, kind, h.maxPerLane())
			}
			body := strings.Join(h.bodies()[nb:], "\n")
			for k := 0; k < 40; k++ {
				if id := fmt.Sprint("JUNK-", 5001+k); !strings.Contains(body, id) {
					t.Fatalf("%s: %s neither posted nor named", kind, id)
				}
			}
			if p := sentryPendingCount(t, h); p != 0 {
				t.Fatalf("%d transitions pending after their notes posted", p)
			}
			if o := sentryTick(t, h, wf); sentrySingles(o, kind, "JUNK-") != 0 || len(sentryFolds(o)) != 0 {
				t.Fatalf("the transitions said last tick were said again: %v %v", sentryAlerts(o), sentryFolds(o))
			}
		})
	}
}

// TestProdWatch_SentryAFoldIsNeverCutSoNothingQueues: a steady flood, with a
// fresh alert of the same rank every tick and a small cap, queues nothing: the
// notes are never cut, so their members never go pending.
func TestProdWatch_SentryAFoldIsNeverCutSoNothingQueues(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	h.alertCap.Store(6)
	for k := 0; k < 4; k++ {
		sentryJunk(h, 5001+100*k, 20)
		now := time.Now()
		id := fmt.Sprint(7001 + k)
		treg := now.Add(-10 * time.Second)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("R-" + id), Title: "regressed", Level: "fatal", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: treg,
			Acts: []pwSentryAct{{Type: "set_resolved", At: now.Add(-3 * 24 * time.Hour)}, {Type: "set_regression", At: treg}}})
		sentryTick(t, h, wf)
		if p := sentryPendingCount(t, h); p > 5 {
			t.Fatalf("tick %d: a steady flood queued %d pending alerts", k, p)
		}
	}
}

// TestProdWatch_SentryARealIssueIsSaidDuringASustainedFlood: at the default
// caps, thirty minted issues and twenty junk regressions a tick never hold a
// real issue back: it is said the tick it arrives.
func TestProdWatch_SentryARealIssueIsSaidDuringASustainedFlood(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 60 }))
	sentryTick(t, h, wf)
	for k := 0; k < 3; k++ {
		sentryJunk(h, 10001+100*k, 30)
		now := time.Now()
		for r := 0; r < 20; r++ {
			id := fmt.Sprint(20001 + 100*k + r)
			treg := now.Add(-10 * time.Second)
			h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("R-" + id), Title: "junk regressed", Level: "fatal", Substatus: strp("regressed"),
				FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: treg,
				Acts: []pwSentryAct{{Type: "set_resolved", At: now.Add(-3 * 24 * time.Hour)}, {Type: "set_regression", At: treg}}})
		}
		if k == 1 {
			h.sentry.put(&pwSentryIssue{ID: "9999", ShortID: strp("REAL-9999"), Title: "the real outage", Level: "fatal",
				FirstProcessed: time.Now(), LastSeen: time.Now(), Count: 50})
		}
		n := len(h.bodies())
		sentryTick(t, h, wf)
		if k == 1 && !strings.Contains(strings.Join(h.bodies()[n:], "\n"), "REAL-9999") {
			t.Fatalf("a real fatal issue arriving mid-flood was not said the tick it arrived (pending=%v)", sentryIncident(t, h, "9999")["pending"])
		}
	}
}

func pwMintedTemplates(n int, text func(k int) string) []any {
	var tpls []any
	for k := 0; k < n; k++ {
		tpls = append(tpls, map[string]any{"template_id": fmt.Sprintf("t%02d", k), "template": text(k), "count": 3, "count_live": 3,
			"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
			"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"})
	}
	return tpls
}

func pwLokiState(incidents map[string]any) map[string]any {
	return map[string]any{"version": 1, "generation": 1, "incidents": incidents, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{"errors": map[string]any{"covered_to_ns": "100"}}}}
}

// TestProdWatch_ALogTemplateFloodIsFolded: log lines carry user input — an
// attacker can mint error templates; the Loki lane posts max_alerts_per_lane
// new ones one by one and names the others in one note, which marks them
// as said.
func TestProdWatch_ALogTemplateFloodIsFolded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(8, func(k int) string { return fmt.Sprint("ERROR minted pattern ", k) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}), nil)
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
	said := 0
	for fp, r := range pwStateNext(t, out)["incidents"].(map[string]any) {
		rec := r.(map[string]any)
		if strings.HasPrefix(fp, "loki:") && rec["alerted"] == true && rec["pending"] == nil {
			said++
		}
	}
	if said != 8 {
		t.Fatalf("8 templates, 5 posted and 3 in the note: %d are marked said", said)
	}
}

// TestProdWatch_ALogTemplateFoldTellsTemplatesApart: templates sharing their
// first sixty characters are each told apart in the note — its id, its whole
// title — so every template marked said was shown.
func TestProdWatch_ALogTemplateFoldTellsTemplatesApart(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(12, func(k int) string {
		return fmt.Sprintf("ERROR payment gateway refused the request for merchant pattern %02d", k)
	})
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}), nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{"folded_detail": "{n} more of this kind this tick, not posted one by one: {names}"}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": true, "max_message_chars": 14000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, nerr)
	}
	text := fmt.Sprint(nout["messages"])
	for k := 0; k < 12; k++ {
		if !strings.Contains(text, fmt.Sprintf("merchant pattern %02d", k)) {
			t.Fatalf("template %02d, marked said, is not told apart in the channel:\n%s", k, text)
		}
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

func pwAlertedTemplates(n int, prefix, stamp string) map[string]any {
	incidents := map[string]any{}
	for k := 0; k < n; k++ {
		id := fmt.Sprintf("t%02d", k)
		incidents["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": fmt.Sprint(prefix, k), "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": stamp, "last_seen": stamp, "count": 3, "alerted": true, "last_notified": stamp, "quiet_noted": false}
	}
	return incidents
}

// TestProdWatch_ALogTemplateReminderFloodIsFolded: minted templates come back
// as follow-ups too — reminders of eight recurring templates post
// max_alerts_per_lane of them one by one and name the others in one note of
// their kind, which stamps them reminded.
func TestProdWatch_ALogTemplateReminderFloodIsFolded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	tpls := pwMintedTemplates(8, func(k int) string { return fmt.Sprint("ERROR minted pattern ", k) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}},
		pwLokiState(pwAlertedTemplates(8, "ERROR minted pattern ", old)), nil)
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
	for fp, r := range pwStateNext(t, out)["incidents"].(map[string]any) {
		if strings.HasPrefix(fp, "loki:") && r.(map[string]any)["last_notified"] == old {
			t.Fatalf("%s, reminded or named in the note, is not stamped reminded", fp)
		}
	}
}

// TestProdWatch_AFoldOfEscalationsIsSaidWhole: a note of escalations is never
// cut by the cap: each escalated template is stamped at its new severity.
func TestProdWatch_AFoldOfEscalationsIsSaidWhole(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	tpls := pwMintedTemplates(3, func(k int) string { return fmt.Sprint("ERROR spiking pattern ", k) })
	for _, tp := range tpls {
		tp.(map[string]any)["count"], tp.(map[string]any)["count_live"] = 60, 60
	}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}},
		pwLokiState(pwAlertedTemplates(3, "ERROR spiking pattern ", recent)), map[string]any{"max_alerts": 1, "max_alerts_per_lane": 1})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	if n := len(out["alerts"].([]any)); n != 2 {
		t.Fatalf("cap 1, max_alerts_per_lane 1, three escalations: want one alert and its note, got %v", out["alerts"])
	}
	for fp, r := range pwStateNext(t, out)["incidents"].(map[string]any) {
		if rec := r.(map[string]any); strings.HasPrefix(fp, "loki:") && (rec["severity"] != "high" || rec["last_notified"] == recent) {
			t.Fatalf("%s, escalated in a note, is not stamped at its new severity: %v", fp, rec)
		}
	}
}

// TestProdWatch_ALogTemplateFoldFitsTheMessageBudget: a note's names never
// outgrow a message — with a small max_message_chars the fold splits into as
// many notes as needed, and every template is named in a message delivered
// whole (a longer one would be cut on a line boundary, its names dropped).
func TestProdWatch_ALogTemplateFoldFitsTheMessageBudget(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(40, func(k int) string {
		return fmt.Sprintf("ERROR the payment gateway refused the request of merchant %02d with an unknown code and no retry", k)
	})
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 2000})
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{"folded_detail": "{n} more of this kind this tick, not posted one by one: {names}"}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": true, "max_message_chars": 2000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, nerr)
	}
	text := fmt.Sprint(nout["messages"])
	for k := 0; k < 40; k++ {
		if !strings.Contains(text, fmt.Sprintf("merchant %02d with", k)) {
			t.Fatalf("template %02d, marked said, is named in no message delivered whole", k)
		}
	}
}
