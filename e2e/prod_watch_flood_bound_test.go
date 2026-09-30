package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestProdWatch_SentryAFloodNamesWhatFitsAndHoldsTheRest: one note of a kind a
// tick, whatever the flood — it names what the message budget holds, the
// others wait (pending, counted in the note) and the next tick names them
// first: every issue is said once, and no tick posts more than
// max_alerts_per_lane alerts and the note.
func TestProdWatch_SentryAFloodNamesWhatFitsAndHoldsTheRest(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	h.msgChars.Store(1500) // a 500-character names budget: about 45 short ids
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 90)
	said := map[string]int{}
	for k := 0; k < 2; k++ {
		n := len(h.bodies())
		o := sentryTick(t, h, wf)
		notes, more := 0, ""
		for _, a := range o["decide"]["alerts"].([]any) {
			m := a.(map[string]any)
			if ms, ok := m["members"].([]any); ok {
				notes++
				more = fmt.Sprint(m["fields"].(map[string]any)["more"])
				for _, x := range ms {
					said[x.(map[string]any)["fp"].(string)]++
				}
			} else {
				said[m["fingerprint"].(string)]++
			}
		}
		if posted := len(h.bodies()) - n; notes != 1 || posted != 6 {
			t.Fatalf("tick %d: want 5 alerts and ONE note posted, got %d note(s) and %d message(s)", k, notes, posted)
		}
		if p := fmt.Sprint(sentryPendingCount(t, h)); k == 0 && (more == "0" || more != p) {
			t.Fatalf("85 issues past a 500-character names budget: want some held — pending, counted in the note: %s counted, %s pending", more, p)
		}
	}
	for k := 0; k < 90; k++ {
		if fp := fmt.Sprint("sentry:", 5001+k); said[fp] != 1 {
			t.Fatalf("%s said %d time(s) over two ticks", fp, said[fp])
		}
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("%d issues still pending after the second note", p)
	}
}

// TestProdWatch_ALogTemplateIdleStormIsOneNoteATick: thousands of minted
// templates going idle together (a flood that stopped) post one note a tick,
// never a storm — max_alerts_per_lane idle notes one by one and one note
// naming what fits; the others keep their idle note owed.
func TestProdWatch_ALogTemplateIdleStormIsOneNoteATick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}},
		pwLokiState(pwAlertedTemplates(2400, "ERROR route not found for a path a flood minted, template ", old)), nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": out["overflow_count"], "stale_sources": []any{},
		"sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}}, "labels": map[string]any{},
		"app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": true, "max_message_chars": 14000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, lastN(nerr, 400))
	}
	if msgs := nout["messages"].([]any); len(msgs) != 6 {
		t.Fatalf("2400 idle templates: want 5 idle notes and one note in the tick, got %d message(s)", len(msgs))
	}
	named := 0
	for _, a := range out["alerts"].([]any) {
		if ms, ok := a.(map[string]any)["members"].([]any); ok {
			named += len(ms)
		} else {
			named++
		}
	}
	noted := 0
	for _, r := range pwStateNext(t, out)["incidents"].(map[string]any) {
		if r.(map[string]any)["quiet_noted"] == true {
			noted++
		}
	}
	if noted != named || named >= 2400 {
		t.Fatalf("%d templates said idle this tick, %d marked noted: the others must keep their note owed", named, noted)
	}
}

