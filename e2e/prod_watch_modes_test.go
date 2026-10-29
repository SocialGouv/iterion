package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The digest (#1701) and the session (#1708) ride the watch tick's chain:
// plan routes by four exclusive booleans, the lifecycle runs on the mode's
// state dir, notify renders the reports and ends every message with its
// run. modeTick walks the workflow's edges node script by node script, as
// the engine would, and hands back every node's output.
func (h *pwHarness) modeTick(t *testing.T, wf *ir.Workflow, mode string, extraVars map[string]any) map[string]map[string]any {
	t.Helper()
	outs := map[string]map[string]any{}
	vars := map[string]any{
		"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": mode, "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000,
	}
	for k, v := range extraVars {
		vars[k] = v
	}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile, "sentry_token": h.sentryTokenFile}
	h.stderrs = map[string]string{}
	run := func(id string, inputs map[string]any) map[string]any {
		t.Helper()
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, id).Script, inputs, vars, secrets))
		if err != nil {
			t.Fatalf("%s failed: %v\nstderr: %s", id, err, stderr)
		}
		outs[id] = out
		h.stderrs[id] = stderr
		return out
	}
	plan := run("plan", nil)
	if plan["halted"] == true || plan["step_back"] == true {
		return outs
	}
	notifyIn := func(src map[string]any, release, releaseKnown any) map[string]any {
		return map[string]any{
			"alerts": src["alerts"], "overflow_count": src["overflow_count"], "stale_sources": src["stale_sources"], "reports": src["reports"],
			"sinks": plan["sinks"], "labels": plan["labels"], "app": plan["app"], "sentry": plan["sentry"],
			"release": release, "release_known": releaseKnown, "dry_run": false, "max_message_chars": h.maxMsgChars(),
			"deliver_by": pwDeliverBy(), "persist_state": plan["persist"], "session_next_file": src["session_next_file"],
		}
	}
	promIn := map[string]any{"grafana": plan["grafana"], "prometheus": plan["prometheus"], "timeout_secs": 5, "allow_private": true}
	if plan["collect"] == true {
		prom := run("poll_prom", promIn)
		dg := run("digest", map[string]any{"digest": plan["digest"], "workspace": h.ws, "state_dir": plan["state_dir"], "scratch_dir": h.scratch,
			"prom_ranges": prom["ranges"], "prom_ok": prom["ok"], "prom_errors": prom["errors"], "lanes": plan["lanes"], "source_stale_hours": 6})
		notify := run("notify", notifyIn(dg, dg["release"], dg["release_known"]))
		if notify["persist"] == true {
			t.Fatalf("a digest delivery must persist nothing, notify said persist=true")
		}
		return outs
	}
	if plan["poll"] != true {
		t.Fatalf("plan routed the %s tick nowhere: %v", mode, plan["summary"])
	}
	rel := run("resolve_release", map[string]any{"release": plan["release"], "timeout_secs": 5, "allow_private": true})
	loki := run("poll_loki", map[string]any{"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true})
	prom := run("poll_prom", promIn)
	sentry := run("poll_sentry", map[string]any{"sentry": plan["sentry"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true})
	galerts := run("poll_galerts", map[string]any{"grafana": plan["grafana"], "grafana_alerts": plan["grafana_alerts"],
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true})
	probe := run("probe_http", map[string]any{"probes": plan["probes"], "timeout_secs": 5, "allow_private": true})
	leak := run("leak_scan", map[string]any{"raw_file": loki["raw_file"], "per_query": loki["per_query"],
		"sentry_file": sentry["raw_file"], "sentry_issues": sentry["issues"],
		"galerts_file": galerts["raw_file"], "galerts_expected": galerts["alerts"], "app": plan["app"], "scratch_dir": h.scratch})
	decide := run("decide", map[string]any{
		"mode": mode, "signals_file": leak["signals_file"], "prom_results": prom["results"], "prom_metrics": prom["metrics"], "http_results": probe["results"],
		"loki":    plan["loki"],
		"loki_ok": loki["ok"], "loki_truncated": loki["truncated"], "loki_errors": loki["errors"], "loki_per_query": loki["per_query"],
		"prom_ok": prom["ok"], "prom_errors": prom["errors"], "release": rel["release"], "release_known": rel["release_known"],
		"sentry": plan["sentry"], "sentry_ok": sentry["ok"], "sentry_truncated": sentry["truncated"], "sentry_errors": sentry["errors"],
		"sentry_walk": sentry["walk"], "sentry_issues": sentry["issues"],
		"grafana_alerts": plan["grafana_alerts"], "galerts_ok": galerts["ok"], "galerts_truncated": galerts["truncated"],
		"galerts_errors": galerts["errors"], "galerts_walk": galerts["walk"], "galerts_expected": galerts["alerts"],
		"lanes": plan["lanes"], "app": plan["app"], "workspace": h.ws, "state_dir": plan["state_dir"], "state_seed_file": plan["state_seed_file"],
		"scratch_dir": h.scratch, "renotify_hours": 24, "quiet_after_hours": 48, "forget_after_days": 14, "source_stale_hours": 6,
		"max_alerts": h.maxAlerts(), "max_alerts_per_lane": h.maxPerLane(), "max_message_chars": h.maxMsgChars(),
		"session_cap_left": plan["session_cap_left"],
	})
	src := decide
	if mode == "session" {
		src = run("session", map[string]any{"session": plan["session"], "workspace": h.ws, "alerts": decide["alerts"], "overflow_count": decide["overflow_count"],
			"stale_sources": decide["stale_sources"], "held": decide["held"], "tick_file": decide["tick_file"], "prom_results": prom["results"],
			"prom_metrics": prom["metrics"], "http_results": probe["results"], "state_dir": plan["state_dir"], "scratch_dir": h.scratch,
			"release": rel["release"]})
	}
	notify := run("notify", notifyIn(src, rel["release"], rel["release_known"]))
	if notify["persist"] == true {
		run("commit_state", map[string]any{"state_next_file": decide["state_next_file"], "alertlog_file": decide["alertlog_file"],
			"tick_file": decide["tick_file"], "session_next_file": notify["session_next_file"], "generation": decide["generation"],
			"state_commit": false, "workspace": h.ws, "state_dir": plan["state_dir"], "ledger": plan["ledger"]})
	}
	return outs
}

// pwTickRecord is one healthy watch tick record stamped at `at`.
func pwTickRecord(at time.Time, gen int) map[string]any {
	return map[string]any{"at": at.Format(time.RFC3339), "generation": gen, "lines": 10, "templates": 0, "alerts": 0, "overflow": 0,
		"lanes_ok": map[string]any{"loki": true, "prometheus": true, "probes": true}, "coverage": "full", "release": "abc1234",
		"release_known": false, "bootstrap": false, "sentry_issues": 0, "sentry_bootstrap": false,
		"probes_ok": 1, "probes_total": 1, "metrics": []any{}, "escalations": 0, "held": 0}
}

// pwWriteLedger writes records as one JSON line each into <ws>/<rel>.
func pwWriteLedger(t *testing.T, h *pwHarness, rel string, recs []map[string]any) {
	t.Helper()
	path := filepath.Join(h.ws, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pwGridTo is the end of the digest's window: now, aligned down to the tick
// grid — the same alignment plan applies. A fixture written just before a
// grid mark and a digest run just after it would disagree on the window by
// one slot: the helper waits out the mark instead.
func pwGridTo(interval time.Duration) time.Time {
	for {
		now := time.Now().UTC()
		to := now.Truncate(interval)
		if to.Add(interval).Sub(now) > 15*time.Second {
			return to
		}
		time.Sleep(time.Second)
	}
}

// writeTicks writes one healthy tick record per slot for the newest n
// slots of `interval`, each in the middle of its slot, into the watch
// ledger; mod reshapes a record before it is written.
func (h *pwHarness) writeTicks(t *testing.T, n int, interval time.Duration, mod func(i int, rec map[string]any)) {
	t.Helper()
	to := pwGridTo(interval)
	recs := []map[string]any{}
	for i := 0; i < n; i++ {
		rec := pwTickRecord(to.Add(-interval/2-time.Duration(i)*interval), i+1)
		if mod != nil {
			mod(i, rec)
		}
		recs = append(recs, rec)
	}
	pwWriteLedger(t, h, ".prod-watch/ticks.jsonl", recs)
}

// pwEmptyState is an established, quiet watch state.
func pwEmptyState(gen int) map[string]any {
	return map[string]any{"version": 1, "generation": gen, "cursors": map[string]any{"loki": map[string]any{}},
		"incidents": map[string]any{}, "health": map[string]any{}}
}

// pwSessionConfig declares one window of the given days around now (two
// hours each side, clipped to the day) and one heartbeat metric.
func pwSessionConfig(t *testing.T, h *pwHarness, days []string, probes []map[string]any) {
	t.Helper()
	pwSessionWindowConfig(t, h, days, probes, -120, 120)
}

// pwSessionWindowConfig declares one window of the given days from now+fromMin
// to now+toMin (clipped to the day) and one heartbeat metric.
func pwSessionWindowConfig(t *testing.T, h *pwHarness, days []string, probes []map[string]any, fromMin, toMin int) {
	t.Helper()
	now := time.Now().UTC()
	nowMin := now.Hour()*60 + now.Minute()
	from, to := nowMin+fromMin, nowMin+toMin
	if from < 0 {
		from = 0
	}
	toText := "24:00"
	if to < 1440 {
		toText = fmt.Sprintf("%02d:%02d", to/60, to%60)
	}
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["prometheus"] = map[string]any{"probes": probes}
		cfg["session"] = map[string]any{
			"active_windows": []map[string]any{{"days": days, "from": fmt.Sprintf("%02d:%02d", from/60, from%60), "to": toText, "tz": "UTC"}},
			"heartbeat":      map[string]any{"metrics": []map[string]any{{"id": "rps", "title": "requests/s", "query": "rps-q", "unit": "req/s"}}},
		}
	})
}

func pwToday() string { return strings.ToLower(time.Now().UTC().Weekday().String()[:3]) }

// pwNotToday is a day that is never today: two days ahead.
func pwNotToday() string {
	return strings.ToLower(time.Now().UTC().Add(48 * time.Hour).Weekday().String()[:3])
}

var pwRestartsProbe = []map[string]any{{"id": "restarts", "title": "restarts", "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"}}

// sessionFile reads the session envelope the session mode keeps.
func (h *pwHarness) sessionFile(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"))
	if err != nil {
		t.Fatalf("session.json: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("session.json: %v", err)
	}
	return env
}

// editSession reshapes the session envelope in place.
func (h *pwHarness) editSession(t *testing.T, mod func(env map[string]any)) {
	t.Helper()
	env := h.sessionFile(t)
	mod(env)
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func pwCount(bodies []string, marker string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, marker) {
			n++
		}
	}
	return n
}

// TestProdWatch_DigestSaysTheTicksMissing: a day with 60 ticks in the ledger
// against 144 expected (one every 10 min) — the digest carries the explicit
// "ticks missing" line with both numbers, and ends with its run.
func TestProdWatch_DigestSaysTheTicksMissing(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"tick_interval_minutes": 10} })
	h.writeTicks(t, 60, 10*time.Minute, nil)
	h.setState(t, pwEmptyState(60))
	outs := h.modeTick(t, wf, "digest", nil)
	if outs["plan"]["collect"] != true || outs["plan"]["poll"] == true {
		t.Fatalf("the digest tick must collect, not poll: %v", outs["plan"]["summary"])
	}
	if got := h.sinkHits.Load(); got != 1 {
		t.Fatalf("the digest must post exactly one message, got %d", got)
	}
	body := h.bodies()[0]
	for _, want := range []string{"DAILY DIGEST", "`84` tick(s) MISSING", "`60` executed of `144` expected", "`60` tick(s) executed of `144` expected (one every `10` min)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "run `harness`") {
		t.Fatalf("the digest does not end with its run:\n%s", body)
	}
	if _, wrote := outs["commit_state"]; wrote {
		t.Fatalf("the digest wrote state")
	}
}

