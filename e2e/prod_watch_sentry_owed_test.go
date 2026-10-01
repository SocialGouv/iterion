package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryOpenIssuesAreReadBeforeResolvedOnes: resolved, noted issues
// are read by id for a reopening by hand, best effort — after the open ones, so
// they never thin out an open issue's reads without the cut being said (1 open,
// 3 resolved, max_tracked 2: the open one is read every tick).
func TestProdWatch_SentryOpenIssuesAreReadBeforeResolvedOnes(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 2 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"31", "32", "33", "40"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 4 {
		t.Fatalf("setup new: %v", got)
	}
	for _, id := range []string{"31", "32", "33", "40"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	for _, id := range []string{"31", "32", "33"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.Status = "resolved" })
	}
	for k := 0; k < 8; k++ {
		closed := true
		for _, id := range []string{"31", "32", "33"} {
			closed = closed && sentryIncident(t, h, id)["closed_said"] == true
		}
		if closed {
			break
		}
		time.Sleep(1100 * time.Millisecond)
		sentryTick(t, h, wf)
	}
	for k := 0; k < 3; k++ {
		time.Sleep(1100 * time.Millisecond)
		o := sentryTick(t, h, wf)
		if ids := fmt.Sprint(o["plan"]["sentry"].(map[string]any)["tracked_ids"]); !strings.Contains(ids, "40") {
			t.Fatalf("tick %d: 1 open and 3 resolved issues, max_tracked 2: the open one was not read by id (%s), and no cut is said", k, ids)
		}
	}
}

// TestProdWatch_ALogTemplateIdleNoteOwedLongestComesFirst: a note derived again
// from the state (here an idle note) that a tick had no room for comes first
// once owed — a steady supply of fresher idle templates, sorting before it, never
// holds it back.
func TestProdWatch_ALogTemplateIdleNoteOwedLongestComesFirst(t *testing.T) {
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
	for tick := 0; tick < 4; tick++ {
		for k := 0; k < 30; k++ { // 30 minted templates go idle every tick: more than a tick names
			id := fmt.Sprintf("ta%02d%02d", tick, k)
			incidents["loki:"+id] = idle(id, "ERROR minted "+id+" "+long)
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents), nil)
		if err != nil {
			t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
		}
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		if incidents["loki:tzz"].(map[string]any)["quiet_noted"] == true {
			if tick > 1 {
				t.Fatalf("the real template's idle note came at tick %d: owed since tick 0, it goes first at tick 1", tick)
			}
			return
		}
		time.Sleep(1100 * time.Millisecond) // the owed stamp has a one-second resolution
	}
	t.Fatalf("a real template idle 49 h was never named in 4 ticks: fresher idle notes sorting before it took every note")
}