// TestProdWatch_SentryAFloodAcrossKindsCannotHoldTheCap: a minting lane posts
// at most max_alerts_per_lane alerts one by one per tick whatever their kinds
// — a flood of new issues and regressions at fatal cannot fill the per-run
// cap, and a real error-level issue arriving with it is named the same tick,
// never cut behind the flood.
func TestProdWatch_SentryAFloodAcrossKindsCannotHoldTheCap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 60 }))
	sentryTick(t, h, wf)
	h.alertCap.Store(5)
	sentryJunk(h, 5001, 10)
	now := time.Now()
	for r := 0; r < 10; r++ {
		treg := now.Add(-10 * time.Second)
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(7001 + r), ShortID: strp(fmt.Sprint("R-", 7001+r)), Title: "junk regressed", Level: "fatal",
			Substatus: strp("regressed"), FirstProcessed: now.Add(-40 * 24 * time.Hour), LastSeen: treg,
			Acts: []pwSentryAct{{Type: "set_resolved", At: now.Add(-3 * 24 * time.Hour)}, {Type: "set_regression", At: treg}}})
	}
	h.sentry.put(&pwSentryIssue{ID: "4242", ShortID: strp("REAL-4242"), Title: "the real outage", Level: "error",
		FirstProcessed: now, LastSeen: now, Count: 50})
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	singles := 0
	for _, a := range o["decide"]["alerts"].([]any) {
		if m := a.(map[string]any); m["kind"] == "sentry" && m["members"] == nil {
			singles++
		}
	}
	if singles != 5 {
		t.Fatalf("ten new issues and ten regressions at fatal: want 5 alerts one by one, got %d (%v %v)", singles, sentryAlerts(o), sentryFolds(o))
	}
	if !strings.Contains(strings.Join(h.bodies()[n:], "\n"), "REAL-4242") {
		t.Fatalf("the real issue was not named the tick it arrived (pending=%v), notes %v", sentryIncident(t, h, "4242")["pending"], sentryFolds(o))
	}
	if p := sentryPendingCount(t, h); p != 0 {
		t.Fatalf("the lane's alerts held the cap of 5: %d pending", p)
	}
}

// TestProdWatch_SentryALanesMostSevereGoOneByOne: the alerts a lane posts one
// by one are its most severe — an error-level issue read first never takes a
// slot from fatal ones; it is named.
func TestProdWatch_SentryALanesMostSevereGoOneByOne(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1001", ShortID: strp("ERR-1001"), Title: "x", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	for k := 0; k < 6; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(2001 + k), ShortID: strp(fmt.Sprint("FATAL-", 2001+k)), Title: "x", Level: "fatal",
			FirstProcessed: now, LastSeen: now, Count: 1})
	}
	o := sentryTick(t, h, wf)
	for _, a := range o["decide"]["alerts"].([]any) {
		if m := a.(map[string]any); m["kind"] == "sentry" && m["members"] == nil && m["severity"] != "high" {
			t.Fatalf("a %v alert went one by one ahead of fatal ones: %v %v", m["severity"], sentryAlerts(o), sentryFolds(o))
		}
	}
	if single := sentrySingles(o, "new", "FATAL-"); single != 5 || strings.Join(sentryFolds(o), " ") != "new:1 new:1" {
		t.Fatalf("six fatal issues and an error-level one: want 5 fatal alerts and two notes of one, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
}

// TestProdWatch_SentryAFloodsMessagesAreBounded: nearly the most a lane reads
// in a tick at the default caps — 190 minted issues at the two levels a DSN
// holder sets — posts max_alerts_per_lane alerts and one note per level:
// seven messages, every issue named.
func TestProdWatch_SentryAFloodsMessagesAreBounded(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	for k := 0; k < 190; k++ {
		lvl := map[int]string{0: "fatal", 1: "error"}[k%2]
		id := fmt.Sprint(30001 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("JUNK-" + id), Title: "junk", Level: lvl, FirstProcessed: now, LastSeen: now, Count: 1})
	}
	n := len(h.bodies())
	sentryTick(t, h, wf)
	if posted := len(h.bodies()) - n; posted != 7 {
		t.Fatalf("190 minted issues at two levels: want 5 alerts and 2 notes, got %d message(s)", posted)
	}
	body := strings.Join(h.bodies()[n:], "\n")
	for k := 0; k < 190; k++ {
		if id := fmt.Sprint("JUNK-", 30001+k); !strings.Contains(body, id) {
			t.Fatalf("%s is named nowhere", id)
		}
	}
}

// TestProdWatch_AMessageBudgetTooSmallForANoteIsRefused: a note names its
// members inside one message; a max_message_chars that cannot hold them is
// refused by name — never a note clipped of the names it stamps as said.
func TestProdWatch_AMessageBudgetTooSmallForANoteIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	_, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 1499})
	if err == nil || !strings.Contains(stderr, "max_message_chars must be 1500 or more") || strings.Contains(stderr, "Traceback") {
		t.Fatalf("max_message_chars 1499 was not refused by name: %v %s", err, lastN(stderr, 400))
	}
	if _, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 1500}); err != nil {
		t.Fatalf("max_message_chars 1500 refused: %v %s", err, lastN(stderr, 400))
	}
}

