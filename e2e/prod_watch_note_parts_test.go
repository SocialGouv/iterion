package e2e

import (
	"fmt"
	"regexp"
	"sort"
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
// note writer agree to the character, on notes decide builds. With the longest
// header on one kind's note — a reopened Sentry issue, a log template's
// reminder, both at medium (the longest icon) — and a fold label holding no
// number, the check's worst note IS a real note whose member's name is as long
// as NAME_CAP lets it render: at the check's own limit the note goes out with its
// whole names line (the clip keeps a line ending at the limit), one character
// less is refused. A closing note, always at low, keeps its whole names line at
// that limit too.
func TestProdWatch_ANoteCheckMeetsItsWriterAtTheBoundary(t *testing.T) {
	t.Parallel()
	fold := "{names} " + strings.Repeat("&", 100)
	name := strings.Repeat("ftp:", 50) // 200 characters: NAME_CAP renders 130 of them
	for _, c := range []struct {
		name, kind, state, sev, status, marker, title string
		exact, held                                   bool
	}{
		{"sentry", "sentry", "reopened", "medium", "", "sentry_reopened", "sentry_issue", true, false},
		{"loki", "loki", "reminder", "medium", "", "reminder", "loki_template", true, false},
		{"closing", "sentry", "resolved", "low", "closed", "sentry_closed", "sentry_issue", false, false},
		{"held", "loki", "reminder", "medium", "", "reminder", "loki_template", true, true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			labels := map[string]any{c.marker: strings.Repeat("&", 40), c.title: strings.Repeat("&", 120),
				"folded_detail": fold, "folded_detail_more": fold}
			if c.held {
				// A held note's last part says an eight-digit `more`: the check counts it at that width.
				labels["folded_detail"] = "{names} " + strings.Repeat("&", 50)
				labels["folded_detail_more"] = "{names} {more} " + strings.Repeat("&", 100)
			}
			_, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{})
			m := pwCheckLimit.FindStringSubmatch(nerr)
			if err == nil || m == nil {
				t.Fatalf("setup: at 1500 the check must refuse and name its numbers: %v %s", err, lastN(nerr, 300))
			}
			limit := atoiOr(m[1]) + 1 + atoiOr(m[2])
			if _, nerr, err := pwNotifyDry(t, h, labels, limit-1, []any{}); err == nil || !strings.Contains(nerr, "no room to name one member") {
				t.Fatalf("one character under the check's limit (%d) was not refused: %v %s", limit, err, lastN(nerr, 300))
			}
			note := pwOneMemberNote(c.kind, c.state, c.sev, c.status, name)
			if c.held {
				note["detail_key"] = "folded_detail_more"
				note["fields"].(map[string]any)["more"] = 99999999
			}
			out, nerr, err := pwNotifyDry(t, h, labels, limit, []any{note})
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
			whole := len(lines) >= 2 && strings.Contains(lines[1], shown) && strings.HasSuffix(lines[1], strings.Repeat("&amp;", 100))
			if !whole || (c.exact && (len([]rune(text)) != limit || len(lines) != 2)) {
				t.Fatalf("at the check's limit %d the note must keep its header and whole names line, its name cut to NAME_CAP: %d characters, %d line(s):\n%s",
					limit, len([]rune(text)), len(lines), text)
			}
		})
	}
}