// TestProdWatch_DigestPostsAQuietDay: a window with every tick executed, no
// alert, no incident and healthy probes STILL posts — the digest is the
// heartbeat — and carries the configured ranges.
func TestProdWatch_DigestPostsAQuietDay(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["digest"] = map[string]any{"tick_interval_minutes": 30, "prometheus": map[string]any{
			"ranges": []map[string]any{{"id": "req", "title": "requests", "query": "req-q", "agg": "sum", "unit": "req"}}}}
	})
	h.promRanges.Store(map[string][]float64{"req-q": {10, 20, 30}})
	h.writeTicks(t, 48, 30*time.Minute, nil)
	h.setState(t, pwEmptyState(48))
	h.modeTick(t, wf, "digest", nil)
	if got := h.sinkHits.Load(); got != 1 {
		t.Fatalf("a quiet day must still post its digest, got %d message(s)", got)
	}
	body := h.bodies()[0]
	for _, want := range []string{"no alert posted", "`48` tick(s) executed of `48` expected", "`requests`: `60` `req`", "`3` point(s)", "`48` tick(s)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "MISSING") {
		t.Fatalf("a complete day must not say ticks are missing:\n%s", body)
	}
	h.callMu.Lock()
	ranges := len(h.rangeCalls)
	h.callMu.Unlock()
	if ranges != 1 {
		t.Fatalf("expected one query_range call, got %d", ranges)
	}
}

