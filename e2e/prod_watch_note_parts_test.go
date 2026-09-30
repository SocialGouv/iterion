package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pwLongLabels: labels plan accepts, written as prose — the fold label within
// its 400 characters, {names} once — and a count label longer than the
// defaults' (plan bounds none of the meta labels).
func pwLongLabels() map[string]any {
	fold := "{n} more issues of this kind this tick were folded into this note instead of being posted one by one: the " +
		"per-lane cap (max_alerts_per_lane) keeps a flood from drowning the channel, since anyone holding the public DSN " +
		"can mint issues. Each one stays tracked; one that comes back is said again. Open Sentry for the details of " +
		"each: {names}"
	return map[string]any{
		"folded_detail":      fold,
		"folded_detail_more": fold + " — {more} more held for the next ticks",
		"alert":              "PRODUCTION ALERT (NEW, NOT YET TRIAGED)",
		"sentry_issue":       "new Sentry issue on the production platform (the on-call engineer triages it within the hour, see runbook)",
		"sentry_resolved":    "RESOLVED IN SENTRY (NO ACTION NEEDED NOW)",
		"count": "{n} occurrence(s) in total over the period the watchdog read (Sentry counts them per environment; the " +
			"lists count the last 90 days or the retention, a by-id read the whole life of the issue in every environment; " +
			"the runbook explains how to read them)",
		"new_since": "first seen by the watchdog on {date} (UTC, the runner clock)",
		"severity":  "severity (the level mapped by config.sentry.severity, capped by config.sentry.max_severity)",
	}
}

// pwNoteBodies: the delivered bodies that are notes of folded alerts (the long
// fold label's words), and whether any body passes max.
func pwNoteBodies(t *testing.T, bodies []string, max int) []string {
	t.Helper()
	var notes []string
	for _, b := range bodies {
		if n := len([]rune(b)); n > max {
			t.Fatalf("a message of %d characters, over max_message_chars %d:\n%.300s", n, max, b)
		}
		if strings.Contains(b, "folded into this note") {
			notes = append(notes, b)
		}
	}
	return notes
}

// TestProdWatch_AFloodNoteLongerThanTheMessageLeavesTheProbeAlert: the tick the
// app goes down, 40 new issues arrive (anyone holding the public DSN can mint
// them) and the labels make their note longer than max_message_chars: the note
// goes out in parts, every issue is named, and the app-down alert leaves with
// them — a note never holds the tick.
func TestProdWatch_AFloodNoteLongerThanTheMessageLeavesTheProbeAlert(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.msgChars.Store(1500)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryWithProbe(h, nil)(cfg)
		cfg["app"] = map[string]any{"name": "plateforme-emplois-api-gateway", "environment": "production-ovh-gra-cluster-2"}
		cfg["labels"] = pwLongLabels()
	})
	sentryTick(t, h, wf) // the lane arms; the app is up
	now := time.Now()
	for k := 0; k < 40; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(5001 + k), ShortID: strp(fmt.Sprintf("PLATEFORME-EMPLOIS-API-%d", 5001+k)),
			Title: "boom", Level: "error", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	h.healthStatus.Store(503)
	n := len(h.bodies())
	sentryTick(t, h, wf)
	bodies := h.bodies()[n:]
	all := strings.Join(bodies, "\n")
	if !strings.Contains(all, "health probe failing") {
		t.Fatalf("the app-down alert did not leave with the flood's note:\n%.1500s", all)
	}
	notes := pwNoteBodies(t, bodies, 1500)
	named := strings.Join(notes, "\n")
	singles := 0
	for k := 0; k < 40; k++ {
		id := fmt.Sprintf("PLATEFORME-EMPLOIS-API-%d", 5001+k)
		if strings.Contains(named, id) {
			continue
		}
		if !strings.Contains(all, id) {
			t.Fatalf("%s was named in no message", id)
		}
		singles++
	}
	if singles > 5 || len(notes) < 3 {
		t.Fatalf("want the issues past the per-lane quota named in notes, in parts: %d posted one by one, %d note message(s)", singles, len(notes))
	}
}

