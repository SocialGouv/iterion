package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
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
// through a public DSN cannot mention the channel nor become a link. Escaping
// is not enough — Mattermost autolinks a host after a word character or a
// hyphen (`-sso-portal.com/reset`, `_https://`, `éhttps://`, `www1.`, a host
// in parentheses) whatever backslashes precede it — so every value renders
// inside inline code, which neither links nor notifies; and U+2424, a line
// break to Mattermost's markdown, never reaches the channel to end a span.
func TestProdWatch_SentryAttackerTextCannotPingOrLink(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.setMaxPerLane(20) // every culprit renders in its own alert
	cases := []struct{ culprit, rendered string }{
		{"@here @jo please re-login at www.evillogin.example/reset", "@here @jo please re-login at www.evillogin.example/reset"},
		{"please re-login at -sso-portal.com/reset", "please re-login at -sso-portal.com/reset"},
		{"_https://evil.com/reset", "_https\u200b://evil.com/reset"},
		{"éhttps://evil.com/reset", "éhttps\u200b://evil.com/reset"},
		{"see www1.evil.com/reset", "see www1.evil.com/reset"},
		{"please re-login (evil.com/reset)", "please re-login (evil.com/reset)"},
		{"app\u2424\u2424www.evil-sso.com/login", "app www.evil-sso.com/login"},
		{"x\u2424\u2424# FORGED HEADING", "x # FORGED HEADING"},
		{"a {short_id} b www.evil-sso.com/x", "a {short_id} b www.evil-sso.com/x"},
	}
	for i, c := range cases {
		h.sentry.put(&pwSentryIssue{ID: strconv.Itoa(1501 + i), ShortID: strp("PROJ-" + strconv.Itoa(150+i)),
			Title: "@channel @all read www.evillogin.example/reset", Culprit: c.culprit, FirstProcessed: now, LastSeen: now, Count: 1})
	}
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies(), "\n")
	if strings.ContainsRune(body, '\u2424') {
		t.Fatalf("U+2424 reached the channel (a line break to Mattermost's markdown, it ends a code span):\n%s", body)
	}
	for _, c := range cases {
		if !strings.Contains(body, "`"+c.rendered+"`") {
			t.Fatalf("culprit %q did not render as %q inside inline code:\n%s", c.culprit, c.rendered, body)
		}
	}
	for _, bad := range []string{"@channel", "@all", "@here", "@jo", "www.evillogin", "portal.com", "evil.com", "evil-sso", "FORGED"} {
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(pwOutsideCode(line), bad) {
				t.Fatalf("attacker text %q reached the channel outside inline code:\n%s", bad, line)
			}
		}
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
// Sentry field of another type by name — never a traceback.
func TestProdWatch_SentryForeignIncidentFieldIsRefusedByName(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	cases := []struct {
		field string
		state map[string]any
	}{
		{"backlog", map[string]any{"incidents": map[string]any{"sentry:1": map[string]any{"kind": "sentry", "backlog": "yes"}}}},
		{"closed_said", map[string]any{"incidents": map[string]any{"sentry:1": map[string]any{"kind": "sentry", "closed_said": "yes"}}}},
		{"tracked_read_at", map[string]any{"incidents": map[string]any{"sentry:1": map[string]any{"kind": "sentry", "tracked_read_at": 5}}}},
		{"pending_since", map[string]any{"incidents": map[string]any{"leak:email": map[string]any{"kind": "leak", "pending_since": "soon"}}}},
		{"transition_seen_at", map[string]any{"incidents": map[string]any{"sentry:1": map[string]any{"kind": "sentry", "transition_seen_at": 5}}}},
		{"sentry_identity", map[string]any{"incidents": map[string]any{}, "sentry_identity": 7}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.field, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			st := map[string]any{"version": 1, "generation": 1, "cursors": map[string]any{}, "health": map[string]any{}}
			for k, v := range c.state {
				st[k] = v
			}
			h.setState(t, st)
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, nil))
			if err == nil || !strings.Contains(stderr, c.field) || strings.Contains(stderr, "Traceback") {
				t.Fatalf("a foreign %s was not refused by name: err=%v %s", c.field, err, stderr)
			}
		})
	}
}