// TestProdWatch_TheNoteCheckPairsEachKindWithItsOwnStates: the per-tick check's
// limit is the need of the longest one-member note a kind can carry — each
// kind with its own states (a log template's note never carries a Sentry
// state), at the longest icon, its names line counting one member as one —
// never a pairing no note makes; and its refusal names the labels behind it.
func TestProdWatch_TheNoteCheckPairsEachKindWithItsOwnStates(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	labels := pwPlanLabels(t, h, map[string]any{"sentry_resolved": strings.Repeat("&", 40), "loki_template": strings.Repeat("&", 120),
		"folded_detail": "{n} {names} " + strings.Repeat("&", 380), "folded_detail_more": "{n} {names} {more} " + strings.Repeat("&", 300)})
	_, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{})
	m := pwCheckLimit.FindStringSubmatch(nerr)
	if err == nil || m == nil {
		t.Fatalf("setup: at 1500 the check must refuse and name its numbers: %v %s", err, lastN(nerr, 300))
	}
	limit := atoiOr(m[1]) + 1 + atoiOr(m[2])
	if !strings.Contains(nerr, "(labels.quiet with labels.loki_template)") || !strings.Contains(nerr, "(labels.folded_detail)") {
		t.Fatalf("the refusal does not name the labels behind its numbers: %s", lastN(nerr, 400))
	}
	name := strings.Repeat("ftp:", 50)
	var notes []any
	for _, st := range []string{"new", "escalated", "reminder", "quiet", "regressed", "escalating", "reopened"} {
		notes = append(notes, pwOneMemberNote("sentry", st, "medium", "", name))
	}
	for _, status := range []string{"resolved", "ignored", "closed"} {
		notes = append(notes, pwOneMemberNote("sentry", "resolved", "medium", status, name))
	}
	for _, st := range []string{"new", "escalated", "reminder", "quiet"} {
		notes = append(notes, pwOneMemberNote("loki", st, "medium", "", name))
	}
	out, nerr, err := pwNotifyDry(t, h, labels, 4000, notes)
	if err != nil {
		t.Fatalf("setup: at 4000 the notes must render: %v %s", err, lastN(nerr, 300))
	}
	need := 0
	for _, msg := range out["messages"].([]any) {
		lines := strings.Split(msg.(map[string]any)["text"].(string), "\n")
		if k := len([]rune(lines[0])) + 1 + len([]rune(lines[1])); k > need {
			need = k
		}
	}
	if limit != need {
		t.Fatalf("the check demands %d characters; the longest one-member note a kind carries needs %d", limit, need)
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

// pwCheckLimit reads the per-tick note check's numbers out of its refusal.
var pwCheckLimit = regexp.MustCompile(`header renders up to (\d+) characters .*and its names line up to (\d+) with one name`)

// pwPlanLabels: the labels plan hands notify for these overrides (defaults merged).
func pwPlanLabels(t *testing.T, h *pwHarness, overrides map[string]any) map[string]any {
	t.Helper()
	wf := compileFixture(t, "prod-watch/main.bot")
	h.writeConfig(t, func(cfg map[string]any) { cfg["labels"] = overrides })
	plan, perr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, map[string]any{"workspace_dir": h.ws,
		"config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch", "max_window_minutes": 60, "fetch_timeout_secs": 20,
		"ingest_lag_seconds": 0, "max_lines": 5000}, map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}))
	if err != nil {
		t.Fatalf("setup: plan refused the labels: %v %s", err, lastN(perr, 300))
	}
	return plan["labels"].(map[string]any)
}

// pwNotifyDry runs notify alone, dry, on these alerts.
func pwNotifyDry(t *testing.T, h *pwHarness, labels map[string]any, max int, alerts []any) (map[string]any, string, error) {
	t.Helper()
	wf := compileFixture(t, "prod-watch/main.bot")
	return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": alerts, "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": labels, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": true, "max_message_chars": max, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": h.webhooksFile}))
}

// pwOneMemberNote: a one-member note of folded alerts as decide builds it.
func pwOneMemberNote(kind, state, sev, status, name string) map[string]any {
	title := map[string]string{"sentry": "sentry_issue", "loki": "loki_template"}[kind]
	ev := map[string]any{}
	if status != "" {
		ev["status"] = status
	}
	return map[string]any{"fingerprint": fmt.Sprintf("folded:%s:%s:%s:%s:0", kind, state, sev, status), "kind": kind, "state": state,
		"severity": sev, "title_key": title, "title_arg": "", "detail_key": "folded_detail",
		"fields": map[string]any{"n": 1, "names": name, "more": 0}, "evidence": ev, "count": 1,
		"first_seen": "2026-09-30T10:00:00+00:00", "prev_severity": "",
		"members": []any{map[string]any{"fp": kind + ":1", "severity": sev, "prev_severity": "", "transition_at": nil, "name": name, "count": 1}}}
}

