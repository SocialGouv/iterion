package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"time"
)

// feedWatchTool extracts a tool node from the compiled feed-watch fixture.
func feedWatchTool(t *testing.T, wf *ir.Workflow, id string) *ir.ToolNode {
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

// subScript replaces {{input.K}} template refs with JSON literals (the
// engine's injection contract for script tool nodes) plus the webhooks
// secret path, and fails on any leftover ref so a renamed input cannot
// silently produce a broken script.
func subScript(t *testing.T, script string, inputs map[string]any, secretPath string) string {
	t.Helper()
	for k, v := range inputs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal input %s: %v", k, err)
		}
		script = strings.ReplaceAll(script, "{{input."+k+"}}", string(b))
	}
	b, _ := json.Marshal(secretPath)
	script = strings.ReplaceAll(script, "{{secrets.webhooks.path}}", string(b))
	if i := strings.Index(script, "{{"); i >= 0 {
		end := i + 60
		if end > len(script) {
			end = len(script)
		}
		t.Fatalf("unsubstituted template ref in script: %s", script[i:end])
	}
	return script
}

// runPy executes a substituted python tool script, returning the parsed
// JSON from the last stdout line, raw stderr, and the exit error (nil on
// success). Output-less failures still surface stderr for assertions.
func runPy(t *testing.T, dir, script string) (map[string]any, string, error) {
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
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	out := map[string]any{}
	if last != "" && strings.HasPrefix(last, "{") {
		if err := json.Unmarshal([]byte(last), &out); err != nil && runErr == nil {
			t.Fatalf("parse tool output %q: %v", last, err)
		}
	}
	return out, stderr.String(), runErr
}

const feedWatchRSS = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
<title>Demo RSS</title>
<item><title>Story A</title><link>https://example.com/a</link><guid>rss-a</guid><pubDate>Mon, 13 Jul 2026 08:00:00 GMT</pubDate><description>Alpha &lt;b&gt;story&lt;/b&gt; details</description></item>
<item><title>Story B</title><link>https://example.com/b</link><guid>rss-b</guid><pubDate>Tue, 14 Jul 2026 08:00:00 GMT</pubDate><description>Beta story</description></item>
</channel></rss>`

const feedWatchAtom = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
<title>Demo Atom</title>
<entry><title>Story A (atom mirror)</title><link rel="alternate" href="https://example.com/a"/><id>atom-a</id><updated>2026-07-14T09:00:00Z</updated><summary>Alpha again</summary></entry>
<entry><title>Story C</title><link href="https://example.com/c"/><id>atom-c</id><updated>2026-07-15T09:00:00Z</updated><summary>Gamma</summary></entry>
</feed>`

