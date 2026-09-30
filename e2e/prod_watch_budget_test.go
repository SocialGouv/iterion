package e2e

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// pwStallThenTrickleServer holds its FIRST connection silent until the client
// hangs up (a socket timeout), and trickles a byte every 200 ms on every
// later one for 30 s: a retry that starts early and never finishes.
func pwStallThenTrickleServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn, first bool) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				if first {
					_ = c.SetReadDeadline(time.Now().Add(time.Minute))
					_, _ = c.Read(buf)
					return
				}
				if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\nX-Pad: ")); err != nil {
					return
				}
				for end := time.Now().Add(30 * time.Second); time.Now().Before(end); {
					time.Sleep(200 * time.Millisecond)
					if _, err := c.Write([]byte("a")); err != nil {
						return
					}
				}
			}(c, n == 0)
		}
	}()
	return "http://" + ln.Addr().String()
}

func pwPromInputs(base string, deadline, probes, timeout int) map[string]any {
	var ps []any
	for k := 0; k < probes; k++ {
		ps = append(ps, map[string]any{"id": fmt.Sprint("p", k), "query": fmt.Sprint("q", k), "op": ">", "threshold": 0,
			"severity": "high", "title": "t", "agg": "max"})
	}
	return map[string]any{"grafana": map[string]any{"base_url": base, "prometheus_uid": "prom", "deadline_secs": deadline},
		"prometheus": map[string]any{"probes": ps}, "timeout_secs": timeout, "allow_private": true}
}

// TestProdWatch_PlanRefusesFetchesOverTheRunBudget: a tick the run budget
// kills posts nothing, the health probes included — plan refuses a config
// whose fetches can wait longer than the budget keeps for them, naming each
// wait (a probe counts its retry and its host's lookup, the release endpoint
// its lookup); the defaults fit.
func TestProdWatch_PlanRefusesFetchesOverTheRunBudget(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	probes := func(n, timeout int, oneHost bool) func(cfg map[string]any) {
		return func(cfg map[string]any) {
			var ps []any
			for k := 0; k < n; k++ {
				host := fmt.Sprintf("h%d.example", k)
				if oneHost {
					host = "app.example"
				}
				ps = append(ps, map[string]any{"id": fmt.Sprint("h", k), "url": "http://" + host + fmt.Sprintf("/health%d", k), "timeout_secs": timeout})
			}
			cfg["probes"] = ps
		}
	}
	for name, c := range map[string]struct {
		edit   func(cfg map[string]any)
		fetch  int
		refuse string
	}{
		"the defaults": {func(cfg map[string]any) {}, 20, ""},
		"slow probes":  {probes(10, 40, false), 20, "probes"},
		// Counted once each they would fit (6 x 30 s on one host, looked up
		// once); tried twice, two seconds apart, they do not.
		"probes with their retry": {probes(6, 30, true), 20, "probes"},
		"long lane deadlines":     {func(cfg map[string]any) { cfg["grafana"].(map[string]any)["deadline_secs"] = 300 }, 20, "loki"},
		"a slow release endpoint": {func(cfg map[string]any) {}, 250, "release"},
		"a long Sentry deadline": {func(cfg map[string]any) {
			cfg["sentry"] = map[string]any{"base_url": "https://sentry.example", "org": "o", "project": "p", "deadline_secs": 600}
		}, 20, "sentry"},
		// Five probes on five hosts: their two tries fit (5 x 42 s), not with
		// each host's lookup (5 x 62 s).
		"probes and their lookups": {probes(5, 20, false), 20, "probes"},
		// Every lane on at its defaults, three probes: the defaults fit.
		"every lane on, three probes": {func(cfg map[string]any) {
			probes(3, 10, false)(cfg)
			cfg["sentry"] = map[string]any{"base_url": "https://sentry.example", "org": "o", "project": "p"}
		}, 20, ""},
		"no fetch timeout": {func(cfg map[string]any) {}, 0, "fetch_timeout_secs"},
	} {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPWHarness(t)
			h.writeConfig(t, c.edit)
			vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
				"max_window_minutes": 60, "fetch_timeout_secs": c.fetch, "ingest_lag_seconds": 0, "max_lines": 5000}
			_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars,
				map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}))
			refused := err != nil && (strings.Contains(stderr, "run budget") || strings.Contains(stderr, "fetch_timeout_secs must"))
			if refused != (c.refuse != "") {
				t.Fatalf("%s: refused=%v, want %v: %v %s", name, refused, c.refuse != "", err, stderr)
			}
			if refused && (!strings.Contains(stderr, c.refuse+" ") || strings.Contains(stderr, "Traceback")) {
				t.Fatalf("%s: the refusal does not name the %s wait: %s", name, c.refuse, stderr)
			}
		})
	}
}