// pwHeldNote: an idle-template note holding `more` members back, as decide builds one (a quiet note at low).
func pwHeldNote(names []string, more int) map[string]any {
	var members []any
	for k, n := range names {
		members = append(members, map[string]any{"fp": fmt.Sprintf("loki:t%07d", k), "severity": "low", "prev_severity": "",
			"transition_at": nil, "name": n, "count": k + 1})
	}
	return map[string]any{"fingerprint": "folded:loki:quiet:low::0", "kind": "loki", "state": "quiet", "severity": "low",
		"title_key": "loki_template", "title_arg": "", "detail_key": "folded_detail_more",
		"fields": map[string]any{"n": len(names), "names": strings.Join(names, ", "), "more": more}, "evidence": map[string]any{},
		"count": len(names) * (len(names) + 1) / 2, "first_seen": "2026-09-28T10:00:00+00:00", "prev_severity": "", "members": members}
}

// TestProdWatch_APartFitsInBothOfItsForms: the greedy sizes each part against
// both of its forms — the plain one (not last) and the held one (last). With the
// held form the longer, a greedy sized on the plain form alone leaves the last
// part over the limit (its names line clipped); with the plain form the longer,
// a greedy sized on the held form alone leaves every other part over it.
func TestProdWatch_APartFitsInBothOfItsForms(t *testing.T) {
	t.Parallel()
	var names []string
	for k := 0; k < 60; k++ {
		names = append(names, fmt.Sprintf("t%07d ERROR template number %03d went quiet", k, k))
	}
	for name, c := range map[string]struct {
		plain, held string
		members     int
	}{
		"held form longer":  {"{n} more: {names}", "{n} more: {names} — {more} more held for the next ticks " + strings.Repeat("&", 150), 24},
		"plain form longer": {"{n} more: {names} " + strings.Repeat("&", 150), "{n} more: {names} — {more} held", 60},
	} {
		c := c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			labels := pwPlanLabels(t, h, map[string]any{"folded_detail": c.plain, "folded_detail_more": c.held})
			out, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{pwHeldNote(names[:c.members], 20)})
			if err != nil {
				t.Fatalf("notify: %v %s", err, lastN(nerr, 300))
			}
			var texts []string
			for _, m := range out["messages"].([]any) {
				texts = append(texts, m.(map[string]any)["text"].(string))
			}
			all := strings.Join(texts, "\n")
			for _, n := range names[:c.members] {
				if strings.Count(all, n) != 1 {
					t.Fatalf("member %q is named %d time(s) across %d part(s)", n, strings.Count(all, n), len(texts))
				}
			}
			// The labels leave over 600 characters to these 45-character names: a part holds ten of them at least.
			if most := (c.members+9)/10 + 1; len(texts) > most {
				t.Fatalf("%d members went out in %d parts, more than %d: the parts are not packed", c.members, len(texts), most)
			}
		})
	}
}

// TestProdWatch_EachPartSaysItsOwnNumberAndCount: a part names its own members —
// its {n} is their number and its meta line's count the sum of their counts, not
// the whole note's.
func TestProdWatch_EachPartSaysItsOwnNumberAndCount(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	labels := pwPlanLabels(t, h, map[string]any{"folded_detail": "{n} more: {names} " + strings.Repeat("&", 150),
		"folded_detail_more": "{n} more: {names} — {more} held " + strings.Repeat("&", 150)})
	var names []string
	for k := 0; k < 24; k++ {
		names = append(names, fmt.Sprintf("t%07d ERROR template number %03d went quiet", k, k))
	}
	out, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{pwHeldNote(names, 20)})
	if err != nil {
		t.Fatalf("notify: %v %s", err, lastN(nerr, 300))
	}
	msgs := out["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("setup: want the note in parts, got %d message(s)", len(msgs))
	}
	nRe := regexp.MustCompile("^`(\\d+)` more")
	cRe := regexp.MustCompile("`(\\d+)` occurrence")
	metas := 0
	for i, m := range msgs {
		lines := strings.Split(m.(map[string]any)["text"].(string), "\n")
		want, sum := 0, 0
		for k, n := range names {
			if strings.Contains(lines[1], n) {
				want++
				sum += k + 1
			}
		}
		if g := nRe.FindStringSubmatch(lines[1]); g == nil || atoiOr(g[1]) != want {
			t.Fatalf("part %d names %d member(s) but says %v", i+1, want, g)
		}
		if len(lines) > 2 {
			metas++
			if g := cRe.FindStringSubmatch(lines[2]); g == nil || atoiOr(g[1]) != sum {
				t.Fatalf("part %d: its members' counts sum to %d, its meta line says %v", i+1, sum, g)
			}
		}
	}
	if metas == 0 {
		t.Fatalf("setup: no part kept its meta line — the count is not witnessed")
	}
}