// feedWatchWorkspace builds a workspace with two local feeds (file://) —
// one RSS2, one Atom sharing one story URL with the RSS feed, so the
// cross-source URL dedup is exercised — and the matching config.
func feedWatchWorkspace(t *testing.T) (ws string, feeds []string) {
	t.Helper()
	ws = t.TempDir()
	rss := filepath.Join(ws, "rss2.xml")
	atom := filepath.Join(ws, "atom.xml")
	if err := os.WriteFile(rss, []byte(feedWatchRSS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(atom, []byte(feedWatchAtom), 0o644); err != nil {
		t.Fatal(err)
	}
	feeds = []string{"file://" + rss, "file://" + atom}
	cfg := map[string]any{
		"categories": map[string]any{
			"demo": map[string]any{
				"digest_title": "Demo Watch",
				"editorial":    "Audience: test. Language: English.",
				"feeds":        feeds,
				"sinks": []map[string]any{
					{"webhook": "w1", "channel": "#demo", "username": "Vigie Demo"},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(ws, "feed-watch.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, feeds
}

// runPlan executes the real `plan` entry command (sh -c) with the given
// mode/category against the workspace.
func runPlan(t *testing.T, wf *ir.Workflow, ws, mode, category string) map[string]any {
	t.Helper()
	cmd := feedWatchTool(t, wf, "plan").Command
	cmd = strings.ReplaceAll(cmd, "{{vars.mode}}", mode)
	cmd = strings.ReplaceAll(cmd, "{{vars.category}}", category)
	cmd = strings.ReplaceAll(cmd, "{{vars.config_path}}", "feed-watch.json")
	cmd = strings.ReplaceAll(cmd, "{{vars.workspace_dir}}", ws)
	c := exec.Command("sh", "-c", cmd)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		t.Fatalf("plan(%s,%s) failed: %v\nstderr: %s", mode, category, err, stderr.String())
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &out); err != nil {
		t.Fatalf("parse plan output: %v (%q)", err, stdout.String())
	}
	return out
}

// TestFeedWatch_ScriptsStateMachine drives the REAL python tool scripts
// (extracted from the compiled bot) through a full collect → collect
// (idempotence) → digest-load → notify → commit-state → reload cycle
// against local file:// feeds. This is the zero-LLM proof: parsing,
// cross-source dedup, queue snapshot semantics, webhook delivery
// (httptest) and archive-based semantic-dedup context all run for real.
func TestFeedWatch_ScriptsStateMachine(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	ws, _ := feedWatchWorkspace(t)
	scratch := t.TempDir()

	// plan (collect) — config resolution.
	plan := runPlan(t, wf, ws, "collect", "")
	if plan["collect"] != true || plan["digest"] != false {
		t.Fatalf("plan collect flags wrong: %v", plan)
	}
	targets := plan["targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("want 1 target category, got %v", targets)
	}

	// fetch_feeds — real stdlib parsing of RSS2 + Atom via file://.
	fetchScript := feedWatchTool(t, wf, "fetch_feeds").Script
	fetchInputs := map[string]any{
		"targets": targets, "timeout_secs": 5, "max_per_feed": 10, "scratch_dir": scratch,
		// The hermetic feeds are file:// URLs; the strict SSRF guard blocks
		// them, so this test polls in trusted-local mode (production defaults
		// to allow_private_feeds=false — see TestFeedWatch_FetchRejectsSSRF).
		"allow_private": true,
	}
	out, stderr, err := runPy(t, ws, subScript(t, fetchScript, fetchInputs, ""))
	if err != nil {
		t.Fatalf("fetch_feeds failed: %v\nstderr: %s", err, stderr)
	}
	if got := out["fetched_count"].(float64); got != 4 {
		t.Fatalf("fetched_count = %v, want 4", got)
	}
	if got := out["feeds_ok"].(float64); got != 2 {
		t.Fatalf("feeds_ok = %v, want 2 (errors: %v)", got, out["errors"])
	}

	// dedup_queue #1 — 4 fetched, 1 cross-source URL duplicate → 3 new.
	dedupScript := feedWatchTool(t, wf, "dedup_queue").Script
	dedupInputs := map[string]any{
		"entries_file": out["entries_file"], "errors": []any{},
		"workspace": ws, "state_dir": ".feed-watch", "state_commit": false,
	}
	out, stderr, err = runPy(t, ws, subScript(t, dedupScript, dedupInputs, ""))
	if err != nil {
		t.Fatalf("dedup_queue failed: %v\nstderr: %s", err, stderr)
	}
	if out["new_count"].(float64) != 3 || out["duplicate_count"].(float64) != 1 {
		t.Fatalf("dedup #1 = %v new / %v dup, want 3/1", out["new_count"], out["duplicate_count"])
	}

	// collect again — everything already seen → 0 new (idempotence).
	out, stderr, err = runPy(t, ws, subScript(t, fetchScript, fetchInputs, ""))
	if err != nil {
		t.Fatalf("fetch_feeds #2 failed: %v\nstderr: %s", err, stderr)
	}
	dedupInputs["entries_file"] = out["entries_file"]
	out, stderr, err = runPy(t, ws, subScript(t, dedupScript, dedupInputs, ""))
	if err != nil {
		t.Fatalf("dedup_queue #2 failed: %v\nstderr: %s", err, stderr)
	}
	if out["new_count"].(float64) != 0 || out["duplicate_count"].(float64) != 4 {
		t.Fatalf("dedup #2 = %v new / %v dup, want 0/4", out["new_count"], out["duplicate_count"])
	}

	// plan (digest) — category resolution.
	plan = runPlan(t, wf, ws, "digest", "demo")
	if plan["digest"] != true || plan["digest_title"] != "Demo Watch" {
		t.Fatalf("plan digest wrong: %v", plan)
	}

	// load_pending — snapshot of the 3 queued items, no digest history yet.
	loadScript := feedWatchTool(t, wf, "load_pending").Script
	loadInputs := map[string]any{
		"category": "demo", "editorial": plan["editorial"], "digest_title": plan["digest_title"],
		"sinks": plan["sinks"], "workspace": ws, "state_dir": ".feed-watch", "max_items": 150,
		"silence_alert_days": 3,
	}
	pending, stderr, err := runPy(t, ws, subScript(t, loadScript, loadInputs, ""))
	if err != nil {
		t.Fatalf("load_pending failed: %v\nstderr: %s", err, stderr)
	}
	if pending["has_items"] != true || pending["items_count"].(float64) != 3 {
		t.Fatalf("load_pending = %v", pending)
	}
	if pending["recent_topics"].(string) != "" {
		t.Fatalf("recent_topics should be empty on first digest, got %q", pending["recent_topics"])
	}
	snapshot := pending["snapshot_ids"].([]any)
	if len(snapshot) != 3 {
		t.Fatalf("snapshot_ids = %v, want 3", snapshot)
	}
	// A queue with items is never silent, whatever the digest history —
	// the guard that makes the silence alert impossible to misfire.
	if pending["silence_alert"] != false {
		t.Fatalf("a non-empty queue must never raise a silence alert: %v", pending["silence_alert"])
	}

	// notify — dry-run prepares payloads, delivers nothing.
	notifyScript := feedWatchTool(t, wf, "notify").Script
	notifyInputs := map[string]any{
		"message_markdown": "**digest**", "sinks": plan["sinks"], "dry_run": true,
		"category": "demo", "digest_title": "Demo Watch",
	}
	out, stderr, err = runPy(t, ws, subScript(t, notifyScript, notifyInputs, ""))
	if err != nil {
		t.Fatalf("notify dry-run failed: %v\nstderr: %s", err, stderr)
	}
	if out["dry_run"] != true || out["posted"] != false {
		t.Fatalf("notify dry-run = %v", out)
	}

	// notify — sink configured but no secret bound → loud failure.
	notifyInputs["dry_run"] = false
	_, stderr, err = runPy(t, ws, subScript(t, notifyScript, notifyInputs, ""))
	if err == nil {
		t.Fatal("notify without a bound webhooks secret must fail")
	}
	if !strings.Contains(stderr, "not found in the bound") {
		t.Fatalf("missing-webhook error not explicit: %s", stderr)
	}

	// notify — real POST against an httptest sink.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	secretFile := filepath.Join(t.TempDir(), "webhooks.json")
	if err := os.WriteFile(secretFile, []byte(`{"w1": "`+srv.URL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = runPy(t, ws, subScript(t, notifyScript, notifyInputs, secretFile))
	if err != nil {
		t.Fatalf("notify POST failed: %v\nstderr: %s", err, stderr)
	}
	if out["posted"] != true || out["delivered"].(float64) != 1 {
		t.Fatalf("notify = %v", out)
	}
	if gotBody["channel"] != "#demo" || gotBody["text"] != "**digest**" {
		t.Fatalf("webhook payload = %v", gotBody)
	}

	// commit_state — clears exactly the snapshot, archives the digest.
	commitScript := feedWatchTool(t, wf, "commit_state").Script
	commitInputs := map[string]any{
		"category": "demo", "snapshot_ids": snapshot, "message_markdown": "**digest**",
		"headline": "Demo headline", "state_commit": false, "workspace": ws, "state_dir": ".feed-watch",
	}
	out, stderr, err = runPy(t, ws, subScript(t, commitScript, commitInputs, ""))
	if err != nil {
		t.Fatalf("commit_state failed: %v\nstderr: %s", err, stderr)
	}
	if out["cleared"].(float64) != 3 {
		t.Fatalf("cleared = %v, want 3", out["cleared"])
	}
	mds, _ := filepath.Glob(filepath.Join(ws, ".feed-watch", "demo", "digests", "*.md"))
	if len(mds) != 1 {
		t.Fatalf("archived digests = %v, want 1", mds)
	}

	// load_pending again — empty queue, digest history now feeds the
	// semantic-dedup context.
	pending, stderr, err = runPy(t, ws, subScript(t, loadScript, loadInputs, ""))
	if err != nil {
		t.Fatalf("load_pending #2 failed: %v\nstderr: %s", err, stderr)
	}
	if pending["has_items"] != false {
		t.Fatalf("queue should be empty after commit_state: %v", pending)
	}
	if !strings.Contains(pending["recent_topics"].(string), "Demo headline") {
		t.Fatalf("recent_topics should carry the archived digest, got %q", pending["recent_topics"])
	}
	// An empty queue right after a delivered digest is an ordinary quiet
	// morning, not a silence worth announcing.
	if pending["silence_alert"] != false {
		t.Fatalf("a digest delivered moments ago must not raise a silence alert: %v", pending)
	}

	// The silence guard, end to end. A digest whose queue is empty exits
	// green and posts nothing — which is exactly how a broken collector went
	// unnoticed for five days (2026-08-13 → 18). Back-date the delivered
	// digest and the same empty queue must now speak up.
	digestsPath := filepath.Join(ws, ".feed-watch", "demo", "digests.jsonl")
	raw, err := os.ReadFile(digestsPath)
	if err != nil {
		t.Fatal(err)
	}
	var archived map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &archived); err != nil {
		t.Fatalf("digests.jsonl is not one JSON object per line: %v", err)
	}
	archived["ts"] = time.Now().UTC().AddDate(0, 0, -6).Format("2006-01-02T15:04:05-07:00")
	staleRow, _ := json.Marshal(archived)
	if err := os.WriteFile(digestsPath, append(staleRow, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, stderr, err = runPy(t, ws, subScript(t, loadScript, loadInputs, ""))
	if err != nil {
		t.Fatalf("load_pending #3 failed: %v\nstderr: %s", err, stderr)
	}
	if pending["silence_alert"] != true {
		t.Fatalf("6 days without a delivered digest must raise the alert: %v", pending)
	}
	if got := pending["silence_days"].(float64); got < 5 {
		t.Fatalf("silence_days = %v, want ~6", got)
	}

	// …once per window, not once per run: a daily category would otherwise
	// shout every morning, which trains the reader to ignore it.
	stamp := filepath.Join(ws, ".feed-watch", "demo", "silence.json")
	if err := os.WriteFile(stamp, []byte(`{"last_alert_ts": "`+
		time.Now().UTC().Format("2006-01-02T15:04:05")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pending, stderr, err = runPy(t, ws, subScript(t, loadScript, loadInputs, ""))
	if err != nil {
		t.Fatalf("load_pending #4 failed: %v\nstderr: %s", err, stderr)
	}
	if pending["silence_alert"] != false {
		t.Fatalf("a fresh silence.json must suppress the repeat: %v", pending)
	}

	// Disabling it is a real off switch, not a smaller number.
	offInputs := map[string]any{}
	for k, v := range loadInputs {
		offInputs[k] = v
	}
	offInputs["silence_alert_days"] = 0
	os.Remove(stamp)
	pending, stderr, err = runPy(t, ws, subScript(t, loadScript, offInputs, ""))
	if err != nil {
		t.Fatalf("load_pending #5 failed: %v\nstderr: %s", err, stderr)
	}
	if pending["silence_alert"] != false {
		t.Fatalf("silence_alert_days=0 must disable the guard: %v", pending)
	}
}

// TestFeedWatch_GraphRouting checks the workflow edges with a stub
// executor: collect never reaches the LLM branch, an empty digest queue
// ends the run before the LLM step, and a full digest runs
// synthesize → notify → commit_state in order.
func TestFeedWatch_GraphRouting(t *testing.T) {
	planOut := func(collect bool) map[string]any {
		return map[string]any{
			"collect": collect, "digest": !collect, "category": "demo",
			"targets": []any{}, "editorial": "e", "digest_title": "Demo", "sinks": []any{},
			"_tokens": 1,
		}
	}

	t.Run("collect never reaches the digest branch", func(t *testing.T) {
		wf := compileFixture(t, "feed-watch/main.bot")
		exec := newScenarioExecutor()
		exec.on("plan", func(_ map[string]any) (map[string]any, error) { return planOut(true), nil })
		exec.on("fetch_feeds", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"entries_file": "", "fetched_count": 0, "feeds_ok": 1, "feeds_failed": 0, "errors": []any{}, "_tokens": 1}, nil
		})
		exec.on("dedup_queue", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"new_count": 0, "duplicate_count": 0, "pending_totals": map[string]any{}, "committed": false, "summary": "s", "_tokens": 1}, nil
		})
		s := tmpStore(t)
		if err := runtime.New(wf, s, exec).Run(context.Background(), "fw-collect", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, _ := s.LoadRun(context.Background(), "fw-collect")
		if r.Status != store.RunStatusFinished {
			t.Fatalf("status = %s", r.Status)
		}
		if !exec.wasCalled("dedup_queue") || exec.wasCalled("load_pending") || exec.wasCalled("synthesize") {
			t.Fatalf("wrong path: calls = %v", exec.calls)
		}
	})

	t.Run("empty digest queue ends before the LLM step", func(t *testing.T) {
		wf := compileFixture(t, "feed-watch/main.bot")
		exec := newScenarioExecutor()
		exec.on("plan", func(_ map[string]any) (map[string]any, error) { return planOut(false), nil })
		exec.on("load_pending", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"has_items": false, "category": "demo", "items": []any{}, "items_count": 0,
				"overflow_count": 0, "snapshot_ids": []any{}, "recent_topics": "",
				"editorial": "e", "digest_title": "Demo", "sinks": []any{}, "_tokens": 1,
			}, nil
		})
		s := tmpStore(t)
		if err := runtime.New(wf, s, exec).Run(context.Background(), "fw-empty", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, _ := s.LoadRun(context.Background(), "fw-empty")
		if r.Status != store.RunStatusFinished {
			t.Fatalf("status = %s", r.Status)
		}
		if exec.wasCalled("synthesize") || exec.wasCalled("notify") {
			t.Fatalf("empty queue must not reach the LLM: calls = %v", exec.calls)
		}
	})

	t.Run("undelivered digest keeps the queue", func(t *testing.T) {
		wf := compileFixture(t, "feed-watch/main.bot")
		exec := newScenarioExecutor()
		exec.on("plan", func(_ map[string]any) (map[string]any, error) { return planOut(false), nil })
		exec.on("load_pending", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"has_items": true, "category": "demo", "items": []any{map[string]any{"id": "x"}},
				"items_count": 1, "overflow_count": 0, "snapshot_ids": []any{"x"},
				"recent_topics": "", "editorial": "e", "editorial_nonce": "n", "digest_title": "Demo",
				"sinks": []any{}, "_tokens": 1,
			}, nil
		})
		exec.on("synthesize", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"headline": "h", "message_markdown": "m", "items_included": 1,
				"items_skipped_duplicates": []any{}, "board_cards_created": 0, "summary": "s",
				"_tokens": 100, "_cost_usd": 0.01,
			}, nil
		})
		exec.on("notify", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"posted": false, "dry_run": true, "delivered": 0, "targets": []any{}, "summary": "dry", "_tokens": 1}, nil
		})
		s := tmpStore(t)
		if err := runtime.New(wf, s, exec).Run(context.Background(), "fw-dry", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, _ := s.LoadRun(context.Background(), "fw-dry")
		if r.Status != store.RunStatusFinished {
			t.Fatalf("status = %s", r.Status)
		}
		if exec.wasCalled("commit_state") {
			t.Fatalf("an undelivered digest must not consume the queue: calls = %v", exec.calls)
		}
	})

	t.Run("full digest runs synthesize, notify, commit_state in order", func(t *testing.T) {
		wf := compileFixture(t, "feed-watch/main.bot")
		exec := newScenarioExecutor()
		exec.on("plan", func(_ map[string]any) (map[string]any, error) { return planOut(false), nil })
		exec.on("load_pending", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"has_items": true, "category": "demo", "items": []any{map[string]any{"id": "x"}},
				"items_count": 1, "overflow_count": 0, "snapshot_ids": []any{"x"},
				"recent_topics": "", "editorial": "e", "editorial_nonce": "n", "digest_title": "Demo",
				"sinks": []any{map[string]any{"webhook": "w1"}}, "_tokens": 1,
			}, nil
		})
		exec.on("synthesize", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{
				"headline": "h", "message_markdown": "m", "items_included": 1,
				"items_skipped_duplicates": []any{}, "board_cards_created": 0, "summary": "s",
				"_tokens": 100, "_cost_usd": 0.01,
			}, nil
		})
		exec.on("notify", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"posted": true, "dry_run": false, "delivered": 1, "targets": []any{"w1"}, "summary": "s", "_tokens": 1}, nil
		})
		exec.on("commit_state", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"cleared": 1, "archived": true, "committed": false, "summary": "s", "_tokens": 1}, nil
		})
		s := tmpStore(t)
		if err := runtime.New(wf, s, exec).Run(context.Background(), "fw-full", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		r, _ := s.LoadRun(context.Background(), "fw-full")
		if r.Status != store.RunStatusFinished {
			t.Fatalf("status = %s", r.Status)
		}
		want := []string{"plan", "load_pending", "synthesize", "verify_message", "notify", "commit_state"}
		if got := strings.Join(exec.calls, ","); got != strings.Join(want, ",") {
			t.Fatalf("call order = %v, want %v", exec.calls, want)
		}
	})
}

