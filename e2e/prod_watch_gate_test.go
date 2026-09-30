package e2e

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pwOutsideSpans returns what markdown reads outside the inline code spans of
// one line: a backslash escapes the next character outside a span; inside a
// span a backtick always closes it (the bot's values carry no backtick).
func pwOutsideSpans(line string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case !in && c == '\\' && i+1 < len(line):
			b.WriteByte(c)
			b.WriteByte(line[i+1])
			i++
		case c == '`':
			in = !in
		case !in:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestProdWatch_ALabelsAngleBracketKeepsTheCulpritInItsSpan: an operator label
// with a `>` or a `|` after {culprit} (`-> triage`, `=> {level}`, a plain `>`)
// and a culprit anyone holding the public DSN writes: once Mattermost's server
// has rewritten `<url|text>` into a link (the fake stores what it stores), the
// culprit's own words are still inside a code span — never a live `www.` link
// or an @channel.
func TestProdWatch_ALabelsAngleBracketKeepsTheCulpritInItsSpan(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ label, culprit string }{
		"an arrow after the culprit": {"{level} · {count} event(s) · {culprit} -> triage", "x<www.evil-sso.com/login @channel|y"},
		"a fat arrow before a value": {"{culprit} => {level}", "x<www.evil-sso.com/login @channel|y"},
		"a plain greater-than":       {"{level} · {culprit} > see Sentry", "x<www.evil-sso.com/login @channel|y"},
		// The label's pipe pairs the culprit's `<` with the `>` of its second span.
		"the culprit twice around a pipe": {"{culprit} | {culprit}", "x>y<www.evil-sso.com/login @channel"},
	} {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := pwLabelDetail(t, c.label, c.culprit)
			found := false
			for _, line := range strings.Split(body, "\n") {
				if !strings.Contains(line, "evil-sso") {
					continue
				}
				found = true
				if out := pwOutsideSpans(line); strings.Contains(out, "evil-sso") || strings.Contains(out, "@channel") {
					t.Fatalf("%s: the culprit left its code span once Mattermost stored the post (a live link and an @channel):\n"+
						"stored: %q\noutside the spans: %q", name, line, out)
				}
			}
			if !found {
				t.Fatalf("%s: setup: the detail line with the culprit was not posted:\n%s", name, body)
			}
		})
	}
}

// TestProdWatch_SentryAPageLongerThanItsLimitIsNotRead: the lane asks
// `limit=100`; a server answering one page of 3000 issues (a compromised
// Sentry, anything answering at base_url) is a lane error, named — none of its
// issues becomes an incident the committed state keeps.
func TestProdWatch_SentryAPageLongerThanItsLimitIsNotRead(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	now := time.Now()
	const n = 3000
	for k := 0; k < n; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(900000 + k), ShortID: strp(fmt.Sprint("P-", k)), Title: "t",
			FirstProcessed: now.Add(-10 * time.Minute), LastSeen: now.Add(-time.Minute), Count: 1})
	}
	h.sentry.mu.Lock()
	h.sentry.pageSize = n // one page, whatever `limit` asked
	h.sentry.mu.Unlock()
	outs := sentryTick(t, h, wf)
	inc, _ := h.state(t)["incidents"].(map[string]any)
	got := 0
	for fp := range inc {
		if strings.HasPrefix(fp, "sentry:") {
			got++
		}
	}
	if errs := fmt.Sprint(outs["poll_sentry"]["errors"]); got != 0 || !strings.Contains(errs, "3000 issues for a limit of 100") {
		t.Fatalf("one page of 3000 issues (limit 100): %d Sentry incidents in the state, errors %s", got, errs)
	}
}

// TestProdWatch_SentryAnOversizedPageFloodsNoChannel: the same oversized
// answer on an armed lane — its issues would all be NEW, each named in full:
// refused, the tick posts no flood.
func TestProdWatch_SentryAnOversizedPageFloodsNoChannel(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf) // arms on an empty project
	now := time.Now()
	const n = 12000
	for k := 0; k < n; k++ {
		h.sentry.put(&pwSentryIssue{ID: fmt.Sprint(700000 + k), ShortID: strp(fmt.Sprint("P-", k)), Title: "t",
			FirstProcessed: now, LastSeen: now, Count: 1})
	}
	h.sentry.mu.Lock()
	h.sentry.pageSize = n
	h.sentry.mu.Unlock()
	before := len(h.bodies())
	sentryTick(t, h, wf)
	if posted := len(h.bodies()) - before; posted > 2 {
		t.Fatalf("one oversized page made the tick post %d messages to the channel", posted)
	}
}