// TestProdWatch_AFoldedClosingNoteNamesItsStatus: closings fold per status — a
// note of resolved issues says RESOLVED, never the status-less "deleted or
// merged".
func TestProdWatch_AFoldedClosingNoteNamesItsStatus(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 12)
	sentryTick(t, h, wf)
	for k := 0; k < 12; k++ {
		h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) { i.Status = "resolved" })
	}
	n := len(h.bodies())
	o := sentryTick(t, h, wf)
	if strings.Join(sentryFolds(o), " ") != "resolved:7" {
		t.Fatalf("setup: 12 closings at max_alerts_per_lane 5: want a note of 7, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	body := strings.Join(h.bodies()[n:], "\n")
	if strings.Contains(body, "DELETED OR MERGED") || strings.Count(body, "RESOLVED IN SENTRY") != 6 {
		t.Fatalf("12 issues resolved (5 notes one by one, 7 folded): every message must say RESOLVED IN SENTRY:\n%s", body)
	}
}

// TestProdWatch_AFoldedClosingMemberSaysReopened: a member of a folded closing
// note is stamped closed like a closing note posted one by one — reopened, it
// says so, folded again.
func TestProdWatch_AFoldedClosingMemberSaysReopened(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	sentryJunk(h, 5001, 12)
	sentryTick(t, h, wf)
	for k := 0; k < 12; k++ {
		h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) {
			i.Status = "ignored"
			i.Substatus = strp("archived_until_condition_met")
		})
	}
	if o := sentryTick(t, h, wf); strings.Join(sentryFolds(o), " ") != "resolved:7" {
		t.Fatalf("setup: 12 archived: want 5 notes one by one and a note of 7, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	for k := 0; k < 12; k++ {
		h.sentry.edit(fmt.Sprint(5001+k), func(i *pwSentryIssue) {
			i.Status = "unresolved"
			i.Substatus = strp("ongoing")
		})
	}
	o := sentryTick(t, h, wf)
	if single, folds := sentrySingles(o, "reopened", "JUNK-"), sentryFolds(o); single != 5 || strings.Join(folds, " ") != "reopened:7" {
		t.Fatalf("12 archived issues (7 closed in a note) open again: want 5 reopened one by one and a note of 7, got %v %v", sentryAlerts(o), folds)
	}
}

// TestProdWatch_AFoldedEscalationIsAnnounced: a member of a note of
// escalations is stamped at the severity the note said — its next event at
// that level is no escalation.
func TestProdWatch_AFoldedEscalationIsAnnounced(t *testing.T) {
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
	sentryTick(t, h, wf)
	for k := 0; k < 4; k++ {
		h.sentry.edit(fmt.Sprint(9001+k), func(i *pwSentryIssue) {
			i.Level = "fatal"
			i.FirstProcessed = now.Add(-3 * time.Hour)
			i.LastSeen = time.Now().Add(time.Second)
		})
	}
	if o := sentryTick(t, h, wf); strings.Join(sentryFolds(o), " ") != "escalated:3" {
		t.Fatalf("setup: four escalations at max_alerts_per_lane 1: want a note of 3, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
	for k := 0; k < 4; k++ {
		h.sentry.edit(fmt.Sprint(9001+k), func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(2 * time.Second) })
	}
	if o := sentryTick(t, h, wf); len(sentryAlerts(o)) != 0 {
		t.Fatalf("escalated to high (said, one by one or in a note), firing again at high: want nothing, got %v %v", sentryAlerts(o), sentryFolds(o))
	}
}

// TestProdWatch_TemplatesSharingTheirTitleAreToldApart: two templates can share
// their first 120 characters — the title a note shows — so a note names each
// by its id too.
func TestProdWatch_TemplatesSharingTheirTitleAreToldApart(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	prefix := "ERROR the payment gateway refused the request of the merchant because the signature did not match the expected value for"
	tpls := pwMintedTemplates(12, func(k int) string { return fmt.Sprintf("%s account %02d", prefix, k) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}), nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	names := ""
	for _, a := range out["alerts"].([]any) {
		if m := a.(map[string]any); m["members"] != nil {
			names += fmt.Sprint(m["fields"].(map[string]any)["names"])
		}
	}
	for k := 5; k < 12; k++ {
		if !strings.Contains(names, fmt.Sprintf("t%02d ERROR", k)) {
			t.Fatalf("template t%02d (its first 120 characters shared with the others) is not told apart in the note: %q", k, names)
		}
	}
}