// TestFeedWatch_VerifyMessageBlocksInjectedLinks drives the real
// verify_message tool script: a digest whose every hyperlink is an input
// item's url (bare-vs-www and a static reference domain included) passes
// through unchanged, while a digest carrying an off-item link — the shape
// an injected editorial produces to phish or exfiltrate — hard-fails the
// run before delivery.
func TestFeedWatch_VerifyMessageBlocksInjectedLinks(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	script := feedWatchTool(t, wf, "verify_message").Script
	items := []any{
		map[string]any{"url": "https://www.bleepingcomputer.com/news/x"},
		map[string]any{"url": "https://thehackernews.com/y"},
	}

	good := "Headline\n**[A](https://bleepingcomputer.com/news/x)** — takeaway " +
		"([also](https://thehackernews.com/y)) ([cve](https://cve.mitre.org/z))"
	out, stderr, err := runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"message_markdown": good, "items": items,
	}, ""))
	if err != nil {
		t.Fatalf("legit item-only digest rejected: %v\nstderr: %s", err, stderr)
	}
	if out["message_markdown"] != good {
		t.Fatalf("verified digest not passed through unchanged: %v", out["message_markdown"])
	}

	bad := "**[A](https://bleepingcomputer.com/news/x)** — see " +
		"[details](https://cert-fr-metrics.attacker.com/click?d=leak)"
	_, stderr, err = runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"message_markdown": bad, "items": items,
	}, ""))
	if err == nil {
		t.Fatal("digest with an off-item link must hard-fail")
	}
	if !strings.Contains(stderr, "REJECTED") || !strings.Contains(stderr, "attacker.com") {
		t.Fatalf("rejection not explicit about the bad host: %s", stderr)
	}
}

