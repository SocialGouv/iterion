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

// Guards of the Sentry lane, each with its own witness (the security round
// found them green under mutation otherwise).

func sentryPlan(t *testing.T, h *pwHarness, wfPath string) (map[string]any, map[string]any, map[string]string) {
	t.Helper()
	wf := compileFixture(t, wfPath)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v %s", err, stderr)
	}
	return plan, vars, secrets
}

func runPollSentry(t *testing.T, h *pwHarness, plan, vars map[string]any, secrets map[string]string, allowPrivate bool, env []string) (map[string]any, string) {
	t.Helper()
	wf := compileFixture(t, "prod-watch/main.bot")
	out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "poll_sentry").Script, map[string]any{"sentry": plan["sentry"],
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": allowPrivate}, vars, secrets), env)
	if err != nil {
		t.Fatalf("poll_sentry died instead of reporting a lane error: %v %s", err, stderr)
	}
	return out, stderr
}

// TestProdWatch_SentrySetupGuardsAreLaneErrors: every refusal before a request
// leaves — a loopback host in strict posture (a proxy variable naming it does
// not disarm the guard), an http:// base URL for a bearer, a token with a
// space inside, a redirect — is a named lane error, the token never quoted,
// the node alive.
func TestProdWatch_SentrySetupGuardsAreLaneErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, want string
		mod        func(h *pwHarness, s map[string]any)
		prep       func(h *pwHarness)
		private    bool
		env        []string
	}{
		{name: "loopback in strict posture despite a proxy variable", want: "SSRF-unsafe",
			mod: func(h *pwHarness, s map[string]any) { s["base_url"] = "https://127.0.0.1:9" },
			env: []string{"HTTPS_PROXY=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9"}},
		{name: "http base url", want: "must be https", mod: func(h *pwHarness, s map[string]any) { s["base_url"] = "http://sentry.example" }},
		{name: "token with a space", want: "not one token on one line", private: true,
			prep: func(h *pwHarness) { _ = os.WriteFile(h.sentryTokenFile, []byte("abc def\n"), 0o600) }},
		{name: "redirect not followed", want: "Sentry refused the project lookup (HTTP 302)", private: true,
			prep: func(h *pwHarness) { h.sentry.redirectTo = "http://127.0.0.1:9/elsewhere" }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			var mod func(s map[string]any)
			if c.mod != nil {
				mod = func(s map[string]any) { c.mod(h, s) }
			}
			h.writeConfig(t, sentryOnly(h, mod))
			if c.prep != nil {
				c.prep(h)
			}
			plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
			out, stderr := runPollSentry(t, h, plan, vars, secrets, c.private, c.env)
			errs := fmt.Sprint(out["errors"])
			if out["ok"] != false || !strings.Contains(errs, c.want) {
				t.Fatalf("want a lane error naming %q, got %v", c.want, out["errors"])
			}
			if strings.Contains(errs+stderr, "abc def") || strings.Contains(errs+stderr, pwSentryToken) {
				t.Fatalf("the token was quoted: %s %s", errs, stderr)
			}
		})
	}
}

// TestProdWatch_SentryServerTextIsWithheld: a refused request's body (the
// server's words) never reaches the lane error.
func TestProdWatch_SentryServerTextIsWithheld(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	h.sentry.failBody = `{"detail": "SERVER-SAYS-zq81 for environment evil"}`
	h.sentry.failNext("list", 400)
	plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
	out, _ := runPollSentry(t, h, plan, vars, secrets, true, nil)
	if errs := fmt.Sprint(out["errors"]); !strings.Contains(errs, "HTTP 400") || strings.Contains(errs, "SERVER-SAYS") {
		t.Fatalf("the server's text reached the lane error, or the status did not: %v", errs)
	}
}

// TestProdWatch_SentryAttackerTextCannotPingOrLink: a culprit or title written
// through a public DSN cannot mention the channel nor become a link — in the
// Sentry detail line and in any other lane's label placeholders alike.
func TestProdWatch_SentryAttackerTextCannotPingOrLink(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1501", ShortID: strp("PROJ-150"), Title: "@channel @all read www.evillogin.example/reset",
		Culprit: "@here @jo please re-login at www.evillogin.example/reset", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies(), "\n")
	for _, bad := range []string{"@channel", "@all", "@here", "@jo", "www.evillogin"} {
		for _, line := range strings.Split(body, "\n") {
			// Inline code renders neither mentions nor autolinks.
			outside := line
			for strings.Count(outside, "`") >= 2 {
				a := strings.Index(outside, "`")
				b := strings.Index(outside[a+1:], "`")
				outside = outside[:a] + outside[a+1+b+1:]
			}
			if strings.Contains(outside, bad) {
				t.Fatalf("attacker text %q reached the channel outside inline code:\n%s", bad, line)
			}
		}
	}
	if !strings.Contains(body, "@\u200bhere") {
		t.Fatalf("the culprit was not rendered (neutralized):\n%s", body)
	}
}