// TestProdWatch_AClippedMessageStillEndsWithItsRun: a report longer than the
// message budget is cut on a line boundary and STILL ends with the run link
// — the footer's room is reserved before the clip (#2430).
func TestProdWatch_AClippedMessageStillEndsWithItsRun(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, stderr)
	}
	lines := []map[string]any{}
	for i := 0; i < 80; i++ {
		lines = append(lines, map[string]any{"label": "digest_range", "fields": map[string]any{
			"title": fmt.Sprintf("a long metric title number %02d that fills the line", i), "value": i, "unit": "req", "agg": "sum", "points": 288}})
	}
	report := map[string]any{"marker": "digest", "icon": ":bar_chart:", "severity": "low", "title_key": "digest_title", "lines": lines}
	const url = "https://iterion.example/studio/runs/harness"
	script := strings.ReplaceAll(pwTool(t, wf, "notify").Script, "{{run.url}}", `"`+url+`"`)
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, script, map[string]any{
		"alerts": []any{}, "overflow_count": 0, "stale_sources": []any{}, "reports": []any{report},
		"sinks": plan["sinks"], "labels": plan["labels"], "app": plan["app"], "sentry": map[string]any{},
		"release": "", "release_known": false, "dry_run": false, "max_message_chars": 1500, "deliver_by": pwDeliverBy(),
		"persist_state": true, "session_next_file": "",
	}, vars, secrets))
	if err != nil {
		t.Fatalf("notify: %v\n%s", err, stderr)
	}
	if out["delivered"] != float64(1) {
		t.Fatalf("expected one delivery, got %v", out["delivered"])
	}
	body := h.bodies()[0]
	if n := utf8.RuneCountInString(body); n > 1500 {
		t.Fatalf("the message runs %d characters, over the 1500 budget", n)
	}
	footer := "🤖 [iterion](https://iterion.cloud) · run [`harness`](" + url + ")"
	if !strings.HasSuffix(body, footer) {
		t.Fatalf("the clipped message does not end with its run link:\n…%s", body[len(body)-200:])
	}
	if !strings.Contains(body, "DAILY DIGEST") || !strings.Contains(body, "number 00") {
		t.Fatalf("the head of the message did not survive the clip:\n%s", body)
	}
	if strings.Contains(body, "number 79") {
		t.Fatalf("the message was not clipped at all:\n%s", body)
	}
}

// TestProdWatch_ASessionSaysStartEscalationHeartbeatSummaryOnce: a three-tick
// session with a signal appearing at tick 2, then the closing tick — exactly
// ONE start, ONE escalation, ONE heartbeat, ONE summary; the heartbeat and the
// summary carry the positive numbers; the watch state is never written.
func TestProdWatch_ASessionSaysStartEscalationHeartbeatSummaryOnce(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.prom.Store(map[string]pwProm{"rps-q": {Value: "12.5"}})
	// The watch state the session forks from: one quiet watch tick, before
	// the window is declared (inside a declared window the watch steps back).
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	seed, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	before := int(h.sinkHits.Load())

	outs := h.modeTick(t, wf, "session", nil)
	sess, _ := outs["plan"]["session"].(map[string]any)
	if sess["new"] != true || outs["plan"]["state_seed_file"] == "" {
		t.Fatalf("the first session tick must open the session on a fork of the watch state: %v", outs["plan"]["summary"])
	}
	h.prom.Store(map[string]pwProm{"rps-q": {Value: "14"}, "restarts-q": {Value: "3"}})
	h.modeTick(t, wf, "session", nil)
	h.editSession(t, func(env map[string]any) {
		env["last_heartbeat_at"] = time.Now().UTC().Add(-31 * time.Minute).Format(time.RFC3339)
	})
	h.modeTick(t, wf, "session", nil)
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	outs = h.modeTick(t, wf, "session", nil)
	if sess, _ := outs["plan"]["session"].(map[string]any); sess["closing"] != true {
		t.Fatalf("a session tick outside the window must close the open session: %v", outs["plan"]["summary"])
	}

	bodies := h.bodies()[before:]
	for marker, want := range map[string]int{"SESSION START": 1, "metric threshold breached": 1, "HEARTBEAT": 1, "SESSION SUMMARY": 1} {
		if got := pwCount(bodies, marker); got != want {
			t.Fatalf("%q posted %d time(s), want %d:\n%s", marker, got, want, strings.Join(bodies, "\n---\n"))
		}
	}
	if len(bodies) != 4 {
		t.Fatalf("expected exactly 4 messages (start, escalation, heartbeat, summary), got %d:\n%s", len(bodies), strings.Join(bodies, "\n---\n"))
	}
	for _, want := range []string{"`requests/s`: `12.5` `req/s`", "active until", "probe `api`: `ok`"} {
		if !strings.Contains(bodies[0], want) {
			t.Fatalf("the start message lacks %q:\n%s", want, bodies[0])
		}
	}
	for _, want := range []string{"`3` tick(s) · `1` escalation(s)", "`requests/s`: `14` `req/s`"} {
		if !strings.Contains(bodies[2], want) {
			t.Fatalf("the heartbeat lacks %q:\n%s", want, bodies[2])
		}
	}
	for _, want := range []string{"`1` escalation(s): critical `0` · high `1`", "`requests/s`: min `12.5` · max `14` · last `14`", "escalated: `restarts ×1`", "`4` tick(s) · `1` heartbeat(s)"} {
		if !strings.Contains(bodies[3], want) {
			t.Fatalf("the summary lacks %q:\n%s", want, bodies[3])
		}
	}
	env := h.sessionFile(t)
	if env["closed"] != true || env["escalations"] != float64(1) || env["heartbeats"] != float64(1) || env["ticks"] != float64(4) {
		t.Fatalf("the envelope is wrong: %v", env)
	}
	after, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(seed) {
		t.Fatalf("a session tick wrote the watch state")
	}
	if _, err := os.Stat(filepath.Join(h.ws, ".prod-watch", "session", "ticks.jsonl")); err != nil {
		t.Fatalf("the session keeps its own tick ledger: %v", err)
	}
}

// TestProdWatch_AWatchTickStepsBackWhileASessionIsOpen: the idle tick reads
// session.json and steps back — nothing polled, nothing posted — while the
// session's window runs; once the window is over it resumes and sees the
// signal with its own lifecycle.
func TestProdWatch_AWatchTickStepsBackWhileASessionIsOpen(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	h.modeTick(t, wf, "session", nil)
	if got := pwCount(h.bodies(), "metric threshold breached"); got != 1 {
		t.Fatalf("the session must escalate the breach once, got %d", got)
	}
	hits := h.sinkHits.Load()
	outs := h.tick(t, wf, false)
	if outs["plan"]["step_back"] != true || !strings.Contains(fmt.Sprint(outs["plan"]["summary"]), "session active") {
		t.Fatalf("the watch tick must step back while a session is open: %v", outs["plan"]["summary"])
	}
	if _, polled := outs["poll_loki"]; polled {
		t.Fatalf("the watch tick polled during a session")
	}
	if h.sinkHits.Load() != hits {
		t.Fatalf("the watch tick posted during a session: the escalation went out twice")
	}
	// The window is over but the envelope still open: the closing tick may
	// still be running its budget — the watch waits for it.
	h.editSession(t, func(env map[string]any) {
		env["ends_at"] = time.Now().UTC().Add(-11 * time.Minute).Format(time.RFC3339)
	})
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	if outs = h.tick(t, wf, false); outs["plan"]["step_back"] != true {
		t.Fatalf("the watch must wait out the closing tick's budget while the envelope is open: %v", outs["plan"]["summary"])
	}
	// Past that budget (and the closing tick never came): the watch resumes.
	h.editSession(t, func(env map[string]any) {
		env["ends_at"] = time.Now().UTC().Add(-23 * time.Minute).Format(time.RFC3339)
	})
	outs = h.tick(t, wf, false)
	if outs["plan"]["step_back"] == true {
		t.Fatalf("the watch tick must resume once the window is over: %v", outs["plan"]["summary"])
	}
	if got := pwCount(h.bodies(), "metric threshold breached"); got != 2 {
		t.Fatalf("the watch lifecycle must see the breach as its own news after the window, got %d escalation(s)", got)
	}
}

