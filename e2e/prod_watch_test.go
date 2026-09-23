package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// pwTool extracts a tool node from the compiled prod-watch fixture.
func pwTool(t *testing.T, wf *ir.Workflow, id string) *ir.ToolNode {
	t.Helper()
	node, ok := wf.Nodes[id]
	if !ok {
		t.Fatalf("workflow missing %s node", id)
	}
	tool, ok := node.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is not a ToolNode (got %T)", id, node)
	}
	return tool
}

// pwSub replaces {{input.K}} / {{vars.K}} refs with JSON literals (the
// engine's injection contract for script nodes) and {{secrets.N.path}}
// with the mounted path, failing on any leftover ref.
func pwSub(t *testing.T, script string, inputs, vars map[string]any, secrets map[string]string) string {
	t.Helper()
	for k, v := range inputs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal input %s: %v", k, err)
		}
		script = strings.ReplaceAll(script, "{{input."+k+"}}", string(b))
	}
	for k, v := range vars {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal var %s: %v", k, err)
		}
		script = strings.ReplaceAll(script, "{{vars."+k+"}}", string(b))
	}
	for name, path := range secrets {
		b, _ := json.Marshal(path)
		script = strings.ReplaceAll(script, "{{secrets."+name+".path}}", string(b))
	}
	if i := strings.Index(script, "{{"); i >= 0 {
		end := i + 60
		if end > len(script) {
			end = len(script)
		}
		t.Fatalf("unsubstituted template ref in script: %s", script[i:end])
	}
	return script
}