// pwSkewedTick runs a tick on a runner whose clock is `skew` late (negative:
// early), for the wall stamps plan reads: seen from that runner every stamp is
// `skew` later than from a correct one, and the stamps it writes `skew` earlier.
func pwSkewedTick(t *testing.T, h *pwHarness, skew time.Duration, tick func()) {
	t.Helper()
	shift := func(d time.Duration) {
		st := h.state(t)
		for _, v := range st["incidents"].(map[string]any) {
			rec := v.(map[string]any)
			for _, f := range []string{"transition_checked_at", "tracked_read_at"} {
				s, ok := rec[f].(string)
				if !ok || s == "" {
					continue
				}
				tt, err := time.Parse(time.RFC3339, s)
				if err != nil {
					t.Fatalf("shift %s %q: %v", f, s, err)
				}
				rec[f] = tt.Add(d).UTC().Format("2006-01-02T15:04:05+00:00")
			}
		}
		h.setState(t, st)
	}
	shift(skew)
	tick()
	shift(-skew)
}

// TestProdWatch_SentryRotationsIgnoreTheRunnersClocks: runners share one state
// and disagree on the time — one runs 3 days late, or early, taking turns with a
// correct one. The by-id reads and the transition re-checks still take turns:
// every issue is read, every regressed one re-checked, and a second regression
// is found on its turn — they are ordered by the state generation, never by a
// clock (a late runner saw every stamp as future: the lowest id every tick).
func TestProdWatch_SentryRotationsIgnoreTheRunnersClocks(t *testing.T) {
	t.Parallel()
	for name, skew := range map[string]time.Duration{"late": 72 * time.Hour, "early": -72 * time.Hour} {
		skew := skew
		t.Run("re-checks/"+name, func(t *testing.T) {
			t.Parallel()
			pwRechecksUnderSkew(t, skew)
		})
		t.Run("by-id reads/"+name, func(t *testing.T) {
			t.Parallel()
			pwByIdReadsUnderSkew(t, skew)
		})
	}
}

func pwRechecksUnderSkew(t *testing.T, skew time.Duration) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	ids := []string{"1901", "1902", "1903"}
	for _, id := range ids {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PROJ-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	}
	for k := 0; k < 3; k++ {
		sentryTick(t, h, wf)
		time.Sleep(1100 * time.Millisecond)
	}
	for _, id := range ids {
		if sentryIncident(t, h, id)["transition_checked_at"] == nil {
			t.Fatalf("setup: issue %s was never re-checked", id)
		}
	}
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("1903", func(i *pwSentryIssue) { i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1}) })
	checked := map[string]int{}
	var said []string
	for k := 0; k < 8; k++ {
		time.Sleep(1100 * time.Millisecond)
		before := map[string]any{}
		for _, id := range ids {
			before[id] = sentryIncident(t, h, id)["transition_checked_gen"]
		}
		tick := func() { said = append(said, sentryAlerts(sentryTick(t, h, wf))...) }
		if k%2 == 0 {
			pwSkewedTick(t, h, skew, tick)
		} else {
			tick()
		}
		for _, id := range ids {
			if sentryIncident(t, h, id)["transition_checked_gen"] != before[id] {
				checked[id]++
			}
		}
	}
	if checked["1901"] == 0 || checked["1902"] == 0 || checked["1903"] == 0 || !strings.Contains(strings.Join(said, " "), "regressed:PROJ-1903") {
		t.Fatalf("8 ticks, max_transition_checks 1, a runner %v off taking turns with a correct one: re-checks per issue %v, said %v", skew, checked, said)
	}
}

func pwByIdReadsUnderSkew(t *testing.T, skew time.Duration) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	ids := []string{"21", "22", "23"}
	for _, id := range ids {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf) // new: alerted, tracked by id from now on
	for _, id := range ids {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	read := map[string]int{}
	for k := 0; k < 8; k++ {
		time.Sleep(1100 * time.Millisecond)
		var o map[string]map[string]any
		tick := func() { o = sentryTick(t, h, wf) }
		if k%2 == 0 {
			pwSkewedTick(t, h, skew, tick)
		} else {
			tick()
		}
		for _, x := range o["plan"]["sentry"].(map[string]any)["tracked_ids"].([]any) {
			read[fmt.Sprint(x)]++
		}
	}
	if read["21"] == 0 || read["22"] == 0 || read["23"] == 0 {
		t.Fatalf("8 ticks, max_tracked 1, a runner %v off taking turns with a correct one: by-id reads per issue %v", skew, read)
	}
}