// TestProdWatch_SentryDrippingBodyStopsAtTheDeadline: a body dripping one byte
// every 400 ms (never tripping the per-read timeout) stops at the walk's
// deadline, not when the body ends — one recv at a time, the deadline checked
// between them.
func TestProdWatch_SentryDrippingBodyStopsAtTheDeadline(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	body := `{"id": "63", "slug": "proj", "name": "proj"}` + strings.Repeat(" ", 16) // 60 bytes: 24 s at 400 ms a byte
	drip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/0/projects/org/proj/") || strings.Contains(r.URL.Path, "/environments/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < len(body); i++ {
			if _, err := w.Write([]byte{body[i]}); err != nil {
				return
			}
			f.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(400 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(drip.Close)
	h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["base_url"] = drip.URL; s["deadline_secs"] = 10; delete(s, "environment") }))
	plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
	start := time.Now()
	out, _ := runPollSentry(t, h, plan, vars, secrets, true, nil)
	if took := time.Since(start); took > 15*time.Second || !strings.Contains(fmt.Sprint(out["errors"]), "deadline") {
		t.Fatalf("a dripping body held poll_sentry %v past a 10 s deadline (errors: %v)", took, out["errors"])
	}
}

// TestProdWatch_SentrySecretScanResistsBacktracking: a key word followed by a
// long digit run, or a quoted value of backslash pairs that never closes — in
// every form the secret class knows (assignment, flag, SQL; either quote) —
// scans in well under a second: every repeated group has disjoint
// alternatives. With overlapping ones the same 35 characters took seconds,
// doubling with each extra character.
func TestProdWatch_SentrySecretScanResistsBacktracking(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	pairs := strings.Repeat(`\ `, 25)
	cases := map[string]string{
		"digit run after a key":    "password" + strings.Repeat("1", 26) + "!",
		"assignment, double quote": `password="` + pairs,
		"assignment, single quote": `password='` + pairs,
		"flag, double quote":       `--password "` + pairs,
		"flag, single quote":       `--password '` + pairs,
		"sql, single quote":        `identified by '` + pairs,
		"sql, double quote":        `identified by "` + pairs,
		// More text after the value: the alternatives that run to the end of
		// the line have to fail too.
		"assignment, double quote, then a line": `password="` + pairs + "\nx",
		"assignment, single quote, then a line": `password='` + pairs + "\nx",
		"flag, double quote, then a line":       `--password "` + pairs + "\nx",
		"flag, single quote, then a line":       `--password '` + pairs + "\nx",
		"sql, single quote, then a line":        `identified by '` + pairs + "\nx",
		"sql, double quote, then a line":        `identified by "` + pairs + "\nx",
	}
	for name, text := range cases {
		name, text := name, text
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			raw := filepath.Join(h.scratch, "sentry_raw-kv.jsonl")
			b, _ := json.Marshal(map[string]any{"id": "1", "short_id": "P-1", "title": "x", "culprit": text, "last_seen": "2026-09-29T10:00:00+00:00"})
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
			if d := time.Since(start); d > 3*time.Second {
				t.Fatalf("one crafted culprit stalled the scan %v (overlapping alternatives in a repeated group)", d)
			}
		})
	}
}

// TestProdWatch_SentryHostileJSONIsTakenWhole: valid JSON the runner's Python
// cannot take at face value — a lone surrogate in a title (invalid UTF-8 once
// written), a count in digits int() refuses — neither kills the node nor cuts
// the list: the text is repaired, the count unknown, the issue posted.
func TestProdWatch_SentryHostileJSONIsTakenWhole(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, raw := range map[string]map[string]string{
		"a lone surrogate in the title":               {"title": `"\ud800 boom"`},
		"a count in superscript digits":               {"count": `"²"`},
		"a count longer than int() takes":             {"count": `"` + strings.Repeat("9", 5000) + `"`},
		"a count of 400 digits as a JSON number":      {"count": strings.Repeat("9", 400)},
		"a user count of 400 digits as a JSON number": {"userCount": strings.Repeat("9", 400)},
	} {
		name, raw := name, raw
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4401", ShortID: strp("P-4401"), Title: "boom", FirstProcessed: now, LastSeen: now, Count: 3, Raw: raw})
			o := sentryTick(t, h, wf)
			if got := sentryAlerts(o); strings.Join(got, " ") != "new:P-4401:medium" || o["poll_sentry"]["ok"] != true {
				t.Fatalf("want the issue read whole and posted: %v, errors %v", got, o["poll_sentry"]["errors"])
			}
		})
	}
}