// runPyWhole runs a node script under python3 and parses the WHOLE stdout
// as one JSON object — the engine's contract (parseToolNodeOutput), not
// the last-line shortcut: a stray print before the object is a real bug
// here, exactly as it is in a run.
func runPyWhole(t *testing.T, dir, script string) (map[string]any, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	path := filepath.Join(t.TempDir(), "tool.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	c := exec.Command("python3", path)
	c.Dir = dir
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()
	out := map[string]any{}
	if s := strings.TrimSpace(stdout.String()); s != "" {
		if err := json.Unmarshal([]byte(s), &out); err != nil && runErr == nil {
			t.Fatalf("stdout is not ONE json object (the engine would wrap it as {result: …}):\n%s", s)
		}
	}
	return out, stderr.String(), runErr
}

// runPyEnv is runPyWhole with extra environment entries (KEY=value).
func runPyEnv(t *testing.T, dir, script string, env []string) (map[string]any, string, error) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	path := filepath.Join(t.TempDir(), "tool.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	c := exec.Command("python3", path)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()
	out := map[string]any{}
	if s := strings.TrimSpace(stdout.String()); s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out, stderr.String(), runErr
}

type pwLine struct {
	TS        int64
	Line      string
	Container string
	Q         string // the LogQL text this line answers to
}

type pwProm struct {
	Status   int
	Value    string
	NoData   bool
	Warnings []string
}

// pwHarness is one hermetic deployment: a workspace with a config, a fake
// Grafana (datasource proxy for Loki + Prometheus behind a bearer token),
// a fake health endpoint and two webhook sinks (one healthy, one down).
type pwHarness struct {
	ws, scratch, tokenFile, webhooksFile string
	lines                                atomic.Value // []pwLine
	prom                                 atomic.Value // map[string]pwProm
	healthStatus                         atomic.Int64
	srv                                  *httptest.Server
	callMu                               sync.Mutex
	lokiCalls                            []url.Values
	sinkMu                               sync.Mutex
	sinkBodies                           []string
	sinkHits                             atomic.Int64
}

const pwToken = "glsa_test_token_0123456789"

func newPWHarness(t *testing.T) *pwHarness {
	t.Helper()
	h := &pwHarness{ws: t.TempDir(), scratch: t.TempDir()}
	h.lines.Store([]pwLine{})
	h.prom.Store(map[string]pwProm{})
	h.healthStatus.Store(200)
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+pwToken {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/datasources/proxy/uid/loki/loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query()
		h.callMu.Lock()
		h.lokiCalls = append(h.lokiCalls, q)
		h.callMu.Unlock()
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		var sel []pwLine
		for _, l := range h.lines.Load().([]pwLine) {
			// Loki: start inclusive, end exclusive.
			if l.Q == q.Get("query") && l.TS >= start && l.TS < end {
				sel = append(sel, l)
			}
		}
		sort.Slice(sel, func(i, j int) bool { return sel[i].TS < sel[j].TS })
		if q.Get("direction") != "forward" {
			// The bot must ask forward; a backward walk would silently
			// serve the NEWEST lines and let the cursor jump the oldest.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(sel) > limit {
			sel = sel[:limit]
		}
		streams := map[string][][]string{}
		for _, l := range sel {
			streams[l.Container] = append(streams[l.Container], []string{strconv.FormatInt(l.TS, 10), l.Line})
		}
		var result []map[string]any
		for c, vals := range streams {
			result = append(result, map[string]any{"stream": map[string]string{"container": c, "namespace": "ns", "pod": c + "-0"}, "values": vals})
		}
		if result == nil {
			result = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": result}})
	})
	mux.HandleFunc("/api/datasources/proxy/uid/prom/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		p, ok := h.prom.Load().(map[string]pwProm)[r.URL.Query().Get("query")]
		if !ok {
			p = pwProm{NoData: true}
		}
		if p.Status != 0 {
			w.WriteHeader(p.Status)
			return
		}
		result := []map[string]any{}
		if !p.NoData {
			result = append(result, map[string]any{"metric": map[string]string{"job": "x"}, "value": []any{float64(time.Now().Unix()), p.Value}})
		}
		resp := map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}}
		if len(p.Warnings) > 0 {
			resp["warnings"] = p.Warnings
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(h.healthStatus.Load()))
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "abc1234"})
	})
	var flaky atomic.Int64
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		// First call answers 503, every later call 200: a blip, not an outage.
		if flaky.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.sinkHits.Add(1)
		h.sinkMu.Lock()
		h.sinkBodies = append(h.sinkBodies, body.Text)
		h.sinkMu.Unlock()
	})
	mux.HandleFunc("/hook-down", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	h.tokenFile = filepath.Join(h.scratch, "grafana_token")
	if err := os.WriteFile(h.tokenFile, []byte(pwToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.webhooksFile = filepath.Join(h.scratch, "webhooks.json")
	if err := os.WriteFile(h.webhooksFile, []byte(`{"w1": "`+h.srv.URL+`/hook", "w2": "`+h.srv.URL+`/hook-down"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeConfig(t, nil)
	return h
}

// writeConfig writes the default config, then lets a test reshape it.
func (h *pwHarness) writeConfig(t *testing.T, mod func(cfg map[string]any)) {
	t.Helper()
	cfg := map[string]any{
		"app":     map[string]any{"name": "demo", "environment": "preprod"},
		"release": map[string]any{"source": "health_field", "health_url": h.srv.URL + "/health", "field": "version"},
		"grafana": map[string]any{"base_url": h.srv.URL, "loki_uid": "loki", "prometheus_uid": "prom"},
		"loki": map[string]any{
			"queries":                  map[string]any{"errors": "errors-q", "leak_sweep": "sweep-q"},
			"overlap_seconds":          60,
			"bootstrap_window_minutes": 10,
			"page_size":                1000,
		},
		"prometheus": map[string]any{"probes": []map[string]any{
			{"id": "restarts", "title": "restarts", "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"},
		}},
		"probes": []map[string]any{{"id": "api", "url": h.srv.URL + "/health", "expect_status": 200, "severity": "critical"}},
		"sinks":  []map[string]any{{"webhook": "w1", "channel": "#ops", "min_severity": "low"}},
	}
	if mod != nil {
		mod(cfg)
	}
	b, _ := json.MarshalIndent(cfg, "", " ")
	if err := os.WriteFile(filepath.Join(h.ws, "prod-watch.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *pwHarness) bodies() []string {
	h.sinkMu.Lock()
	defer h.sinkMu.Unlock()
	return append([]string(nil), h.sinkBodies...)
}

func (h *pwHarness) calls() []url.Values {
	h.callMu.Lock()
	defer h.callMu.Unlock()
	return append([]url.Values(nil), h.lokiCalls...)
}

func nsAgo(d time.Duration) int64 { return time.Now().Add(-d).UnixNano() }

// tick drives the whole watch graph by hand, node by node, in the order
// the workflow declares. A node added to the graph has to be added HERE
// too or it silently never runs. Returns every node's output.
func (h *pwHarness) tick(t *testing.T, wf *ir.Workflow, dryRun bool) map[string]map[string]any {
	t.Helper()
	outs := map[string]map[string]any{}
	vars := map[string]any{
		"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000,
	}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	run := func(id string, inputs map[string]any) map[string]any {
		t.Helper()
		out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, id).Script, inputs, vars, secrets))
		if err != nil {
			t.Fatalf("%s failed: %v\nstderr: %s", id, err, stderr)
		}
		outs[id] = out
		return out
	}
	plan := run("plan", nil)
	if plan["halted"] == true {
		return outs
	}
	rel := run("resolve_release", map[string]any{"release": plan["release"], "timeout_secs": 5, "allow_private": true})
	loki := run("poll_loki", map[string]any{"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5,
		"scratch_dir": h.scratch, "allow_private": true})
	prom := run("poll_prom", map[string]any{"grafana": plan["grafana"], "prometheus": plan["prometheus"], "timeout_secs": 5, "allow_private": true})
	probe := run("probe_http", map[string]any{"probes": plan["probes"], "timeout_secs": 5, "allow_private": true})
	leak := run("leak_scan", map[string]any{"raw_file": loki["raw_file"], "per_query": loki["per_query"], "app": plan["app"], "scratch_dir": h.scratch})
	decide := run("decide", map[string]any{
		"signals_file": leak["signals_file"], "prom_results": prom["results"], "http_results": probe["results"],
		"loki_ok": loki["ok"], "loki_truncated": loki["truncated"], "loki_errors": loki["errors"], "loki_per_query": loki["per_query"],
		"prom_ok": prom["ok"], "prom_errors": prom["errors"], "release": rel["release"], "release_known": rel["release_known"],
		"lanes": plan["lanes"], "app": plan["app"], "workspace": h.ws, "state_dir": ".prod-watch", "scratch_dir": h.scratch,
		"renotify_hours": 24, "quiet_after_hours": 48, "forget_after_days": 14, "source_stale_hours": 6, "max_alerts": 20,
	})
	notify := run("notify", map[string]any{
		"alerts": decide["alerts"], "overflow_count": decide["overflow_count"], "stale_sources": decide["stale_sources"],
		"sinks": plan["sinks"], "labels": plan["labels"], "app": plan["app"], "dry_run": dryRun, "max_message_chars": 14000,
	})
	if notify["consume"] == true {
		run("commit_state", map[string]any{"state_next_file": decide["state_next_file"], "alertlog_file": decide["alertlog_file"],
			"tick_file": decide["tick_file"], "state_commit": false, "workspace": h.ws, "state_dir": ".prod-watch"})
	}
	return outs
}

func (h *pwHarness) state(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.ws, ".prod-watch", "state.json"))
	if err != nil {
		t.Fatalf("state.json: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("state.json: %v", err)
	}
	return s
}

// alertsOf returns the (kind, state) pairs decide posted this tick.
func alertsOf(t *testing.T, outs map[string]map[string]any) []string {
	t.Helper()
	var got []string
	for _, a := range outs["decide"]["alerts"].([]any) {
		m := a.(map[string]any)
		got = append(got, fmt.Sprint(m["kind"], ":", m["state"], ":", m["severity"]))
	}
	sort.Strings(got)
	return got
}

// TestProdWatch_TickLifecycle: the bootstrap tick observes the log
// templates without posting them but DOES post a leak and any present-state
// failure; a template recurring after the bootstrap posts as NEW; a steady
// tick posts nothing and still consumes; a failing probe posts critical;
// the cursor advances to the window's end; no raw value ever leaves the
// scan.
func TestProdWatch_TickLifecycle(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	const rawEmail = "jean.dupont@example.org"
	h.lines.Store([]pwLine{
		{TS: nsAgo(3 * time.Minute), Line: "ERROR worker: job 4711 failed: TimeoutError after 30s", Container: "worker", Q: "errors-q"},
		{TS: nsAgo(2 * time.Minute), Line: "INFO api: session opened for " + rawEmail, Container: "api", Q: "sweep-q"},
	})
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}})

	// ── Tick 1: bootstrap ──
	outs := h.tick(t, wf, false)
	if outs["decide"]["bootstrap"] != true {
		t.Fatalf("first tick must be a bootstrap: %v", outs["decide"]["summary"])
	}
	if got := alertsOf(t, outs); strings.Join(got, ",") != "leak:new:high" {
		t.Fatalf("bootstrap alerts = %v, want only the leak (templates are observed, probe + metric healthy)", got)
	}
	if h.sinkHits.Load() != 1 {
		t.Fatalf("bootstrap must deliver exactly the leak alert, sink got %d", h.sinkHits.Load())
	}
	msg := h.bodies()[0]
	if strings.Contains(msg, rawEmail) || strings.Contains(msg, "jean.dupont") {
		t.Fatalf("the raw email reached the channel:\n%s", msg)
	}
	if !strings.Contains(msg, "je***") {
		t.Fatalf("the masked sample is missing from the leak message:\n%s", msg)
	}
	st := h.state(t)
	inc := st["incidents"].(map[string]any)
	if len(inc) != 2 {
		t.Fatalf("state must carry the observed template AND the leak incident, got %d: %v", len(inc), inc)
	}
	cur := st["cursors"].(map[string]any)["loki"].(map[string]any)["errors"].(map[string]any)
	wantTo := outs["poll_loki"]["per_query"].(map[string]any)["errors"].(map[string]any)["to_ns"]
	if cur["covered_to_ns"] != wantTo {
		t.Fatalf("cursor must stop at the window's end: got %v want %v", cur["covered_to_ns"], wantTo)
	}
	// The raw file is where the raw line must live — and ONLY there: the
	// scan read it from here, so an empty file would mean it saw nothing.
	if b, _ := os.ReadFile(filepath.Join(h.scratch, "loki_raw.jsonl")); !strings.Contains(string(b), rawEmail) {
		t.Fatal("the raw file must hold the raw line the scan redacted")
	}

	// ── Tick 2: the template recurs (a NEW line after the cursor) → NEW ──
	time.Sleep(20 * time.Millisecond)
	h.lines.Store(append(h.lines.Load().([]pwLine),
		pwLine{TS: time.Now().UnixNano() - 5*int64(time.Millisecond), Line: "ERROR worker: job 4712 failed: TimeoutError after 30s", Container: "worker", Q: "errors-q"}))
	time.Sleep(20 * time.Millisecond)
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); strings.Join(got, ",") != "loki:new:medium" {
		t.Fatalf("tick 2 alerts = %v, want the recurring template as NEW (the leak is inside its renotify window)", got)
	}
	if h.sinkHits.Load() != 2 {
		t.Fatalf("tick 2 must deliver one message, sink got %d", h.sinkHits.Load())
	}
	if m := h.bodies()[1]; !strings.Contains(m, "job # failed") || !strings.Contains(m, "worker") {
		t.Fatalf("template message must carry the fingerprinted template and the container:\n%s", m)
	}

	// ── Tick 3: steady state — nothing new, nothing posts, state consumed ──
	before := h.sinkHits.Load()
	outs = h.tick(t, wf, false)
	if len(alertsOf(t, outs)) != 0 || h.sinkHits.Load() != before {
		t.Fatalf("steady tick must be silent: %v (sink %d→%d)", outs["decide"]["summary"], before, h.sinkHits.Load())
	}
	if outs["notify"]["consume"] != true {
		t.Fatalf("a nothing-to-deliver tick still consumes: %v", outs["notify"])
	}

	// ── Tick 4: the probe fails → critical, present-state ──
	h.healthStatus.Store(503)
	outs = h.tick(t, wf, false)
	if got := alertsOf(t, outs); strings.Join(got, ",") != "probe:new:critical" {
		t.Fatalf("tick 4 alerts = %v, want the failing probe", got)
	}
	if m := h.bodies()[len(h.bodies())-1]; !strings.Contains(m, "503") || !strings.Contains(m, "CRITICAL") {
		t.Fatalf("probe message must name the status and the severity:\n%s", m)
	}

	// ── Raw values never leave the scan: not in any node output, state file or message ──
	for node, out := range outs {
		if b, _ := json.Marshal(out); strings.Contains(string(b), rawEmail) {
			t.Fatalf("raw email in the %s node output", node)
		}
	}
	for _, f := range []string{"state.json", "alertlog.jsonl", "ticks.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(h.ws, ".prod-watch", f))
		if strings.Contains(string(b), rawEmail) {
			t.Fatalf("raw email persisted in %s", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h.scratch, "signals.json")); strings.Contains(string(b), rawEmail) {
		t.Fatal("raw email in the derived signals file")
	}
}

// TestProdWatch_LokiWindowPagingAndTruncation pins the cursor contract: the
// walk is forward and paged, the cap TRUNCATES the window (cursor at the
// last line fetched, coverage partial), the next tick resumes there and
// the overlap's already-counted lines are skipped.
func TestProdWatch_LokiWindowPagingAndTruncation(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"].(map[string]any)["page_size"] = 4
		cfg["loki"].(map[string]any)["overlap_seconds"] = 600
		cfg["loki"].(map[string]any)["queries"] = map[string]any{"errors": "errors-q"}
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
		cfg["probes"] = []map[string]any{}
	})
	var lines []pwLine
	base := nsAgo(8 * time.Minute)
	for i := 0; i < 25; i++ {
		lines = append(lines, pwLine{TS: base + int64(i)*int64(time.Second), Line: fmt.Sprintf("ERROR api: request %d failed", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)

	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 10}
	secrets := map[string]string{"grafana_token": h.tokenFile, "webhooks": h.webhooksFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, stderr)
	}
	loki, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{
		"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki: %v\n%s", err, stderr)
	}
	if loki["truncated"] != true || loki["lines"].(float64) != 10 {
		t.Fatalf("10-line cap over 25 lines must truncate at 10: %v", loki)
	}
	pq := loki["per_query"].(map[string]any)["errors"].(map[string]any)
	if pq["covered_to_ns"] != strconv.FormatInt(base+9*int64(time.Second), 10) {
		t.Fatalf("truncated cursor must stop at the 10th line, got %v", pq["covered_to_ns"])
	}
	calls := h.calls()
	if len(calls) < 3 {
		t.Fatalf("a 4-line page over 10 lines needs 3 pages, got %d calls", len(calls))
	}
	prev := int64(-1)
	for _, c := range calls {
		if c.Get("direction") != "forward" {
			t.Fatalf("walk must be forward: %v", c)
		}
		s, _ := strconv.ParseInt(c.Get("start"), 10, 64)
		if s <= prev {
			t.Fatalf("pages must advance: start %d after %d", s, prev)
		}
		prev = s
	}
	leak, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": loki["raw_file"], "per_query": loki["per_query"], "app": plan["app"], "scratch_dir": h.scratch}, nil, nil))
	if err != nil || leak["coverage"] != "partial" {
		t.Fatalf("a truncated window is PARTIAL coverage: %v %v", leak, err)
	}
	// Persist the cursor as decide would, then tick again: the walk resumes
	// from the cursor − overlap and the overlap's lines are not re-counted.
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateJSON := fmt.Sprintf(`{"version":1,"generation":1,"cursors":{"loki":{"errors":{"covered_to_ns":"%s","overlap_hashes":%s}}},"incidents":{},"health":{}}`,
		pq["covered_to_ns"], mustJSON(t, pq["overlap_hashes"]))
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	vars["max_lines"] = 5000
	plan, _, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatal(err)
	}
	loki2, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{
		"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki 2: %v\n%s", err, stderr)
	}
	pq2 := loki2["per_query"].(map[string]any)["errors"].(map[string]any)
	if loki2["truncated"] == true || loki2["lines"].(float64) != 15 {
		t.Fatalf("second tick must fetch exactly the 15 remaining lines (overlap lines skipped): %v", loki2)
	}
	if pq2["skipped_overlap"].(float64) != 10 {
		t.Fatalf("the 10 already-counted overlap lines must be skipped, got %v", pq2["skipped_overlap"])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestProdWatch_PromResultTyping: an empty vector is no_data (not healthy),
// a breach is an incident at the probe's severity, a 5xx is a lane error
// that is reported but not fatal, and a tick where every probe errored
// hard-fails.
func TestProdWatch_PromResultTyping(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{
			{"id": "restarts", "title": "restarts", "query": "restarts-q", "op": ">", "threshold": 0, "severity": "high"},
			{"id": "ratio", "title": "5xx ratio", "query": "ratio-q", "op": ">", "threshold": 0.02, "severity": "critical"},
			{"id": "ghost", "title": "ghost metric", "query": "ghost-q", "op": ">", "threshold": 1, "severity": "low"},
			{"id": "broken", "title": "broken", "query": "broken-q", "op": ">", "threshold": 1, "severity": "low"},
		}}
	})
	h.prom.Store(map[string]pwProm{"restarts-q": {Value: "0"}, "ratio-q": {Value: "0.05", Warnings: []string{"partial"}}, "ghost-q": {NoData: true}, "broken-q": {Status: 500}})
	outs := h.tick(t, wf, false)
	states := map[string]string{}
	for _, r := range outs["poll_prom"]["results"].([]any) {
		m := r.(map[string]any)
		states[m["id"].(string)] = m["state"].(string)
	}
	want := map[string]string{"restarts": "healthy", "ratio": "breached", "ghost": "no_data", "broken": "error"}
	for id, w := range want {
		if states[id] != w {
			t.Fatalf("probe %s state = %q, want %q (all: %v)", id, states[id], w, states)
		}
	}
	if outs["poll_prom"]["ok"] != false {
		t.Fatal("a failed probe must flip the lane's ok to false (reported, not fatal)")
	}
	got := alertsOf(t, outs)
	if strings.Join(got, ",") != "prom:new:critical,prom_no_data:new:medium" {
		t.Fatalf("alerts = %v, want the breach (critical) and the no_data incident (medium)", got)
	}
	// Every probe erroring is a façade, not a quiet tick.
	h.prom.Store(map[string]pwProm{"restarts-q": {Status: 500}, "ratio-q": {Status: 500}, "ghost-q": {Status: 500}, "broken-q": {Status: 500}})
	plan := outs["plan"]
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_prom").Script, map[string]any{
		"grafana": plan["grafana"], "prometheus": plan["prometheus"], "timeout_secs": 5, "allow_private": true}, nil,
		map[string]string{"grafana_token": h.tokenFile}))
	if err == nil || !strings.Contains(stderr, "every Prometheus probe failed") {
		t.Fatalf("all-probes-failed must hard-fail naming the cause, got err=%v stderr=%s", err, stderr)
	}
	// A refused token is a credential problem, actionable now: hard failure.
	bad := filepath.Join(h.scratch, "bad_token")
	_ = os.WriteFile(bad, []byte("glsa_wrong"), 0o600)
	_, stderr, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_prom").Script, map[string]any{
		"grafana": plan["grafana"], "prometheus": plan["prometheus"], "timeout_secs": 5, "allow_private": true}, nil,
		map[string]string{"grafana_token": bad}))
	if err == nil || !strings.Contains(stderr, "refused the token") {
		t.Fatalf("401 must hard-fail naming the token, got err=%v stderr=%s", err, stderr)
	}
}

// TestProdWatch_LeakScanValidators drives the scan alone over a raw file:
// each class fires on a valid value and NOT on the invalid twin, the
// samples are masked, the sweep query yields no template, and the
// template fingerprint ignores the numbers.
func TestProdWatch_LeakScanValidators(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	raw := filepath.Join(h.scratch, "raw.jsonl")
	lines := []string{
		`user nir=1 85 03 75 123 456 41 checked`,                              // valid key
		`user nir=1 85 03 75 123 456 42 checked`,                              // invalid key
		`iban FR76 3000 6000 0112 3456 7890 189 saved`,                        // valid
		`iban FR76 3000 6000 0112 3456 7890 180 saved`,                        // invalid
		`card 4539 1488 0343 6467 charged`,                                    // Luhn ok
		`card 4539 1488 0343 6468 charged`,                                    // Luhn ko
		`ts=1727086800000 request done`,                                       // an epoch, not a card
		`iban fr7630006000011234567890189 saved`,                              // lowercase IBAN (valid) — F1
		`{"iban":"nl91abna0417164300","status":"ko"}`,                         // lowercase, inside a JSON value — F1 (never phone_fr)
		`{"level":"error","ts":1727086800123456789,"msg":"upstream timeout"}`, // 19-digit epoch ns that passes Luhn? never a card — F2
		`{"level":"error","ts":1727086800123456,"msg":"x"}`,                   // 16-digit epoch us — F2
		`ref 4539148803436467 done`,                                           // bare Luhn-valid run, no card word — not a card — F2
		`card=4539148803436467 charged`,                                       // bare run WITH a card word — a card — F2
		`password=[REDACTED:secret_kv]hunter2SuperSecret injected marker`,     // a literal marker must not disarm the scrubber — F7
		`auth token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnopqrstuvwxyz used`,
		`Authorization: Bearer sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345 sent`,
		`db password=SuperSecret123 connected`,
		`mail to marie.curie@example.fr phone 06 12 34 56 78`,
	}
	var buf strings.Builder
	for i, l := range lines {
		rec := map[string]any{"q": "errors", "ts": strconv.FormatInt(nsAgo(time.Minute)+int64(i), 10), "line": l, "stream": map[string]string{"container": "api"}}
		b, _ := json.Marshal(rec)
		buf.Write(b)
		buf.WriteString("\n")
	}
	// The sweep query feeds the scan but produces no template.
	rec := map[string]any{"q": "leak_sweep", "ts": strconv.FormatInt(nsAgo(time.Minute), 10), "line": "INFO all good for pierre@example.fr", "stream": map[string]string{"container": "api"}}
	b, _ := json.Marshal(rec)
	buf.Write(b)
	buf.WriteString("\n")
	if err := os.WriteFile(raw, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": raw, "per_query": map[string]any{"errors": map[string]any{}, "leak_sweep": map[string]any{}},
		"app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil {
		t.Fatalf("leak_scan: %v\n%s", err, stderr)
	}
	counts := map[string]float64{}
	samples := map[string]string{}
	for _, f := range out["leak_findings"].([]any) {
		m := f.(map[string]any)
		counts[m["class"].(string)] = m["count"].(float64)
		samples[m["class"].(string)] = m["sample_masked"].(string)
	}
	want := map[string]float64{"nir": 1, "iban": 3, "card": 2, "jwt": 1, "bearer": 1, "secret_kv": 2, "email": 2, "phone_fr": 1}
	for cls, n := range want {
		if counts[cls] != n {
			t.Fatalf("class %s count = %v, want %v (all: %v)", cls, counts[cls], n, counts)
		}
	}
	if counts["cloud_or_forge_token"] != 0 {
		// The bearer rule consumed the Anthropic key first; the class must
		// not be double-counted.
		t.Fatalf("token double-counted: %v", counts)
	}
	if samples["nir"] != "nir:***41" || samples["card"] != "card:***67" || !strings.HasPrefix(samples["email"], "ma") || strings.Contains(samples["email"], "curie") {
		t.Fatalf("masks leak the value: %v", samples)
	}
	// Two pairs collapse into one template each once redacted — the two
	// JSON epoch lines (`{"...":"...","...":#,"...":"..."}`) and the two
	// IBAN lines (`iban [REDACTED:iban] saved`) — and the sweep line adds
	// none: 18 lines, 16 templates.
	if out["templates"].(float64) != float64(len(lines)-2) {
		t.Fatalf("templates = %v, want %d (two redaction collapses, no template for the sweep line)", out["templates"], len(lines)-2)
	}
	sig, _ := os.ReadFile(out["signals_file"].(string))
	for _, raw := range []string{"1 85 03 75 123 456 41", "3456 7890 189", "4539 1488 0343 6467", "SuperSecret123", "marie.curie", "pierre@", "eyJhbGciOiJIUzI1NiJ9",
		"fr7630006000011234567890189", "nl91abna0417164300", "hunter2SuperSecret", "4539148803436467"} {
		if strings.Contains(string(sig), raw) {
			t.Fatalf("raw value %q reached the derived signals", raw)
		}
	}
	if !strings.Contains(string(sig), "[REDACTED:nir]") || !strings.Contains(string(sig), "[REDACTED:secret_kv]") {
		t.Fatal("redaction markers missing from the samples")
	}
	// Template fingerprints ignore numbers: two lines differing only by
	// their ids share one template.
	raw2 := filepath.Join(h.scratch, "raw2.jsonl")
	var buf2 strings.Builder
	for i, l := range []string{"ERROR job 41 failed after 30s", "ERROR job 4711 failed after 12s"} {
		rec := map[string]any{"q": "errors", "ts": strconv.FormatInt(nsAgo(time.Minute)+int64(i), 10), "line": l, "stream": map[string]string{"container": "w"}}
		b, _ := json.Marshal(rec)
		buf2.Write(b)
		buf2.WriteString("\n")
	}
	_ = os.WriteFile(raw2, []byte(buf2.String()), 0o644)
	out, _, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": raw2, "per_query": map[string]any{}, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil || out["templates"].(float64) != 1 {
		t.Fatalf("numbers must not split a template: %v %v", out, err)
	}
}

// TestProdWatch_DeliverySemantics drives notify alone: a REQUIRED sink down
// fails the tick without consuming; an optional one down still consumes;
// alerts with nowhere to go fail loudly; a dry-run posts nothing and
// consumes nothing; min_severity filters per sink.
func TestProdWatch_DeliverySemantics(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	labels := map[string]any{"alert": "production alert", "severity": "severity", "new_since": "first seen {date}", "count": "{n} occurrence(s)",
		"probe_down": "health probe failing", "probe_detail": "{url} answered {status} in {ms} ms (expected {expected})", "overflow": "{n} more"}
	alerts := []map[string]any{{"fingerprint": "probe:api", "kind": "probe", "severity": "critical", "state": "new", "title_key": "probe_down",
		"title_arg": "api", "detail_key": "probe_detail", "fields": map[string]any{"url": "u", "status": 503, "ms": 12, "expected": 200},
		"evidence": map[string]any{}, "count": 1, "first_seen": "2026-09-23T10:00:00+00:00"}}
	run := func(sinks []map[string]any, dry bool) (map[string]any, string, error) {
		return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "notify").Script, map[string]any{
			"alerts": alerts, "overflow_count": 0, "stale_sources": []any{}, "sinks": sinks, "labels": labels,
			"app": map[string]any{"name": "demo", "environment": "preprod"}, "dry_run": dry, "max_message_chars": 14000},
			nil, map[string]string{"webhooks": h.webhooksFile}))
	}
	// required sink down → fails, nothing consumed
	out, stderr, err := run([]map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low", "required": true},
		{"webhook": "w2", "channel": "#b", "min_severity": "low", "required": true}}, false)
	if err == nil || !strings.Contains(stderr, "required sink(s) down: w2") {
		t.Fatalf("a required sink down must fail the tick: err=%v out=%v stderr=%s", err, out, stderr)
	}
	// optional sink down → consumed, failure reported
	out, stderr, err = run([]map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low", "required": true},
		{"webhook": "w2", "channel": "#b", "min_severity": "low", "required": false}}, false)
	if err != nil || out["consume"] != true || !strings.Contains(fmt.Sprint(out["summary"]), "FAILED: w2") {
		t.Fatalf("an optional sink down must still consume and say so: err=%v out=%v stderr=%s", err, out, stderr)
	}
	// nowhere to go → hard failure
	_, stderr, err = run([]map[string]any{}, false)
	if err == nil || !strings.Contains(stderr, "NO sinks") {
		t.Fatalf("alerts without a sink must fail loudly: %v %s", err, stderr)
	}
	// dry-run → nothing posted, nothing consumed
	before := h.sinkHits.Load()
	out, _, err = run([]map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "low"}}, true)
	if err != nil || out["consume"] != false || h.sinkHits.Load() != before {
		t.Fatalf("dry-run must post nothing and consume nothing: %v", out)
	}
	// min_severity above the alert → nothing posted, state may advance
	out, _, err = run([]map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "critical"}}, false)
	if err != nil || out["consume"] != true || out["delivered"].(float64) != 1 {
		t.Fatalf("a critical alert must reach a critical-threshold sink: %v", out)
	}
	alerts[0]["severity"] = "medium"
	out, _, err = run([]map[string]any{{"webhook": "w1", "channel": "#a", "min_severity": "critical"}}, false)
	if err != nil || out["consume"] != true || out["delivered"].(float64) != 0 {
		t.Fatalf("a medium alert must be filtered by a critical-threshold sink and still consume: %v", out)
	}
	if b := h.bodies(); len(b) == 0 || !strings.Contains(b[0], "503") || strings.Contains(b[0], "\n\n\n") {
		t.Fatalf("delivered message must carry the detail: %v", b)
	}
}