// TestProdWatch_SentryAClosingOwedLongestComesFirst: the same for Sentry closing
// notes — a steady stream of closings of other issues (a flood auto-resolved)
// never holds a real issue's closing note once it is owed.
func TestProdWatch_SentryAClosingOwedLongestComesFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1000; s["max_issues"] = 400 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for k := 0; k < 300; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(10001 + k), ShortID: strp(fmt.Sprintf("J%05d-%s", k, strings.Repeat("X", 57))), Title: "junk",
			Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	h.sentry.put(&pwSentryIssue{ID: "99999", ShortID: strp("REAL-99999"), Title: "real", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	if rec := sentryIncident(t, h, "99999"); rec["alerted"] != true {
		t.Fatalf("setup: the real issue was not said: %v", rec)
	}
	h.sentry.edit("99999", func(x *pwSentryIssue) { x.FirstProcessed = now.Add(-3 * time.Hour) })
	for k := 0; k < 300; k++ {
		h.sentry.edit(fmt.Sprint(10001+k), func(x *pwSentryIssue) { x.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	h.sentry.edit("99999", func(x *pwSentryIssue) { x.Status = "resolved" })
	for tick := 0; tick < 4; tick++ {
		for k := 0; k < 60; k++ { // 60 closings a tick: more than a tick names
			h.sentry.edit(fmt.Sprint(10001+60*tick+k), func(x *pwSentryIssue) { x.Status = "resolved" })
		}
		time.Sleep(1100 * time.Millisecond)
		sentryTick(t, h, wf)
		if sentryIncident(t, h, "99999")["closed_said"] == true {
			if tick > 1 {
				t.Fatalf("the real issue's closing note came at tick %d: owed since tick 0, it goes first at tick 1", tick)
			}
			return
		}
	}
	t.Fatalf("the real issue, resolved and read by id every tick, got no closing note in 4 ticks: other closings took every note")
}

// TestProdWatch_SentryEveryTrackedIssueIsReadInItsTurn: three open issues out of
// every list, max_tracked 1 — least recently read first, each is read once in
// three ticks (the most recently read first would read one for ever).
func TestProdWatch_SentryEveryTrackedIssueIsReadInItsTurn(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"21", "22", "23"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	for _, id := range []string{"21", "22", "23"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	read := map[string]bool{}
	for k := 0; k < 3; k++ {
		time.Sleep(1100 * time.Millisecond)
		o := sentryTick(t, h, wf)
		for _, x := range o["plan"]["sentry"].(map[string]any)["tracked_ids"].([]any) {
			read[fmt.Sprint(x)] = true
		}
	}
	if len(read) != 3 {
		t.Fatalf("three open issues, max_tracked 1, three ticks: want each read once, read %v", read)
	}
}

// TestProdWatch_SentryALaneErrorHidesNoTrackedCut: the tracked cut is said
// whatever else the lane says — a lasting lane error (one issue's activities
// failing every tick) does not hide it.
func TestProdWatch_SentryALaneErrorHidesNoTrackedCut(t *testing.T) {
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
	h.sentry.put(&pwSentryIssue{ID: "30", ShortID: strp("P-30"), Title: "r", FirstProcessed: now.Add(-48 * time.Hour), LastSeen: now,
		Count: 1, Substatus: strp("regressed"), Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	var notes []string
	for k := 0; k < 3; k++ {
		h.sentry.failNext("activities", 500, 500)
		n := len(h.bodies())
		o := sentryTick(t, h, wf)
		if k == 0 && (o["plan"]["sentry"].(map[string]any)["tracked_cut"] != true || fmt.Sprint(o["poll_sentry"]["errors"]) == "[]") {
			t.Fatalf("setup: want a tracked cut and a lane error, got cut=%v errors=%v", o["plan"]["sentry"].(map[string]any)["tracked_cut"], o["poll_sentry"]["errors"])
		}
		notes = append(notes, h.bodies()[n:]...)
	}
	if all := strings.Join(notes, "\n"); !strings.Contains(all, "tracked issues for max_tracked") {
		t.Fatalf("2 open tracked issues, max_tracked 1, and a lasting activities error: the cut was never said over 3 ticks:\n%s", all)
	}
}

// TestProdWatch_SentryAnEscalationFloodIsNamedWhole: escalations are facts — a
// sighting that will not recur: 120 issues escalating in one tick, past one
// note's names, are all said that tick.
func TestProdWatch_SentryAnEscalationFloodIsNamedWhole(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["min_level"] = "warning" }))
	sentryTick(t, h, wf)
	now := time.Now()
	for k := 0; k < 120; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(20001 + k), ShortID: strp(fmt.Sprintf("E%05d-%s", k, strings.Repeat("X", 57))), Title: "w",
			Level: "warning", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	seen := time.Now().Add(time.Second)
	for k := 0; k < 120; k++ {
		h.sentry.edit(fmt.Sprint(20001+k), func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = seen; i.Count++ })
	}
	sentryTick(t, h, wf)
	missed := 0
	for k := 0; k < 120; k++ {
		if rec := sentryIncident(t, h, fmt.Sprint(20001+k)); rec["announced_severity"] != "high" {
			missed++
		}
	}
	if missed > 0 {
		t.Fatalf("%d of 120 escalations (low to high, one event each) were not said the tick they were seen", missed)
	}
}

// TestProdWatch_ANotesNamesFitDespiteSchemeLookingNames: inline code puts a
// zero-width space before a scheme's colon — template names full of `https:`,
// `mailto:`, `ftp:` grow in the message; the names budget counts it, so each note
// at max_message_chars 4000 goes out whole — one message, every member named, its
// meta line kept (a budget that missed it would push the meta line past the clip).
func TestProdWatch_ANotesNamesFitDespiteSchemeLookingNames(t *testing.T) {
	t.Parallel()
	for scheme, reps := range map[string]int{"https:": 19, "mailto:": 16, "ftp:": 29, "HTTP:": 23} {
		scheme, reps := scheme, reps
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			pwNamesFitCase(t, scheme, reps)
		})
	}
}

func pwNamesFitCase(t *testing.T, scheme string, reps int) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(60, func(k int) string { return fmt.Sprintf("E%02d %s", k, strings.Repeat(scheme, reps)) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 4000})
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	// A fold label as long as plan allows (400) and a long count label: the note's overhead near the 1000
	// characters the budget leaves it — with the expansion uncounted, the names push the note into a second part.
	label := "{n} more of this kind this tick, not posted one by one " + strings.Repeat("(the operator's long wording) ", 11) + ": {names}"
	if len(label) > 400 {
		t.Fatalf("setup: the label is %d characters, plan allows 400", len(label))
	}
	count := "{n} occurrence(s) " + strings.Repeat("(the operator's long wording) ", 11)
	nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{"folded_detail": label, "count": count}, "app": map[string]any{"name": "demo"},
		"release": "", "release_known": false, "dry_run": true, "max_message_chars": 4000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, lastN(nerr, 400))
	}
	notes, parts := 0, 0
	for _, a := range out["alerts"].([]any) {
		if len(pwMembers(a)) > 0 {
			notes++
		}
	}
	var texts []string
	for _, m := range nout["messages"].([]any) {
		text := m.(map[string]any)["text"].(string)
		texts = append(texts, text)
		if strings.Contains(text, "not posted one by one") {
			parts++
			if !strings.Contains(text, "occurrence(s) (the operator") {
				t.Fatalf("%s: a note lost its meta line to the clip (%d characters): the names budget missed their expansion", scheme, len([]rune(text)))
			}
		}
	}
	if notes == 0 || parts != notes {
		t.Fatalf("%s: %d note(s) went out in %d message(s): the names budget missed their expansion", scheme, notes, parts)
	}
	all := strings.Join(texts, "\n")
	for k := 0; k < 60; k++ {
		if !strings.Contains(all, fmt.Sprintf("E%02d %s", k, scheme[:2])) {
			t.Fatalf("%s: template E%02d, marked said, is named in no message delivered whole", scheme, k)
		}
	}
}