// TestProdWatch_TheSessionCapHoldsTheRest: two signals under a cap of one —
// one escalation, one "cap reached" note, the second signal HELD (not posted,
// not re-fired as overflow, named in the summary).
func TestProdWatch_TheSessionCapHoldsTheRest(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	probes := []map[string]any{
		{"id": "restarts", "title": "restarts", "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"},
		{"id": "oom", "title": "oom", "query": "oom-q", "op": ">", "threshold": 0, "severity": "high"},
	}
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, probes)
	// The per-run cap at one: a signal past the session cap is HELD, never
	// deferred to re-fire as overflow (the session cap runs before the cut).
	h.alertCap.Store(1)
	before := int(h.sinkHits.Load())
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}, "oom-q": {Value: "2"}, "rps-q": {Value: "1"}})
	capped := map[string]any{"max_analyses_per_session": 1}
	outs := h.modeTick(t, wf, "session", capped)
	if outs["decide"]["held_count"] != float64(1) || outs["decide"]["overflow_count"] != float64(0) {
		t.Fatalf("the cap must hold one of the two signals and defer none, held_count=%v overflow=%v", outs["decide"]["held_count"], outs["decide"]["overflow_count"])
	}
	bodies := h.bodies()[before:]
	if got := pwCount(bodies, "metric threshold breached"); got != 1 {
		t.Fatalf("the cap must let exactly one escalation out, got %d:\n%s", got, strings.Join(bodies, "\n---\n"))
	}
	if got := pwCount(bodies, "ANALYSIS CAP"); got != 1 {
		t.Fatalf("the cap must be said once, got %d:\n%s", got, strings.Join(bodies, "\n---\n"))
	}
	hits := h.sinkHits.Load()
	outs = h.modeTick(t, wf, "session", capped)
	if outs["decide"]["held_count"] != float64(1) || outs["decide"]["overflow_count"] != float64(0) {
		t.Fatalf("the held signal must stay held (not overflow) every tick: held=%v overflow=%v", outs["decide"]["held_count"], outs["decide"]["overflow_count"])
	}
	if h.sinkHits.Load() != hits {
		t.Fatalf("a held signal posted on a later tick")
	}
	pwSessionConfig(t, h, []string{pwNotToday()}, probes)
	h.modeTick(t, wf, "session", capped)
	summary := h.bodies()[len(h.bodies())-1]
	if !strings.Contains(summary, "SESSION SUMMARY") || !strings.Contains(summary, "`1` held by the analysis cap (`1`)") {
		t.Fatalf("the summary must name the held signal:\n%s", summary)
	}
}

