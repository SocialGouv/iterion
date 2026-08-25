package e2e

import (
	"bytes"
	"compress/gzip"
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
	"time"

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

// setFloor switches the Dependabot severity floor (exploited | critical |
// high) — the knob that decides whether a signal-less alert may post.
func (h *vulnWatchHarness) setFloor(floor string) error {
	path := filepath.Join(h.ws, "vuln-watch.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg["dependabot_alert_floor"] = floor
	out, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// setFeedKind switches the harness feed between alert-class (an
// exploitation signal by itself) and plain advisory (observe-by-default).
func (h *vulnWatchHarness) setFeedKind(kind string) error {
	path := filepath.Join(h.ws, "vuln-watch.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	feeds := cfg["advisory_feeds"].([]any)
	feeds[0].(map[string]any)["kind"] = kind
	out, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
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
	return h.runWatchOpts(t, wf, dryRun, nil)
}

// runWatchOpts is runWatch with per-run overrides of the match_policy inputs
// (the knobs a deployment tunes).
func (h *vulnWatchHarness) runWatchOpts(t *testing.T, wf *ir.Workflow, dryRun bool, overrides map[string]any) (match, notify map[string]any) {
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

	matchInputs := map[string]any{
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
		// Wide open in tests: the fixtures use fixed historical dates, and
		// the age belt is exercised by its own test below.
		"kev_max_age_days":    36500,
		"observe_window_days": 60, "source_stale_hours": 24,
	}
	for k, v := range overrides {
		matchInputs[k] = v
	}
	match, stderr, err = runPy(t, h.ws, vwSub(t, vwTool(t, wf, "match_policy").Script, matchInputs, nil, nil))
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
	// 2 observed: the new silent critical, plus the bootstrap-boundary alert
	// re-emitted by the STRICT cursor comparison (Dependabot bulk-creates an
	// advisory's alerts within one second, so an inclusive comparison would
	// drop whichever of them the previous snapshot missed). Re-emission is
	// the safe side: it stays silent and nothing is lost.
	if got := match["observed_count"].(float64); got != 2 {
		t.Fatalf("run 2 observed_count = %v, want 2 (silent critical + re-emitted boundary)", got)
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
	_, stderr, err := runPy(t, h.ws, vwSub(t, vwTool(t, wf, "notify").Script, map[string]any{
		"alerts": match["alerts"], "overflow_count": 0, "stale_sources": []any{},
		"sinks": []any{}, "labels": map[string]any{
			"overflow": "x", "stale": "x", "kev_signal": "x", "ale_signal": "x",
			"epss_signal": "x", "sev_signal": "x", "exploited": "x", "alert": "x",
			"signal": "x", "severity": "x", "fix": "x", "no_fix": "x", "fix_see_source": "x",
			"projects": "x", "repos": "x", "via_dependabot": "x", "sources": "x",
			"more": "x", "refire": "x",
		}, "dry_run": false,
	}, nil, map[string]string{"webhooks": h.webhooksFile}))
	// Alerts with nowhere to go are a config gap, and a config gap that exits
	// 0 is the silent-green outcome this bot exists to end.
	if err == nil {
		t.Fatal("notify must FAIL when there are alerts and no sink")
	}
	if !strings.Contains(stderr, "NO sinks are configured") {
		t.Fatalf("the failure must name the config gap, got: %s", stderr)
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

// ── Regressions from the adversarial review ──────────────────────────

// TestVulnWatch_AlertedCVEDoesNotSterilizeOtherProjects pins the worst
// failure a sentinel can have: a MISSED alert. A CERT-FR advisory carries
// hundreds of CVE ids; marking them all "alerted" globally would silence
// every one of them for every OTHER project of the inventory. Here an
// advisory alerts for one techno, and a KEV entry on a DIFFERENT techno
// sharing one of its CVEs must still fire.
func TestVulnWatch_AlertedCVEDoesNotSterilizeOtherProjects(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	// A non-empty catalog at bootstrap: the KEV id set arms from the first
	// catalog that actually loads (an empty one would mark the real
	// catalog as "already seen" the moment it came back).
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	h.runWatch(t, wf, false) // bootstrap

	// Tick 1: an alert-class advisory about Metabase that also lists a CVE
	// which happens to affect Spring Boot (another project entirely).
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-700", Title: "Multiples vulnerabilites dans Metabase",
		CVEs: []string{"CVE-2026-7001", "CVE-2026-7002"}, Products: []string{"Metabase"},
	}})
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("tick 1 should post the Metabase advisory: %v", match["summary"])
	}

	// Tick 2: KEV picks up CVE-2026-7002 and names Spring Boot — ACCOLADE
	// has never been told. It must fire.
	h.feedItems.Store([]vwFeedItem{})
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2026-7002", "vendorProject": "VMware", "product": "Spring Boot",
			"vulnerabilityName": "Spring Boot RCE", "dateAdded": "2026-08-24"},
	})
	match, _ = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("a CVE alerted for one project must still fire for another: %v", match["summary"])
	}
	msg := h.sinkBodies[len(h.sinkBodies)-1]
	if !strings.Contains(msg, "ACCOLADE") || !strings.Contains(msg, "CVE-2026-7002") {
		t.Fatalf("second alert must name the newly-affected project:\n%s", msg)
	}
}