// pwTrickleTemplate: a log template live this tick, as leak_scan hands it to decide.
func pwTrickleTemplate(id string) map[string]any {
	return map[string]any{"template_id": id, "template": "ERROR pattern " + id, "count": 3, "count_live": 3,
		"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "x", "sample_live": "x", "queries": []any{"errors"},
		"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"}
}

// pwPendingTemplate: a NEW log-template alert the per-run cap deferred, pending since `since`.
func pwPendingTemplate(id, since string) map[string]any {
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	return map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
		"title_key": "loki_template", "title_arg": "ERROR pattern " + id, "detail_key": "loki_detail", "fields": map[string]any{},
		"first_seen": old, "last_seen": old, "count": 3, "alerted": false, "last_notified": nil, "quiet_noted": false,
		"pending": "new", "pending_since": since}
}

// pwShiftPending moves every pending_since by d: a runner `d` late sees the
// others' stamps d later, and the stamps it writes are d early to the others.
func pwShiftPending(t *testing.T, incidents map[string]any, d time.Duration) {
	t.Helper()
	for fp, v := range incidents {
		rec := v.(map[string]any)
		s, ok := rec["pending_since"].(string)
		if !ok || s == "" {
			continue
		}
		tt, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("shift %s pending_since %q: %v", fp, s, err)
		}
		rec["pending_since"] = tt.Add(d).UTC().Format("2006-01-02T15:04:05+00:00")
	}
}

// pwPendingTrickle: two runners take turns on one state — A correct, B `skew`
// late (A on even ticks). A per-run cap of 1; one new log template a tick, each
// recurring while it waits; X, a one-off pattern A deferred 10 minutes ago.
// Returns what each tick posted and the tick X was posted on (-1: never).
func pwPendingTrickle(t *testing.T, skew time.Duration) ([]string, int) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format("2006-01-02T15:04:05+00:00") }
	incidents := map[string]any{"loki:x": pwPendingTemplate("x", stamp(-10*time.Minute)),
		"loki:y0": pwPendingTemplate("y0", stamp(-5*time.Minute-skew))}
	var posted []string
	for tick := 0; tick < 8; tick++ {
		late := tick%2 == 1
		var recurring []string
		for fp, v := range incidents {
			if v.(map[string]any)["pending"] == "new" && fp != "loki:x" && fp != "loki:y0" {
				recurring = append(recurring, strings.TrimPrefix(fp, "loki:"))
			}
		}
		sort.Strings(recurring)
		var tpls []any
		for _, id := range recurring {
			tpls = append(tpls, pwTrickleTemplate(id))
		}
		tpls = append(tpls, pwTrickleTemplate(fmt.Sprintf("n%02d", tick)))
		if late {
			pwShiftPending(t, incidents, skew)
		}
		out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": tpls, "leak": []any{}}, pwLokiState(incidents),
			map[string]any{"max_alerts": 1, "max_alerts_per_lane": 20})
		if err != nil {
			t.Fatalf("decide tick %d: %v %s", tick, err, lastN(stderr, 400))
		}
		var this []string
		for _, a := range out["alerts"].([]any) {
			this = append(this, fmt.Sprint(a.(map[string]any)["fingerprint"]))
		}
		posted = append(posted, fmt.Sprintf("t%d(%s):%s", tick, map[bool]string{false: "A", true: "B"}[late], strings.Join(this, ",")))
		incidents = pwStateNext(t, out)["incidents"].(map[string]any)
		if late {
			pwShiftPending(t, incidents, -skew)
		}
		for _, fp := range this {
			if fp == "loki:x" {
				return posted, tick
			}
		}
	}
	return posted, -1
}

