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
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
// secret path and the run-namespace members the scripts read directly, and
// fails on any leftover ref so a renamed input cannot silently produce a
// broken script. Run refs default to the deployment shape (a public origin,
// a plain id); a test overrides any of them — an empty run.url is the
// no-public-origin case.
func subScript(t *testing.T, script string, inputs map[string]any, secretPath string, runRefs ...map[string]any) string {
	t.Helper()
	for k, v := range inputs {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal input %s: %v", k, err)
		}
		script = strings.ReplaceAll(script, "{{input."+k+"}}", string(b))
	}
	run := map[string]any{
		"run.url":    "https://iterion.cloud/studio/runs/run-e2e-1",
		"run.id":     "run-e2e-1",
		"run.tokens": 4321,
	}
	for _, over := range runRefs {
		for k, v := range over {
			run[k] = v
		}
	}
	for k, v := range run {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal run ref %s: %v", k, err)
		}
		script = strings.ReplaceAll(script, "{{"+k+"}}", string(b))
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
	t.Parallel()
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
		"max_message_chars": 14000, "max_messages": 5,
		"ai_model": "claude-fable-x", "ai_backend": "claude_code",
		"ai_tokens": 15340, "ai_fallback_used": false, "ai_served_by": "",
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
	// A digest under the budget stays ONE message with no part marker —
	// the split path must not touch the nominal case.
	if out["parts"].(float64) != 1 || out["posts"].(float64) != 1 {
		t.Fatalf("short digest must be a single unsplit post: %v", out)
	}
	// The delivered text is the digest plus the run footer; everything
	// else in the payload is unchanged.
	if gotBody["channel"] != "#demo" {
		t.Fatalf("webhook payload = %v", gotBody)
	}
	text, _ := gotBody["text"].(string)
	if body := stripFooter(text); body != "**digest**" {
		t.Fatalf("webhook payload body = %q", body)
	}
	if !strings.Contains(text, footerHeading) || !strings.HasSuffix(text, runFooterSig(runURLDefault, runIDDefault)) {
		t.Fatalf("webhook payload must carry the run footer: %q", text)
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
	t.Parallel()
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
		if err := newEngine(t, wf, s, exec).Run(context.Background(), "fw-collect", nil); err != nil {
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
		if err := newEngine(t, wf, s, exec).Run(context.Background(), "fw-empty", nil); err != nil {
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
		if err := newEngine(t, wf, s, exec).Run(context.Background(), "fw-dry", nil); err != nil {
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
		if err := newEngine(t, wf, s, exec).Run(context.Background(), "fw-full", nil); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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

// longDigest builds a digest of `blocks` blank-line-separated entries in
// the real shape the synthesis emits (one link-bearing line per entry).
func longDigest(blocks int) string {
	parts := make([]string, 0, blocks)
	for i := 0; i < blocks; i++ {
		parts = append(parts, fmt.Sprintf(
			"**[Story %02d](https://example.com/%02d)** — %s",
			i, i, strings.Repeat("takeaway sentence ", 12)))
	}
	return "Headline of the day\n\n" + strings.Join(parts, "\n\n")
}

// partMarker matches the "_(i/n)_" (or the truncated variant) footer the
// notify node appends to every message of a split digest.
var partMarker = regexp.MustCompile(`\n\n_\(\d+/\d+[^)]*\)_$`)

// The run footer the notify/notify_silence nodes append (the details
// heading when AI provenance is shown, the signature always).
const (
	footerHeading = "##### Détails du run IA · Iterion"
	footerSig     = "🤖 [iterion](https://iterion.cloud)"

	// The run refs subScript substitutes by default (the deployment shape).
	runURLDefault = "https://iterion.cloud/studio/runs/run-e2e-1"
	runIDDefault  = "run-e2e-1"
)

// runFooterSig returns a footer-bearing message's expected FINAL line for
// the given run refs — the signature plus the run mention, mirroring the
// python's four cases.
func runFooterSig(runURL, runID string) string {
	sig := footerSig
	switch {
	case runURL != "" && runID != "":
		sig += " · run [`" + runID + "`](" + runURL + ")"
	case runURL != "":
		sig += " · run [this run](" + runURL + ")"
	case runID != "":
		sig += " · run `" + runID + "`"
	}
	return sig
}

// stripFooter removes the run footer from a message: the block starts at
// the details heading when provenance is shown, at the signature when not.
func stripFooter(m string) string {
	if i := strings.LastIndex(m, footerHeading); i >= 0 {
		return strings.TrimRight(m[:i], "\n")
	}
	if i := strings.LastIndex(m, footerSig); i >= 0 {
		return strings.TrimRight(m[:i], "\n")
	}
	return m
}

// notifySink is a webhook stand-in accumulating every payload it receives,
// in order, optionally failing from the Nth post of a given channel on.
type notifySink struct {
	mu       sync.Mutex
	bodies   []map[string]any
	failFrom map[string]int // channel → 1-based post index that starts failing
	seen     map[string]int
}

func newNotifySink() *notifySink {
	return &notifySink{failFrom: map[string]int{}, seen: map[string]int{}}
}

func (s *notifySink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		defer s.mu.Unlock()
		ch, _ := body["channel"].(string)
		s.seen[ch]++
		if n, ok := s.failFrom[ch]; ok && s.seen[ch] >= n {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		s.bodies = append(s.bodies, body)
		fmt.Fprint(w, "ok")
	}
}

// texts returns the received message texts for one channel, in order.
func (s *notifySink) texts(channel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var got []string
	for _, b := range s.bodies {
		if ch, _ := b["channel"].(string); ch == channel {
			got = append(got, b["text"].(string))
		}
	}
	return got
}

// TestFeedWatch_NotifySplitsLongDigest drives the REAL notify script: a
// digest above a sink's per-message budget is delivered as consecutive
// numbered messages carrying ALL of it — the failure this replaces cut the
// digest at 14000 chars and pointed its chat readers at run artifacts they
// cannot open. Covers the per-sink budget override, the max_messages
// ceiling, and the intra-sink partial failure.
func TestFeedWatch_NotifySplitsLongDigest(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	script := feedWatchTool(t, wf, "notify").Script

	// run posts `msg` to `sinks` through the real script and returns the
	// node output plus the recording sink. The opts maps carry AI
	// provenance inputs (`ai_*`) and run-namespace overrides (`run.*`) on
	// top of the defaults below.
	aiDefaults := map[string]any{
		"ai_model": "claude-fable-x", "ai_backend": "claude_code",
		"ai_tokens": 15340, "ai_fallback_used": false, "ai_served_by": "",
	}
	run := func(t *testing.T, msg string, sinks []any, maxChars, maxMessages int, sink *notifySink, opts ...map[string]any) (map[string]any, error) {
		t.Helper()
		srv := httptest.NewServer(sink.handler())
		defer srv.Close()
		secret := filepath.Join(t.TempDir(), "webhooks.json")
		if err := os.WriteFile(secret, []byte(`{"w1": "`+srv.URL+`", "w2": "`+srv.URL+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		inputs := map[string]any{
			"message_markdown": msg, "sinks": sinks, "dry_run": false,
			"category": "demo", "digest_title": "Demo Watch",
			"max_message_chars": maxChars, "max_messages": maxMessages,
		}
		for k, v := range aiDefaults {
			inputs[k] = v
		}
		runRefs := map[string]any{}
		for _, over := range opts {
			for k, v := range over {
				if strings.HasPrefix(k, "run.") {
					runRefs[k] = v
				} else {
					inputs[k] = v
				}
			}
		}
		out, stderr, err := runPy(t, t.TempDir(), subScript(t, script, inputs, secret, runRefs))
		if err != nil && stderr != "" {
			t.Logf("notify stderr: %s", stderr)
		}
		return out, err
	}

	// reassemble strips the run footer from the last message and the part
	// markers from every one, and re-joins, so the assertion is "the
	// channel received the whole digest", not "it received something".
	reassemble := func(t *testing.T, got []string) string {
		t.Helper()
		var body []string
		for i, m := range got {
			if i == len(got)-1 {
				m = stripFooter(m)
			}
			stripped := partMarker.ReplaceAllString(m, "")
			if stripped == m && len(got) > 1 {
				t.Fatalf("message %d/%d carries no part marker: %q", i+1, len(got), tail(m))
			}
			body = append(body, stripped)
		}
		return strings.Join(body, "\n\n")
	}

	t.Run("whole digest arrives in numbered parts", func(t *testing.T) {
		msg := longDigest(20)
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		out, err := run(t, msg, sinks, 1200, 0, sink)
		if err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) < 3 {
			t.Fatalf("a %d-char digest at a 1200-char budget must split into several messages, got %d", len(msg), len(got))
		}
		if int(out["parts"].(float64)) != len(got) || int(out["posts"].(float64)) != len(got) {
			t.Fatalf("output must account for every message: parts=%v posts=%v, posted %d", out["parts"], out["posts"], len(got))
		}
		if out["posted"] != true || out["delivered"].(float64) != 1 {
			t.Fatalf("a fully delivered sink must count as delivered: %v", out)
		}
		for i, m := range got {
			// The budget is a CHARACTER budget (the python reserves and
			// splits on len()), and the footer carries multibyte text.
			if utf8.RuneCountInString(m) > 1200 {
				t.Fatalf("message %d is over the 1200-char budget: %d runes", i+1, utf8.RuneCountInString(m))
			}
			marker := fmt.Sprintf("_(%d/%d)_", i+1, len(got))
			if i < len(got)-1 {
				if !strings.HasSuffix(m, marker) {
					t.Fatalf("message %d marker wrong: %q", i+1, tail(m))
				}
				continue
			}
			// The last part: its marker first, the run footer at the very
			// bottom.
			if !strings.Contains(m, marker) || !strings.HasSuffix(m, runFooterSig(runURLDefault, runIDDefault)) {
				t.Fatalf("last message must carry its marker then the run footer: %q", tail(m))
			}
		}
		if body := reassemble(t, got); body != msg {
			t.Fatalf("content lost in the split:\nwant %d chars, got %d chars", len(msg), len(body))
		}
	})

	// The footer carries the AI provenance and the run link WHOLE on the
	// last message: a footer split across parts is a broken table in the
	// channel, and the reserve that keeps it whole is the whole mechanism.
	t.Run("the run footer rides the last part whole", func(t *testing.T) {
		msg := longDigest(20)
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 1200, 0, sink); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) < 3 {
			t.Fatalf("expected a split, got %d message(s)", len(got))
		}
		last := got[len(got)-1]
		for _, want := range []string{
			footerHeading, "claude-fable-x", "claude_code",
			"[`run-e2e-1`](https://iterion.cloud/studio/runs/run-e2e-1)",
			"Tokens du run",
		} {
			if !strings.Contains(last, want) {
				t.Fatalf("the run footer must carry %q whole on the last message, got: %q", want, tail(last))
			}
		}
		for _, m := range got[:len(got)-1] {
			if strings.Contains(m, footerHeading) || strings.Contains(m, "/studio/runs/") {
				t.Fatalf("an earlier message carries part of the run footer: %q", tail(m))
			}
		}
		if body := reassemble(t, got); body != msg {
			t.Fatal("the footer reserve must not lose digest content")
		}
	})

	// A digest that fits one message has no part marker, and still ends
	// with the footer.
	t.Run("a digest that fits one message still ends with the footer", func(t *testing.T) {
		msg := "# Demo Watch\n\nshort enough to fit in one message"
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 4000, 0, sink); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) != 1 {
			t.Fatalf("want 1 message, got %d", len(got))
		}
		if !strings.HasSuffix(got[0], runFooterSig(runURLDefault, runIDDefault)) || !strings.Contains(got[0], footerHeading) {
			t.Fatalf("the single message must end with the run footer: %q", tail(got[0]))
		}
		if body := stripFooter(got[0]); body != msg {
			t.Fatalf("footer appended over unchanged content:\nwant %q\ngot %q", msg, body)
		}
	})

	// A deployment with no public origin must not publish a dead or
	// relative link: the run is named by its id alone.
	t.Run("no public origin prints the run id alone", func(t *testing.T) {
		msg := "# Demo Watch\n\nshort enough to fit in one message"
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 4000, 0, sink,
			map[string]any{"run.url": ""}); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) != 1 {
			t.Fatalf("want 1 message, got %d", len(got))
		}
		if strings.Contains(got[0], "/studio/runs/") {
			t.Fatalf("no origin must publish no run link: %q", tail(got[0]))
		}
		if !strings.Contains(got[0], "run `run-e2e-1`") {
			t.Fatalf("the run must still be named, by its id alone: %q", tail(got[0]))
		}
	})

	// A run id is caller-chosen at launch and screened only for path
	// traversal, so it may carry markdown: an id like this one would close
	// a label early and publish a foreign link under Vigie's name. No
	// address AND an unusable label: the id is still NAMED, in a code span
	// a hostile id can neither close nor turn into a link.
	t.Run("a markdown-bearing run id is named, never labeled", func(t *testing.T) {
		msg := "# Demo Watch\n\nshort enough to fit in one message"
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 4000, 0, sink, map[string]any{
			"run.url": "", "run.id": "x](https://evil.example",
		}); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) != 1 {
			t.Fatalf("want 1 message, got %d", len(got))
		}
		if strings.Contains(got[0], "](https://evil.example)") {
			t.Fatalf("a caller-chosen id must never become a link label: %q", tail(got[0]))
		}
		if !strings.Contains(got[0], "run `x](https://evil.example`") {
			t.Fatalf("the run must still be named, in a code span: %q", tail(got[0]))
		}
	})

	// Provenance that never reached the node publishes no table: an empty
	// row would claim a step nobody served.
	t.Run("no AI provenance publishes the run link alone", func(t *testing.T) {
		msg := "# Demo Watch\n\nshort enough to fit in one message"
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 4000, 0, sink, map[string]any{
			"ai_model": "", "ai_backend": "", "ai_tokens": 0,
		}); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) != 1 {
			t.Fatalf("want 1 message, got %d", len(got))
		}
		if strings.Contains(got[0], footerHeading) {
			t.Fatalf("an empty provenance table claims a step nobody served: %q", tail(got[0]))
		}
		if !strings.HasSuffix(got[0], runFooterSig(runURLDefault, runIDDefault)) {
			t.Fatalf("the run signature must still close the message: %q", tail(got[0]))
		}
	})

	t.Run("per-sink max_chars splits each channel to its own budget", func(t *testing.T) {
		msg := longDigest(20)
		sinks := []any{
			map[string]any{"webhook": "w1", "channel": "#wide"},
			map[string]any{"webhook": "w2", "channel": "#narrow", "max_chars": 900},
		}
		sink := newNotifySink()
		out, err := run(t, msg, sinks, 4000, 0, sink)
		if err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		wide, narrow := sink.texts("#wide"), sink.texts("#narrow")
		if len(narrow) <= len(wide) {
			t.Fatalf("the narrower sink must take more messages: wide=%d narrow=%d", len(wide), len(narrow))
		}
		if out["delivered"].(float64) != 2 {
			t.Fatalf("both sinks fully delivered, got %v", out)
		}
		for _, m := range narrow {
			if utf8.RuneCountInString(m) > 900 {
				t.Fatalf("a #narrow message is over its own 900-char budget: %d runes", utf8.RuneCountInString(m))
			}
		}
		if body := reassemble(t, narrow); body != msg {
			t.Fatal("the narrow sink did not receive the whole digest")
		}
		if body := reassemble(t, wide); body != msg {
			t.Fatal("the wide sink did not receive the whole digest")
		}
	})

	t.Run("a section heading is not stranded at the end of a part", func(t *testing.T) {
		// Sized so the "also seen" header lands EXACTLY on a boundary:
		// at a 900-char budget an entry (400) plus the header (20) fits
		// the body budget the footer reserve leaves, the next entry does
		// not — so without the fix the header closes message 1 while its
		// items open message 2.
		entry := strings.Repeat("mot ", 100)
		msg := strings.Join([]string{entry, "### Egalement vus", entry, entry}, "\n\n")
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		if _, err := run(t, msg, sinks, 900, 0, sink); err != nil {
			t.Fatalf("notify failed: %v", err)
		}
		got := sink.texts("#demo")
		if len(got) < 2 {
			t.Fatalf("expected a split, got %d message(s)", len(got))
		}
		for i, m := range got[:len(got)-1] {
			last := strings.TrimSpace(partMarker.ReplaceAllString(m, ""))
			if idx := strings.LastIndex(last, "\n\n"); idx >= 0 {
				last = last[idx+2:]
			}
			if strings.HasPrefix(strings.TrimSpace(last), "#") {
				t.Fatalf("message %d ends on an orphaned heading: %q", i+1, last)
			}
		}
		if body := reassemble(t, got); body != msg {
			t.Fatal("moving the heading must not lose or duplicate content")
		}
	})

	// Truncation is the quietest way this node can drop content: the ceiling
	// removes the tail parts BEFORE any POST, so every sink reports a
	// complete send and the queue would be consumed for items whose text
	// never left. The channel only sees a notice pointing at run artifacts
	// its readers cannot open. What fits is still posted — but the run must
	// FAIL so the queue survives and the items are re-digested.
	t.Run("max_messages ceiling posts what fits, says so, and keeps the queue", func(t *testing.T) {
		msg := longDigest(20)
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		_, err := run(t, msg, sinks, 1200, 2, sink)
		if err == nil {
			t.Fatal("a truncated digest must not report success — the queue would be consumed for items that never left")
		}
		got := sink.texts("#demo")
		if len(got) != 2 {
			t.Fatalf("max_messages=2 must post exactly 2 messages, got %d", len(got))
		}
		if !strings.Contains(got[1], "digest truncated") {
			t.Fatalf("the capped tail must be announced, got: %q", tail(got[1]))
		}
		if !strings.Contains(got[1], footerHeading) || !strings.HasSuffix(got[1], runFooterSig(runURLDefault, runIDDefault)) {
			t.Fatalf("the capped tail must still carry the run footer under its truncation notice: %q", tail(got[1]))
		}
		if strings.Contains(got[0], "digest truncated") {
			t.Fatal("only the LAST message announces the truncation")
		}
		if strings.Contains(got[0], footerHeading) {
			t.Fatal("only the LAST message carries the run footer")
		}
	})

	// The capped last message carries the LONGEST marker there is (the
	// truncation notice). A reserve sized for the ordinary "_(i/n)_" puts it
	// over the budget — and the budget exists to stay under the platform's
	// hard post limit, so that POST is the one the platform rejects.
	// The input is a single unbroken line on purpose: only the hard-cut path
	// packs a part to exactly `limit - reserve`, where the shortfall shows.
	// Block-separated text leaves slack that hides it.
	t.Run("no message exceeds the budget, marker included", func(t *testing.T) {
		msg := strings.Repeat("x", 9000)
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#demo"}}
		sink := newNotifySink()
		// Truncation keeps the queue, so this run fails by design; what is
		// under test is what reached the channel before it did.
		if _, err := run(t, msg, sinks, 1200, 3, sink); err == nil {
			t.Fatal("a truncated digest must not report success")
		}
		got := sink.texts("#demo")
		if len(got) != 3 {
			t.Fatalf("expected 3 capped messages, got %d", len(got))
		}
		if !strings.Contains(got[2], "digest truncated") {
			t.Fatalf("the capped tail must be announced: %q", tail(got[2]))
		}
		for i, m := range got {
			if utf8.RuneCountInString(m) > 1200 {
				t.Fatalf("message %d/%d is over the 1200-char budget: %d runes: %q",
					i+1, len(got), utf8.RuneCountInString(m), tail(m))
			}
		}
	})

	// `posted` is not "something went out" — it is the QUEUE-CONSUMPTION
	// signal (`notify -> commit_state when posted`), and commit_state clears
	// the delivered snapshots from pending.jsonl. Returning it true on a
	// partial delivery ends the run GREEN having permanently dropped whatever
	// never left: the channel gets part 1 of 5 and the rest is gone from the
	// queue forever. Louder-to-quieter than failing, and it contradicts the
	// splitting's whole purpose. A replay may duplicate what already landed —
	// a duplicate is recoverable, a lost digest is not.
	t.Run("a part failure stops the sink AND refuses to consume the queue", func(t *testing.T) {
		msg := longDigest(20)
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#flaky"}}
		sink := newNotifySink()
		sink.failFrom["#flaky"] = 2 // part 1 lands, part 2 onwards 500s
		out, err := run(t, msg, sinks, 1200, 0, sink)
		if err == nil {
			t.Fatalf("a sink that got part 1 of n must NOT report success — the queue would be consumed and the tail lost: %v", out)
		}
		if got := sink.texts("#flaky"); len(got) != 1 {
			t.Fatalf("delivery must stop at the first failed part, got %d message(s)", len(got))
		}
	})

	// The same defect predates splitting: with two sinks, one served used to
	// be enough to consume the queue for both, so the second channel lost the
	// digest silently. One predicate closes both.
	t.Run("one sink of two is not a delivery", func(t *testing.T) {
		msg := "# Demo Watch\n\nshort enough to fit in one message"
		sinks := []any{
			map[string]any{"webhook": "w1", "channel": "#ok"},
			map[string]any{"webhook": "w2", "channel": "#down"},
		}
		sink := newNotifySink()
		sink.failFrom["#down"] = 1 // this channel never accepts anything
		out, err := run(t, msg, sinks, 4000, 0, sink)
		if err == nil {
			t.Fatalf("one sink of two served must not consume the queue for both: %v", out)
		}
		if got := sink.texts("#ok"); len(got) != 1 {
			t.Fatalf("the healthy sink must still have been served, got %d", len(got))
		}
	})

	t.Run("every part failing fails the run", func(t *testing.T) {
		sinks := []any{map[string]any{"webhook": "w1", "channel": "#dead"}}
		sink := newNotifySink()
		sink.failFrom["#dead"] = 1
		if _, err := run(t, longDigest(20), sinks, 1200, 0, sink); err == nil {
			t.Fatal("a delivery that reached nobody must fail the run (safe to resume)")
		}
	})
}