// TestVulnWatch_KEVSecondBatchSameDayStillFires pins the id-set cursor: KEV
// dateAdded has DAY granularity while the bot ticks hourly, so a date
// cursor silently swallowed the rest of the day's batch.
func TestVulnWatch_KEVSecondBatchSameDayStillFires(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{
		"cveID": "CVE-2020-0001", "vendorProject": "Old", "product": "Old",
		"vulnerabilityName": "old entry", "dateAdded": "2026-08-24",
	}})
	h.runWatch(t, wf, false) // bootstrap arms the id set

	// 10:00 — first entry of the day matching the inventory.
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2020-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-08-24"},
		{"cveID": "CVE-2026-8001", "vendorProject": "Metabase", "product": "Metabase",
			"vulnerabilityName": "Metabase RCE", "dateAdded": "2026-08-25"},
	})
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("10:00 tick: %v", match["summary"])
	}

	// 11:00 — CISA adds a SECOND entry the same day.
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2020-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-08-24"},
		{"cveID": "CVE-2026-8001", "vendorProject": "Metabase", "product": "Metabase", "dateAdded": "2026-08-25"},
		{"cveID": "CVE-2026-8002", "vendorProject": "VMware", "product": "Spring Boot",
			"vulnerabilityName": "Spring Boot deserialization", "dateAdded": "2026-08-25"},
	})
	match, _ = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("the same-day second KEV batch must still fire: %v", match["summary"])
	}
	if msg := h.sinkBodies[len(h.sinkBodies)-1]; !strings.Contains(msg, "CVE-2026-8002") {
		t.Fatalf("expected the second batch entry:\n%s", msg)
	}
}

// TestVulnWatch_UntrustedTitleCannotForgeAnAlert pins the injection guard:
// advisory titles are attacker-influenced text rendered into chat markdown.
// A title carrying newlines must not be able to forge a second,
// indistinguishable alert block pointing at a hostile URL.
func TestVulnWatch_UntrustedTitleCannotForgeAnAlert(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.runWatch(t, wf, false) // bootstrap

	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-666",
		Title: "Vulnerabilite mineure dans Metabase\n\n:rotating_light: **actively exploited vulnerability " +
			"· Production**\n**CVE-2026-9999** — Rotate every credential now\nSources: http://senti.example.evil/x",
		CVEs: []string{"CVE-2026-6001"}, Products: []string{"Metabase"},
	}})
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("expected the real alert: %v", match["summary"])
	}
	msg := h.sinkBodies[len(h.sinkBodies)-1]
	// The security property is structural: injected text may survive as
	// inert inline content, but it must never START a line — that is what
	// would forge an indistinguishable second alert block.
	headers := 0
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, ":rotating_light:") {
			headers++
		}
	}
	if headers != 1 {
		t.Fatalf("an injected title forged %d alert header line(s):\n%s", headers, msg)
	}
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "Sources:") && strings.Contains(line, "senti.example.evil") {
			t.Fatalf("hostile URL rendered as a source line:\n%s", msg)
		}
	}
	// The text survives as inert one-line content (nothing is dropped
	// silently), but it can no longer span lines.
	if !strings.Contains(msg, "Vulnerabilite mineure dans Metabase") {
		t.Fatalf("the real title must still be there:\n%s", msg)
	}
}