// TestProdWatch_SentryScanResistsUnicodeDigitRuns: a title of Unicode digits
// that NFKC keeps (Arabic-Indic) scans in well under a second — the grouped
// number classes used to backtrack for seconds per run.
func TestProdWatch_SentryScanResistsUnicodeDigitRuns(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	run := strings.Repeat("١", 70)
	title := run + " x " + run + " y " + run + " z " + run
	raw := filepath.Join(h.scratch, "sentry_raw-redos.jsonl")
	b, _ := json.Marshal(map[string]any{"id": "1", "short_id": "P-1", "title": title, "culprit": title, "meta_value": title, "last_seen": "2026-09-29T10:00:00+00:00"})
	if err := os.WriteFile(raw, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": filepath.Join(h.scratch, "none.jsonl"), "per_query": map[string]any{}, "sentry_file": raw,
		"sentry_issues": 1, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil {
		t.Fatalf("leak_scan: %v %s", err, stderr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("one crafted issue stalled the scan %v (the separator class admits the digits it separates)", d)
	}
}

// TestProdWatch_SentryEveryTextFieldIsScrubbed: a planted value in the
// culprit, the metadata type, filename and function never leaves the scan raw.
func TestProdWatch_SentryEveryTextFieldIsScrubbed(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	email := "zq7c.plantx@qz9mail.fr"
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1601", ShortID: strp("PROJ-160"), Title: "plain title", Culprit: "app.users in find " + email,
		Meta:           map[string]any{"type": "Err" + email, "filename": "/srv/" + email + "/x.py", "function": "f_" + email},
		FirstProcessed: now, LastSeen: now, Count: 1})
	outs := sentryTick(t, h, wf)
	haystacks := []string{strings.Join(h.bodies(), "\n")}
	for id, out := range outs {
		b, _ := json.Marshal(out)
		haystacks = append(haystacks, string(b), h.stderrs[id])
	}
	sig, _ := os.ReadFile(outs["leak_scan"]["signals_file"].(string))
	haystacks = append(haystacks, string(sig))
	for _, name := range []string{"state.json", "alertlog.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(h.ws, ".prod-watch", name))
		haystacks = append(haystacks, string(b))
	}
	for _, text := range haystacks {
		if strings.Contains(text, email) || strings.Contains(text, email[:8]) {
			t.Fatalf("a planted value in a non-title field left the scan raw")
		}
	}
}

// TestProdWatch_SentrySightingsEscalateAndRemind: an alerted issue whose last
// event moved escalates when its level-mapped severity rose, capped by
// max_severity, and is reminded once per renotify window.
func TestProdWatch_SentrySightingsEscalateAndRemind(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_severity"] = "high" }))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1701", ShortID: strp("PROJ-170"), Title: "x", Level: "warning", FirstProcessed: now, LastSeen: now})
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_severity"] = "high"; s["min_level"] = "warning" }))
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:PROJ-170:low" {
		t.Fatalf("new warning: %v", got)
	}
	h.sentry.edit("1701", func(i *pwSentryIssue) { i.Level = "fatal"; i.LastSeen = time.Now().Add(time.Second) })
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) {
		s["max_severity"] = "high"
		s["min_level"] = "warning"
		s["severity"] = map[string]any{"fatal": "critical"}
	}))
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "escalated:PROJ-170:high" {
		t.Fatalf("escalation (fatal→critical capped at high): %v", got)
	}
	st := h.state(t)
	st["incidents"].(map[string]any)["sentry:1701"].(map[string]any)["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	h.setState(t, st)
	h.sentry.edit("1701", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(2 * time.Second) })
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "reminder:PROJ-170:high" {
		t.Fatalf("reminder after the renotify window: %v", got)
	}
}