// TestProdWatch_ABulkResolveWithLongLabelsKeepsTheChannel: 25 alerted issues
// resolved in one bulk action while the app goes down — their closing note,
// longer than max_message_chars with these labels, goes out with the app-down
// alert (the clip drops its meta line, never a name); each member named is
// said, and the others follow on the next ticks.
func TestProdWatch_ABulkResolveWithLongLabelsKeepsTheChannel(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.msgChars.Store(1500)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryWithProbe(h, nil)(cfg)
		cfg["app"] = map[string]any{"name": "plateforme-emplois-api-gateway", "environment": "production-ovh-gra-cluster-2"}
		labels := pwLongLabels()
		labels["count"] = labels["count"].(string) + " — and the watchdog's own ledger keeps them too"
		cfg["labels"] = labels
	})
	sentryTick(t, h, wf)
	var ids []string
	for batch := 0; batch < 5; batch++ {
		now := time.Now()
		for k := 0; k < 5; k++ {
			id := fmt.Sprint(6001 + 5*batch + k)
			ids = append(ids, id)
			h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PLATEFORME-EMPLOIS-API-" + id), Title: "boom", Level: "error",
				FirstProcessed: now, LastSeen: now, Count: 1})
		}
		if o := sentryTick(t, h, wf); len(sentryAlerts(o)) != 5 {
			t.Fatalf("setup batch %d: want 5 new issues posted one by one, got %v", batch, sentryAlerts(o))
		}
		time.Sleep(1100 * time.Millisecond)
	}
	for _, id := range ids {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.Status = "resolved"; i.FirstProcessed = time.Now().Add(-3 * time.Hour) })
	}
	h.healthStatus.Store(503)
	n := len(h.bodies())
	sentryTick(t, h, wf)
	bodies := h.bodies()[n:]
	if !strings.Contains(strings.Join(bodies, "\n"), "health probe failing") {
		t.Fatalf("the app-down alert did not leave with the closing notes:\n%.1500s", strings.Join(bodies, "\n"))
	}
	notes := pwNoteBodies(t, bodies, 1500)
	if len(notes) == 0 || strings.Contains(notes[0], "occurrence(s) in total") {
		t.Fatalf("want the closing note over the limit, its meta line clipped: %d note message(s):\n%.600s", len(notes), strings.Join(notes, "\n"))
	}
	// Every member said is named in a delivered part; the others are named on the next ticks.
	for k := 0; k < 6; k++ {
		all := strings.Join(h.bodies()[n:], "\n")
		missing := 0
		for _, id := range ids {
			said := sentryIncident(t, h, id)["closed_said"] == true
			if said && !strings.Contains(all, "PLATEFORME-EMPLOIS-API-"+id) {
				t.Fatalf("issue %s is marked said, yet no delivered message names it", id)
			}
			if !said {
				missing++
			}
		}
		if missing == 0 {
			return
		}
		time.Sleep(1100 * time.Millisecond)
		sentryTick(t, h, wf)
	}
	t.Fatalf("the closing notes of 25 resolved issues were not all said in 6 ticks")
}

// TestProdWatch_ANoteOverTheLimitByItsMetaLineKeepsEveryName: a note longer than
// max_message_chars through its meta labels alone goes out whole — the clip
// drops its meta line, never a name.
func TestProdWatch_ANoteOverTheLimitByItsMetaLineKeepsEveryName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	tpls := pwMintedTemplates(60, func(k int) string { return fmt.Sprintf("E%02d %s", k, strings.Repeat("y", 110)) })
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_message_chars": 1500})
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	labels := pwLongLabels()
	labels["count"] = labels["count"].(string) + "; a count that grows fast between two notes is worth a look even when the " +
		"severity stays the same, and the ledger of every alert keeps the numbers of each tick for a later review"
	labels["release"] = "release deployed on the platform (as its health endpoint reports it, not verified against the repository)"
	nout, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": out["alerts"], "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": labels, "app": map[string]any{"name": "demo"}, "release": "v2.41.0-rc.3+sha.1a2b3c4", "release_known": false,
		"dry_run": true, "max_message_chars": 1500, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify: %v %s", err, lastN(nerr, 400))
	}
	var texts []string
	for _, m := range nout["messages"].([]any) {
		texts = append(texts, m.(map[string]any)["text"].(string))
	}
	notes := pwNoteBodies(t, texts, 1500)
	named := strings.Join(notes, "\n")
	for _, a := range out["alerts"].([]any) {
		for _, m := range pwMembers(a) {
			if !strings.Contains(named, m) {
				t.Fatalf("member %q, marked said, is named in no delivered note", m)
			}
		}
	}
	for _, n := range notes {
		if strings.Contains(n, "occurrence(s) in total") {
			t.Fatalf("setup: the meta line fit — this note is not over the limit by it:\n%.400s", n)
		}
	}
}