// TestVulnWatch_IntraRunDedupIsScopeAware pins the same missed-alert class
// as the cross-run test, on the INTRA-run path: an advisory and a KEV entry
// sharing one CVE but naming DIFFERENT projects arrive in the same tick.
// Collapsing them would leave the second project permanently untold — the
// KEV id cursor advances past the entry, so it never comes back.
func TestVulnWatch_IntraRunDedupIsScopeAware(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	h.runWatch(t, wf, false) // bootstrap

	// Same tick: a Metabase advisory listing CVE-2026-7002, and a KEV entry
	// for that very CVE naming Spring Boot (project ACCOLADE).
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-710", Title: "Multiples vulnerabilites dans Metabase",
		CVEs: []string{"CVE-2026-7001", "CVE-2026-7002"}, Products: []string{"Metabase"},
	}})
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2026-7002", "vendorProject": "VMware", "product": "Spring Boot",
			"vulnerabilityName": "Spring Boot RCE", "dateAdded": "2026-08-24"},
	})
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 2 {
		t.Fatalf("both scopes must be told in the same tick: %v", match["summary"])
	}
	joined := strings.Join(h.sinkBodies, "\n")
	if !strings.Contains(joined, "DOMIFA") || !strings.Contains(joined, "ACCOLADE") {
		t.Fatalf("expected both projects named:\n%s", joined)
	}

	// A genuine same-scope duplicate still collapses to one.
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-ALE-711", Title: "Vulnerabilite dans Metabase",
		CVEs: []string{"CVE-2026-7100"}, Products: []string{"Metabase"},
	}})
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2026-7002", "vendorProject": "VMware", "product": "Spring Boot", "dateAdded": "2026-08-24"},
		{"cveID": "CVE-2026-7100", "vendorProject": "Metabase", "product": "Metabase",
			"vulnerabilityName": "Metabase RCE", "dateAdded": "2026-08-25"},
	})
	match, _ = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 || match["suppressed_count"].(float64) != 1 {
		t.Fatalf("same-scope duplicate must still collapse: %v", match["summary"])
	}
}

// TestVulnWatch_NewOrgBacklogDoesNotFlood pins the per-org bootstrap: adding
// a second GitHub org to an ALREADY-armed deployment must arm that org's
// cursor without posting its whole open backlog — the day-one flood that
// teaches everyone to mute the channel.
func TestVulnWatch_NewOrgBacklogDoesNotFlood(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	h.runWatch(t, wf, false) // bootstrap: state armed, org "testorg" cursored

	// The org's historical open alerts, all exploitation-signalled (so only
	// the backlog rule can keep them quiet).
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2024-5001", "vendorProject": "x", "product": "x", "dateAdded": "2026-01-02"},
		{"cveID": "CVE-2024-5002", "vendorProject": "x", "product": "x", "dateAdded": "2026-01-02"},
	})
	h.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-old-0001", "CVE-2024-5001", "critical", "testorg/domifa", "pkg-a", "2024-01-01T00:00:00Z", "1.0.0"),
		vwDepAlert("GHSA-old-0002", "CVE-2024-5002", "high", "testorg/accolade-env", "pkg-b", "2024-01-02T00:00:00Z", "2.0.0"),
	})
	// Simulate "org just added": drop its cursor from the armed state.
	statePath := filepath.Join(h.ws, ".vuln-watch", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	cursors := st["cursors"].(map[string]any)
	cursors["dependabot"] = map[string]any{}
	out, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// The backlog rule silences HISTORY, not EVIDENCE: these two are already
	// in KEV, which is exactly what the bot exists to announce. Swallowing
	// them would be permanent — the org cursor advances on this same tick and
	// no lane re-produces them.
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 2 {
		t.Fatalf("already-exploited backlog alerts must fire: %v", match["summary"])
	}

	// The anti-flood half: with a severity floor, a signal-LESS backlog stays
	// quiet (that is the day-one flood the rule exists to prevent).
	h2 := newVulnWatchHarness(t)
	h2.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	if err := h2.setFloor("critical"); err != nil {
		t.Fatal(err)
	}
	h2.runWatch(t, wf, false) // bootstrap
	h2.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-hist-0001", "CVE-2024-7001", "critical", "testorg/domifa", "pkg-c", "2024-01-01T00:00:00Z", "1.0.0"),
		vwDepAlert("GHSA-hist-0002", "CVE-2024-7002", "critical", "testorg/accolade-env", "pkg-d", "2024-01-02T00:00:00Z", "2.0.0"),
	})
	statePath2 := filepath.Join(h2.ws, ".vuln-watch", "state.json")
	raw2, err := os.ReadFile(statePath2)
	if err != nil {
		t.Fatal(err)
	}
	var st2 map[string]any
	if err := json.Unmarshal(raw2, &st2); err != nil {
		t.Fatal(err)
	}
	st2["cursors"].(map[string]any)["dependabot"] = map[string]any{}
	out2, _ := json.Marshal(st2)
	if err := os.WriteFile(statePath2, out2, 0o644); err != nil {
		t.Fatal(err)
	}
	match2, _ := h2.runWatch(t, wf, false)
	if match2["alert_count"].(float64) != 0 {
		t.Fatalf("a signal-less backlog must stay quiet under a severity floor: %v", match2["summary"])
	}
	if match2["observed_count"].(float64) != 2 {
		t.Fatalf("it must still be observed (re-fireable): %v", match2["summary"])
	}
}