// TestProdWatch_AGrafanaLaneStopsAtItsDeadline: a Grafana lane starts no
// request past grafana.deadline_secs, and the request running at the
// deadline stops there: a trickling Grafana costs the tick the deadline,
// not probes × six fetch timeouts.
func TestProdWatch_AGrafanaLaneStopsAtItsDeadline(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	base := pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60))
	start := time.Now()
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_prom").Script, pwPromInputs(base, 10, 3, 3), nil,
		map[string]string{"grafana_token": h.tokenFile}))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("poll_prom: %v %s", err, stderr)
	}
	if took > 14*time.Second {
		t.Fatalf("a lane with a 10 s deadline ran %v against a trickling Grafana", took)
	}
	errs := fmt.Sprint(out["errors"])
	if !strings.Contains(errs, "ExchangeTimeout") || strings.Count(errs, "grafana.deadline_secs") != 2 {
		t.Fatalf("want the first probe stopped by its clock and the two others never started: %v", out["errors"])
	}
}

// TestProdWatch_AGrafanaCallAndItsRetryShareOneClock: the retry after a
// socket timeout runs on the call's own clock (six fetch timeouts), never a
// fresh one: a first attempt that stalls at once and a retry that trickles
// cost one clock.
func TestProdWatch_AGrafanaCallAndItsRetryShareOneClock(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	base := pwStallThenTrickleServer(t)
	start := time.Now()
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_prom").Script, pwPromInputs(base, 120, 1, 1), nil,
		map[string]string{"grafana_token": h.tokenFile}))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("poll_prom: %v %s", err, stderr)
	}
	if took > 8*time.Second {
		t.Fatalf("one call, fetch timeout 1 s: its retry ran on a fresh clock (%v, want about 6 s): %v", took, out["errors"])
	}
	if !strings.Contains(fmt.Sprint(out["errors"]), "ExchangeTimeout") {
		t.Fatalf("the call did not stop on its clock: %v", out["errors"])
	}
}

func pwProbeAlerts(n int) []map[string]any {
	var alerts []map[string]any
	for k := 0; k < n; k++ {
		alerts = append(alerts, map[string]any{"fingerprint": fmt.Sprint("probe:api", k), "kind": "probe", "severity": "critical", "state": "new",
			"title_key": "probe_down", "title_arg": fmt.Sprint("api", k), "detail_key": "probe_detail",
			"fields":   map[string]any{"url": "u", "status": 503, "ms": 5, "expected": 200},
			"evidence": map[string]any{}, "count": 1, "first_seen": "2026-09-29T10:00:00+00:00"})
	}
	return alerts
}

func pwHooksFile(t *testing.T, hooks map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "webhooks.json")
	b, _ := json.Marshal(hooks)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProdWatch_ADeliveryPostHasAWallClock: a sink trickling its answer fails