// TestProdWatch_SentryLocalClockIsReadBeforeTheWalk: without a usable Date
// header the runner's clock stands in — read BEFORE the first request, so an
// issue processed while the walk ran never falls behind the next cursor.
func TestProdWatch_SentryLocalClockIsReadBeforeTheWalk(t *testing.T) {
	t.Parallel()
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	h.sentry.rawDate = "not a date"
	h.sentry.delay = 1500 * time.Millisecond // each of the two lists
	plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
	out, _ := runPollSentry(t, h, plan, vars, secrets, true, nil)
	walk, _ := out["walk"].(map[string]any)
	asOf, err := time.Parse(time.RFC3339, fmt.Sprint(walk["as_of"]))
	if err != nil || walk["clock"] != "local" {
		t.Fatalf("want the local clock: %v", walk)
	}
	h.sentry.mu.Lock()
	first := h.sentry.calls[0].At
	h.sentry.mu.Unlock()
	if asOf.After(first) {
		t.Fatalf("the local clock was read %v after the first request (the walk took ≥ 3 s)", asOf.Sub(first))
	}
}

// TestProdWatch_SentryControlCharactersNeverReachThePost: a NUL or another
// control character in attacker text is dropped before the post — Mattermost
// cannot store a NUL, the delivery would fail and the tick replay for ever.
func TestProdWatch_SentryControlCharactersNeverReachThePost(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		cfg["labels"] = map[string]any{"sentry_issue": "Sentry\x00 issue\x1b[31m", "sentry_detail": "{level}\x07 · {culprit}"}
	})
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4601", ShortID: strp("P-4601"), Title: "boom\x00 x", Culprit: "a\x00b\x07c\x1b[31md\u0085e\u009bf",
		FirstProcessed: now, LastSeen: now, Count: 1})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies()[n:], "\n")
	if !strings.Contains(body, "P-4601") {
		t.Fatalf("setup: the issue did not post:\n%s", body)
	}
	for _, r := range body {
		if r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) {
			t.Fatalf("control character %U reached the post:\n%q", r, body)
		}
	}
}

// TestProdWatch_SentryLabelsCannotBreakACodeSpan: the operator's label words
// render as written, a backtick or a backslash in them escaped — never pairing
// with a value's code span, or escaping its opening backtick, to let the
// attacker's text out.
func TestProdWatch_SentryLabelsCannotBreakACodeSpan(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, label := range map[string]string{
		"a stray backtick":               "{level} · l`origine : {culprit}",
		"a backslash before a value":     `{level} · C:\{culprit}`,
		"emphasis around a value":        "{level} · **{culprit}**",
		"a URL around a value":           "{level} · https://github.com/o/r/search?q={culprit}",
		"a bare host around a value":     "{level} · www.runbook.example/{culprit}",
		"a markdown link around a value": "{level} · [search](https://s.example/?q={culprit})",
		"angle brackets around a value":  "{level} · <{culprit}>",
		"a Slack link around a value":    "{level} · <https://s.example/?q={culprit}|search>",
		"strong emphasis by underscores": "{level} · __{culprit}__",
		"emphasis by an underscore":      "{level} · _{culprit}_",
		"a strike around a value":        "{level} · ~~{culprit}~~",
		"inline LaTeX around a value":    "{level} · $ {culprit} $",
		"a tel: scheme before a value":   "{level} · tel:{culprit}",
		"the mattermost: scheme":         "{level} · mattermost://x/{culprit}",
		"a custom scheme before a value": "{level} · vscode://file/{culprit}",
		"an image around a value":        "{level} · ![{culprit}](https://i.example/a.png)",
		"www. right before a value":      "{level} · see www.{culprit}",
	} {
		name, label := name, label
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) {
				sentryOnly(h, nil)(cfg)
				cfg["labels"] = map[string]any{"sentry_detail": label}
			})
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4501", ShortID: strp("P-4501"), Title: "x", Culprit: "@channel see www.evil-sso.com/login",
				FirstProcessed: now, LastSeen: now, Count: 1})
			n := len(h.bodies())
			sentryTick(t, h, wf)
			body := strings.Join(h.bodies()[n:], "\n")
			if !strings.Contains(body, "`@channel see www.evil-sso.com/login`") {
				t.Fatalf("the culprit did not render inside inline code:\n%s", body)
			}
			for _, line := range strings.Split(body, "\n") {
				if o := pwOutsideCode(line); strings.Contains(o, "@channel") || strings.Contains(o, "evil-sso") {
					t.Fatalf("%s in the label let the value out of its code span:\n%s", name, line)
				}
				if strings.Contains(line, "`@channel see www.evil-sso.com/login`") {
					if bad := pwUnescapedActives(line); len(bad) > 0 {
						t.Fatalf("%s: the label's own markdown stays live around the value (%q):\n%s", name, bad, line)
					}
				}
			}
		})
	}
}