// TestProdWatch_SentryAnIssueNotAskedIsNotRead: the by-id read asks for the
// tracked ids; an answer holding an issue not asked (a hostile server) is a lane
// error — one more than asked, or one in place of the one asked — and the issue
// not asked never becomes an incident.
func TestProdWatch_SentryAnIssueNotAskedIsNotRead(t *testing.T) {
	t.Parallel()
	for _, inPlace := range []bool{false, true} {
		inPlace := inPlace
		t.Run(map[bool]string{false: "one more", true: "in place"}[inPlace], func(t *testing.T) {
			t.Parallel()
			pwAnIssueNotAsked(t, inPlace)
		})
	}
}

func pwAnIssueNotAsked(t *testing.T, inPlace bool) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "21", ShortID: strp("P-21"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
	sentryTick(t, h, wf) // 21 is new: alerted, tracked by id
	h.sentry.edit("21", func(i *pwSentryIssue) { i.FirstProcessed = now.Add(-3 * time.Hour) })
	// 99 exists but no list returns it (no event in 90 days): only the hostile by-id answer carries it.
	h.sentry.put(&pwSentryIssue{ID: "99", ShortID: strp("P-99"), Title: "x", FirstProcessed: now.Add(-200 * 24 * time.Hour),
		LastSeen: now.Add(-100 * 24 * time.Hour), Count: 1})
	h.sentry.mu.Lock()
	h.sentry.extraTracked = []string{"99"}
	if inPlace {
		delete(h.sentry.issues, "21") // the answer holds 99 alone, as many issues as asked
	}
	h.sentry.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	outs := sentryTick(t, h, wf)
	_, minted := h.state(t)["incidents"].(map[string]any)["sentry:99"]
	if errs := fmt.Sprint(outs["poll_sentry"]["errors"]); minted || !strings.Contains(errs, "an issue not asked") {
		t.Fatalf("a by-id answer holding an issue not asked: minted=%v, errors %s", minted, errs)
	}
}

// TestProdWatch_TheRunBudgetFollowsTheRunsCap: plan sizes the fetches' waits to
// the run's EFFECTIVE budget — `iterion run --max-duration` reaches it. Every
// lane at its defaults plus five health probes on five hosts waits 500 s at
// worst: refused under the bot's 12 minutes (naming the way out), accepted
// when the run is given 30.
func TestProdWatch_TheRunBudgetFollowsTheRunsCap(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		var ps []any
		for k := 0; k < 5; k++ {
			ps = append(ps, map[string]any{"id": fmt.Sprint("h", k), "url": fmt.Sprintf("https://h%d.example/health", k)})
		}
		cfg["probes"] = ps
		cfg["sentry"] = map[string]any{"base_url": "https://sentry.example", "org": "o", "project": "p"}
	})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	script := pwTool(t, wf, "plan").Script
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, script, nil, vars, secrets)); err == nil ||
		!strings.Contains(stderr, "the run budget (720 s)") || !strings.Contains(stderr, "--max-duration") {
		t.Fatalf("500 s of waits under a 12-minute run: want a refusal naming the budget and --max-duration: %v %s", err, lastN(stderr, 400))
	}
	given := strings.ReplaceAll(script, "{{run.max_duration_seconds}}", "1800") // iterion run --max-duration 30m
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, given, nil, vars, secrets)); err != nil {
		t.Fatalf("500 s of waits under a 30-minute run was refused: %v %s", err, lastN(stderr, 400))
	}
}

