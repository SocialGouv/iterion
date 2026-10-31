package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The deterministic half of supply-shield-cve (Vulny), the CVE sibling of
// supply-shield (Shieldy): same pipeline shape, analysis axis KNOWN
// VULNERABILITIES. The tests pin what no LLM is needed for: the osv-scanner
// parser (the CVE-specific block Shieldy does not carry), the anti-façade
// coverage gate keyed on the trivy+osv floor, and the cache's cve isolation —
// only kind=cve lines are consulted, on a SHORT TTL because a clean-today
// version can gain a CVE tomorrow as advisories land.
//
// Same harness as the Shieldy file: scripts extracted from the COMPILED bot
// and run for real.

// runCveTool substitutes refs into a cve-bot tool command and runs it,
// returning the parsed stdout JSON.
func runCveTool(t *testing.T, dir, cmd string, refs map[string]string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not in PATH — required in CI; skipping here would silently drop the supply-shield-cve pipeline coverage")
		}
		t.Skip("python3 not on PATH — skipping supply-shield-cve pipeline test")
	}
	for k, v := range refs {
		cmd = strings.ReplaceAll(cmd, "{{"+k+"}}", v)
	}
	if i := strings.Index(cmd, "{{"); i >= 0 {
		end := i + 60
		if end > len(cmd) {
			end = len(cmd)
		}
		t.Fatalf("unsubstituted template ref in tool command: %s", cmd[i:end])
	}
	return runShellJSON(t, dir, cmd)
}

// runShellJSON runs a shell command, returning its last stdout line as JSON.
// A non-zero exit fails the test with both streams attached.
func runShellJSON(t *testing.T, dir, cmd string) map[string]any {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
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
	if runErr != nil {
		t.Fatalf("tool command failed: %v\nstdout: %s\nstderr: %s", runErr, stdout.String(), stderr.String())
	}
	return out
}