// TestProdWatch_SentryAnEmptyValueOpensNoSpan: a value empty once flattened
// (whitespace, control characters) renders as nothing — an empty span, two
// backticks, would pair with the next value's backtick and flip every span
// after it.
func TestProdWatch_SentryAnEmptyValueOpensNoSpan(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		cfg["labels"] = map[string]any{"sentry_detail": "{culprit}{level} · {count}"}
	})
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4701", ShortID: strp("P-4701"), Title: "x", Culprit: " \t\u0007 ", FirstProcessed: now, LastSeen: now, Count: 3})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies()[n:], "\n")
	if !strings.Contains(body, "`error` · `3`") {
		t.Fatalf("setup: the detail line did not render:\n%s", body)
	}
	if strings.Contains(body, "``") {
		t.Fatalf("an empty value rendered as an empty span:\n%s", body)
	}
}

// TestProdWatch_ALabelsEmojiCodeIsJustText: an emoji code in a label is text
// like the rest of it — a colon after a letter or digit gets its zero-width
// space unless a space follows (one before a space still renders), a dot after
// a letter or digit its backslash — and an emoji glued to a value opens no
// scheme (its name could be one).
func TestProdWatch_ALabelsEmojiCodeIsJustText(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		sentryOnly(h, nil)(cfg)
		cfg["labels"] = map[string]any{"sentry_issue": ":rotating_light: issue",
			"sentry_detail": ":fire:, {level} since v1.2.3 · {culprit} :bell:", "sentry_detail_total": "{level} :bell:{culprit}"}
	})
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4801", ShortID: strp("P-4801"), Title: "x", Culprit: "app.views", FirstProcessed: now, LastSeen: now, Count: 1})
	n := len(h.bodies())
	sentryTick(t, h, wf)
	body := strings.Join(h.bodies()[n:], "\n")
	for _, want := range []string{":rotating\\_light: issue", ":fire\u200b:, `error`", " since v1\\.2\\.3 · `app.views` :bell\u200b:"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the label's text %q is not rendered as plain text:\n%q", want, body)
		}
	}
	// Read by id hours later, the detail is the whole-life one, the emoji glued to the value.
	h.sentry.edit("4801", func(i *pwSentryIssue) {
		i.FirstProcessed = now.Add(-3 * time.Hour)
		i.LastSeen = time.Now().Add(time.Second)
	})
	sentryEditRecord(t, h, "4801", func(r map[string]any) {
		r["last_notified"] = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	})
	n = len(h.bodies())
	sentryTick(t, h, wf)
	body = strings.Join(h.bodies()[n:], "\n")
	if !strings.Contains(body, ":bell\u200b:`app.views`") {
		t.Fatalf("an emoji code glued to a value was not escaped:\n%q", body)
	}
}

// TestProdWatch_ALabelsEmojiNamedLikeASchemeOpensNothing: an emoji code's name
// can be a URL scheme (`:tel:`, `:https:`, the server's custom ones); in a
// label, before a value or not, its colons are plain text like any other — no
// scheme opens around the value's code span, and label words split by an
// emoji never form a host.
func TestProdWatch_ALabelsEmojiNamedLikeASchemeOpensNothing(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	for name, label := range map[string]string{
		"a tel-named emoji then a slash":  "{level} · :tel:/{culprit}",
		"an https-named emoji and a path": "{level} · :https:/{culprit}",
		"a mattermost-named emoji":        "{level} · :mattermost://x{culprit}",
		"an ftp-named emoji then a slash": "{level} · :ftp:/{culprit}",
		"a URL inside an emoji shape":     "{level} · :https://runbook:{culprit}",
		"www split by an emoji":           "{level} · www.:fire:evil-sso.com/login {culprit}",
	} {
		name, label := name, label
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, func(cfg map[string]any) {
				sentryOnly(h, nil)(cfg)
				cfg["labels"] = map[string]any{"sentry_detail": label}
			})
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4501", ShortID: strp("P-4501"), Title: "x", Culprit: "@channel see www.evil-sso.com/login",
				FirstProcessed: now, LastSeen: now, Count: 1})
			n := len(h.bodies())
			sentryTick(t, h, wf)
			body := strings.Join(h.bodies()[n:], "\n")
			found := false
			for _, line := range strings.Split(body, "\n") {
				if strings.Contains(line, "`@channel see www.evil-sso.com/login`") {
					found = true
					if bad := pwUnescapedActives(line); len(bad) > 0 {
						t.Fatalf("%s: the label's own markdown stays live around the value (%q):\n%s", name, bad, line)
					}
				}
			}
			if !found {
				t.Fatalf("%s: setup: the detail line with the value was not posted:\n%s", name, body)
			}
		})
	}
}