// TestProdWatch_TheRunBudgetIsWhatTheRunHasLeft: a lowered budget, or one the
// run already spent, holds the waits too — the Sentry lane alone (120 s)
// passes under 12 minutes, and is refused under 5 (`--max-duration 5m`), or
// when the run spent 500 s of its 12 before plan ran.
func TestProdWatch_TheRunBudgetIsWhatTheRunHasLeft(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["sentry"] = map[string]any{"base_url": "https://sentry.example", "org": "o", "project": "p"}
	})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	script := pwTool(t, wf, "plan").Script
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, script, nil, vars, secrets)); err != nil {
		t.Fatalf("the Sentry lane alone under a 12-minute run was refused: %v %s", err, lastN(stderr, 400))
	}
	for name, run := range map[string]map[string]string{
		"lowered": {"{{run.max_duration_seconds}}": "300"},
		"spent":   {"{{run.elapsed_seconds}}": "500"},
	} {
		s := script
		for ref, v := range run {
			s = strings.ReplaceAll(s, ref, v)
		}
		if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, s, nil, vars, secrets)); err == nil || !strings.Contains(stderr, "the fetches can wait") {
			t.Fatalf("%s: the Sentry lane's 120 s past what the run has left was not refused: %v %s", name, err, lastN(stderr, 400))
		}
	}
}

// TestProdWatch_AProbeThatTimesOutOnceIsRetried: a health URL whose FIRST answer
// outlasts timeout_secs (a cold start, a GC pause) and whose second is
// immediate is reported healthy — the probe gets its second try, as plan
// budgets it; one slow answer posts no critical.
func TestProdWatch_AProbeThatTimesOutOnceIsRetried(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			time.Sleep(3 * time.Second) // the first answer outlasts timeout_secs 2
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	in := map[string]any{"probes": []map[string]any{{"id": "api", "url": slow.URL + "/health", "expect_status": 200,
		"timeout_secs": 2, "severity": "critical"}}, "timeout_secs": 20, "allow_private": true}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "probe_http").Script, in, nil, nil))
	if err != nil {
		t.Fatalf("probe_http: %v %s", err, stderr)
	}
	if r := out["results"].([]any)[0].(map[string]any); r["ok"] != true || hits.Load() != 2 {
		t.Fatalf("one slow answer, then a fast one: want the probe healthy on its second try, got %d request(s): %v", hits.Load(), r)
	}
}

// TestProdWatch_ALanesFirstLookupStaysInsideItsDeadline: a lane's host lookup
// is bounded by the time its deadline leaves, not by fetch_timeout_secs — a
// resolver answering in 16 s holds a 10 s lane 10 s, Sentry and Grafana alike
// (plan budgets each lane at its deadline).
func TestProdWatch_ALanesFirstLookupStaysInsideItsDeadline(t *testing.T) {
	t.Parallel()
	t.Run("sentry", func(t *testing.T) {
		t.Parallel()
		wf := compileFixture(t, "prod-watch/main.bot")
		h := newPWHarness(t)
		port := h.srv.URL[strings.LastIndex(h.srv.URL, ":")+1:]
		h.writeConfig(t, sentryOnly(h, func(s map[string]any) { s["base_url"] = "http://slowsentry.test:" + port; s["deadline_secs"] = 10 }))
		plan, vars, secrets := sentryPlan(t, h, "prod-watch/main.bot")
		start := time.Now()
		out, stderr, err := runPyWhole(t, h.ws, pwSlowResolverStub("slowsentry.test", port, "16")+
			pwSub(t, pwTool(t, wf, "poll_sentry").Script, map[string]any{"sentry": plan["sentry"], "timeout_secs": 20,
				"scratch_dir": h.scratch, "allow_private": true}, vars, secrets))
		if took := time.Since(start); err != nil || took > 13*time.Second {
			t.Fatalf("a 10 s Sentry deadline behind a 16 s lookup ran %v: %v %s %v", took, err, lastN(stderr, 200), out["errors"])
		}
	})
	t.Run("loki", func(t *testing.T) {
		t.Parallel()
		wf := compileFixture(t, "prod-watch/main.bot")
		h := newPWHarness(t)
		port := h.srv.URL[strings.LastIndex(h.srv.URL, ":")+1:]
		h.writeConfig(t, func(cfg map[string]any) {
			lokiOnly(1000, 60)(cfg)
			g := cfg["grafana"].(map[string]any)
			g["base_url"] = "http://slowloki.test:" + port
			g["deadline_secs"] = 10
		})
		secrets := map[string]string{"grafana_token": h.tokenFile}
		plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, cursorVars(h, 60, 0, 5000), secrets))
		if err != nil {
			t.Fatalf("plan: %v %s", err, stderr)
		}
		start := time.Now()
		out, stderr, err := runPyWhole(t, h.ws, pwSlowResolverStub("slowloki.test", port, "16")+
			pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{"grafana": plan["grafana"], "loki": plan["loki"],
				"timeout_secs": 20, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
		if took := time.Since(start); err != nil || took > 13*time.Second {
			t.Fatalf("a 10 s Loki deadline behind a 16 s lookup ran %v: %v %s %v", took, err, lastN(stderr, 200), out["errors"])
		}
	})
	t.Run("grafana", func(t *testing.T) {
		t.Parallel()
		wf := compileFixture(t, "prod-watch/main.bot")
		h := newPWHarness(t)
		raw := pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60))
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(raw, "http://"))
		start := time.Now()
		out, stderr, err := runPyWhole(t, h.ws, pwSlowResolverStub("slowgraf.test", port, "16")+
			pwSub(t, pwTool(t, wf, "poll_prom").Script, pwPromInputs("http://slowgraf.test:"+port, 10, 1, 20), nil,
				map[string]string{"grafana_token": h.tokenFile}))
		if took := time.Since(start); err != nil || took > 13*time.Second {
			t.Fatalf("a 10 s Grafana deadline behind a 16 s lookup ran %v: %v %s %v", took, err, lastN(stderr, 200), out["errors"])
		}
	})
}