// pwMembers: the names a note of folded alerts carries (none for an alert).
func pwMembers(a any) []string {
	var names []string
	ms, _ := a.(map[string]any)["members"].([]any)
	for _, m := range ms {
		names = append(names, fmt.Sprint(m.(map[string]any)["name"]))
	}
	return names
}

// TestProdWatch_LabelsLeavingANoteNoRoomAreRefusedEveryTick: labels plan accepts
// that leave a note no room to name one member (a fold label escaping into 5×
// its length) fail the tick at once — no flood, no note — and pass with a
// budget that holds them.
func TestProdWatch_LabelsLeavingANoteNoRoomAreRefusedEveryTick(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	fold := "{names} " + strings.Repeat("&", 392)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["labels"] = map[string]any{"folded_detail": fold, "folded_detail_more": fold}
	})
	plan, perr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, map[string]any{"workspace_dir": h.ws,
		"config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch", "max_window_minutes": 60, "fetch_timeout_secs": 20,
		"ingest_lag_seconds": 0, "max_lines": 5000}, map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}))
	if err != nil {
		t.Fatalf("setup: plan refused the labels: %v %s", err, lastN(perr, 300))
	}
	notify := func(max int) (string, error) {
		_, nerr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
			"alerts": []any{}, "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
			"labels": plan["labels"], "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
			"dry_run": false, "max_message_chars": max, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
		return nerr, err
	}
	if nerr, err := notify(1500); err == nil || !strings.Contains(nerr, "no room to name one member") || strings.Contains(nerr, "Traceback") {
		t.Fatalf("a nothing-to-deliver tick with labels leaving a note no room was not refused by name: %v %s", err, lastN(nerr, 300))
	}
	if nerr, err := notify(4000); err != nil {
		t.Fatalf("the same labels with max_message_chars 4000 were refused: %v %s", err, lastN(nerr, 300))
	}
}

// TestProdWatch_ANoteCheckMeetsItsWriterAtTheBoundary: the per-tick check and the
// note writer agree to the character, for each kind decide folds. With the
// longest header on one kind's note (a closed Sentry issue; an idle log
// template) and a fold label holding no number, the check's worst note IS a
// real note whose member's name is as long as NAME_CAP lets it render: at the
// check's own limit the note goes out with its whole names line (the clip
// keeps a line ending at the limit), one character less is refused.
func TestProdWatch_ANoteCheckMeetsItsWriterAtTheBoundary(t *testing.T) {
	t.Parallel()
	fold := "{names} " + strings.Repeat("&", 100)
	name := strings.Repeat("ftp:", 50) // 200 characters: NAME_CAP renders 130 of them
	for _, c := range []struct {
		kind, state, marker, title string
		evidence                   map[string]any
	}{
		{"sentry", "resolved", "sentry_closed", "sentry_issue", map[string]any{"status": "closed"}},
		{"loki", "quiet", "quiet", "loki_template", map[string]any{}},
	} {
		c := c
		t.Run(c.kind, func(t *testing.T) {
			t.Parallel()
			wf := compileFixture(t, "prod-watch/main.bot")
			h := newPWHarness(t)
			labels := map[string]any{c.marker: strings.Repeat("&", 40), c.title: strings.Repeat("&", 120),
				"folded_detail": fold, "folded_detail_more": fold}
			note := map[string]any{"fingerprint": "folded:" + c.kind + ":" + c.state + ":medium::0", "kind": c.kind, "state": c.state,
				"severity": "medium", "title_key": c.title, "title_arg": "", "detail_key": "folded_detail",
				"fields": map[string]any{"n": 1, "names": name, "more": 0}, "evidence": c.evidence, "count": 1,
				"first_seen": "2026-09-30T10:00:00+00:00", "prev_severity": "",
				"members": []any{map[string]any{"fp": c.kind + ":1", "severity": "medium", "prev_severity": "", "transition_at": nil, "name": name, "count": 1}}}
			notify := func(max int, alerts []any) (map[string]any, string, error) {
				return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
					"alerts": alerts, "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
					"labels": labels, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
					"dry_run": true, "max_message_chars": max, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
			}
			_, nerr, err := notify(1500, []any{})
			m := regexp.MustCompile(`header renders up to (\d+) characters and its names line up to (\d+)`).FindStringSubmatch(nerr)
			if err == nil || m == nil {
				t.Fatalf("setup: at 1500 the check must refuse and name its numbers: %v %s", err, lastN(nerr, 300))
			}
			limit := atoiOr(m[1]) + 1 + atoiOr(m[2])
			if _, nerr, err := notify(limit-1, []any{}); err == nil || !strings.Contains(nerr, "no room to name one member") {
				t.Fatalf("one character under the check's limit (%d) was not refused: %v %s", limit, err, lastN(nerr, 300))
			}
			out, nerr, err := notify(limit, []any{note})
			if err != nil {
				t.Fatalf("at the check's limit (%d) the tick was refused: %v %s", limit, err, lastN(nerr, 300))
			}
			msgs := out["messages"].([]any)
			if len(msgs) != 1 {
				t.Fatalf("want the note in one message at the limit, got %d", len(msgs))
			}
			text := msgs[0].(map[string]any)["text"].(string)
			lines := strings.Split(text, "\n")
			shown := strings.Repeat("ftp\u200b:", 32) + "ft`"
			if len([]rune(text)) != limit || len(lines) != 2 || !strings.Contains(lines[1], shown) || !strings.HasSuffix(lines[1], strings.Repeat("&amp;", 100)) {
				t.Fatalf("at the check's limit %d the note must keep its header and whole names line, its name cut to NAME_CAP: %d characters, %d line(s):\n%s",
					limit, len([]rune(text)), len(lines), text)
			}
		})
	}
}