// TestProdWatch_AWatchCommitSurvivesTheSessionsRotation: once the session's
// ledger rotated, `git ls-files -- <dir>` recursing into session/ handed the
// rotation's basename to the watch's `git add` as a path its own dir never
// had — every watch commit died after posting. The watch commit stages its
// own files and leaves the session's alone.
func TestProdWatch_AWatchCommitSurvivesTheSessionsRotation(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string { return gittest.Run(t, h.ws, args...) }
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	sd := filepath.Join(h.ws, ".prod-watch", "session")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(sd, "ticks-2.jsonl"), []byte("{\"at\":\"x\"}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(sd, "ticks.jsonl"), []byte(""), 0o644)
	git("add", ".prod-watch/session/ticks-2.jsonl", ".prod-watch/session/ticks.jsonl")
	git("commit", "-q", "-m", "a session rotation")
	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":1,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, []byte(""), 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","generation":1}`), 0o644)
	out, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": 0, "state_commit": true,
		"workspace": h.ws, "state_dir": ".prod-watch", "ledger": pwLedgerOff()}, nil, nil), gittest.Env())
	if err != nil || out["committed"] != true {
		t.Fatalf("the watch tick must commit beside a session rotation: %v %s %v", out, stderr, err)
	}
	shown := git("show", "--stat", "--format=", "HEAD")
	if strings.Contains(shown, "session/") {
		t.Fatalf("the watch commit carried the session's files:\n%s", shown)
	}
	for _, want := range []string{".prod-watch/state.json", ".prod-watch/ticks.jsonl"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("the watch commit lacks %s:\n%s", want, shown)
		}
	}
}

// TestProdWatch_EverySessionForksTheWatchState: the second window's first
// tick starts from the watch's CURRENT state — what the watch posted between
// the windows is not news and spends no cap — and its summary counts its own
// ticks only.
func TestProdWatch_EverySessionForksTheWatchState(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	h.modeTick(t, wf, "session", nil)
	h.modeTick(t, wf, "session", nil)
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	outs := h.modeTick(t, wf, "session", nil)
	if sess, _ := outs["plan"]["session"].(map[string]any); sess["closing"] != true {
		t.Fatalf("the tick after the window must close the first session: %v", outs["plan"]["summary"])
	}
	// Between the windows the watch runs MORE ticks than the session did in
	// its window; at the last of them it admits a log template and posts a
	// breach: the template's record is stamped with a generation past the
	// session copy's.
	for i := 0; i < 4; i++ {
		if outs = h.tick(t, wf, false); outs["plan"]["step_back"] == true {
			t.Fatalf("the watch must resume after the window: %v", outs["plan"]["summary"])
		}
	}
	// Stamped a few seconds back: the watch reads forward from its cursor.
	h.lines.Store([]pwLine{{TS: nsAgo(5 * time.Second), Line: "ERROR boom between the windows", Container: "api", Q: "errors-q"}})
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	before := int(h.sinkHits.Load())
	outs = h.tick(t, wf, false)
	// Two posts: the breach, and the new log pattern.
	if outs["plan"]["step_back"] == true || int(h.sinkHits.Load()) != before+2 {
		t.Fatalf("the watch must resume after the window and post the breach and the pattern: step_back=%v hits=%d", outs["plan"]["step_back"], h.sinkHits.Load())
	}
	// A second window of the day (another id): its first tick forks the
	// watch state the breach is already in.
	pwSessionWindowConfig(t, h, []string{pwToday()}, pwRestartsProbe, -60, 120)
	outs = h.modeTick(t, wf, "session", nil)
	sess, _ := outs["plan"]["session"].(map[string]any)
	if sess["new"] != true || sess["closing"] == true || outs["plan"]["state_seed_file"] == "" {
		t.Fatalf("the second window's first tick must open a new session on a fork of the watch state: %v", outs["plan"]["summary"])
	}
	bodies := h.bodies()[before+2:]
	if pwCount(bodies, "metric threshold breached") != 0 || pwCount(bodies, "new error pattern") != 0 || pwCount(bodies, "SESSION START") != 1 {
		t.Fatalf("the breach the watch posted must not be escalated again by the new session:\n%s", strings.Join(bodies, "\n---\n"))
	}
	// The second tick of the new window reads the fork plan validates: a
	// fork written under the replaced copy's generation would carry records
	// stamped past itself and die here.
	outs = h.modeTick(t, wf, "session", nil)
	if sess, _ := outs["plan"]["session"].(map[string]any); sess["new"] == true || outs["plan"]["step_back"] == true {
		t.Fatalf("the second tick of the new window must continue the session: %v", outs["plan"]["summary"])
	}
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	h.modeTick(t, wf, "session", nil)
	summary := h.bodies()[len(h.bodies())-1]
	for _, want := range []string{"SESSION SUMMARY", "`3` tick(s)", "`0` escalation(s)"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("the second session's summary must count its own three ticks and no escalation (lacks %q):\n%s", want, summary)
		}
	}
}

// TestProdWatch_AWatchTickStepsBackInsideADeclaredWindow: at a window's
// first slot both crons fire together and the envelope does not exist yet —
// the watch steps back on the DECLARED window, so a new signal posts once.
func TestProdWatch_AWatchTickStepsBackInsideADeclaredWindow(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	outs := h.tick(t, wf, false)
	if outs["plan"]["step_back"] != true || !strings.Contains(fmt.Sprint(outs["plan"]["summary"]), "session window") {
		t.Fatalf("the watch tick must step back inside a declared window, envelope or not: %v", outs["plan"]["summary"])
	}
	if h.sinkHits.Load() != 0 {
		t.Fatalf("the watch tick posted inside a declared window")
	}
}

// TestProdWatch_DigestCoversTheSessionsTicks: the session's ticks cover the
// slots the watch stepped back from, its escalation is counted, and a lane
// the session polled is not silent — the digest reads both sets of files.
func TestProdWatch_DigestCoversTheSessionsTicks(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"tick_interval_minutes": 30} })
	// The watch: every slot but the newest four (a two-hour window it stepped back from).
	h.writeTicks(t, 48, 30*time.Minute, func(i int, rec map[string]any) {
		if i < 4 {
			rec["at"] = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339) // outside the window
		}
	})
	to := pwGridTo(30 * time.Minute)
	var sessionTicks []map[string]any
	for i := 0; i < 8; i++ {
		sessionTicks = append(sessionTicks, pwTickRecord(to.Add(-5*time.Minute-time.Duration(i)*15*time.Minute), i+1))
	}
	pwWriteLedger(t, h, ".prod-watch/session/ticks.jsonl", sessionTicks)
	pwWriteLedger(t, h, ".prod-watch/session/alertlog.jsonl", []map[string]any{{
		"fp": "prom:restarts", "title": "restarts", "count": 1, "at": to.Add(-time.Hour).Format(time.RFC3339),
		"kind": "prom", "severity": "critical", "state": "new", "release": "abc1234"}})
	stale := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	h.setState(t, map[string]any{"version": 1, "generation": 44, "cursors": map[string]any{"loki": map[string]any{}},
		"incidents": map[string]any{}, "health": map[string]any{"probes": map[string]any{"last_ok": stale}}})
	fresh := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	b, _ := json.Marshal(map[string]any{"version": 1, "generation": 50, "cursors": map[string]any{"loki": map[string]any{}},
		"incidents": map[string]any{}, "health": map[string]any{"probes": map[string]any{"last_ok": fresh}}})
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.modeTick(t, wf, "digest", nil)
	body := h.bodies()[0]
	for _, want := range []string{"`48` tick(s) executed of `48` expected", "`1` alert(s) posted: critical `1`", "`52` tick record(s)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"MISSING", "lane silent", "no alert posted"} {
		if strings.Contains(body, bad) {
			t.Fatalf("the digest says %q although the session observed the window:\n%s", bad, body)
		}
	}
}

// TestProdWatch_DigestCountsSlotsNotRecords: ten manual runs in one slot do
// not stand in for a five-hour outage — executed counts covered slots, and
// the records are said beside them.
func TestProdWatch_DigestCountsSlotsNotRecords(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"tick_interval_minutes": 30} })
	to := pwGridTo(30 * time.Minute)
	var recs []map[string]any
	for i := 0; i < 38; i++ {
		recs = append(recs, pwTickRecord(to.Add(-15*time.Minute-time.Duration(i)*30*time.Minute), i+1))
	}
	for k := 0; k < 10; k++ {
		recs = append(recs, pwTickRecord(to.Add(-time.Duration(1+k)*time.Minute), 100+k))
	}
	pwWriteLedger(t, h, ".prod-watch/ticks.jsonl", recs)
	h.setState(t, pwEmptyState(48))
	h.modeTick(t, wf, "digest", nil)
	body := h.bodies()[0]
	for _, want := range []string{"`10` tick(s) MISSING: `38` executed of `48` expected", "`48` tick record(s)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
}

// TestProdWatch_DigestWeighsFoldedNotes: a note's line stands for its
// members, a template's count is its incident's at the last post (never a
// sum of cumulatives), a folded note is no pattern of its own.
func TestProdWatch_DigestWeighsFoldedNotes(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["digest"] = map[string]any{"tick_interval_minutes": 30}
		cfg["sentry"] = map[string]any{"base_url": "https://sentry.example", "org": "o", "project": "p", "environment": "preprod"}
	})
	h.writeTicks(t, 48, 30*time.Minute, nil)
	at := pwGridTo(30 * time.Minute).Add(-2 * time.Hour).Format(time.RFC3339)
	var lines []map[string]any
	for i := 0; i < 5; i++ {
		lines = append(lines, map[string]any{"fp": fmt.Sprintf("sentry:%d", i), "title": fmt.Sprintf("PROJ-%d", i), "count": 1, "at": at,
			"kind": "sentry", "severity": "high", "state": "new", "release": ""})
	}
	lines = append(lines, map[string]any{"fp": "folded:sentry:new:high::1", "title": "PROJ-5, PROJ-6", "count": 40, "folded": 40, "at": at,
		"kind": "sentry", "severity": "high", "state": "new", "release": ""})
	lines = append(lines,
		map[string]any{"fp": "loki:aaa", "title": "OldTimeout <*>", "count": 100000, "at": at, "kind": "loki", "severity": "high", "state": "reminder", "release": ""},
		map[string]any{"fp": "loki:bbb", "title": "DbPool <*>", "count": 10, "at": at, "kind": "loki", "severity": "medium", "state": "new", "release": ""},
		map[string]any{"fp": "loki:bbb", "title": "DbPool <*>", "count": 500, "at": at, "kind": "loki", "severity": "high", "state": "escalated", "release": ""},
		map[string]any{"fp": "folded:loki:new:medium::1", "title": "tplD, tplE", "count": 1600, "folded": 2, "at": at, "kind": "loki", "severity": "medium", "state": "new", "release": ""})
	pwWriteLedger(t, h, ".prod-watch/alertlog.jsonl", lines)
	h.setState(t, pwEmptyState(48))
	h.modeTick(t, wf, "digest", nil)
	body := h.bodies()[0]
	for _, want := range []string{"`50` alert(s) posted", "by lane: `loki 5, sentry 45`", "Sentry: `45` new", "`OldTimeout <*> ×100000, DbPool <*> ×500`", "top error patterns (`2`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "×510") || strings.Contains(body, "tplD") {
		t.Fatalf("the digest sums cumulatives or names a note as a pattern:\n%s", body)
	}
}

// TestProdWatch_DigestSaysWhatItCouldNotRead: a ledger line that does not
// parse — text, an array, a record without a readable stamp, a byte that is
// not UTF-8 — is counted and said, never read as a missing tick nor the
// digest's death; a session left open past its window is said as such.
func TestProdWatch_DigestSaysWhatItCouldNotRead(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"tick_interval_minutes": 30} })
	h.writeTicks(t, 48, 30*time.Minute, nil)
	path := filepath.Join(h.ws, ".prod-watch", "ticks.jsonl")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := "<<<<<<< HEAD\n[1,2]\n{\"at\":\"yesterday\"}\n\xc3{\"at\":\"x\"}\n"
	if err := os.WriteFile(path, append([]byte(bad), good...), 0o644); err != nil {
		t.Fatal(err)
	}
	h.setState(t, pwEmptyState(48))
	old := time.Now().UTC().Add(-72 * time.Hour)
	b, _ := json.Marshal(map[string]any{"version": 1, "id": "2026-10-26T13:00-19:00@UTC", "started_at": old.Format(time.RFC3339),
		"ends_at": old.Add(6 * time.Hour).Format(time.RFC3339), "ends_local": "19:00 (UTC)", "ticks": 3, "escalations": 0, "held": 0,
		"heartbeats": 0, "closed": false})
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch", "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.modeTick(t, wf, "digest", nil)
	body := h.bodies()[0]
	for _, want := range []string{"`4` ledger line(s) unreadable", "`48` tick(s) executed of `48` expected", "a session was left open: `2026-10-26T13:00-19:00@UTC`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "MISSING") || strings.Contains(body, "a session is open") {
		t.Fatalf("the digest reads an unreadable line as a missing tick, or a stale session as open:\n%s", body)
	}
}

// TestProdWatch_NotifyBoundsTheBudget: the budget has a ceiling on every
// path — the webhook's post limit — and a post is sized as the server will
// STORE it: Mattermost rewrites every `<A|B>` one character longer before it
// splits a post at that limit.
func TestProdWatch_NotifyBoundsTheBudget(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, stderr)
	}
	notify := func(max int, reports []any) (map[string]any, string, error) {
		return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
			"alerts": []any{}, "overflow_count": 0, "stale_sources": []any{}, "reports": reports,
			"sinks": plan["sinks"], "labels": plan["labels"], "app": plan["app"], "sentry": map[string]any{},
			"release": "", "release_known": false, "dry_run": false, "max_message_chars": max, "deliver_by": pwDeliverBy(),
			"persist_state": false, "session_next_file": "",
		}, vars, secrets))
	}
	if _, nerr, err := notify(20000, []any{}); err == nil || !strings.Contains(nerr, "at most 16383") {
		t.Fatalf("max_message_chars over the webhook's limit must be refused by name: %v %s", err, lastN(nerr, 200))
	}
	links := strings.Repeat("<a|b>", 40)
	lines := []map[string]any{}
	for i := 0; i < 30; i++ {
		lines = append(lines, map[string]any{"label": "digest_range", "fields": map[string]any{"title": links, "value": i, "unit": "req", "agg": "sum", "points": 1}})
	}
	report := map[string]any{"marker": "digest", "icon": ":bar_chart:", "severity": "low", "title_key": "digest_title", "lines": lines}
	out, nerr, err := notify(1500, []any{report})
	if err != nil || out["delivered"] != float64(1) {
		t.Fatalf("the report must be delivered once: %v %s %v", out, lastN(nerr, 200), err)
	}
	stored := h.bodies()[0]
	if n := utf8.RuneCountInString(stored); n > 1500 {
		t.Fatalf("as Mattermost stores it the post runs %d characters, over the 1500 budget:\n…%s", n, stored[len(stored)-120:])
	}
	if !strings.Contains(stored, "[b](a)") || !strings.HasSuffix(stored, "run `harness`") {
		t.Fatalf("the stored post must carry the rewritten links and end with its run:\n%s", stored)
	}
}

// TestProdWatch_SessionRefusalsAndEdges: a digest window without a tick
// slot is refused by name, and a session tick with nothing to poll steps
// back whatever the halt says.
func TestProdWatch_SessionRefusalsAndEdges(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"hours": 1, "tick_interval_minutes": 90} })
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets)); err == nil || !strings.Contains(stderr, "exceeds the window") {
		t.Fatalf("a digest window without a tick slot must be refused by name: %v %s", err, stderr)
	}
	h.writeConfig(t, nil)
	over := map[string]any{}
	for k, v := range vars {
		over[k] = v
	}
	over["max_message_chars"] = 20000
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, over, secrets)); err == nil || !strings.Contains(stderr, "at most 16383") {
		t.Fatalf("a budget over the webhook's limit must be refused at the entry, before any poll: %v %s", err, stderr)
	}
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch", "session", "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "session", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}, secrets)); err == nil || !strings.Contains(stderr, "is a directory -- remove it") {
		t.Fatalf("a directory where the session copy belongs must be refused by name: %v %s", err, stderr)
	}
	if err := os.Remove(filepath.Join(h.ws, ".prod-watch", "session", "state.json")); err != nil {
		t.Fatal(err)
	}
	h.setState(t, map[string]any{"version": 1, "generation": 2, "cursors": map[string]any{"loki": map[string]any{}}, "incidents": map[string]any{},
		"health": map[string]any{}, "halt": map[string]any{"since": "2026-10-28T10:00:00+00:00", "reason": "leak"}})
	outs := h.modeTick(t, wf, "session", nil)
	if outs["plan"]["halted"] == true || outs["plan"]["step_back"] != true {
		t.Fatalf("a session tick outside its window steps back, halt or not: %v", outs["plan"]["summary"])
	}
	outs = h.tick(t, wf, false)
	if outs["plan"]["halted"] != true {
		t.Fatalf("the watch tick must still refuse under a halt: %v", outs["plan"]["summary"])
	}
}

// TestProdWatch_ASecondHalfContinuesTheFirstsFork: two windows back to back
// — the documented overnight pair — with no watch tick between them: the
// second half continues the first's fork (the watch has not moved), so what
// the first escalated is not escalated again and spends no cap.
func TestProdWatch_ASecondHalfContinuesTheFirstsFork(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionWindowConfig(t, h, []string{pwToday()}, pwRestartsProbe, -120, 60)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	h.modeTick(t, wf, "session", nil)
	if got := pwCount(h.bodies(), "metric threshold breached"); got != 1 {
		t.Fatalf("the first half must escalate the breach once, got %d", got)
	}
	before := len(h.bodies())
	pwSessionWindowConfig(t, h, []string{pwToday()}, pwRestartsProbe, -60, 120)
	outs := h.modeTick(t, wf, "session", nil)
	sess, _ := outs["plan"]["session"].(map[string]any)
	if sess["new"] != true || sess["closing"] != true || outs["plan"]["state_seed_file"] != "" {
		t.Fatalf("the second half must close the first and continue its fork, not re-fork a watch that has not moved: new=%v closing=%v seed=%q", sess["new"], sess["closing"], outs["plan"]["state_seed_file"])
	}
	bodies := h.bodies()[before:]
	if pwCount(bodies, "metric threshold breached") != 0 || pwCount(bodies, "SESSION SUMMARY") != 1 || pwCount(bodies, "SESSION START") != 1 {
		t.Fatalf("the second half re-escalated the first's breach, or lacks its summary/start:\n%s", strings.Join(bodies, "\n---\n"))
	}
}

// TestProdWatch_AnUnreadableSessionCopyDoesNotStopTheWatch: the session's
// state file this mode does not own is read for what it says; one that does
// not parse is absent, said, and the watch tick posts its alert.
func TestProdWatch_AnUnreadableSessionCopyDoesNotStopTheWatch(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch", "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	outs := h.tick(t, wf, false)
	if _, ran := outs["commit_state"]; !ran || pwCount(h.bodies(), "metric threshold breached") != 1 {
		t.Fatalf("the watch tick must post its alert beside an unreadable session copy (stderr: %s)", h.stderrs["decide"])
	}
	if !strings.Contains(h.stderrs["decide"], "-- read as absent") {
		t.Fatalf("the unreadable copy must be said: %s", h.stderrs["decide"])
	}
}

// TestProdWatch_LastTakesTheLatestFinitePoint: a NaN at the last step of a
// range (a 0/0 ratio) is no latest value — `last` reads the latest finite
// point, and refuses several series at it.
func TestProdWatch_LastTakesTheLatestFinitePoint(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["digest"] = map[string]any{"tick_interval_minutes": 30, "prometheus": map[string]any{
			"ranges": []map[string]any{{"id": "ratio", "title": "ratio", "query": "ratio-q", "agg": "last", "unit": ""}}}}
	})
	h.promRanges.Store(map[string][]float64{"ratio-q": {0.25, 0.5, math.NaN()}})
	h.writeTicks(t, 48, 30*time.Minute, nil)
	h.setState(t, pwEmptyState(48))
	outs := h.modeTick(t, wf, "digest", nil)
	r := outs["poll_prom"]["ranges"].([]any)[0].(map[string]any)
	if r["state"] != "observed" || r["value"] != float64(0.5) {
		t.Fatalf("last must be the latest finite point: %v", r)
	}
	if !strings.Contains(h.bodies()[0], "`ratio`: `0.5`") {
		t.Fatalf("the digest lacks the latest finite point:\n%s", h.bodies()[0])
	}
}

// TestProdWatch_RotationsNeverSortBelowTheirElders: a rotation named from a
// generation that went backwards (a session copy re-forked from an older
// watch generation) passes every existing rotation of its stem, so `keep`
// prunes the elders, never the new rows.
func TestProdWatch_RotationsNeverSortBelowTheirElders(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out := ledgerRun(t, wf, h, 3, map[string]any{"max_bytes": 100, "keep": 8}, "", "{\"at\":\"x\"}",
		map[string]string{"ticks.jsonl": bigLines(3), "ticks-9.jsonl": "old\n"})
	if !mustExist(t, sdPath(h, "ticks-10.jsonl")) || mustExist(t, sdPath(h, "ticks-3.jsonl")) {
		t.Fatalf("the rotation must pass its elders (ticks-9 → ticks-10), got %v", out["rotations"])
	}
}

// TestProdWatch_TheWatchHearsTheSessionsPolls: a lane the session polled
// while the watch stepped back is not silent — the first watch tick after
// the window reads the session state's last success and posts no false
// "source silent".
func TestProdWatch_TheWatchHearsTheSessionsPolls(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	h.modeTick(t, wf, "session", nil)
	// The window is over and closed; the watch's own health is eight hours
	// old (it stepped back that long) and its Loki poll fails now.
	h.editSession(t, func(env map[string]any) { env["closed"] = true })
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	st := h.state(t)
	old := time.Now().UTC().Add(-8 * time.Hour).Format(time.RFC3339)
	for _, hrec := range st["health"].(map[string]any) {
		hrec.(map[string]any)["last_ok"] = old
	}
	h.setState(t, st)
	h.failLokiFrom.Store(1)
	h.failLokiCount.Store(100)
	before := len(h.bodies())
	outs := h.tick(t, wf, false)
	if outs["plan"]["step_back"] == true {
		t.Fatalf("the watch must resume after a closed window: %v", outs["plan"]["summary"])
	}
	for _, b := range h.bodies()[before:] {
		if strings.Contains(b, "source silent") {
			t.Fatalf("the watch called a lane silent that the session polled minutes ago:\n%s", b)
		}
	}
}

// TestProdWatch_AForkReplacesAnyCopy: a session copy that is no readable
// JSON object — text, an array — is replaced by the fork at a window's first
// tick, in plan, decide AND commit_state's guard alike: the tick commits once
// and the next tick continues it, so START is said once.
func TestProdWatch_AForkReplacesAnyCopy(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	dir := filepath.Join(h.ws, ".prod-watch", "session")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, copy := range []string{"{not json", "[1]", `{"generation":"abc"}`} {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(copy), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(dir, "session.json"))
		before := len(h.bodies())
		outs := h.modeTick(t, wf, "session", nil)
		if _, ran := outs["commit_state"]; !ran || outs["plan"]["state_seed_file"] == "" {
			t.Fatalf("copy %d (%q): the window's first tick must fork over it and commit: seed=%q", i, copy, outs["plan"]["state_seed_file"])
		}
		h.modeTick(t, wf, "session", nil)
		if got := pwCount(h.bodies()[before:], "SESSION START"); got != 1 {
			t.Fatalf("copy %d (%q): START said %d time(s), want once:\n%s", i, copy, got, strings.Join(h.bodies()[before:], "\n---\n"))
		}
		pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
		h.modeTick(t, wf, "session", nil)
		pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	}
}

// TestProdWatch_LastRefusesSeveralSeries: an instant `last` over several
// series has no value Prometheus orders — refused by name, never a series
// picked by its position in the answer.
func TestProdWatch_LastRefusesSeveralSeries(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["session"] = map[string]any{
			"active_windows": []map[string]any{{"from": "00:00", "to": "24:00", "tz": "UTC"}},
			"heartbeat":      map[string]any{"metrics": []map[string]any{{"id": "rps", "title": "requests/s", "query": "rps-q", "agg": "last"}}},
		}
	})
	two := map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []map[string]any{
		{"metric": map[string]string{"pod": "a"}, "value": []any{float64(time.Now().Unix()), "5"}},
		{"metric": map[string]string{"pod": "b"}, "value": []any{float64(time.Now().Unix()), "7"}},
	}}}
	h.prom.Store(map[string]pwProm{"rps-q": {Body: two}})
	outs := h.modeTick(t, wf, "session", nil)
	m := outs["poll_prom"]["metrics"].([]any)[0].(map[string]any)
	if m["state"] != "error" || !strings.Contains(fmt.Sprint(m["error"]), "needs one series") {
		t.Fatalf("last over two instant series must be refused by name: %v", m)
	}
}

// TestProdWatch_ARewrittenWatchFileKeepsTheFork: between two windows back
// to back the watch file is rewritten — an editor's round trip, a halt
// armed then lifted — without the watch moving: its bytes change, its
// content does not, and the second half still continues the first's fork.
func TestProdWatch_ARewrittenWatchFileKeepsTheFork(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionWindowConfig(t, h, []string{pwToday()}, pwRestartsProbe, -120, 60)
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "3"}})
	h.modeTick(t, wf, "session", nil)
	// The operator's round trip: the same state, spelled otherwise, a halt
	// armed and lifted on the way.
	st := h.state(t)
	st["halt"] = nil
	b, err := json.MarshalIndent(st, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(h.bodies())
	pwSessionWindowConfig(t, h, []string{pwToday()}, pwRestartsProbe, -60, 120)
	outs := h.modeTick(t, wf, "session", nil)
	if outs["plan"]["state_seed_file"] != "" || pwCount(h.bodies()[before:], "metric threshold breached") != 0 {
		t.Fatalf("a rewritten watch file with the same content must not re-fork: seed=%q\n%s", outs["plan"]["state_seed_file"], strings.Join(h.bodies()[before:], "\n---\n"))
	}
}

// TestProdWatch_AFutureSiblingWarningIsNoWarning: a sibling stamp from a
// clock running ahead is none — it would win every comparison and read as
// "not said" on every tick. The watch warns once, then stays quiet.
func TestProdWatch_AFutureSiblingWarningIsNoWarning(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	old := time.Now().UTC().Add(-8 * time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
	st := h.state(t)
	for _, hrec := range st["health"].(map[string]any) {
		hrec.(map[string]any)["last_ok"] = old
	}
	h.setState(t, st)
	sb, _ := json.Marshal(map[string]any{"version": 1, "generation": 3, "cursors": map[string]any{"loki": map[string]any{}},
		"incidents": map[string]any{}, "health": map[string]any{"loki": map[string]any{"last_ok": old, "last_warned": future}}})
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch", "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "state.json"), sb, 0o644); err != nil {
		t.Fatal(err)
	}
	h.failLokiFrom.Store(1)
	h.failLokiCount.Store(1000)
	h.tick(t, wf, false)
	h.tick(t, wf, false)
	if got := pwCount(h.bodies(), "source silent"); got != 1 {
		t.Fatalf("a silent lane is warned once, whatever a future sibling stamp says; got %d:\n%s", got, strings.Join(h.bodies(), "\n---\n"))
	}
}

// TestProdWatch_TheWatchWaitsTheSessionTicksBudget: past the window the
// watch waits the closing tick's grace plus the SESSION tick's budget — the
// one the envelope recorded — not its own.
func TestProdWatch_TheWatchWaitsTheSessionTicksBudget(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.tick(t, wf, false)
	pwSessionConfig(t, h, []string{pwToday()}, pwRestartsProbe)
	outs := h.modeTick(t, wf, "session", nil)
	if sess, _ := outs["plan"]["session"].(map[string]any); sess["tick_secs"] != float64(720) {
		t.Fatalf("the session plan must carry the tick's budget (720 by default): %v", sess["tick_secs"])
	}
	if h.sessionFile(t)["tick_secs"] != float64(720) {
		t.Fatalf("the envelope must record the tick's budget: %v", h.sessionFile(t))
	}
	pwSessionConfig(t, h, []string{pwNotToday()}, pwRestartsProbe)
	for _, c := range []struct {
		secs   float64
		resume bool
	}{{60, true}, {1200, false}} {
		h.editSession(t, func(env map[string]any) {
			env["tick_secs"] = c.secs
			env["ends_at"] = time.Now().UTC().Add(-12 * time.Minute).Format(time.RFC3339)
		})
		outs = h.tick(t, wf, false)
		if (outs["plan"]["step_back"] != true) != c.resume {
			t.Fatalf("tick_secs %v at twelve minutes past the window: resume=%v, got step_back=%v (%v)", c.secs, c.resume, outs["plan"]["step_back"], outs["plan"]["summary"])
		}
	}
}

// TestProdWatch_DigestSaysNothingWhileTheClosingTickMayRun: between the
// closing tick's grace and the end of its budget the session is neither
// "open" nor "left open" to the digest — a closing tick it admitted may still
// be running.
func TestProdWatch_DigestSaysNothingWhileTheClosingTickMayRun(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) { cfg["digest"] = map[string]any{"tick_interval_minutes": 30} })
	h.writeTicks(t, 48, 30*time.Minute, nil)
	h.setState(t, pwEmptyState(48))
	b, _ := json.Marshal(map[string]any{"version": 1, "id": "w", "started_at": time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		"ends_at": time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339), "ends_local": "x", "ticks": 3, "escalations": 0,
		"held": 0, "heartbeats": 0, "closed": false, "tick_secs": 720})
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch", "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "session", "session.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.modeTick(t, wf, "digest", nil)
	body := h.bodies()[0]
	if strings.Contains(body, "a session is open") || strings.Contains(body, "left open") {
		t.Fatalf("the digest must say nothing of a session whose closing tick may still run:\n%s", body)
	}
}

// TestProdWatch_AWindowMayEndAtMidnight: `to: "24:00"` is a window end the
// doc promises — the whole day, at any hour — and the session's envelope
// says so; a window written backwards is refused by name, and the refusal
// names the form that is accepted.
func TestProdWatch_AWindowMayEndAtMidnight(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	window := func(from, to string) {
		h.writeConfig(t, func(cfg map[string]any) {
			cfg["session"] = map[string]any{"active_windows": []map[string]any{{"from": from, "to": to, "tz": "UTC"}}}
		})
	}
	window("00:00", "24:00")
	outs := h.modeTick(t, wf, "session", nil)
	sess, _ := outs["plan"]["session"].(map[string]any)
	if sess["new"] != true || !strings.Contains(fmt.Sprint(sess["ends_local"]), "24:00") {
		t.Fatalf("a 00:00-24:00 window must open a session at any hour and say it ends at 24:00: %v", outs["plan"]["summary"])
	}
	ends, err := time.Parse(time.RFC3339, fmt.Sprint(sess["ends_at"]))
	if err != nil || !ends.Equal(time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour)) {
		t.Fatalf("the window must end at the next midnight UTC, got %v (%v)", sess["ends_at"], err)
	}
	window("18:00", "09:00")
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "fetch_timeout_secs": 20, "ingest_lag_seconds": 0, "max_lines": 5000}
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
	if err == nil || !strings.Contains(stderr, "an overnight window is two windows") {
		t.Fatalf("a window written backwards must be refused by name, in every mode: %v %s", err, stderr)
	}
	window("09:00", "25:00")
	_, stderr, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
	if err == nil || !strings.Contains(stderr, "`to` may be 24:00") {
		t.Fatalf("an hour past 24 must be refused naming the accepted form: %v %s", err, stderr)
	}
}

// TestProdWatch_TheModesAreRoutedByPlan: the compiled wiring — plan's four
// exclusive booleans route the modes, the quoted expressions split the
// chain on vars.mode, notify's persist gates the state commit.
func TestProdWatch_TheModesAreRoutedByPlan(t *testing.T) {
	wf := compileFixture(t, "prod-watch/main.bot")
	find := func(from, to string) *ir.Edge {
		for _, e := range wf.Edges {
			if e.From == from && e.To == to {
				return e
			}
		}
		t.Fatalf("no edge %s -> %s", from, to)
		return nil
	}
	for _, c := range []struct {
		from, to, cond string
		negated        bool
	}{
		{"plan", "watch_halted", "halted", false}, {"plan", "done", "step_back", false}, {"plan", "poll_prom", "collect", false},
		{"notify", "done", "persist", true}, {"notify", "commit_state", "persist", false},
	} {
		e := find(c.from, c.to)
		if e.Condition != c.cond || e.Negated != c.negated {
			t.Fatalf("%s -> %s: condition %q negated %v, want %q %v", c.from, c.to, e.Condition, e.Negated, c.cond, c.negated)
		}
	}
	for _, c := range [][2]string{{"poll_prom", "digest"}, {"decide", "session"}} {
		if find(c[0], c[1]).Expression == nil {
			t.Fatalf("%s -> %s must be guarded by a vars.mode expression", c[0], c[1])
		}
	}
	// The first matching edge wins; the chain's next node is the fallback.
	for _, c := range [][2]string{{"plan", "resolve_release"}, {"poll_prom", "poll_sentry"}, {"decide", "notify"}} {
		if !find(c[0], c[1]).IsElse {
			t.Fatalf("%s -> %s must be the else fallback", c[0], c[1])
		}
	}
	for _, id := range []string{"session", "digest"} {
		pwTool(t, wf, id)
	}
}