// TestProdWatch_AMaxAlertsPerRunBelowZeroIsRefused: a negative per-run cap is
// refused by name — as a slice bound it would post every alert but the last.
func TestProdWatch_AMaxAlertsPerRunBelowZeroIsRefused(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	_, stderr, err := pwDecide(t, wf, h, map[string]any{"templates": []any{}, "leak": []any{}}, pwLokiState(map[string]any{}),
		map[string]any{"max_alerts": -1})
	if err == nil || !strings.Contains(stderr, "max_alerts_per_run must be 0 or more") || strings.Contains(stderr, "Traceback") {
		t.Fatalf("max_alerts_per_run -1 was not refused by name: %v %s", err, lastN(stderr, 300))
	}
}

// pwCaseSplitResolverStub answers the lower-case name with a public address
// (what the SSRF guard checks) and any other spelling with 127.0.0.1 — the
// second answer of a DNS rebinding.
func pwCaseSplitResolverStub(lower, port string) string {
	return "import os as _o, socket as _s\n" +
		"_o.environ['no_proxy'] = '*'; _o.environ['NO_PROXY'] = '*'\n" +
		"_gai0 = _s.getaddrinfo\n" +
		"def _split(host, *a, **k):\n" +
		"    if host == '" + lower + "':\n" +
		"        return [(_s.AF_INET, _s.SOCK_STREAM, 6, '', ('93.184.216.34', " + port + "))]\n" +
		"    if host.lower() == '" + lower + "':\n" +
		"        return [(_s.AF_INET, _s.SOCK_STREAM, 6, '', ('127.0.0.1', " + port + "))]\n" +
		"    return _gai0(host, *a, **k)\n" +
		"_s.getaddrinfo = _split\n"
}

// TestProdWatch_AMixedCaseHostConnectsWhereTheGuardChecked: the guard resolves
// the URL's lower-cased host; a probe URL (or a redirect) spelling it
// `Guard.test` connects to the addresses the guard checked, never to a second
// answer nobody checked (here the loopback fake, private sources refused).
func TestProdWatch_AMixedCaseHostConnectsWhereTheGuardChecked(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	port := h.srv.URL[strings.LastIndex(h.srv.URL, ":")+1:]
	for _, host := range []string{"guard.test", "Guard.test"} {
		in := map[string]any{"probes": []map[string]any{{"id": "api", "url": "http://" + host + ":" + port + "/health", "expect_status": 200,
			"timeout_secs": 3, "severity": "critical"}}, "timeout_secs": 5, "allow_private": false}
		out, stderr, err := runPyWhole(t, h.ws, pwCaseSplitResolverStub("guard.test", port)+pwSub(t, pwTool(t, wf, "probe_http").Script, in, nil, nil))
		if err != nil {
			t.Fatalf("probe_http: %v %s", err, stderr)
		}
		if r := out["results"].([]any)[0].(map[string]any); r["ok"] == true {
			t.Fatalf("%s reached the loopback fake (status %v) with private sources refused: the connection used an address the guard never checked", host, r["status"])
		}
	}
}