// TestProdWatch_PlanGuards: config problems fail at plan, before any
// network work; a configured Grafana without a token refuses; a halt in the
// state is reported as halted (the workflow routes it to the typed fail).
func TestProdWatch_PlanGuards(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
	plan := func(secretPath string) (map[string]any, string, error) {
		return runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, map[string]string{"grafana_token": secretPath}))
	}
	if _, stderr, err := plan(""); err == nil || !strings.Contains(stderr, "grafana_token") {
		t.Fatalf("Grafana configured without a bound token must refuse at plan: %v %s", err, stderr)
	}
	out, stderr, err := plan(h.tokenFile)
	if err != nil || out["halted"] != false {
		t.Fatalf("plan: %v %s %v", err, stderr, out)
	}
	if lanes := out["lanes"].(map[string]any); lanes["loki"] != true || lanes["prometheus"] != true || lanes["probes"] != true {
		t.Fatalf("lanes = %v", lanes)
	}
	// An operator's explicit 0 is kept, never replaced by the default.
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"].(map[string]any)["overlap_seconds"] = 0
		cfg["loki"].(map[string]any)["page_size"] = 0
	})
	out, _, err = plan(h.tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	lp := out["loki"].(map[string]any)
	if lp["windows"].(map[string]any)["errors"].(map[string]any)["overlap_ns"] != "0" || lp["page_size"].(float64) != 1 {
		t.Fatalf("explicit overlap_seconds=0 must give overlap_ns 0 and page_size 0 must floor at 1: %v", lp)
	}
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"] = map[string]any{"queries": map[string]any{}}
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
		cfg["probes"] = []map[string]any{}
	})
	if _, stderr, err := plan(h.tokenFile); err == nil || !strings.Contains(stderr, "no lane at all") {
		t.Fatalf("a config with no lane must refuse: %v %s", err, stderr)
	}
	h.writeConfig(t, nil)
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"),
		[]byte(`{"version":1,"generation":3,"cursors":{"loki":{}},"incidents":{},"health":{},"halt":{"since":"2026-09-23T10:00:00+00:00","reason":"leak"}}`), 0o644)
	out, _, err = plan(h.tokenFile)
	if err != nil || out["halted"] != true || !strings.Contains(fmt.Sprint(out["summary"]), "HALTED") {
		t.Fatalf("an armed halt must surface as halted: %v %v", out, err)
	}
	_ = os.Remove(filepath.Join(h.ws, "prod-watch.json"))
	if _, stderr, err := plan(h.tokenFile); err == nil || !strings.Contains(stderr, "not found") {
		t.Fatalf("a missing config must refuse by name: %v %s", err, stderr)
	}
}