// TestSupplyShieldCve_GenericHeuristicParsesOsvScanner pins the CVE floor's
// osv-scanner parser (the block Shieldy does not carry): an osv-scanner json
// in the scan dir becomes vuln-db-known signals with the advisory id, the
// group's max_severity (a CVSS float folds to a label) and the aliases — and
// two advisories on one package fold into one entry with summed weights, the
// shape the reviewer's verdicts start from.
func TestSupplyShieldCve_GenericHeuristicParsesOsvScanner(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	scanDir := t.TempDir()
	// The universal floor (trivy + osv-scanner over a bare checkout) is
	// run_generic_heuristics; both scanners are command-guarded and absent
	// here, so the parser reads exactly the seeded osv.json and its own
	// generic.json doubles as the coverage floor artifact it writes.
	tool := shieldNode(t, "supply-shield-cve/main.bot", "run_generic_heuristics")

	osv := map[string]any{
		"results": []any{
			map[string]any{
				"source": map[string]any{"path": "/ws/go.sum"},
				"packages": []any{
					map[string]any{
						"package": map[string]any{"name": "github.com/example/foo", "ecosystem": "Go", "version": "1.0.0"},
						"vulnerabilities": []any{
							map[string]any{"id": "GHSA-aaaa-bbbb-cccc", "summary": "DoS in Parse", "aliases": []any{"CVE-2026-1111"}},
							map[string]any{"id": "GHSA-dddd-eeee-ffff", "summary": "info leak in Marshal"},
						},
						"groups": []any{
							map[string]any{"ids": []any{"GHSA-aaaa-bbbb-cccc"}, "max_severity": "7.5"},
							map[string]any{"ids": []any{"GHSA-dddd-eeee-ffff"}, "max_severity": "LOW"},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(osv)
	writeWSFile(t, scanDir, "osv.json", string(b))

	out := runCveTool(t, ws, tool.Command, map[string]string{
		"vars.scan_dir":      shqs(scanDir),
		"vars.workspace_dir": shqs(ws),
	})
	pkgs, _ := out["packages"].([]any)
	if len(pkgs) != 1 {
		t.Fatalf("packages = %d, want 1 (both advisories fold into their package): %v", len(pkgs), out)
	}
	p0, _ := pkgs[0].(map[string]any)
	if p0["name"] != "github.com/example/foo" || p0["ecosystem"] != "go" || p0["version"] != "1.0.0" {
		t.Fatalf("package identity = %v, want go github.com/example/foo@1.0.0 (ecosystem folded lowercase)", p0)
	}
	sigs, _ := p0["signals"].([]any)
	if len(sigs) != 2 {
		t.Fatalf("signals = %d, want 2", len(sigs))
	}
	weightSum := 0.0
	var sawCVSS, sawLabel bool
	for _, s := range sigs {
		sm, _ := s.(map[string]any)
		if sm["type"] != "vuln-db-known" {
			t.Errorf("signal type = %v, want vuln-db-known", sm["type"])
		}
		w, _ := sm["weight"].(float64)
		weightSum += w
		switch sm["advisory"] {
		case "GHSA-aaaa-bbbb-cccc":
			// max_severity "7.5" folds to HIGH (25); the alias CVE rides the
			// aliases list, the evidence cites the primary id + source.
			if w != 25 || sm["severity"] != "high" {
				t.Errorf("CVSS 7.5 advisory = weight %v severity %v, want 25/high", w, sm["severity"])
			}
			aliases, _ := sm["aliases"].([]any)
			if len(aliases) == 0 || aliases[0] != "CVE-2026-1111" {
				t.Errorf("aliases = %v, want [CVE-2026-1111]", aliases)
			}
			if ev, _ := sm["evidence"].(string); !strings.Contains(ev, "GHSA-aaaa-bbbb-cccc") || !strings.Contains(ev, "/ws/go.sum") {
				t.Errorf("evidence = %q, want the advisory id and the scanned source", ev)
			}
			sawCVSS = true
		case "GHSA-dddd-eeee-ffff":
			if w != 8 || sm["severity"] != "low" {
				t.Errorf("LOW advisory = weight %v severity %v, want 8/low", w, sm["severity"])
			}
			sawLabel = true
		}
	}
	if !sawCVSS || !sawLabel {
		t.Fatalf("missing advisory signals: %v", sigs)
	}
	if score, _ := p0["heuristic_score"].(float64); score != weightSum {
		t.Errorf("heuristic_score = %v, want the signal sum %v", p0["heuristic_score"], weightSum)
	}
}

// TestSupplyShieldCve_CoverageGateRequiresTheCveFloor pins the gate's CVE
// wording: the façade arm hard-fails naming the trivy + osv floor, a floor-only
// run is healthy, and a missing per-ecosystem scanner degrades without failing.
func TestSupplyShieldCve_CoverageGateRequiresTheCveFloor(t *testing.T) {
	t.Parallel()
	tool := shieldNode(t, "supply-shield-cve/main.bot", "coverage_gate")

	// Façade arm: no scan output at all — exit 1, the CVE floor named.
	scanDir := t.TempDir()
	c := exec.Command("sh", "-c", substGate(tool.Command, "[]", scanDir))
	out, err := c.Output()
	if err == nil {
		t.Fatalf("coverage_gate exited 0 on an empty scan dir — the 0-CVE façade is not refused (out: %s)", out)
	}
	var facade map[string]any
	if uerr := json.Unmarshal(out, &facade); uerr != nil {
		t.Fatalf("gate output on the facade arm is not JSON: %v (out %q)", uerr, out)
	}
	if healthy, _ := facade["healthy"].(bool); healthy {
		t.Fatalf("facade arm reads healthy: %v", facade)
	}
	reason := ""
	if missing, _ := facade["missing"].([]any); len(missing) > 0 {
		if m0, ok := missing[0].(map[string]any); ok {
			reason, _ = m0["reason"].(string)
		}
	}
	if !strings.Contains(reason, "trivy + osv") {
		t.Fatalf("facade floor reason = %q, want the trivy + osv floor named", reason)
	}

	// Healthy arm: the floor ran, no deeper ecosystems were in scope.
	writeWSFile(t, scanDir, "generic.json", `{"packages":[]}`)
	healthyOut := runCveTool(t, scanDir, substGate(tool.Command, `[]`, scanDir), nil)
	if healthy, _ := healthyOut["healthy"].(bool); !healthy {
		t.Fatalf("floor-only run reads degraded: %v", healthyOut)
	}

	// Degraded arm: a go change without govulncheck banners, never fails.
	degraded := runCveTool(t, scanDir, substGate(tool.Command, `["gomod"]`, scanDir), nil)
	if healthy, _ := degraded["healthy"].(bool); healthy {
		t.Fatalf("go change without govulncheck reads healthy: %v", degraded)
	}
	if banner, _ := degraded["banner"].(string); !strings.Contains(banner, "govulncheck") {
		t.Fatalf("degraded banner = %q, want govulncheck named", banner)
	}
}

// cveVerdict is the reviewer's per-package verdict shape the cve cache trio
// consumes.
type cveVerdict struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	RiskLevel string `json:"risk_level"`
	Summary   string `json:"summary"`
}

// TestSupplyShieldCve_CacheConsultsOnlyCveLinesOnShortTtl pins the cache
// axis-isolation and the short TTL promise: update_cache writes kind=cve lines
// stamped with advisory_db_date and ttl_days 7, filter_cached trusts only
// kind=cve lines (the malware sibling's verdicts never satisfy a CVE shield),
// and a verdict older than the short TTL is re-inspected.
func TestSupplyShieldCve_CacheConsultsOnlyCveLinesOnShortTtl(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "packages.jsonl")

	upd := shieldNode(t, "supply-shield-cve/main.bot", "update_cache")
	res := runCveTool(t, ws, upd.Command, map[string]string{
		"input.verdicts":        shq([]cveVerdict{{Ecosystem: "npm", Name: "lodash", Version: "4.17.20", RiskLevel: "HIGH", Summary: "prototype pollution"}}),
		"input.cache_path":      shqs(cachePath),
		"input.scanner_version": shqs("0.1.6"),
	})
	if n, _ := res["appended_count"].(float64); n != 1 {
		t.Fatalf("appended_count = %v, want 1 (out: %v)", res["appended_count"], res)
	}

	// The line the bot wrote: kind=cve, the advisory-db stamp, the short TTL.
	b, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &line); err != nil {
		t.Fatalf("cache line not JSON: %v\n%s", err, b)
	}
	if line["kind"] != "cve" {
		t.Fatalf("cache line kind = %v, want cve", line["kind"])
	}
	if ttl, _ := line["ttl_days"].(float64); ttl != 7 {
		t.Fatalf("cache line ttl_days = %v, want 7 (advisories land daily)", line["ttl_days"])
	}
	// The stamp semantic, not this read's wall clock: the bot derives
	// scanned_at and advisory_db_date from one `now` inside the subprocess,
	// so the line must carry its own scan date — and that date is fresh,
	// today or yesterday in UTC (the subprocess span may straddle 00:00 UTC
	// between the write and this read without that failing the test).
	scannedAt, _ := line["scanned_at"].(string)
	if len(scannedAt) < 10 || line["advisory_db_date"] != scannedAt[:10] {
		t.Fatalf("advisory_db_date = %v, want the line's own scan date (scanned_at %q)", line["advisory_db_date"], scannedAt)
	}
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if line["advisory_db_date"] != today && line["advisory_db_date"] != yesterday {
		t.Fatalf("advisory_db_date = %v, want fresh — today %s or yesterday %s in UTC", line["advisory_db_date"], today, yesterday)
	}
	raw := string(b)

	filt := shieldNode(t, "supply-shield-cve/main.bot", "filter_cached")
	dep := []map[string]any{{"ecosystem": "npm", "name": "lodash", "version": "4.17.20"}}

	// Fresh kind=cve line: a hit.
	hit := runCveTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq(dep),
		"input.cache_raw_jsonl": shqs(raw),
		"input.ttl_days":        "7",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ := hit["stats"].(map[string]any)
	if n, _ := stats["cache_hits"].(float64); n != 1 {
		t.Fatalf("fresh cve verdict read pending: %v", hit)
	}

	// A kind=malware line (Shieldy's verdict) must NOT satisfy the CVE shield.
	malLine := `{"kind":"malware","ecosystem":"npm","name":"lodash","version":"4.17.20","scanned_at":"` + nowRFC3339(0) + `","scanner_version":"0.1.6","ttl_days":30}`
	mixed := runCveTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq(dep),
		"input.cache_raw_jsonl": shqs(malLine),
		"input.ttl_days":        "7",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ = mixed["stats"].(map[string]any)
	if n, _ := stats["pending"].(float64); n != 1 {
		t.Fatalf("a kind=malware line satisfied the CVE filter: %v", mixed)
	}

	// A clean-today version can gain a CVE tomorrow: 8 days old inside the
	// 7-day TTL is stale, re-inspected.
	oldLine := `{"kind":"cve","ecosystem":"npm","name":"lodash","version":"4.17.20","scanned_at":"` + nowRFC3339(-8) + `","scanner_version":"0.1.6","ttl_days":7}`
	aged := runCveTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq(dep),
		"input.cache_raw_jsonl": shqs(oldLine),
		"input.ttl_days":        "7",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ = aged["stats"].(map[string]any)
	if n, _ := stats["pending"].(float64); n != 1 {
		t.Fatalf("an 8-day-old cve verdict inside a 7-day TTL was trusted: %v", aged)
	}
}