// TestFeedWatch_FetchRejectsSSRF drives the real fetch_feeds script with a
// feed list of SSRF/LFI targets under the default strict posture
// (allow_private=false): every one is refused (metadata IP, loopback,
// file://, ftp://) and — since no feed survives — the node hard-fails
// loudly rather than yielding an empty-but-green collect. No network is
// touched: the addresses are rejected at resolution or scheme check.
func TestFeedWatch_FetchRejectsSSRF(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	script := feedWatchTool(t, wf, "fetch_feeds").Script
	targets := []any{map[string]any{"key": "x", "feeds": []any{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:9/rss",
		"file:///etc/passwd",
		"ftp://example.com/feed",
	}}}
	_, stderr, err := runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"targets": targets, "timeout_secs": 5, "max_per_feed": 5,
		"scratch_dir": t.TempDir(), "allow_private": false,
	}, ""))
	if err == nil {
		t.Fatal("a feed list of only SSRF/LFI targets must fail (no feed survived)")
	}
	for _, want := range []string{
		"SSRF-unsafe address 169.254.169.254",
		"SSRF-unsafe address 127.0.0.1",
		"scheme 'file'",
		"scheme 'ftp'",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("SSRF failure missing %q in:\n%s", want, stderr)
		}
	}
}

// TestFeedWatch_FetchProxyAware pins the sandbox-proxy fix: a cloud run reaches
// the internet through iterion's egress proxy, injected as HTTPS_PROXY at the
// runner's own (private) pod IP. urllib then dials the PROXY, not the feed host,
// so the old socket-level getaddrinfo guard rejected our own proxy as an
// "SSRF-unsafe address <pod-ip>" and every feed failed. The guard is now
// proxy-aware: it validates each feed URL's host UP-FRONT (the real SSRF check
// on the untrusted input) and no longer rejects the proxy hop.
func TestFeedWatch_FetchProxyAware(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	// Simulate the sandbox egress proxy advertised at a private pod IP.
	t.Setenv("HTTPS_PROXY", "http://10.2.40.10:45049")
	t.Setenv("HTTP_PROXY", "http://10.2.40.10:45049")
	wf := compileFixture(t, "feed-watch/main.bot")
	script := feedWatchTool(t, wf, "fetch_feeds").Script

	// (1) Protection HOLDS behind the proxy: an attacker feed host resolving to
	// a metadata/link-local address is still rejected up-front — not silently
	// tunnelled through the proxy.
	priv := []any{map[string]any{"key": "x", "feeds": []any{
		"http://169.254.169.254/latest/meta-data/",
	}}}
	_, stderr, err := runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"targets": priv, "timeout_secs": 5, "max_per_feed": 5,
		"scratch_dir": t.TempDir(), "allow_private": false,
	}, ""))
	if err == nil {
		t.Fatal("behind a proxy, a metadata-IP feed host must still be rejected")
	}
	if !strings.Contains(stderr, "SSRF-unsafe address 169.254.169.254") {
		t.Fatalf("expected up-front SSRF rejection of metadata host behind proxy, got:\n%s", stderr)
	}

	// (2) The regression itself: a PUBLIC feed host must NOT be rejected as
	// SSRF just because urllib dials the private proxy IP. A public literal IP
	// keeps the check offline-deterministic; urllib dials the (unreachable in
	// test) proxy, so the fetch fails with a CONNECTION error — never
	// "SSRF-unsafe".
	pub := []any{map[string]any{"key": "x", "feeds": []any{
		"https://8.8.8.8/feed.xml",
	}}}
	_, stderr2, err2 := runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"targets": pub, "timeout_secs": 5, "max_per_feed": 5,
		"scratch_dir": t.TempDir(), "allow_private": false,
	}, ""))
	if err2 == nil {
		t.Fatal("the unreachable test proxy should make the fetch fail (0 feeds ok)")
	}
	if strings.Contains(stderr2, "SSRF-unsafe") {
		t.Fatalf("public feed host must not be rejected as SSRF behind a proxy, got:\n%s", stderr2)
	}
}