// TestProdWatch_LokiSameNanosecondAcrossPages: lines sharing one nanosecond
// that straddle a page boundary are all fetched (the resume is inclusive
// and the re-read is deduplicated), never skipped as "beyond the page".
func TestProdWatch_LokiSameNanosecondAcrossPages(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"].(map[string]any)["page_size"] = 2
		cfg["loki"].(map[string]any)["queries"] = map[string]any{"errors": "errors-q"}
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
		cfg["probes"] = []map[string]any{}
	})
	base := nsAgo(4 * time.Minute)
	h.lines.Store([]pwLine{
		{TS: base, Line: "ERROR alpha", Container: "api", Q: "errors-q"},
		{TS: base + 100, Line: "ERROR beta", Container: "api", Q: "errors-q"},
		{TS: base + 100, Line: "ERROR gamma CRITICAL PAYMENT FAILURE", Container: "api", Q: "errors-q"},
		{TS: base + 100, Line: "ERROR delta", Container: "api", Q: "errors-q"},
		{TS: base + 200, Line: "ERROR epsilon", Container: "api", Q: "errors-q"},
	})
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}
	secrets := map[string]string{"grafana_token": h.tokenFile}
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, stderr)
	}
	loki, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{
		"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki: %v\n%s", err, stderr)
	}
	if loki["lines"].(float64) != 5 || loki["truncated"] == true {
		t.Fatalf("all 5 lines must be fetched across the same-nanosecond page boundary: %v", loki)
	}
	raw, _ := os.ReadFile(loki["raw_file"].(string))
	for _, want := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("line %q lost at the page boundary", want)
		}
	}
	if strings.Count(string(raw), "gamma") != 1 {
		t.Fatal("the inclusive re-read must be deduplicated, not written twice")
	}
}