// TestProdWatch_ALateRunnerHoldsNoPendingAlertBehindFresherOnes: a runner 3 days
// late takes turns with a correct one, a new template recurring every tick at a
// per-run cap of 1: the oldest pending alert still goes within a turn. The late
// runner sees the correct one's stamps as future — they wait first in, first
// out, never in the order the alerts were emitted (a recurring template first).
func TestProdWatch_ALateRunnerHoldsNoPendingAlertBehindFresherOnes(t *testing.T) {
	t.Parallel()
	if got, at := pwPendingTrickle(t, 0); at != 0 {
		t.Fatalf("control (one clock): X, the oldest pending alert, must go first: %v", got)
	}
	if got, at := pwPendingTrickle(t, 72*time.Hour); at < 0 || at > 2 {
		t.Fatalf("a runner 3 days late taking turns with a correct one: X, the oldest pending alert, went at tick %d:\n%s",
			at, strings.Join(got, "\n"))
	}
}

// TestProdWatch_ALateRunnerNamesOwedNotesInTheirWritersOrder: idle notes owed by
// a correct runner, seen by a late one — every owed stamp looks future to it.
// The notes of the earlier debt still go first (thirty owed ten minutes before
// thirty more, their ids sorting after them), never in id order.
func TestProdWatch_ALateRunnerNamesOwedNotesInTheirWritersOrder(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	old := time.Now().Add(-49 * time.Hour).UTC().Format(time.RFC3339)
	ahead := func(d time.Duration) string {
		return time.Now().Add(72*time.Hour + d).UTC().Format("2006-01-02T15:04:05+00:00")
	}
	incidents := map[string]any{}
	for k := 0; k < 30; k++ {
		for _, g := range []struct {
			prefix string
			owed   time.Duration
		}{{"tz", -10 * time.Minute}, {"ta", -5 * time.Minute}} {
			id := fmt.Sprintf("%s%02d", g.prefix, k)
			incidents["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
				"title_key": "loki_template", "title_arg": "ERROR idle " + id + " " + strings.Repeat("x", 100), "detail_key": "loki_detail",
				"fields": map[string]any{}, "first_seen": old, "last_seen": old, "count": 3, "alerted": true, "last_notified": old,
				"quiet_noted": false, "note_owed_at": ahead(g.owed)}
		}
	}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents), nil)
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	var said []string
	for _, a := range out["alerts"].([]any) {
		m := a.(map[string]any)
		if ms, ok := m["members"].([]any); ok {
			for _, x := range ms {
				said = append(said, fmt.Sprint(x.(map[string]any)["fp"]))
			}
			continue
		}
		said = append(said, fmt.Sprint(m["fingerprint"]))
	}
	if len(said) == 0 || len(said) >= 30 {
		t.Fatalf("setup: want fewer idle notes said than the earlier debt's thirty, got %d", len(said))
	}
	for _, fp := range said {
		if !strings.HasPrefix(fp, "loki:tz") {
			t.Fatalf("a note of the later debt (%s) went before the earlier debt was said: %v", fp, said)
		}
	}
}