// TestProdWatch_SentryNullSeenStatsAreNoLaneError: a page whose issues carry
// their seen stats as null (an issue with no event in the environment, read by
// id) is a legitimate answer — only a page carrying none of the keys at all is a
// lane error.
func TestProdWatch_SentryNullSeenStatsAreNoLaneError(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "61", ShortID: strp("P-61"), Title: "x", FirstProcessed: now, LastSeen: now})
	sentryTick(t, h, wf)
	h.sentry.edit("61", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour); i.Env = "staging" }) // no event in preprod
	o := sentryTick(t, h, wf)
	if len(h.sentry.callsTo("tracked")) == 0 || fmt.Sprint(o["poll_sentry"]["errors"]) != "[]" {
		t.Fatalf("a by-id answer whose lastSeen is null was taken for a stripped one: %d by-id call(s), errors %v",
			len(h.sentry.callsTo("tracked")), o["poll_sentry"]["errors"])
	}
}

// TestProdWatch_SentryALateFirstEventStillMakesANewIssue: a list returns only
// issues with an event inside its window — the lists search 90 days, as far
// back as Relay accepts an event's own time: an issue whose first event came
// 20 days late (an SDK's offline cache) is still posted NEW.
func TestProdWatch_SentryALateFirstEventStillMakesANewIssue(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "9001", ShortID: strp("PROJ-901"), Title: "late crash", FirstProcessed: now, LastSeen: now.Add(-20 * 24 * time.Hour), Count: 1})
	if got := sentryAlerts(sentryTick(t, h, wf)); !anyPrefix(got, "new:PROJ-901") {
		t.Fatalf("a new issue whose first event came 20 days late was not posted: %v", got)
	}
}

// TestProdWatch_SentryANotFoundListIsNamed: a 404 on the lists or the by-id read
// (an organization or an environment Sentry does not know) is named as such,
// never as an answer that did not parse.
func TestProdWatch_SentryANotFoundListIsNamed(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryWithProbe(h, nil))
	h.sentry.failNext("list", 404)
	o := sentryTick(t, h, wf)
	if errs := fmt.Sprint(o["poll_sentry"]["errors"]); !strings.Contains(errs, "Sentry answered 404") || strings.Contains(errs, "did not parse") {
		t.Fatalf("a 404 is not named: %s", errs)
	}
}

// TestProdWatch_SentryEnvironmentNoneIsRefused: Sentry reads `none` — that exact
// word — in a path as the empty environment name, and literally in a list's
// query: plan refuses it by name rather than check one environment and list
// another. "None" names one environment on both paths: accepted.
func TestProdWatch_SentryEnvironmentNoneIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for env, refused := range map[string]bool{"none": true, "None": false} {
		env, refused := env, refused
		t.Run(env, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["environment"] = env }))
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars,
				map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
			said := err != nil && strings.Contains(stderr, "config.sentry.environment 'none' names two different environments")
			if said != refused || strings.Contains(stderr, "Traceback") {
				t.Fatalf("environment %q: refused=%v, want %v: %v %s", env, said, refused, err, lastN(stderr, 300))
			}
		})
	}
}