// TestProdWatch_SentryWalkCarriesItsContract: the by-id read carries the
// environment, the cursor is the server's Date (not the runner's clock), and
// the activity lookups stay under their cap.
func TestProdWatch_SentryWalkCarriesItsContract(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_transition_checks"] = 2 }))
	h.sentry.dateOffset = -2 * time.Hour
	sentryTick(t, h, wf)
	cur := h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)
	since, err := time.Parse(time.RFC3339, cur["since"].(string))
	if err != nil || time.Since(since) < 90*time.Minute {
		t.Fatalf("the cursor is not the server's Date (2 h behind): %v %v", cur["since"], err)
	}
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "1801", ShortID: strp("PROJ-180"), Title: "x", FirstProcessed: now.Add(-2 * time.Hour).Add(time.Minute), LastSeen: now})
	for k := 0; k < 4; k++ {
		id := fmt.Sprint(1810 + k)
		h.sentry.put(&pwSentryIssue{ID: id, ShortID: strp("PROJ-" + id), Title: "r", Substatus: strp("regressed"),
			FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
	}
	before := len(h.sentry.callsTo("activities"))
	sentryTick(t, h, wf) // posts PROJ-180 new, dates two regressions
	if n := len(h.sentry.callsTo("activities")) - before; n > 2 {
		t.Fatalf("%d activity lookups under a cap of 2", n)
	}
	sentryTick(t, h, wf)
	calls := h.sentry.callsTo("tracked")
	if len(calls) == 0 || calls[len(calls)-1].Q.Get("environment") != "preprod" {
		t.Fatalf("the by-id read does not carry the environment: %v", calls)
	}
}

// TestProdWatch_SentryRechecksTakeTurns: dated regressed issues are re-checked
// round-robin under the cap — each gets its turn, and a second regression of
// one of them (resolved and regressed between two ticks, no new event in the
// watched environment) is found on its turn.
func TestProdWatch_SentryRechecksTakeTurns(t *testing.T) {
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
	var posted []string
	for k := 0; k < 3; k++ {
		posted = append(posted, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if len(posted) != 3 {
		t.Fatalf("three regressions under a cap of 1 in 3 ticks: %v", posted)
	}
	for _, id := range []string{"1901", "1902", "1903"} {
		if sentryIncident(t, h, id)["transition_checked_at"] == nil {
			t.Fatalf("issue %s was never re-checked", id)
		}
	}
	// 1902 is resolved and regresses again between two ticks — from another
	// environment: nothing moves in the watched one but the activity.
	t1 := time.Now().Add(time.Second)
	h.sentry.edit("1902", func(i *pwSentryIssue) { i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1}) })
	var again []string
	for k := 0; k < 3; k++ {
		again = append(again, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if strings.Join(again, " ") != "regressed:PROJ-1902:medium" {
		t.Fatalf("a second regression found on its re-check turn: %v", again)
	}
}

// TestProdWatch_SentryFloodCannotHoldTheCapAgainstAnotherLane: at one rank,
// the lanes take turns under the per-run cap — a Sentry flood does not push a
// Loki alert of the same rank out.
func TestProdWatch_SentryFloodCannotHoldTheCapAgainstAnotherLane(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	// Driven through decide: 25 new Sentry issues ranked high and one live Loki
	// template ranked high, cap 5.
	var issues []any
	for k := 0; k < 25; k++ {
		issues = append(issues, map[string]any{"id": fmt.Sprint(2000 + k), "short_id": fmt.Sprint("P-", 2000+k), "level": "fatal", "status": "unresolved",
			"in_new": true, "last_seen": "2026-09-29T10:00:00+00:00", "count": 1, "count_scope": "14d", "leaks": []any{}})
	}
	signals := map[string]any{"templates": []any{map[string]any{"template_id": "t1", "template": "boom", "count": 60, "count_live": 60,
		"first_ts": "1", "last_ts": "2", "first_ts_live": "1", "sample": "boom", "sample_live": "boom", "queries": []any{"errors"},
		"queries_live": []any{"errors"}, "streams": []any{}, "streams_live": []any{}, "query": "errors", "query_live": "errors"}},
		"sentry_issues": issues}
	state := map[string]any{"version": 1, "generation": 1, "incidents": map[string]any{}, "health": map[string]any{},
		"cursors": map[string]any{"loki": map[string]any{}, "sentry": map[string]any{"identity": map[string]any{"base_url": "https://s.example", "org": "o", "project": "p", "environment": "preprod"},
			"armed_at": "2026-09-29T09:00:00+00:00", "since": "2026-09-29T09:00:00+00:00", "at": "2026-09-29T09:00:00+00:00"}}}
	in := map[string]any{}
	for k, v := range pwSentryOff {
		in[k] = v
	}
	in["sentry"] = map[string]any{"enabled": true, "bootstrap": false, "armed_at": "2026-09-29T09:00:00+00:00", "overlap_minutes": 60,
		"severity": map[string]any{"fatal": "high"}, "max_severity": "high", "link_prefix": "https://s.example/organizations/o/issues/",
		"identity": map[string]any{"base_url": "https://s.example", "org": "o", "project": "p", "environment": "preprod"}}
	in["sentry_walk"] = map[string]any{"answered": true, "new_complete": true, "transition_complete": true, "tracked_complete": true, "as_of": "2026-09-29T10:00:00+00:00"}
	in["sentry_issues"] = 25
	in["lanes"] = map[string]any{"loki": true, "prometheus": false, "probes": false, "sentry": true}
	in["max_alerts"] = 5
	out, stderr, err := pwDecide(t, wf, h, signals, state, in)
	if err != nil {
		t.Fatalf("decide: %v %s", err, stderr)
	}
	var kinds []string
	for _, a := range out["alerts"].([]any) {
		kinds = append(kinds, fmt.Sprint(a.(map[string]any)["kind"]))
	}
	if !strings.Contains(strings.Join(kinds, " "), "loki") {
		t.Fatalf("a Sentry flood held the cap against a Loki alert of the same rank: %v", kinds)
	}
}

// TestProdWatch_SentryBrokenDateHeaderDegradesToTheLocalClock: a Date header
// out of range (a broken server or proxy) never kills the node: the runner's
// clock stands in and the walk says so.
func TestProdWatch_SentryBrokenDateHeaderDegradesToTheLocalClock(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	h.sentry.rawDate = "Fri, 31 Dec 9999 23:59:59 -2359"
	plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
	out, _ := runPollSentry(t, h, plan, vars, secrets, true, nil)
	if walk, _ := out["walk"].(map[string]any); walk["clock"] != "local" || out["ok"] != true {
		t.Fatalf("a broken Date header: %v", out)
	}
}

// TestProdWatch_SentryNotesAndAnalysisFlags: a Sentry NEW alert asks for the
// analysis agent, a note does not; a sighting after a quiet note re-arms the
// note; a partial by-id read (tracked ids over the cap) concludes no note.
func TestProdWatch_SentryNotesAndAnalysisFlags(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "2101", ShortID: strp("PROJ-2101"), Title: "x", FirstProcessed: now, LastSeen: now})
	if o := sentryTick(t, h, wf); o["decide"]["needs_analysis"] != true {
		t.Fatalf("a new Sentry issue did not ask for the analysis: %v", o["decide"]["needs_analysis"])
	}
	ageIncidents(t, h, 49*time.Hour, "2101")
	o := sentryTick(t, h, wf)
	if got := sentryAlerts(o); strings.Join(got, " ") != "quiet:PROJ-2101:low" || o["decide"]["needs_analysis"] != false {
		t.Fatalf("quiet note: %v needs_analysis=%v", got, o["decide"]["needs_analysis"])
	}
	// A new event: a sighting, which re-arms the note for a later idle period.
	h.sentry.edit("2101", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(time.Second) })
	sentryTick(t, h, wf)
	ageIncidents(t, h, 49*time.Hour, "2101")
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["max_tracked"] = 0 }))
	if got := sentryAlerts(sentryTick(t, h, wf)); len(got) != 0 {
		t.Fatalf("a by-id read cut by max_tracked concluded a note: %v", got)
	}
	h.writeConfig(t, sentryOnly(h, nil))
	if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "quiet:PROJ-2101:low" {
		t.Fatalf("the sighting did not re-arm the quiet note: %v", got)
	}
}

// TestProdWatch_SentryForeignIncidentFieldIsRefusedByName: plan refuses a
// Sentry incident field of another type by name — never a traceback.
func TestProdWatch_SentryForeignIncidentFieldIsRefusedByName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	h.setState(t, map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{}, "health": map[string]any{},
		"incidents": map[string]any{"sentry:1": map[string]any{"kind": "sentry", "backlog": "yes"}}})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
	if err == nil || !strings.Contains(stderr, "backlog") {
		t.Fatalf("a foreign backlog field was not refused by name: err=%v %s", err, stderr)
	}
}