// the post at its wall clock, and is not asked again this tick — its other
// posts fail at once — so a sick sink costs the tick one wall clock, not one
// per message; nothing is consumed, the tick replays.
func TestProdWatch_ADeliveryPostHasAWallClock(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	hooks := pwHooksFile(t, map[string]string{"w1": pwRawServer(t, "HTTP/1.1 200 OK\r\nX-Pad: ", strings.Repeat("a", 60)) + "/hooks/x"})
	start := time.Now()
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": pwProbeAlerts(6), "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": false, "max_message_chars": 14000, "deliver_by": pwDeliverBy()}, nil, map[string]string{"webhooks": hooks}))
	took := time.Since(start)
	if err == nil || !strings.Contains(stderr, "within its wall clock") || strings.Count(stderr, "skipped: the sink timed out") != 5 {
		t.Fatalf("a trickling sink: want its first post failed by the wall clock and the five others skipped, got %v %s", err, stderr)
	}
	if took > 25*time.Second {
		t.Fatalf("the delivery ran %v (one wall clock of 20 s, the rest skipped)", took)
	}
}

// TestProdWatch_TheDeliveryStopsAtTheBudgetsDeadline: the delivery runs until
// the time plan's budget keeps for it (deliver_by), at least a minute, then
// fails the posts left — the state commit keeps its own time after it.
func TestProdWatch_TheDeliveryStopsAtTheBudgetsDeadline(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var got atomic.Int64
	// 13 s a post against deliver_by 75 s away (above the minute's floor):
	// the sixth starts with ~10 s left, the window cuts it, the others never
	// start — five delivered, whatever the jitter (the floor alone: four).
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(13 * time.Second)
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	hooks := pwHooksFile(t, map[string]string{"w1": slow.URL + "/hooks/x"})
	start := time.Now()
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": pwProbeAlerts(8), "overflow_count": 0, "stale_sources": []any{}, "sinks": []map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}},
		"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": false, "max_message_chars": 14000, "deliver_by": time.Now().Add(75 * time.Second).Unix()}, nil, map[string]string{"webhooks": hooks}))
	took := time.Since(start)
	if err == nil || !strings.Contains(stderr, "delivery window closed") {
		t.Fatalf("eight posts of 13 s against a window of 75 s: want the last ones failed by the window, got %v %s", err, stderr)
	}
	if strings.Contains(stderr, "skipped: the sink timed out") {
		t.Fatalf("the window cut a post, and the sink was taken for timed out: %s", stderr)
	}
	if n := got.Load(); n != 5 {
		t.Fatalf("75 s of 13-second posts: want 5 delivered, got %d (took %v)", n, took)
	}
}