func atoiOr(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// TestProdWatch_AFuturePendingStampGoesFirst: a pending alert whose pending_since
// a clock running ahead wrote is taken for the oldest — posted before the
// fresher pending alerts a trickle of new templates brings (one a tick, a
// per-run cap of 1), not after real time catches up.
func TestProdWatch_AFuturePendingStampGoesFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	ahead := time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00") // a runner 3 days ahead
	pend := func(id, since string) map[string]any {
		return map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": "ERROR " + id, "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": old, "last_seen": old, "count": 3, "alerted": false, "last_notified": nil, "quiet_noted": false,
			"pending": "new", "pending_since": since}
	}
	incidents := map[string]any{"loki:pzz": pend("pzz", ahead), "loki:qaa": pend("qaa", old)}
	var posted []string
	for tick := 0; tick < 2; tick++ {
		tpls := pwMintedTemplates(1, func(int) string { return fmt.Sprint("ERROR trickle ", tick) })
		tpls[0].(map[string]any)["template_id"] = fmt.Sprintf("n%03d", tick)
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(incidents),
			map[string]any{"max_alerts": 1, "max_alerts_per_lane": 20})
		if err != nil {
			t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
		}
		for _, a := range out["alerts"].([]any) {
			posted = append(posted, fmt.Sprint(a.(map[string]any)["fingerprint"]))
		}
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		if incidents["loki:pzz"].(map[string]any)["pending"] == nil {
			return
		}
		time.Sleep(1100 * time.Millisecond)
	}
	t.Fatalf("a pending alert stamped 3 days ahead was not among the first two posted (cap 1, one new template a tick): %v", posted)
}

// TestProdWatch_SentryAFutureCheckStampGoesFirst: a re-check stamp a clock
// running ahead wrote is taken for none — that regressed issue is re-checked
// first under max_transition_checks 1, and its second regression is found the
// next tick, not after every other issue's turn.
func TestProdWatch_SentryAFutureCheckStampGoesFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"1901", "1902", "1903"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PROJ-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	}
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
	}
	for _, id := range []string{"1901", "1902", "1903"} {
		if sentryIncident(t, h, id)["transition_checked_at"] == nil {
			t.Fatalf("setup: issue %s was never re-checked", id)
		}
	}
	// 1903, checked last, is stamped 3 days ahead, and regresses again from another environment.
	sentryEditRecord(t, h, "1903", func(r map[string]any) {
		r["transition_checked_at"] = time.Now().Add(72 * time.Hour).UTC().Format("2006-01-02T15:04:05+00:00")
	})
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("1903", func(i *pwSentryIssue) { i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1}) })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "regressed:PROJ-1903:medium" {
		t.Fatalf("an issue whose re-check stamp is 3 days ahead was not re-checked first: %v", got)
	}
}