// TestProdWatch_AFailingLookupHoldsNoCheckSlot: an issue whose activity lookup
// fails every time (a 5xx, then its retry) takes its turn all the same — under
// max_transition_checks 1 the other issues are still checked: a dated one's
// second regression is found; a never-dated one behind a failing never-dated
// one gets its check.
func TestProdWatch_AFailingLookupHoldsNoCheckSlot(t *testing.T) {
	t.Parallel()
	t.Run("re-checks", func(t *testing.T) {
		t.Parallel()
		wf := compileFixture(t, "prod-watch/main.bot")
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
		sentryTick(t, h, wf)
		now := time.Now()
		ids := []string{"1901", "1902", "1903"}
		for _, id := range ids {
			h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PROJ-" + id), Title: "r", Substatus: strp("regressed"),
				FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
		}
		for k := 0; k < 3; k++ {
			sentryTick(t, h, wf)
			time.Sleep(1100 * time.Millisecond)
		}
		gen := map[string]float64{}
		for _, id := range ids {
			g, ok := sentryIncident(t, h, id)["transition_checked_gen"].(float64)
			if !ok {
				t.Fatalf("setup: issue %s was never checked", id)
			}
			gen[id] = g
		}
		sort.Slice(ids, func(a, b int) bool { return gen[ids[a]] < gen[ids[b]] })
		head, other := ids[0], ids[2] // the least recently checked, first in line; the most recently, last
		h.sentry.edit(head, func(i *pwSentryIssue) { i.ActsFail = 500 })
		t1 := time.Now().Add(time.Second)
		h.sentry.edit(other, func(i *pwSentryIssue) { i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1}) })
		var said []string
		for k := 0; k < 5; k++ {
			time.Sleep(1100 * time.Millisecond)
			said = append(said, sentryAlerts(sentryTick(t, h, wf))...)
		}
		if !strings.Contains(strings.Join(said, " "), "regressed:PROJ-"+other) {
			t.Fatalf("5 ticks, max_transition_checks 1, issue %s's lookup failing every time: the second regression of %s was never said: %v", head, other, said)
		}
	})
	t.Run("never dated", func(t *testing.T) {
		t.Parallel()
		wf := compileFixture(t, "prod-watch/main.bot")
		h := newPWHarness(t)
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 1 }))
		sentryTick(t, h, wf)
		now := time.Now()
		// 1912's last event is the newer: first in the never-dated line, its lookup failing every tick.
		for _, c := range []struct {
			id   string
			seen time.Time
			fail int
		}{{"1911", now, 0}, {"1912", now.Add(time.Second), 500}} {
			h.sentry.put(&pwSentryIssue{ID: c.id, ShortID: strp("PROJ-" + c.id), Title: "r", Substatus: strp("regressed"),
				FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: c.seen, Acts: []pwSentryAct{{Type: "set_regression", At: now}},
				ActsFail: c.fail})
		}
		for k := 0; k < 4 && sentryIncident(t, h, "1911")["transition_checked_at"] == nil; k++ {
			time.Sleep(1100 * time.Millisecond)
			sentryTick(t, h, wf)
		}
		if sentryIncident(t, h, "1911")["transition_checked_at"] == nil {
			t.Fatalf("4 ticks, max_transition_checks 1, the never-dated head's lookup failing every tick: 1911 was never checked")
		}
	})
}

// TestProdWatch_EachPartSaysItsOwnFirstSighting: a note split into parts dates
// each part to its own members — thirty idle templates first seen 40 days ago
// down to 11: a later part's meta line says the first sighting of ITS members.
func TestProdWatch_EachPartSaysItsOwnFirstSighting(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	now := time.Now().UTC()
	incidents := map[string]any{}
	first := map[string]string{}
	var names []string
	for k := 0; k < 30; k++ {
		id := fmt.Sprintf("t%02d", k)
		fs := now.Add(-time.Duration(40-k) * 24 * time.Hour).Format("2006-01-02T15:04:00+00:00")
		title := fmt.Sprintf("ERROR template number %02d went quiet after a deploy", k)
		first[id] = fs[:16]
		names = append(names, id+" "+title)
		incidents["loki:"+id] = map[string]any{"fp": "loki:" + id, "kind": "loki", "sources": []any{"errors"}, "severity": "medium",
			"title_key": "loki_template", "title_arg": title, "detail_key": "loki_detail", "fields": map[string]any{},
			"first_seen": fs, "last_seen": now.Add(-49 * time.Hour).Format(time.RFC3339), "count": k + 1,
			"alerted": true, "last_notified": now.Add(-49 * time.Hour).Format(time.RFC3339), "quiet_noted": false}
	}
	out, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(incidents),
		map[string]any{"max_alerts_per_lane": 0, "max_message_chars": 1500})
	if err != nil {
		t.Fatalf("decide: %v %s", err, lastN(stderr, 400))
	}
	labels := pwPlanLabels(t, h, map[string]any{"folded_detail": "{n} idle: {names} " + strings.Repeat("&", 200),
		"folded_detail_more": "{n} idle: {names} — {more} held " + strings.Repeat("&", 200)})
	nout, nerr, err := pwNotifyDry(t, h, labels, 1500, out["alerts"].([]any))
	if err != nil {
		t.Fatalf("notify: %v %s", err, lastN(nerr, 400))
	}
	msgs := nout["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("setup: want the idle note in parts, got %d message(s)", len(msgs))
	}
	dated := 0
	for i, m := range msgs {
		lines := strings.Split(m.(map[string]any)["text"].(string), "\n")
		own := ""
		for k, n := range names {
			id := fmt.Sprintf("t%02d", k)
			if strings.Contains(lines[1], n) && (own == "" || first[id] < own) {
				own = first[id]
			}
		}
		if len(lines) < 3 || own == "" {
			continue
		}
		if !strings.Contains(lines[2], "`"+own+"`") {
			t.Fatalf("part %d of %d names members first seen %s at the earliest; its meta line says: %s", i+1, len(msgs), own, lines[2])
		}
		if own != first["t00"] {
			dated++
		}
	}
	if dated == 0 {
		t.Fatalf("setup: want a part keeping its meta line whose members were first seen after the note's first one")
	}
}