// TestProdWatch_TheStateCommitHasAWindow: a remote that does not answer ends
// the state commit within its window, named — never the run budget's kill.
func TestProdWatch_TheStateCommitHasAWindow(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, h.ws, args...)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	// The push hangs: its pre-push hook outlives the commit window.
	hook := filepath.Join(h.ws, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 150\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":1,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, nil, 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","alerts":0}`), 0o644)
	start := time.Now()
	_, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": 0, "state_commit": true,
		"workspace": h.ws, "state_dir": ".prod-watch"}, nil, nil), gittest.Env())
	took := time.Since(start)
	if err == nil || !strings.Contains(stderr, "state commit window") {
		t.Fatalf("a push that hangs: want the commit ended by its window, got %v %s", err, stderr)
	}
	if took > 80*time.Second {
		t.Fatalf("the state commit ran %v (its window is 90 s, a git call 60 s)", took)
	}
}

// TestProdWatch_TheStateCommitRetriesWithinItsWindow: a push the remote keeps
// refusing slowly (its pre-push hook rejects after 40 s) is retried within
// the state commit's window, never five times over.
func TestProdWatch_TheStateCommitRetriesWithinItsWindow(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	git := func(args ...string) string {
		t.Helper()
		return gittest.Run(t, h.ws, args...)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	gittest.Run(t, filepath.Dir(bare), "init", "--bare", "-q", bare)
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(h.ws, "README.md"), []byte("ops\n"), 0o644)
	git("add", "README.md")
	git("commit", "-q", "-m", "init")
	git("remote", "add", "origin", bare)
	git("push", "-q", "-u", "origin", "main")
	hook := filepath.Join(h.ws, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 40\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := filepath.Join(h.scratch, "state_next.json")
	_ = os.WriteFile(st, []byte(`{"version":1,"generation":1,"cursors":{"loki":{}},"incidents":{},"health":{}}`), 0o644)
	al := filepath.Join(h.scratch, "alertlog_delta.jsonl")
	_ = os.WriteFile(al, nil, 0o644)
	tk := filepath.Join(h.scratch, "tick.json")
	_ = os.WriteFile(tk, []byte(`{"at":"x","alerts":0}`), 0o644)
	start := time.Now()
	_, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": st, "alertlog_file": al, "tick_file": tk, "generation": 0, "state_commit": true,
		"workspace": h.ws, "state_dir": ".prod-watch"}, nil, nil), gittest.Env())
	took := time.Since(start)
	if err == nil || !strings.Contains(stderr, "state commit window") {
		t.Fatalf("a push refused after 40 s each time: want the retries ended by the window, got %v %s", err, stderr)
	}
	if took > 100*time.Second {
		t.Fatalf("the state commit ran %v (its window is 90 s)", took)
	}
}

// TestProdWatch_TheRunBudgetMatchesTheBot: plan's budget is the workflow's
// budget.max_duration; what it keeps after the delivery holds the state
// commit's window, and what it keeps between the fetches and the delivery's
// deadline holds the delivery's floor.
func TestProdWatch_TheRunBudgetMatchesTheBot(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "bots", "prod-watch", "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	num := func(re string) int {
		t.Helper()
		m := regexp.MustCompile(re).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in main.bot", re)
		}
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	minutes := num(`max_duration: "(\d+)m"`)
	budget := num(`RUN_BUDGET_SECS, AFTER_FETCH_SECS, AFTER_DELIVERY_SECS = (\d+), \d+, \d+`)
	afterFetch := num(`RUN_BUDGET_SECS, AFTER_FETCH_SECS, AFTER_DELIVERY_SECS = \d+, (\d+), \d+`)
	afterDelivery := num(`RUN_BUDGET_SECS, AFTER_FETCH_SECS, AFTER_DELIVERY_SECS = \d+, \d+, (\d+)`)
	floor := num(`deliver_until = time.monotonic\(\) \+ max\((\d+), `)
	commit := num(`COMMIT_SECS = (\d+)`)
	if budget != minutes*60 {
		t.Fatalf("plan's RUN_BUDGET_SECS %d is not the workflow's max_duration (%d min)", budget, minutes)
	}
	if afterDelivery < commit+30 {
		t.Fatalf("what the budget keeps after the delivery (%d s) does not hold the commit window (%d s) and a margin", afterDelivery, commit)
	}
	if afterFetch-afterDelivery < floor+30 {
		t.Fatalf("between the fetches and the delivery's deadline (%d s) the delivery's floor (%d s) and the leak scan do not fit",
			afterFetch-afterDelivery, floor)
	}
}

// TestProdWatch_TheRequiredSinksGoFirst: when the time left is short, the
// required sinks take every message before an optional one takes any — an
// optional sink slow but answering (15 s a post) cannot spend it.
func TestProdWatch_TheRequiredSinksGoFirst(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	var got atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fast.Close)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)
	hooks := pwHooksFile(t, map[string]string{"w1": fast.URL + "/hooks/x", "slow": slow.URL + "/hooks/y"})
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
		"alerts": pwProbeAlerts(6), "overflow_count": 0, "stale_sources": []any{},
		"sinks": []map[string]any{{"webhook": "slow", "channel": "#backup", "min_severity": "low", "required": false},
			{"webhook": "w1", "channel": "#ops", "min_severity": "low", "required": true}},
		"labels": map[string]any{}, "app": map[string]any{"name": "demo"}, "release": "", "release_known": false,
		"dry_run": false, "max_message_chars": 14000, "deliver_by": time.Now().Add(30 * time.Second).Unix()}, nil, map[string]string{"webhooks": hooks}))
	if err != nil || out["consume"] != true || got.Load() != 6 {
		t.Fatalf("a minute left, a slow optional sink listed first: want the required one to get 6 of 6 and the tick consumed, got %d, %v %v %s",
			got.Load(), out["consume"], err, lastN(stderr, 300))
	}
}