// TestProdWatch_LokiOverlapAfterTruncation: after a truncated tick the
// overlap hashes are taken relative to the COVERED bound, so the next tick
// skips exactly the lines already counted.
func TestProdWatch_LokiOverlapAfterTruncation(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	h.writeConfig(t, func(cfg map[string]any) {
		cfg["loki"].(map[string]any)["page_size"] = 2
		cfg["loki"].(map[string]any)["overlap_seconds"] = 1
		cfg["loki"].(map[string]any)["queries"] = map[string]any{"errors": "errors-q"}
		cfg["prometheus"] = map[string]any{"probes": []map[string]any{}}
		cfg["probes"] = []map[string]any{}
	})
	base := nsAgo(5 * time.Minute)
	var lines []pwLine
	for i := 0; i < 4; i++ {
		lines = append(lines, pwLine{TS: base + int64(i)*int64(time.Second), Line: fmt.Sprintf("ERROR line %d", i), Container: "api", Q: "errors-q"})
	}
	h.lines.Store(lines)
	vars := map[string]any{"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 2}
	secrets := map[string]string{"grafana_token": h.tokenFile}
	plan, _, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatal(err)
	}
	loki, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{
		"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki: %v\n%s", err, stderr)
	}
	pq := loki["per_query"].(map[string]any)["errors"].(map[string]any)
	if loki["truncated"] != true || len(pq["overlap_hashes"].([]any)) != 2 {
		t.Fatalf("a truncated tick must still hand over the hashes of the lines it counted: %v", pq)
	}
	if err := os.MkdirAll(filepath.Join(h.ws, ".prod-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateJSON := fmt.Sprintf(`{"version":1,"generation":1,"cursors":{"loki":{"errors":{"covered_to_ns":"%s","overlap_hashes":%s}}},"incidents":{},"health":{}}`,
		pq["covered_to_ns"], mustJSON(t, pq["overlap_hashes"]))
	if err := os.WriteFile(filepath.Join(h.ws, ".prod-watch", "state.json"), []byte(stateJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	vars["max_lines"] = 5000
	plan, _, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, vars, secrets))
	if err != nil {
		t.Fatal(err)
	}
	loki2, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "poll_loki").Script, map[string]any{
		"grafana": plan["grafana"], "loki": plan["loki"], "timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true}, nil, secrets))
	if err != nil {
		t.Fatalf("poll_loki 2: %v\n%s", err, stderr)
	}
	pq2 := loki2["per_query"].(map[string]any)["errors"].(map[string]any)
	if pq2["skipped_overlap"].(float64) != 2 || loki2["lines"].(float64) != 2 {
		t.Fatalf("the two already-counted lines must be skipped and the two new ones fetched: %v", pq2)
	}
}