// TestProdWatch_AProbeURLThatDoesNotParseLeavesThePlanRunning: a typo in one
// probe's URL fails that probe, by name, when it runs — never plan, and with
// it every lane of every tick.
func TestProdWatch_AProbeURLThatDoesNotParseLeavesThePlanRunning(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["probes"] = []map[string]any{{"id": "api", "url": "http://[::1/health", "expect_status": 200, "severity": "critical"}}
	})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("a probe URL typo stopped plan: %v %s", err, lastN(stderr, 400))
	}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "probe_http").Script,
		map[string]any{"probes": plan["probes"], "timeout_secs": 5, "allow_private": true}, nil, nil))
	if err != nil {
		t.Fatalf("probe_http: %v %s", err, stderr)
	}
	if r := out["results"].([]any)[0].(map[string]any); r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), "ValueError") {
		t.Fatalf("the probe with the typo must fail by name: %v", r)
	}
}

// pwSaysAhead: the walk of this tick names Sentry stamps it did not take.
func pwSaysAhead(o map[string]map[string]any) bool {
	return strings.Contains(fmt.Sprint(o["poll_sentry"]["walk"].(map[string]any)["partial"]), "ahead of this runner's clock not taken")
}

// TestProdWatch_SentryAFutureDateAtArmingMutesNothing: the arming time is the
// bootstrap answer's Date — one a year ahead (a hostile server, a proxy with a
// broken clock) is not taken: the lane arms on the runner's clock, says so,
// and a regression dated now is posted once the Date is right again.
func TestProdWatch_SentryAFutureDateAtArmingMutesNothing(t *testing.T) {
	t.Parallel()
	for name, offset := range map[string]time.Duration{"control": 0, "a year ahead at the arming": 365 * 24 * time.Hour} {
		name, offset := name, offset
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wf := compileFixture(t, "prod-watch/main.bot")
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			h.sentry.mu.Lock()
			h.sentry.dateOffset = offset
			h.sentry.mu.Unlock()
			if o := sentryTick(t, h, wf); pwSaysAhead(o) != (offset > 0) {
				t.Fatalf("%s: the bootstrap walk says stamps ahead=%v: %v", name, pwSaysAhead(o), o["poll_sentry"]["walk"])
			}
			h.sentry.mu.Lock()
			h.sentry.dateOffset = 0
			h.sentry.mu.Unlock()
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4401", ShortID: strp("PROJ-4401"), Title: "r", Substatus: strp("regressed"),
				FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now, Acts: []pwSentryAct{{Type: "set_regression", At: now}}})
			var got []string
			for k := 0; k < 3; k++ {
				got = append(got, sentryAlerts(sentryTick(t, h, wf))...)
			}
			if !strings.Contains(strings.Join(got, " "), "regressed:PROJ-4401") {
				t.Fatalf("%s: a regression dated now was never posted: %v (armed_at %v)", name, got,
					h.state(t)["cursors"].(map[string]any)["sentry"].(map[string]any)["armed_at"])
			}
		})
	}
}

// TestProdWatch_SentryAFutureLastSeenMutesNoEscalation: one answer carrying a
// lastSeen a year ahead is not taken (the walk says so): the issue's next real
// events are sighted, and its level rising to fatal escalates.
func TestProdWatch_SentryAFutureLastSeenMutesNoEscalation(t *testing.T) {
	t.Parallel()
	for name, poison := range map[string]bool{"control": false, "one answer a year ahead": true} {
		name, poison := name, poison
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wf := compileFixture(t, "prod-watch/main.bot")
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			sentryTick(t, h, wf)
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4601", ShortID: strp("PROJ-4601"), Title: "x", FirstProcessed: now, LastSeen: now, Count: 1})
			if got := sentryAlerts(sentryTick(t, h, wf)); strings.Join(got, " ") != "new:PROJ-4601:medium" {
				t.Fatalf("setup: the new issue was not posted: %v", got)
			}
			if poison {
				h.sentry.edit("4601", func(i *pwSentryIssue) { i.LastSeen = time.Now().Add(365 * 24 * time.Hour) })
				if o := sentryTick(t, h, wf); !pwSaysAhead(o) {
					t.Fatalf("a lastSeen a year ahead was taken in silence: %v", o["poll_sentry"]["walk"])
				}
			}
			time.Sleep(1100 * time.Millisecond)
			h.sentry.edit("4601", func(i *pwSentryIssue) { i.LastSeen = time.Now(); i.Level = "fatal"; i.Count = 9 })
			var got []string
			for k := 0; k < 2; k++ {
				got = append(got, sentryAlerts(sentryTick(t, h, wf))...)
			}
			if !strings.Contains(strings.Join(got, " "), "escalated:PROJ-4601:high") {
				t.Fatalf("%s: the level rose to fatal with new events and it never escalated: %v (sentry_last_seen %v)",
					name, got, sentryIncident(t, h, "4601")["sentry_last_seen"])
			}
		})
	}
}