// pwRawServer answers every connection with head, then trickles tail one byte
// every 400 ms — until the client hangs up or 60 bytes went out (24 s).
func pwRawServer(t *testing.T, head, tail string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf) // the request (small, one read)
				if _, err := c.Write([]byte(head)); err != nil {
					return
				}
				for i := 0; i < 60 && i < len(tail); i++ {
					time.Sleep(400 * time.Millisecond)
					if _, err := c.Write([]byte{tail[i]}); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

// TestProdWatch_SentryTrickledExchangeStopsAtTheDeadline: the deadline is a
// wall clock over the whole exchange — a server (or a proxy) trickling a byte
// at a time in the status line and headers or in a chunk-size line resets the
// socket timeout on every byte, and the walk still stops at deadline_secs.
func TestProdWatch_SentryTrickledExchangeStopsAtTheDeadline(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("a", 60)
	for name, srv := range map[string][2]string{
		"a trickled header":          {"HTTP/1.1 200 OK\r\nX-Pad: ", pad},
		"a trickled chunk-size line": {"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n", strings.Repeat("0", 60)},
	} {
		name, srv := name, srv
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			base := pwRawServer(t, srv[0], srv[1])
			h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["base_url"] = base; s["deadline_secs"] = 10; delete(s, "environment") }))
			plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
			start := time.Now()
			out, _ := runPollSentry(t, h, plan, vars, secrets, true, nil)
			if took := time.Since(start); took > 15*time.Second || !strings.Contains(fmt.Sprint(out["errors"]), "deadline") {
				t.Fatalf("%s held poll_sentry %v past a 10 s deadline (errors: %v)", name, took, out["errors"])
			}
		})
	}
}

// TestProdWatch_LeakScanStaysLinearOnCraftedLines: log lines anyone can shape
// (a request path, a header) scan in linear time — an email local part is
// matched from where its run starts, a placeholder's braces and a shout-case
// value are read without backtracking, and the NFKC fold's output is cut
// (U+FDFA folds to eighteen characters). Quadratic, each shape took seconds
// for a few dozen lines, and the run budget dies long before max_lines.
func TestProdWatch_LeakScanStaysLinearOnCraftedLines(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	cases := []struct {
		name  string
		lines int
		limit time.Duration
		line  func(i int) string
	}{
		{"an email-class run without an at sign", 120, 3 * time.Second, func(i int) string { return strings.Repeat("㏂", 3990) + strconv.Itoa(i) }},
		{"a secret value of open braces", 60, 2500 * time.Millisecond, func(i int) string { return "password=" + strings.Repeat("{", 3980) + strconv.Itoa(i) }},
		{"a flag value shout-cased but for its end", 300, 2 * time.Second, func(i int) string { return "--pass " + strings.Repeat("A_", 1990) + "!" + strconv.Itoa(i) }},
		{"a line NFKC multiplies", 200, 4 * time.Second, func(i int) string { return strings.Repeat("ﷺ", 3990) + strconv.Itoa(i) }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			raw := filepath.Join(h.scratch, "raw-crafted.jsonl")
			var b strings.Builder
			for i := 0; i < c.lines; i++ {
				rec, _ := json.Marshal(map[string]any{"q": "errors", "ts": strconv.FormatInt(1_700_000_000_000_000_000+int64(i), 10),
					"line": c.line(i), "stream": map[string]any{"container": "web"}})
				b.Write(rec)
				b.WriteByte('\n')
			}
			if err := os.WriteFile(raw, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
				"raw_file": raw, "per_query": map[string]any{"errors": map[string]any{"lines": c.lines, "history_to_ns": "0"}},
				"app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
			if err != nil {
				t.Fatalf("leak_scan: %v %s", err, stderr)
			}
			if d := time.Since(start); d > c.limit {
				t.Fatalf("%d crafted lines took %v (limit %v): a class backtracks on them", c.lines, d, c.limit)
			}
		})
	}
}