// TestProdWatch_LeakScanRefusesInconsistentHandoff: a missing or short raw
// file is a refusal, never a "coverage full, nothing found" tick.
func TestProdWatch_LeakScanRefusesInconsistentHandoff(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	perQuery := map[string]any{"errors": map[string]any{"lines": 3}}
	_, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": filepath.Join(h.scratch, "gone.jsonl"), "per_query": perQuery, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err == nil || !strings.Contains(stderr, "missing") {
		t.Fatalf("a missing raw file with reported lines must refuse: %v %s", err, stderr)
	}
	short := filepath.Join(h.scratch, "short.jsonl")
	rec, _ := json.Marshal(map[string]any{"q": "errors", "ts": "1", "line": "ERROR x", "stream": map[string]string{}})
	_ = os.WriteFile(short, append(rec, '\n'), 0o644)
	_, stderr, err = runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": short, "per_query": perQuery, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err == nil || !strings.Contains(stderr, "inconsistent") {
		t.Fatalf("a short raw file must refuse naming the counts: %v %s", err, stderr)
	}
	// An empty handoff (no query) scans nothing and is fine.
	empty := filepath.Join(h.scratch, "empty.jsonl")
	_ = os.WriteFile(empty, nil, 0o644)
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "leak_scan").Script, map[string]any{
		"raw_file": empty, "per_query": map[string]any{}, "app": map[string]any{"name": "demo"}, "scratch_dir": h.scratch}, nil, nil))
	if err != nil || out["coverage"] != "full" {
		t.Fatalf("an empty handoff is a legitimate empty scan: %v %s", err, stderr)
	}
}