// tail returns the last 120 chars of s for readable failure messages.
func tail(s string) string {
	if len(s) <= 120 {
		return s
	}
	return "…" + s[len(s)-120:]
}

// TestFeedWatch_SilenceAlertLinksItsRun drives the REAL notify_silence
// script: the silence alert carries the same run signature the digest does
// (no AI block — no LLM step serves it), naming the run that checked.
func TestFeedWatch_SilenceAlertLinksItsRun(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	script := feedWatchTool(t, wf, "notify_silence").Script
	sink := newNotifySink()
	srv := httptest.NewServer(sink.handler())
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "webhooks.json")
	if err := os.WriteFile(secret, []byte(`{"w1": "`+srv.URL+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runPy(t, t.TempDir(), subScript(t, script, map[string]any{
		"category": "demo", "digest_title": "Demo Watch",
		"sinks":        []any{map[string]any{"webhook": "w1", "channel": "#demo"}},
		"silence_days": 3, "silence_window": 3, "silence_message": "",
		"dry_run": false, "workspace": t.TempDir(), "state_dir": "state",
		"state_commit": false,
	}, secret))
	if err != nil {
		t.Fatalf("notify_silence failed: %v (stderr: %s)", err, stderr)
	}
	if out["posted"] != true {
		t.Fatalf("a delivered silence alert must report posted: %v", out)
	}
	got := sink.texts("#demo")
	if len(got) != 1 {
		t.Fatalf("want 1 silence alert, got %d", len(got))
	}
	if strings.Contains(got[0], footerHeading) {
		t.Fatalf("a silence alert has no AI step behind it — no provenance table: %q", tail(got[0]))
	}
	if !strings.HasSuffix(got[0], runFooterSig(runURLDefault, runIDDefault)) {
		t.Fatalf("the silence alert must end with the run signature: %q", tail(got[0]))
	}
	if !strings.Contains(got[0], "[`run-e2e-1`](https://iterion.cloud/studio/runs/run-e2e-1)") {
		t.Fatalf("the silence alert must link its run: %q", tail(got[0]))
	}
}

// TestFeedWatch_OverflowItemsSurviveCommit: snapshot_ids must name only
// the items actually handed to the LLM. Before the fix, load_pending
// snapshotted the WHOLE queue and then truncated to max_items, so
// commit_state purged overflow items that were never synthesized nor
// published — silent data loss on any backlog larger than max_items.
func TestFeedWatch_OverflowItemsSurviveCommit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "feed-watch/main.bot")
	ws, _ := feedWatchWorkspace(t)

	// Seed 5 pending items, newest-first ids i5..i1 by ts.
	cdir := filepath.Join(ws, ".feed-watch", "demo")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf(`{"id":"i%d","title":"t%d","url":"https://x.test/%d","ts":%d}`, i, i, i, 1000+i))
	}
	if err := os.WriteFile(filepath.Join(cdir, "pending.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := runPlan(t, wf, ws, "digest", "demo")
	loadScript := feedWatchTool(t, wf, "load_pending").Script
	loadInputs := map[string]any{
		"category": "demo", "editorial": plan["editorial"], "digest_title": plan["digest_title"],
		"sinks": plan["sinks"], "workspace": ws, "state_dir": ".feed-watch", "max_items": 3,
		"silence_alert_days": 3,
	}
	pending, stderr, err := runPy(t, ws, subScript(t, loadScript, loadInputs, ""))
	if err != nil {
		t.Fatalf("load_pending failed: %v\nstderr: %s", err, stderr)
	}
	if pending["items_count"].(float64) != 3 || pending["overflow_count"].(float64) != 2 {
		t.Fatalf("load_pending = %v items / %v overflow, want 3/2", pending["items_count"], pending["overflow_count"])
	}
	snapshot := pending["snapshot_ids"].([]any)
	if len(snapshot) != 3 {
		t.Fatalf("snapshot_ids must name only the items handed to the LLM: %v", snapshot)
	}
	got := map[string]bool{}
	for _, id := range snapshot {
		got[id.(string)] = true
	}
	// Newest 3 by ts are i5,i4,i3; the overflow (i1,i2) must NOT be
	// snapshotted, or commit_state would purge them unpublished.
	for _, want := range []string{"i5", "i4", "i3"} {
		if !got[want] {
			t.Fatalf("snapshot_ids missing %s: %v", want, snapshot)
		}
	}
	if got["i1"] || got["i2"] {
		t.Fatalf("overflow items leaked into snapshot_ids: %v", snapshot)
	}

	commitScript := feedWatchTool(t, wf, "commit_state").Script
	commitInputs := map[string]any{
		"category": "demo", "snapshot_ids": snapshot, "message_markdown": "**digest**",
		"headline": "Demo headline", "state_commit": false, "workspace": ws, "state_dir": ".feed-watch",
	}
	out, stderr, err := runPy(t, ws, subScript(t, commitScript, commitInputs, ""))
	if err != nil {
		t.Fatalf("commit_state failed: %v\nstderr: %s", err, stderr)
	}
	if out["cleared"].(float64) != 3 {
		t.Fatalf("cleared = %v, want 3", out["cleared"])
	}
	raw, err := os.ReadFile(filepath.Join(cdir, "pending.jsonl"))
	if err != nil {
		t.Fatalf("pending.jsonl gone after commit: %v", err)
	}
	kept := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1
	if kept != 2 || !strings.Contains(string(raw), `"i1"`) || !strings.Contains(string(raw), `"i2"`) {
		t.Fatalf("overflow items must survive the commit, pending.jsonl = %s", raw)
	}
}