// TestVulnWatch_KEVAgeBeltBoundsAStateLoss pins the blast radius of a lost
// or corrupted KEV cursor: without the belt the next tick replays the whole
// catalog (measured: 83 alerts from 1675 entries on the real one), which is
// the single failure that teaches a team to mute the channel.
func TestVulnWatch_KEVAgeBeltBoundsAStateLoss(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	h.runWatch(t, wf, false) // bootstrap

	// A historical catalogue entry matching the inventory, plus a recent
	// one. A wrecked cursor makes BOTH look unseen.
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2015-1000", "vendorProject": "Metabase", "product": "Metabase",
			"vulnerabilityName": "ancient Metabase flaw", "dateAdded": "2015-06-01"},
		{"cveID": "CVE-2026-1500", "vendorProject": "VMware", "product": "Spring Boot",
			"vulnerabilityName": "recent Spring Boot flaw",
			"dateAdded":         time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")},
	})
	statePath := filepath.Join(h.ws, ".vuln-watch", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	// Armed, but with a bogus id set — the state-loss shape.
	st["cursors"].(map[string]any)["kev_seen"] = []string{"CVE-0000-0000"}
	out, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// Default belt (90 days) — the harness override is dropped for this run.
	match, _ := h.runWatchOpts(t, wf, false, map[string]any{"kev_max_age_days": 90})
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("only the recent entry may fire: %v", match["summary"])
	}
	if msg := h.sinkBodies[len(h.sinkBodies)-1]; !strings.Contains(msg, "CVE-2026-1500") {
		t.Fatalf("expected the recent KEV entry, got:\n%s", msg)
	}
}

// TestVulnWatch_RefireScanIsScopeAware pins the THIRD suppression site (with
// already_alerted and the intra-run dedup): a STORED observed unit must not
// be sterilised because one of its CVEs was alerted for a different project.
func TestVulnWatch_RefireScanIsScopeAware(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	h.runWatch(t, wf, false) // bootstrap

	// Tick 1: a Metabase advisory (kind advisory → observed, not alerted)
	// listing two CVEs. It is STORED as an observed unit for DOMIFA.
	h.feedItems.Store([]vwFeedItem{{
		Ref: "CERTFR-2026-AVI-900", Title: "Multiples vulnerabilites dans Metabase",
		CVEs: []string{"CVE-2026-9001", "CVE-2026-9002"}, Products: []string{"Metabase"},
	}})
	if err := h.setFeedKind("advisory"); err != nil {
		t.Fatal(err)
	}
	match, _ := h.runWatch(t, wf, false)
	if match["observed_count"].(float64) != 1 {
		t.Fatalf("tick 1 should observe the advisory: %v", match["summary"])
	}

	// Tick 2, capped at ONE alert: the Metabase advisory (now KEV-signalled)
	// and a KEV entry for one of its CVEs naming Spring Boot both qualify.
	// One posts, the other is DEFERRED — and the overflow notice promises it
	// fires next run.
	h.feedItems.Store([]vwFeedItem{})
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2026-9001", "vendorProject": "VMware", "product": "Spring Boot",
			"vulnerabilityName": "Spring Boot RCE", "dateAdded": "2026-08-24"},
	})
	match, _ = h.runWatchOpts(t, wf, false, map[string]any{"max_alerts": 1})
	if match["alert_count"].(float64) != 1 || match["overflow_count"].(float64) != 1 {
		t.Fatalf("tick 2 should post one and defer one: %v", match["summary"])
	}

	// Tick 3, cap lifted, nothing new: the deferred unit MUST fire. A
	// scope-blind re-fire guard drops it — one of its CVEs is now globally
	// "alerted" through its sibling — and the promise silently breaks.
	match, _ = h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("the deferred unit must fire on the next tick: %v", match["summary"])
	}
	joined := strings.Join(h.sinkBodies, "\n")
	if !strings.Contains(joined, "DOMIFA") || !strings.Contains(joined, "ACCOLADE") {
		t.Fatalf("both scopes must end up told:\n%s", joined)
	}
}

