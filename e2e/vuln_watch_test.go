package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// vwTool extracts a tool node from the compiled vuln-watch fixture.
func vwTool(t *testing.T, wf *ir.Workflow, id string) *ir.ToolNode {
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

// vwSub replaces {{input.K}} / {{vars.K}} template refs with JSON literals
// (the engine's injection contract for script tool nodes) plus the two
// secret paths, and fails on any leftover ref so a renamed input cannot
// silently produce a broken script.
func vwSub(t *testing.T, script string, inputs, vars map[string]any, secrets map[string]string) string {
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

// vulnWatchHarness is one hermetic deployment: a workspace with config +
// inventory, a fake GitHub API + advisory feed + webhook sink (httptest),
// and mutable fixture state so successive runs see the world move.
type vulnWatchHarness struct {
	ws, scratch, kevPath     string
	tokensFile, webhooksFile string
	depAlerts                atomic.Value // []map[string]any served by /orgs/{org}/dependabot/alerts
	feedItems                atomic.Value // []vwFeedItem
	srv                      *httptest.Server
	sinkHits                 atomic.Int64
	sinkBodies               []string
}

type vwFeedItem struct {
	Ref, Title string
	CVEs       []string
	Products   []string
}

func newVulnWatchHarness(t *testing.T) *vulnWatchHarness {
	t.Helper()
	h := &vulnWatchHarness{ws: t.TempDir(), scratch: t.TempDir()}
	h.depAlerts.Store([]map[string]any{})
	h.feedItems.Store([]vwFeedItem{})

	mux := http.NewServeMux()
	mux.HandleFunc("/orgs/testorg/dependabot/alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-testorg" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(h.depAlerts.Load())
	})
	mux.HandleFunc("/alerte/feed/", func(w http.ResponseWriter, r *http.Request) {
		items := h.feedItems.Load().([]vwFeedItem)
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>ALE</title>`)
		for _, it := range items {
			fmt.Fprintf(&b, `<item><title>%s</title><link>%s/alerte/%s</link><guid>%s</guid></item>`,
				it.Title, h.srv.URL, it.Ref, it.Ref)
		}
		b.WriteString(`</channel></rss>`)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/alerte/", func(w http.ResponseWriter, r *http.Request) {
		// CERT-FR-style structured JSON at <link>/json/.
		if !strings.HasSuffix(r.URL.Path, "/json/") {
			http.NotFound(w, r)
			return
		}
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/alerte/"), "/json/")
		for _, it := range h.feedItems.Load().([]vwFeedItem) {
			if it.Ref != ref {
				continue
			}
			cves := []map[string]string{}
			for _, c := range it.CVEs {
				cves = append(cves, map[string]string{"name": c})
			}
			affected := []map[string]any{}
			for _, p := range it.Products {
				affected = append(affected, map[string]any{
					"description": p + " all versions",
					"product":     map[string]any{"name": p, "vendor": map[string]any{"name": p}},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"title": it.Title, "cves": cves, "affected_systems": affected,
			})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.sinkHits.Add(1)
		h.sinkBodies = append(h.sinkBodies, body.Text)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	// KEV catalog: a mutable local file (poll_exploit reads file:// in
	// trusted-local mode).
	h.kevPath = filepath.Join(h.scratch, "kev.json")
	h.setKEV(nil)

	// Secrets as mounted files.
	h.tokensFile = filepath.Join(h.scratch, "dependabot_tokens.json")
	if err := os.WriteFile(h.tokensFile, []byte(`{"testorg": "tok-testorg"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.webhooksFile = filepath.Join(h.scratch, "webhooks.json")
	if err := os.WriteFile(h.webhooksFile, []byte(`{"w1": "`+h.srv.URL+`/hook"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	inv := map[string]any{
		"technologies": map[string]any{
			"metabase": map[string]any{"label": "Metabase", "match": []string{"metabase"},
				"projects": []string{"domifa"}},
			"golang": map[string]any{"label": "Go", "match": []string{"golang"},
				"projects": []string{"domifa"}},
			"spring-boot": map[string]any{"label": "Spring Boot", "match": []string{"spring boot"},
				"projects": []string{"accolade"}},
			"jwt": map[string]any{"label": "JWT", "match": []string{}, "watch": false, "projects": []string{}},
		},
		"projects": map[string]any{
			"domifa":   map[string]any{"name": "DOMIFA", "repos": []string{"testorg/domifa", "testorg/domifa-migrations"}},
			"accolade": map[string]any{"name": "ACCOLADE", "repos": []string{"testorg/accolade-env"}},
		},
	}
	ib, _ := json.MarshalIndent(inv, "", " ")
	if err := os.WriteFile(filepath.Join(h.ws, "inventory.json"), ib, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"github_orgs":     []string{"testorg"},
		"github_api_base": h.srv.URL,
		"advisory_feeds":  []map[string]any{{"url": h.srv.URL + "/alerte/feed/", "kind": "alert"}},
		"kev_url":         "file://" + h.kevPath,
		"epss_url":        "", // EPSS off in the harness default; policy tests use KEV/ALE
		"sinks": []map[string]any{
			{"webhook": "w1", "channel": "#sec", "username": "Senti"},
		},
	}
	cb, _ := json.MarshalIndent(cfg, "", " ")
	if err := os.WriteFile(filepath.Join(h.ws, "vuln-watch.json"), cb, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *vulnWatchHarness) setKEV(entries []map[string]any) {
	if entries == nil {
		entries = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"vulnerabilities": entries})
	_ = os.WriteFile(h.kevPath, b, 0o644)
}

// runWatch drives one full watch tick through the REAL python scripts of
// every tool node (plan → polls → match → notify → commit), in
// trusted-local mode (httptest/file:// sources). Returns the match and
// notify outputs.
func (h *vulnWatchHarness) runWatch(t *testing.T, wf *ir.Workflow, dryRun bool) (match, notify map[string]any) {
	t.Helper()
	vars := map[string]any{
		"workspace_dir": h.ws, "config_path": "vuln-watch.json",
		"inventory_path": "inventory.json", "mode": "watch",
	}
	plan, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "plan").Script, nil, vars, nil))
	if err != nil {
		t.Fatalf("plan failed: %v\nstderr: %s", err, stderr)
	}

	dep, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "poll_dependabot").Script, map[string]any{
		"orgs": plan["github_orgs"], "api_base": plan["github_api_base"],
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true,
		"workspace": h.ws, "state_dir": ".vuln-watch",
	}, nil, map[string]string{"dependabot_tokens": h.tokensFile}))
	if err != nil {
		t.Fatalf("poll_dependabot failed: %v\nstderr: %s", err, stderr)
	}

	adv, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "poll_advisories").Script, map[string]any{
		"feeds": plan["advisory_feeds"], "timeout_secs": 5, "scratch_dir": h.scratch,
		"allow_private": true, "workspace": h.ws, "state_dir": ".vuln-watch",
	}, nil, nil))
	if err != nil {
		t.Fatalf("poll_advisories failed: %v\nstderr: %s", err, stderr)
	}

	kev, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "poll_exploit").Script, map[string]any{
		"kev_url": plan["kev_url"], "timeout_secs": 5, "scratch_dir": h.scratch,
		"allow_private": true,
	}, nil, nil))
	if err != nil {
		t.Fatalf("poll_exploit failed: %v\nstderr: %s", err, stderr)
	}

	match, stderr, err = runPy(t, h.ws, vwSub(t, vwTool(t, wf, "match_policy").Script, map[string]any{
		"alerts_file": dep["alerts_file"], "units_file": adv["units_file"],
		"kev_file": kev["kev_file"], "dependabot_errors": dep["errors"],
		"advisory_errors": adv["errors"], "kev_error": kev["error"],
		"epss_url": plan["epss_url"], "epss_threshold": plan["epss_threshold"],
		"dependabot_alert_floor": plan["dependabot_alert_floor"],
		"certfr_avis":            plan["certfr_avis"],
		"github_orgs":            plan["github_orgs"], "advisory_feeds": plan["advisory_feeds"],
		"kev_url": plan["kev_url"], "inventory_path": plan["inventory_path"],
		"workspace": h.ws, "state_dir": ".vuln-watch", "scratch_dir": h.scratch,
		"timeout_secs": 5, "allow_private": true, "max_alerts": 20,
		"observe_window_days": 60, "source_stale_hours": 24,
	}, nil, nil))
	if err != nil {
		t.Fatalf("match_policy failed: %v\nstderr: %s", err, stderr)
	}

	notify, stderr, err = runPy(t, h.ws, vwSub(t, vwTool(t, wf, "notify").Script, map[string]any{
		"alerts": match["alerts"], "overflow_count": match["overflow_count"],
		"stale_sources": match["stale_sources"], "sinks": plan["sinks"],
		"labels": plan["labels"], "dry_run": dryRun,
	}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify failed: %v\nstderr: %s", err, stderr)
	}

	if notify["consume"] == true {
		_, stderr, err = runPy(t, h.ws, vwSub(t, vwTool(t, wf, "commit_state").Script, map[string]any{
			"state_next_file": match["state_next_file"], "alertlog_file": match["alertlog_file"],
			"state_commit": false, "workspace": h.ws, "state_dir": ".vuln-watch",
		}, nil, nil))
		if err != nil {
			t.Fatalf("commit_state failed: %v\nstderr: %s", err, stderr)
		}
	}
	return match, notify
}

func vwDepAlert(ghsa, cve, severity, repo, pkg, created, fix string) map[string]any {
	return map[string]any{
		"created_at": created,
		"repository": map[string]any{"full_name": repo},
		"dependency": map[string]any{"package": map[string]any{"name": pkg, "ecosystem": "npm"}},
		"security_advisory": map[string]any{
			"ghsa_id": ghsa, "cve_id": cve, "severity": severity,
			"summary": pkg + " vulnerability",
			"vulnerabilities": []map[string]any{
				{"package": map[string]any{"name": pkg, "ecosystem": "npm"},
					"first_patched_version": map[string]any{"identifier": fix}},
			},
		},
	}
}

// TestVulnWatch_PolicyAndRefire drives the REAL python tool scripts through
// the bot's core promise: bootstrap alerts nothing; an ordinary new critical
// stays silent (observed); an alert-class advisory posts within the tick
// with the inventory projects joined; and the day KEV picks up the observed
// CVE, it RE-FIRES exactly once (the Metabase scenario) — then never again.
func TestVulnWatch_PolicyAndRefire(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)

	// ── Run 1: bootstrap — a pre-existing backlog is not news. ──
	h.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-old1-xxxx-yyyy", "CVE-2024-0001", "critical", "testorg/domifa", "oldpkg", "2026-08-01T00:00:00Z", "1.2.3"),
	})
	match, notify := h.runWatch(t, wf, false)
	if match["bootstrap"] != true || match["alert_count"].(float64) != 0 {
		t.Fatalf("bootstrap run: %v", match["summary"])
	}
	if h.sinkHits.Load() != 0 {
		t.Fatalf("bootstrap must post nothing, sink got %d", h.sinkHits.Load())
	}
	if notify["consume"] != true {
		t.Fatalf("bootstrap tick must consume (persist cursors): %v", notify)
	}

	// ── Run 2: a NEW ordinary critical (no exploitation signal) stays
	// silent; a NEW alert-class advisory naming Metabase posts, with the
	// inventory projects joined deterministically. ──
	h.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-new1-xxxx-yyyy", "CVE-2026-1111", "critical", "testorg/domifa", "leftpad", "2026-08-24T10:00:00Z", "9.9.9"),
		vwDepAlert("GHSA-new1-xxxx-yyyy", "CVE-2026-1111", "critical", "testorg/domifa-migrations", "leftpad", "2026-08-24T10:01:00Z", "9.9.9"),
		vwDepAlert("GHSA-old1-xxxx-yyyy", "CVE-2024-0001", "critical", "testorg/domifa", "oldpkg", "2026-08-01T00:00:00Z", "1.2.3"),
	})
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-777", Title: "Vulnerabilite dans Metabase (exploitee)",
		CVEs: []string{"CVE-2026-2222"}, Products: []string{"Metabase"},
	}})
	match, _ = h.runWatch(t, wf, false)
	if got := match["alert_count"].(float64); got != 1 {
		t.Fatalf("run 2 alert_count = %v, want 1 (only the ALE): %v", got, match["summary"])
	}
	if got := match["observed_count"].(float64); got != 1 {
		t.Fatalf("run 2 observed_count = %v, want 1 (the silent critical)", got)
	}
	alerts := match["alerts"].([]any)
	a0 := alerts[0].(map[string]any)
	if !strings.Contains(fmt.Sprint(a0["signals"]), "ale") {
		t.Fatalf("run 2 alert signal should be alert-class: %v", a0["signals"])
	}
	if h.sinkHits.Load() != 1 {
		t.Fatalf("run 2 should deliver exactly 1 message, sink got %d", h.sinkHits.Load())
	}
	msg := h.sinkBodies[len(h.sinkBodies)-1]
	if !strings.Contains(msg, "CVE-2026-2222") || !strings.Contains(msg, "DOMIFA") {
		t.Fatalf("alert message must carry the CVE and the joined project name:\n%s", msg)
	}

	// ── Run 3: KEV lights up on the OBSERVED critical → re-fire once,
	// with the first-observed note and the Dependabot repos joined. ──
	h.feedItems.Store([]vwFeedItem{})
	h.setKEV([]map[string]any{{
		"cveID": "CVE-2026-1111", "vendorProject": "leftpad", "product": "leftpad",
		"vulnerabilityName": "leftpad code injection", "dateAdded": "2026-08-24",
		"shortDescription": "exploited in the wild",
	}})
	match, _ = h.runWatch(t, wf, false)
	if got := match["refire_count"].(float64); got != 1 {
		t.Fatalf("run 3 refire_count = %v, want 1: %v", got, match["summary"])
	}
	if got := match["alert_count"].(float64); got != 1 {
		t.Fatalf("run 3 alert_count = %v, want 1", got)
	}
	msg = h.sinkBodies[len(h.sinkBodies)-1]
	for _, want := range []string{"CVE-2026-1111", "testorg/domifa", "DOMIFA", "first observed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("re-fire message missing %q:\n%s", want, msg)
		}
	}

	// ── Run 4: steady state — nothing new, nothing re-posts. ──
	before := h.sinkHits.Load()
	match, notify = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 0 || h.sinkHits.Load() != before {
		t.Fatalf("run 4 must be silent: %v (sink %d→%d)", match["summary"], before, h.sinkHits.Load())
	}
	if notify["consume"] != true {
		t.Fatalf("a nothing-to-deliver tick still consumes: %v", notify)
	}

	// The committed alert log carries exactly the two posted alerts.
	logPath := filepath.Join(h.ws, ".vuln-watch", "alertlog.jsonl")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("alertlog: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 2 {
		t.Fatalf("alertlog lines = %d, want 2:\n%s", lines, b)
	}
}

// TestVulnWatch_WordBoundaryMatching pins the false-positive guard: a
// keyword only matches as a whole word/phrase — "go"-style aliases cannot
// fire on Django/MongoDB, and multi-word phrases match case-insensitively.
func TestVulnWatch_WordBoundaryMatching(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.runWatch(t, wf, false) // bootstrap

	h.feedItems.Store([]vwFeedItem{
		{Ref: "CERTFR-2026-ALE-801", Title: "Multiples vulnerabilites dans Django et MongoDB", Products: []string{"Django", "MongoDB"}},
		{Ref: "CERTFR-2026-ALE-802", Title: "Vulnerabilite dans golang", CVEs: []string{"CVE-2026-3333"}, Products: []string{"golang"}},
		{Ref: "CERTFR-2026-ALE-803", Title: "Vulnerabilite dans SPRING BOOT", CVEs: []string{"CVE-2026-4444"}, Products: []string{"Spring Boot"}},
	})
	match, _ := h.runWatch(t, wf, false)
	if got := match["alert_count"].(float64); got != 2 {
		t.Fatalf("alert_count = %v, want 2 (golang + spring boot, NOT Django/MongoDB): %v", got, match["summary"])
	}
	all := fmt.Sprint(match["alerts"])
	if strings.Contains(all, "ALE-801") {
		t.Fatalf("Django/MongoDB advisory must not match the golang keyword: %s", all)
	}
	for _, want := range []string{"ALE-802", "ALE-803"} {
		if !strings.Contains(all, want) {
			t.Fatalf("expected %s in alerts: %s", want, all)
		}
	}
}

// TestVulnWatch_MissingTokenFailsHard pins the explicit-error contract: a
// configured org with no usable Dependabot token fails the poll outright —
// never a silent zero-alert facade.
func TestVulnWatch_MissingTokenFailsHard(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	empty := filepath.Join(h.scratch, "empty-tokens.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "poll_dependabot").Script, map[string]any{
		"orgs": []string{"testorg"}, "api_base": h.srv.URL,
		"timeout_secs": 5, "scratch_dir": h.scratch, "allow_private": true,
		"workspace": h.ws, "state_dir": ".vuln-watch",
	}, nil, map[string]string{"dependabot_tokens": empty}))
	if err == nil {
		t.Fatal("poll_dependabot must fail when a configured org has no token")
	}
	if !strings.Contains(stderr, "no Dependabot token") || !strings.Contains(stderr, "testorg") {
		t.Fatalf("error must name the org and the remediation, got: %s", stderr)
	}
}

// TestVulnWatch_FetchRejectsSSRF pins the strict posture on advisory
// sources: with allow_private=false a loopback feed URL is refused.
func TestVulnWatch_FetchRejectsSSRF(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	_, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "poll_advisories").Script, map[string]any{
		"feeds":        []map[string]any{{"url": h.srv.URL + "/alerte/feed/", "kind": "alert"}},
		"timeout_secs": 5, "scratch_dir": h.scratch,
		"allow_private": false, "workspace": h.ws, "state_dir": ".vuln-watch",
	}, nil, nil))
	if err == nil {
		t.Fatal("loopback feed must be refused under the strict SSRF posture")
	}
	if !strings.Contains(stderr, "SSRF-unsafe") {
		t.Fatalf("expected an SSRF refusal, got: %s", stderr)
	}
}

// TestVulnWatch_DryRunAndNoSinksDoNotConsume pins the at-least-once
// contract: neither a dry-run nor an alert with no sink may advance the
// state — the same alerts must recompute on the next real run.
func TestVulnWatch_DryRunAndNoSinksDoNotConsume(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.runWatch(t, wf, false) // bootstrap
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-901", Title: "Vulnerabilite dans Metabase",
		CVEs: []string{"CVE-2026-5555"}, Products: []string{"Metabase"},
	}})

	// Dry-run: message printed, nothing delivered, nothing consumed.
	match, notify := h.runWatch(t, wf, true)
	if match["alert_count"].(float64) != 1 || notify["consume"] != false || h.sinkHits.Load() != 0 {
		t.Fatalf("dry-run must prepare 1 alert and consume nothing: %v / %v", match["summary"], notify)
	}

	// Same alert recomputes on the next real run (state untouched).
	match, notify = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 || notify["consume"] != true || h.sinkHits.Load() != 1 {
		t.Fatalf("post-dry-run real run must deliver the SAME alert: %v / %v", match["summary"], notify)
	}

	// No sinks: alerts prepared but kept pending, explicitly.
	nb, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "notify").Script, map[string]any{
		"alerts": match["alerts"], "overflow_count": 0, "stale_sources": []any{},
		"sinks": []any{}, "labels": map[string]any{
			"overflow": "x", "stale": "x", "kev_signal": "x", "ale_signal": "x",
			"epss_signal": "x", "sev_signal": "x", "exploited": "x", "alert": "x",
			"signal": "x", "severity": "x", "fix": "x", "no_fix": "x", "projects": "x",
			"repos": "x", "via_dependabot": "x", "sources": "x", "more": "x", "refire": "x",
		}, "dry_run": false,
	}, nil, map[string]string{"webhooks": h.webhooksFile}))
	if err != nil {
		t.Fatalf("notify(no sinks) failed: %v\n%s", err, stderr)
	}
	if nb["consume"] != false {
		t.Fatalf("alerts with no sink must NOT be consumed: %v", nb)
	}
}

// TestVulnWatch_ZeroLLMInvariant pins the privacy/economy guarantee the bot
// sells: the compiled workflow contains NO agent or judge node, so a watch
// tick can neither spend a token nor show a project name to a model.
func TestVulnWatch_ZeroLLMInvariant(t *testing.T) {
	wf := compileFixture(t, "vuln-watch/main.bot")
	for id, node := range wf.Nodes {
		switch node.(type) {
		case *ir.AgentNode, *ir.JudgeNode:
			t.Fatalf("vuln-watch must stay zero-LLM, found LLM node %q (%T)", id, node)
		}
	}
}