// TestProdWatch_ProxyEnvDoesNotDisarmTheGuard: a *_PROXY variable naming
// the target host must not skip the address check (the hatch that let a
// bearer ride to a private address in strict posture).
func TestProdWatch_ProxyEnvDoesNotDisarmTheGuard(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	plan, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "plan").Script, nil, map[string]any{
		"workspace_dir": h.ws, "config_path": "prod-watch.json", "mode": "watch", "state_dir": ".prod-watch",
		"max_window_minutes": 60, "ingest_lag_seconds": 0, "max_lines": 5000}, map[string]string{"grafana_token": h.tokenFile}))
	if err != nil {
		t.Fatalf("plan: %v %s", err, stderr)
	}
	// Strict posture against the loopback fake, with a proxy variable naming
	// that same host: the guard must still refuse (before the fix it returned
	// early and the request — with the bearer — went out).
	for _, node := range []string{"poll_prom", "poll_loki"} {
		inputs := map[string]any{"grafana": plan["grafana"], "timeout_secs": 5, "allow_private": false, "scratch_dir": h.scratch}
		if node == "poll_prom" {
			inputs["prometheus"] = plan["prometheus"]
		} else {
			inputs["loki"] = plan["loki"]
		}
		_, stderr, err := runPyEnv(t, h.ws, pwSub(t, pwTool(t, wf, node).Script, inputs, nil, map[string]string{"grafana_token": h.tokenFile}),
			[]string{"HTTPS_PROXY=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9"})
		if err == nil || !(strings.Contains(stderr, "SSRF-unsafe") || strings.Contains(stderr, "must be https")) {
			t.Fatalf("%s: strict posture must refuse the loopback target even with a proxy env naming it: err=%v stderr=%s", node, err, stderr)
		}
	}
}

// TestProdWatch_ProbeRetriesOnce: a probe answering 503 once and 200 on the
// retry is OK — a dropped packet on a scheduled tick is not an outage.
func TestProdWatch_ProbeRetriesOnce(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "prod-watch/main.bot")
	h := newPWHarness(t)
	out, stderr, err := runPyWhole(t, h.ws, pwSub(t, pwTool(t, wf, "probe_http").Script, map[string]any{
		"probes":       []map[string]any{{"id": "flaky", "url": h.srv.URL + "/flaky", "expect_status": 200, "timeout_secs": 5, "severity": "critical"}},
		"timeout_secs": 5, "allow_private": true}, nil, nil))
	if err != nil {
		t.Fatalf("probe_http: %v %s", err, stderr)
	}
	r := out["results"].([]any)[0].(map[string]any)
	if r["ok"] != true || r["status"].(float64) != 200 {
		t.Fatalf("a single 503 followed by 200 must be OK after the retry: %v", r)
	}
}