// TestVulnWatch_PushConflictKeepsOperatorWork pins the data-safety rule:
// commit_state runs in the operator's own checkout (worktree: none), so a
// failed rebase may drop ONLY the bot's own commit — never their
// uncommitted work or unpushed commits.
func TestVulnWatch_PushConflictKeepsOperatorWork(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	bare := filepath.Join(dir, "origin.git")
	run := func(wd string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = wd
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(dir, "init", "--bare", "-b", "main", bare)
	run(dir, "clone", bare, ws)
	// The bot's own `git commit` runs with the ambient environment, and CI
	// has no global identity — a real workspace does, so configure it here.
	run(ws, "config", "user.email", "senti@example.test")
	run(ws, "config", "user.name", "Senti Test")
	if err := os.WriteFile(filepath.Join(ws, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(ws, "add", "seed.txt")
	run(ws, "commit", "-m", "seed")
	run(ws, "push", "origin", "main")

	// A second clone pushes a CONFLICTING change to the same log file.
	other := filepath.Join(dir, "other")
	run(dir, "clone", bare, other)
	run(other, "config", "user.email", "other@example.test")
	run(other, "config", "user.name", "Other Writer")
	if err := os.MkdirAll(filepath.Join(other, ".vuln-watch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".vuln-watch", "state.json"), []byte(`{"tick":99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run(other, "add", ".")
	run(other, "commit", "-m", "other writer")
	run(other, "push", "origin", "main")

	// The operator has uncommitted work in THEIR checkout.
	precious := filepath.Join(ws, "operator-wip.txt")
	if err := os.WriteFile(precious, []byte("hours of work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// … and an unpushed commit.
	if err := os.WriteFile(filepath.Join(ws, "operator-commit.txt"), []byte("also mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(ws, "add", "operator-commit.txt")
	run(ws, "commit", "-m", "operator's own commit")

	// The bot writes its state and tries to push into that conflict.
	scratch := t.TempDir()
	stateNext := filepath.Join(scratch, "state_next.json")
	if err := os.WriteFile(stateNext, []byte(`{"tick":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	alertlog := filepath.Join(scratch, "alertlog_delta.jsonl")
	if err := os.WriteFile(alertlog, []byte(`{"id":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runPy(t, ws, vwSub(t, vwTool(t, wf, "commit_state").Script, map[string]any{
		"state_next_file": stateNext, "alertlog_file": alertlog,
		"state_commit": true, "workspace": ws, "state_dir": ".vuln-watch",
	}, nil, nil))
	if err == nil {
		t.Skip("the push succeeded — no conflict to assert against on this git version")
	}
	if !strings.Contains(stderr, "vuln-watch:") {
		t.Fatalf("expected an explicit vuln-watch failure, got: %s", stderr)
	}
	// Whatever happened to the bot's commit, the operator's work is intact.
	if b, rerr := os.ReadFile(precious); rerr != nil || string(b) != "hours of work\n" {
		t.Fatalf("the operator's uncommitted file was destroyed (err %v, content %q)", rerr, string(b))
	}
	log := exec.Command("git", "log", "--oneline")
	log.Dir = ws
	out, _ := log.CombinedOutput()
	if !strings.Contains(string(out), "operator's own commit") {
		t.Fatalf("the operator's unpushed commit was destroyed:\n%s", out)
	}
}

// TestVulnWatch_ProjectLabelsAreNotShifted pins the routing promise: an
// org-wide Dependabot poll returns alerts for repos ABSENT from the
// inventory, whose project key is nil. Zipping a filtered name list against
// the unfiltered key list shifted the labels and named the WRONG team.
func TestVulnWatch_ProjectLabelsAreNotShifted(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	h := newVulnWatchHarness(t)
	h.setKEV([]map[string]any{{"cveID": "CVE-2019-0001", "vendorProject": "Old",
		"product": "Old", "dateAdded": "2026-01-01"}})
	// Seed the bootstrap with an OLD alert so the per-org cursor lands in the
	// past — otherwise it arms at "now" and the fixture's dated alerts sit
	// behind it.
	h.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-seed-0001", "CVE-2020-0001", "low", "testorg/domifa", "seed", "2026-08-01T00:00:00Z", "1.0.0"),
	})
	h.runWatch(t, wf, false) // bootstrap

	// One advisory across three repos, the FIRST of which the inventory does
	// not know (project key nil) — the shifting case.
	h.depAlerts.Store([]map[string]any{
		vwDepAlert("GHSA-shift-0001", "CVE-2026-4242", "critical", "testorg/not-inventoried", "pkg", "2026-08-24T10:00:00Z", "3.0.0"),
		vwDepAlert("GHSA-shift-0001", "CVE-2026-4242", "critical", "testorg/domifa", "pkg", "2026-08-24T10:01:00Z", "3.0.0"),
		vwDepAlert("GHSA-shift-0001", "CVE-2026-4242", "critical", "testorg/accolade-env", "pkg", "2026-08-24T10:02:00Z", "3.0.0"),
	})
	h.setKEV([]map[string]any{
		{"cveID": "CVE-2019-0001", "vendorProject": "Old", "product": "Old", "dateAdded": "2026-01-01"},
		{"cveID": "CVE-2026-4242", "vendorProject": "x", "product": "x",
			"vulnerabilityName": "exploited", "dateAdded": "2026-08-24"},
	})
	match, _ := h.runWatch(t, wf, false)
	if match["alert_count"].(float64) != 1 {
		t.Fatalf("expected one grouped alert: %v", match["summary"])
	}
	msg := h.sinkBodies[len(h.sinkBodies)-1]
	// Both real projects must be named by their OWN label, and the raw key
	// must never leak in place of a name.
	if !strings.Contains(msg, "DOMIFA") || !strings.Contains(msg, "ACCOLADE") {
		t.Fatalf("both inventoried projects must be named:\n%s", msg)
	}
	if strings.Contains(msg, "accolade,") || strings.Contains(msg, " domifa") {
		t.Fatalf("a raw project key leaked instead of its label:\n%s", msg)
	}
}

// TestVulnWatch_RefusesADecompressionBomb pins the bounded inflate: a feed
// (or the enrichment URL the feed itself names) can serve a small gzip body
// that expands to gigabytes, killing the run or the pod.
func TestVulnWatch_RefusesADecompressionBomb(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	wf := compileFixture(t, "vuln-watch/main.bot")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// 512 MiB of zeros → a few hundred KiB compressed.
	chunk := make([]byte, 1<<20)
	for i := 0; i < 512; i++ {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	bomb := buf.Bytes()
	t.Logf("bomb: %d compressed bytes → 512 MiB inflated", len(bomb))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(bomb)
	}))
	defer srv.Close()

	scratch := t.TempDir()
	out, stderr, err := runPy(t, t.TempDir(), vwSub(t, vwTool(t, wf, "poll_advisories").Script, map[string]any{
		"feeds":        []map[string]any{{"url": srv.URL + "/feed/", "kind": "alert"}},
		"timeout_secs": 10, "scratch_dir": scratch, "allow_private": true,
		"workspace": t.TempDir(), "state_dir": ".vuln-watch",
	}, nil, nil))
	// Either the node refuses outright (all feeds failed) or it records the
	// refusal per-feed — never a multi-gigabyte allocation.
	if err == nil {
		errs, _ := json.Marshal(out["errors"])
		if !strings.Contains(string(errs), "inflates beyond") {
			t.Fatalf("expected a bounded-inflate refusal, got errors=%s", errs)
		}
		return
	}
	if !strings.Contains(stderr, "inflates beyond") {
		t.Fatalf("expected a bounded-inflate refusal, got: %s", stderr)
	}
}