// TestProdWatch_SentryADateAheadOfSentrysDataHidesNoNewIssue: a front whose
// clock runs ahead of the Sentry backend by more than the overlap — its Date is
// not taken (the cursor follows the runner's clock, the walk says so), and a new
// issue first processed now is posted.
func TestProdWatch_SentryADateAheadOfSentrysDataHidesNoNewIssue(t *testing.T) {
	t.Parallel()
	for name, offset := range map[string]time.Duration{"control": 0, "the front's clock 2 h ahead": 2 * time.Hour} {
		name, offset := name, offset
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wf := compileFixture(t, "prod-watch/main.bot")
			h := newPWHarness(t)
			h.writeConfig(t, sentryOnly(h, nil))
			h.sentry.mu.Lock()
			h.sentry.dateOffset = offset
			h.sentry.mu.Unlock()
			sentryTick(t, h, wf)
			o := sentryTick(t, h, wf)
			if pwSaysAhead(o) != (offset > 0) {
				t.Fatalf("%s: the walk says stamps ahead=%v: %v", name, pwSaysAhead(o), o["poll_sentry"]["walk"])
			}
			if offset > 0 && sentryTickCoverage(t, o) == "full" {
				t.Fatalf("%s: a Date not taken left the coverage note full", name)
			}
			now := time.Now()
			h.sentry.put(&pwSentryIssue{ID: "4501", ShortID: strp("PROJ-4501"), Title: "ValueError: new", FirstProcessed: now,
				LastSeen: now, Count: 1})
			var got []string
			for k := 0; k < 3; k++ {
				got = append(got, sentryAlerts(sentryTick(t, h, wf))...)
			}
			if !strings.Contains(strings.Join(got, " "), "new:PROJ-4501") {
				t.Fatalf("%s: a new issue first processed now was never posted in 3 ticks: %v", name, got)
			}
		})
	}
}

// TestProdWatch_SentryAFutureActivityDateMutesNoRegression: a regression's
// activity dated a year ahead is not taken (the walk says so): once the issue
// regresses for real, now, the regression is posted — a future date never
// becomes the transition every later one must pass.
func TestProdWatch_SentryAFutureActivityDateMutesNoRegression(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, sentryOnly(h, nil))
	sentryTick(t, h, wf)
	now := time.Now()
	h.sentry.put(&pwSentryIssue{ID: "4701", ShortID: strp("PROJ-4701"), Title: "r", Substatus: strp("regressed"),
		FirstProcessed: now.Add(-30 * 24 * time.Hour), LastSeen: now,
		Acts: []pwSentryAct{{Type: "set_regression", At: now.Add(365 * 24 * time.Hour)}}})
	if o := sentryTick(t, h, wf); !pwSaysAhead(o) {
		t.Fatalf("an activity dated a year ahead was taken in silence: %v", o["poll_sentry"]["walk"])
	}
	if at := sentryIncident(t, h, "4701")["transition_at"]; at != nil && fmt.Sprint(at) > time.Now().Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("the future activity date became the issue's transition: %v", at)
	}
	time.Sleep(1100 * time.Millisecond)
	t1 := time.Now()
	h.sentry.edit("4701", func(i *pwSentryIssue) {
		i.Acts = append(i.Acts, pwSentryAct{Type: "set_regression", At: t1})
		i.LastSeen = t1
	})
	var got []string
	for k := 0; k < 3; k++ {
		got = append(got, sentryAlerts(sentryTick(t, h, wf))...)
	}
	if !strings.Contains(strings.Join(got, " "), "regressed:PROJ-4701") {
		t.Fatalf("a regression dated now, after an activity dated a year ahead, was never posted: %v", got)
	}
}