// TestProdWatch_ACheckTieNamesEveryLabel: two labels at the same worst length —
// the refusal names both, so shortening one never moves it to the other.
func TestProdWatch_ACheckTieNamesEveryLabel(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	fold := "{names} " + strings.Repeat("&", 392)
	labels := pwPlanLabels(t, h, map[string]any{"folded_detail": fold, "folded_detail_more": fold})
	_, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{})
	if err == nil || !strings.Contains(nerr, "(labels.folded_detail; labels.folded_detail_more)") {
		t.Fatalf("two fold labels at the worst length: the refusal must name both: %v %s", err, lastN(nerr, 400))
	}
}

// TestProdWatch_TheCheckCountsMoreOnlyWhereItShows: folded_detail never holds
// members back — `more` renders 0 there — so the check counts it at that width:
// its limit is the need of the longest one-member note, no more.
func TestProdWatch_TheCheckCountsMoreOnlyWhereItShows(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	labels := pwPlanLabels(t, h, map[string]any{"folded_detail": "{n} {names} ({more}) " + strings.Repeat("&", 378)})
	_, nerr, err := pwNotifyDry(t, h, labels, 1500, []any{})
	m := pwCheckLimit.FindStringSubmatch(nerr)
	if err == nil || m == nil {
		t.Fatalf("setup: at 1500 the check must refuse and name its numbers: %v %s", err, lastN(nerr, 300))
	}
	limit := atoiOr(m[1]) + 1 + atoiOr(m[2])
	name := strings.Repeat("ftp:", 50)
	var notes []any
	for _, st := range []string{"new", "escalated", "reminder", "quiet", "regressed", "escalating", "reopened"} {
		notes = append(notes, pwOneMemberNote("sentry", st, "medium", "", name))
	}
	for _, status := range []string{"resolved", "ignored", "closed"} {
		notes = append(notes, pwOneMemberNote("sentry", "resolved", "medium", status, name))
	}
	for _, st := range []string{"new", "escalated", "reminder", "quiet"} {
		notes = append(notes, pwOneMemberNote("loki", st, "medium", "", name))
	}
	out, nerr, err := pwNotifyDry(t, h, labels, 4000, notes)
	if err != nil {
		t.Fatalf("setup: at 4000 the notes must render: %v %s", err, lastN(nerr, 300))
	}
	need := 0
	for _, msg := range out["messages"].([]any) {
		lines := strings.Split(msg.(map[string]any)["text"].(string), "\n")
		if k := len([]rune(lines[0])) + 1 + len([]rune(lines[1])); k > need {
			need = k
		}
	}
	if limit != need {
		t.Fatalf("the check demands %d characters; the longest one-member note needs %d (folded_detail renders `more` as 0)", limit, need)
	}
}

// TestProdWatch_SentryNeverServedIssuesGoFirst: an issue no generation ever
// served — a record kept from before the rotations counted in generations —
// goes first: it is read before the ones already read.
func TestProdWatch_SentryNeverServedIssuesGoFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 1 }))
	sentryTick(t, h, wf)
	now := time.Now()
	for _, id := range []string{"21", "22"} {
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("P-" + id), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf) // new: alerted, read in the list — each carries the generation that served it
	for _, id := range []string{"21", "22"} {
		h.sentry.edit(id, func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	}
	sentryEditRecord(t, h, "22", func(r map[string]any) { delete(r, "tracked_read_gen") })
	o := sentryTick(t, h, wf)
	if got := fmt.Sprint(o["plan"]["sentry"].(map[string]any)["tracked_ids"]); got != "[22]" {
		t.Fatalf("max_tracked 1: the issue no generation served must be read first, got %s", got)
	}
}
