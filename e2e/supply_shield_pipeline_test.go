package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterruntime "github.com/SocialGouv/iterion/pkg/runtime"
)

// The deterministic half of supply-shield (Shieldy): diff_scope, normalize_deps,
// coverage_gate, the load_cache/filter_cached/update_cache trio and sarif_gen
// are tool/compute nodes with no LLM in the path — the bot's structural
// promises live here. The reviewer's verdicts (what IS malware) are the LLM's
// call and stay with the live layer; these tests pin everything around them:
// scoping, shape coercion, the anti-façade gate, cache dedup (incl. the
// republish guard) and the SARIF emission.
//
// The scripts are extracted from the COMPILED bot and run for real, the same
// harness as sec-audit-deps' heuristics (runDepHeuristic) and feed-watch
// (runPy/subScript).

// shieldNode extracts a tool node from a compiled shield-family bot.
func shieldNode(t *testing.T, botRel, nodeID string) *ir.ToolNode {
	t.Helper()
	wf := compileFixture(t, botRel)
	node, ok := wf.Nodes[nodeID]
	if !ok {
		t.Fatalf("workflow missing %s node", nodeID)
	}
	tool, ok := node.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is not a ToolNode (got %T)", nodeID, node)
	}
	return tool
}

// shq renders a STRUCTURED input value the way the engine substitutes it into
// a script tool command: JSON literal, then single-quote shell escaping
// (executor_tool.go shellEscape). A value pasted raw would let shell
// metacharacters split the command — the tests must reproduce the engine's
// quoting, not a friendlier shape.
func shq(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		tStr := ""
		if s, ok := v.(string); ok {
			tStr = s
		}
		return "'" + strings.ReplaceAll(tStr, "'", `'\''`) + "'"
	}
	return "'" + strings.ReplaceAll(string(b), "'", `'\''`) + "'"
}

// shqs renders a plain vars value: the string itself, single-quote escaped —
// vars are strings, never JSON literals.
func shqs(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runShieldTool substitutes the given refs into a tool command and runs it
// under sh -c, returning the parsed JSON the command printed on stdout.
func runShieldTool(t *testing.T, dir, cmd string, refs map[string]string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not in PATH — required in CI; skipping here would silently drop the supply-shield pipeline coverage")
		}
		t.Skip("python3 not on PATH — skipping supply-shield pipeline test")
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

// shieldSkillsDir is the bundle-skills value the tests substitute: the path
// the engine would own (the scripts refuse a relative one). The skill files
// themselves are absent outside an engine run, so every reader falls back to
// its in-script defaults — exactly the cold-start shape.
func shieldSkillsDir(t *testing.T, ws string) string {
	t.Helper()
	return iterruntime.OwnedSkillsDir(ws)
}

// shieldGitRepo seeds a two-commit git repo: base has package-lock.json v1
// and README; head bumps the lockfile and touches README. Returns (dir, baseSha).
func shieldGitRepo(t *testing.T) (string, string) {
	t.Helper()
	ws := t.TempDir()
	gitRun(t, ws, "init")
	gitRun(t, ws, "config", "user.email", "shield-e2e@example.com")
	gitRun(t, ws, "config", "user.name", "shield-e2e")
	writeWSFile(t, ws, "package-lock.json", `{"lockfileVersion":1,"dependencies":{"leftpad":{"version":"1.0.0"}}}`)
	writeWSFile(t, ws, "README.md", "seed\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "seed")
	out := gitOut(t, ws, "rev-parse", "HEAD")
	writeWSFile(t, ws, "package-lock.json", `{"lockfileVersion":1,"dependencies":{"leftpad":{"version":"2.0.0"}}}`)
	writeWSFile(t, ws, "README.md", "seed\n+ drift\n")
	gitRun(t, ws, "add", "-A")
	gitRun(t, ws, "commit", "-m", "bump leftpad")
	return ws, out
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

func writeWSFile(t *testing.T, ws, rel, body string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestSupplyShield_DiffScopeScopesToChangedLockfiles pins diff_scope's two
// modes on a real repo: diff mode reports exactly the changed lockfiles with
// their unified diff (the enumerator's scoping input), full mode hands the
// enumerator an empty diff and says so — a full-tree audit must never read
// as "nothing changed".
func TestSupplyShield_DiffScopeScopesToChangedLockfiles(t *testing.T) {
	t.Parallel()
	ws, base := shieldGitRepo(t)
	tool := shieldNode(t, "supply-shield/main.bot", "diff_scope")
	refs := map[string]string{
		"vars.workspace_dir":      shqs(ws),
		"vars.bundle_skills_dir":  shqs(shieldSkillsDir(t, ws)),
	}

	// diff mode with an explicit base: the lockfile bump is the one change
	// that scopes, README drift does not.
	out := runShieldTool(t, ws, tool.Command, map[string]string{
		"vars.scope_mode": shqs("diff"),
		"vars.base_ref":   shqs(base),
		"vars.head_ref":   shqs("HEAD"),
		"vars.workspace_dir":      refs["vars.workspace_dir"],
		"vars.bundle_skills_dir":  refs["vars.bundle_skills_dir"],
	})
	changed, _ := out["changed_lockfiles"].([]any)
	if len(changed) != 1 || changed[0] != "package-lock.json" {
		t.Fatalf("changed_lockfiles = %v, want [package-lock.json] (out: %v)", changed, out)
	}
	diffText, _ := out["diff_text"].(string)
	if !strings.Contains(diffText, "2.0.0") {
		t.Fatalf("diff_text should carry the lockfile bump, got %q", diffText)
	}
	if strings.Contains(diffText, "README") {
		t.Fatalf("diff_text leaked a non-lockfile change: %q", diffText)
	}
	if s, _ := out["summary"].(string); !strings.Contains(s, "1 changed lockfile") {
		t.Fatalf("summary = %q, want the changed-lockfile count", s)
	}

	// full mode: empty diff, the scope named — the enumerator walks the tree.
	full := runShieldTool(t, ws, tool.Command, map[string]string{
		"vars.scope_mode": shqs("full"),
		"vars.base_ref":   shqs(base),
		"vars.head_ref":   shqs("HEAD"),
		"vars.workspace_dir":      refs["vars.workspace_dir"],
		"vars.bundle_skills_dir":  refs["vars.bundle_skills_dir"],
	})
	if m, _ := full["scope_mode"].(string); m != "full" {
		t.Fatalf("scope_mode = %v, want full", full["scope_mode"])
	}
	if d, _ := full["diff_text"].(string); d != "" {
		t.Fatalf("full mode must emit an empty diff, got %q", d)
	}
	if s, _ := full["summary"].(string); !strings.Contains(s, "full-tree") {
		t.Fatalf("full mode summary = %q, want the whole-tree scope note", s)
	}

	// auto-detect: no base given, the script falls back through the origin
	// refs to HEAD~1 — both commits' worth of history is not needed, one
	// parent is enough to scope the bump.
	auto := runShieldTool(t, ws, tool.Command, map[string]string{
		"vars.scope_mode": shqs("diff"),
		"vars.base_ref":   shqs(""),
		"vars.head_ref":   shqs("HEAD"),
		"vars.workspace_dir":      refs["vars.workspace_dir"],
		"vars.bundle_skills_dir":  refs["vars.bundle_skills_dir"],
	})
	if autoChanged, _ := auto["changed_lockfiles"].([]any); len(autoChanged) != 1 {
		t.Fatalf("auto-detected base found %v, want the same single lockfile", auto["changed_lockfiles"])
	}
}

// TestSupplyShield_NormalizeDepsCoercesEnumeratorShapes pins normalize_deps'
// contract: whatever shape the enumerator emitted (objects, {eco: [name@ver]}
// maps, bare strings), the canonical [{ecosystem,name,version,checksum}] comes
// out deduped, with install-path prefixes stripped — the cache key is derived
// AFTER this node, so a shape the coercion misses is a dedup miss.
func TestSupplyShield_NormalizeDepsCoercesEnumeratorShapes(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	tool := shieldNode(t, "supply-shield/main.bot", "normalize_deps")

	// Array of objects (the common shape), with an install-path prefix and a
	// duplicate entry the dedup must fold.
	objs := runShieldTool(t, ws, tool.Command, map[string]string{
		"input.deps": shq([]any{
			map[string]any{"ecosystem": "npm", "name": "node_modules/left-pad", "version": "1.0.0", "checksum": "sha512-aaa"},
			map[string]any{"ecosystem": "npm", "name": "left-pad", "version": "1.0.0", "checksum": "sha512-aaa"},
			map[string]any{"package": "requests", "version": "2.31.0", "integrity": "sha256-bbb"},
		}),
		"input.ecosystems": shq([]any{"npm", "pypi"}),
	})
	deps, _ := objs["deps"].([]any)
	if len(deps) != 2 {
		t.Fatalf("dedup left %d entries, want 2 (node_modules/ prefix stripped, duplicate folded): %v", len(deps), objs)
	}
	first, _ := deps[0].(map[string]any)
	if first["name"] != "left-pad" || first["ecosystem"] != "npm" || first["version"] != "1.0.0" {
		t.Fatalf("first entry = %v, want npm left-pad@1.0.0 with the prefix stripped", first)
	}

	// {ecosystem: ["name@ver", {obj}]} map — the ecosystem rides the key.
	byEco := runShieldTool(t, ws, tool.Command, map[string]string{
		"input.deps":       shq(map[string]any{"npm": []any{"zod@3.0.0", map[string]any{"name": "chalk", "version": "5.0.0"}}}),
		"input.ecosystems": shq([]any{"npm"}),
	})
	deps, _ = byEco["deps"].([]any)
	if len(deps) != 2 {
		t.Fatalf("by-ecosystem map produced %d entries, want 2: %v", len(deps), byEco)
	}
	var zodEco any = ""
	for _, d := range deps {
		dm, _ := d.(map[string]any)
		if dm["name"] == "zod" {
			zodEco = dm["ecosystem"]
			if dm["version"] != "3.0.0" {
				t.Fatalf("zod version = %v, want 3.0.0 parsed from name@ver", dm["version"])
			}
		}
	}
	if zodEco != "npm" {
		t.Fatalf("zod ecosystem = %v, want npm (the map key)", zodEco)
	}

	// Bare "name@version" strings take the sole declared ecosystem.
	strs := runShieldTool(t, ws, tool.Command, map[string]string{
		"input.deps":       shq([]any{"uvicorn@0.29.0"}),
		"input.ecosystems": shq([]any{"pypi"}),
	})
	deps, _ = strs["deps"].([]any)
	if len(deps) != 1 {
		t.Fatalf("string shape produced %d entries, want 1: %v", len(deps), strs)
	}
	d0, _ := deps[0].(map[string]any)
	if d0["ecosystem"] != "pypi" || d0["name"] != "uvicorn" || d0["version"] != "0.29.0" {
		t.Fatalf("string-shape entry = %v, want pypi uvicorn@0.29.0", d0)
	}

	// The declared ecosystems come out deduped in first-seen order; alias
	// folding (javascript -> js) is NOT this node's job — coverage_gate and
	// the heuristics own their alias maps.
	dup := runShieldTool(t, ws, tool.Command, map[string]string{
		"input.deps":       shq([]any{}),
		"input.ecosystems": shq([]any{"pypi", "pypi", "npm"}),
	})
	ecos, _ := dup["ecosystems"].([]any)
	if len(ecos) != 2 || ecos[0] != "pypi" || ecos[1] != "npm" {
		t.Fatalf("ecosystems = %v, want [pypi npm] (deduped, order kept)", ecos)
	}
}

// TestSupplyShield_CoverageGateRefusesTheFacade pins the anti-façade gate:
// a run whose always-on floor produced no output must FAIL (exit 1) — a
// missing scanner never reads as "0 malware found" — while a run with the
// floor but missing deeper analyzers is degraded (banner, healthy=false) and
// still useful. Breaking either arm is how an unscanned repo ships green.
func TestSupplyShield_CoverageGateRefusesTheFacade(t *testing.T) {
	t.Parallel()
	tool := shieldNode(t, "supply-shield/main.bot", "coverage_gate")

	// The facade arm: nothing in scan_dir — the gate hard-fails.
	scanDir := t.TempDir()
	c := exec.Command("sh", "-c", substGate(tool.Command, "[]", scanDir))
	out, err := c.Output()
	if err == nil {
		t.Fatalf("coverage_gate exited 0 on an empty scan dir — the 0-finding façade is not refused (out: %s)", out)
	}
	var facade map[string]any
	if uerr := json.Unmarshal(out, &facade); uerr != nil {
		t.Fatalf("gate output on the facade arm is not JSON: %v (out %q)", uerr, out)
	}
	if healthy, _ := facade["healthy"].(bool); healthy {
		t.Fatalf("facade arm reads healthy: %v", facade)
	}
	if banner, _ := facade["banner"].(string); !strings.Contains(banner, "façade guard") {
		t.Fatalf("facade banner = %q, want the façade-guard wording", banner)
	}

	// The degraded arm: the floor ran, deeper analyzers did not — the run
	// proceeds but is never presented as clean. The ecosystem is spelled
	// "javascript" on purpose: the gate's alias map must fold it to js
	// before looking up the expected analyzers.
	scanDir = t.TempDir()
	writeWSFile(t, scanDir, "generic.json", `{"packages":[]}`)
	degraded := runShieldTool(t, scanDir, substGate(tool.Command, `["javascript"]`, scanDir), nil)
	if healthy, _ := degraded["healthy"].(bool); healthy {
		t.Fatalf("npm run without jsxray/npm-audit reads healthy: %v", degraded)
	}
	missing, _ := degraded["missing"].([]any)
	if len(missing) != 2 {
		t.Fatalf("degraded arm lists %d missing analyzers, want the js pair (jsxray + npm-audit): %v", len(missing), degraded)
	}
	var bannerNamesFloorGap bool
	if banner, _ := degraded["banner"].(string); strings.Contains(banner, "js-x-ray") {
		bannerNamesFloorGap = true
	}
	if !bannerNamesFloorGap {
		t.Fatalf("degraded banner should name the js-x-ray gap, got %q", degraded["banner"])
	}

	// The healthy arm: the floor covers the run's ecosystems.
	healthyOut := runShieldTool(t, scanDir, substGate(tool.Command, `[]`, scanDir), nil)
	if healthy, _ := healthyOut["healthy"].(bool); !healthy {
		t.Fatalf("floor-only run with no deeper ecosystems expected reads degraded: %v", healthyOut)
	}
}

// substGate substitutes coverage_gate's two inputs (ecosystems: json,
// scan_dir: string) the way the engine would.
func substGate(cmd, ecos, scanDir string) string {
	cmd = strings.ReplaceAll(cmd, "{{input.ecosystems}}", shq(json.RawMessage(ecos)))
	return strings.ReplaceAll(cmd, "{{input.scan_dir}}", shqs(scanDir))
}

// verdict is the reviewer's per-package verdict shape sarif_gen and the cache
// trio consume.
type verdict struct {
	Ecosystem    string         `json:"ecosystem"`
	Name         string         `json:"name"`
	Version      string         `json:"version"`
	Checksum     string         `json:"checksum,omitempty"`
	RiskScore    int            `json:"risk_score"`
	RiskLevel    string         `json:"risk_level"`
	Summary      string         `json:"summary"`
	Flags        []verdictFlag  `json:"flags,omitempty"`
	FilesAudited []string       `json:"files_audited,omitempty"`
}

type verdictFlag struct {
	Type         string `json:"type"`
	Description  string `json:"description"`
	EvidenceFile string `json:"evidence_file,omitempty"`
	EvidenceLine int    `json:"evidence_line,omitempty"`
}

// TestSupplyShield_CacheRoundTripDedupTTLAndRepublish walks the cache trio
// the way a second run meets a first one's verdicts: update_cache appends one
// kind=malware line per package, load_cache hands the raw lines to
// filter_cached, and the filter splits fresh hits from stale/version-skewed/
// republished pending — the republish guard being the supply-chain promise:
// the same name@version with DIFFERENT bytes is re-inspected, never trusted.
func TestSupplyShield_CacheRoundTripDedupTTLAndRepublish(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "packages.jsonl")

	// Run 1: two verdicts land in the cache.
	upd := shieldNode(t, "supply-shield/main.bot", "update_cache")
	verdicts := []verdict{
		{Ecosystem: "npm", Name: "left-pad", Version: "1.0.0", Checksum: "sha512-aaa", RiskLevel: "LOW", Summary: "clean"},
		{Ecosystem: "npm", Name: "evil", Version: "2.0.0", Checksum: "sha512-bbb", RiskLevel: "HIGH", Summary: "install hook exfil"},
	}
	res := runShieldTool(t, ws, upd.Command, map[string]string{
		"input.verdicts":        shq(verdicts),
		"input.cache_path":      shqs(cachePath),
		"input.scanner_version": shqs("0.1.6"),
	})
	if n, _ := res["appended_count"].(float64); n != 2 {
		t.Fatalf("appended_count = %v, want 2 (out: %v)", res["appended_count"], res)
	}

	load := shieldNode(t, "supply-shield/main.bot", "load_cache")
	loaded := runShieldTool(t, ws, load.Command, map[string]string{
		"vars.cache_dir":  shqs(cacheDir),
		"vars.cache_path": shqs(cachePath),
	})
	if n, _ := loaded["line_count"].(float64); n != 2 {
		t.Fatalf("line_count = %v, want 2", loaded["line_count"])
	}
	raw, _ := loaded["raw_jsonl"].(string)

	// Run 2, same versions, same bytes: both cache-hit.
	filt := shieldNode(t, "supply-shield/main.bot", "filter_cached")
	deps := []map[string]any{
		{"ecosystem": "npm", "name": "left-pad", "version": "1.0.0", "checksum": "sha512-aaa"},
		{"ecosystem": "npm", "name": "evil", "version": "2.0.0", "checksum": "sha512-bbb"},
	}
	hit := runShieldTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq(deps),
		"input.cache_raw_jsonl": shqs(raw),
		"input.ttl_days":        "30",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ := hit["stats"].(map[string]any)
	if n, _ := stats["cache_hits"].(float64); n != 2 {
		t.Fatalf("cache_hits = %v, want 2 (out: %v)", stats["cache_hits"], hit)
	}

	// A kind=cve line (the CVE sibling's verdicts) must NOT satisfy the
	// malware shield: the axes are different caches.
	cveLine := `{"kind":"cve","ecosystem":"npm","name":"fresh-pkg","version":"9.9.9","scanned_at":"` + nowRFC3339(0) + `","scanner_version":"0.1.6","ttl_days":7}`
	mixed := runShieldTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq([]map[string]any{{"ecosystem": "npm", "name": "fresh-pkg", "version": "9.9.9"}}),
		"input.cache_raw_jsonl": shqs(cveLine),
		"input.ttl_days":        "30",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ = mixed["stats"].(map[string]any)
	if n, _ := stats["pending"].(float64); n != 1 {
		t.Fatalf("a kind=cve line satisfied the malware filter: %v", mixed)
	}

	// Staleness: a verdict older than the TTL is re-inspected.
	stale := `{"kind":"malware","ecosystem":"npm","name":"old-pkg","version":"1.0.0","scanned_at":"` + nowRFC3339(-60) + `","scanner_version":"0.1.6","ttl_days":30}`
	aged := runShieldTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq([]map[string]any{{"ecosystem": "npm", "name": "old-pkg", "version": "1.0.0"}}),
		"input.cache_raw_jsonl": shqs(stale),
		"input.ttl_days":        "30",
		"input.scanner_version": shqs("0.1.6"),
	})
	stats, _ = aged["stats"].(map[string]any)
	if n, _ := stats["pending"].(float64); n != 1 {
		t.Fatalf("a 60-day-old verdict inside a 30-day TTL was trusted: %v", aged)
	}

	// Republish: same name@version, different checksum — re-inspected and
	// flagged with both halves, the filter's own signal to the reviewer.
	republished := `{"kind":"malware","ecosystem":"npm","name":"evil","version":"2.0.0","checksum":"sha512-bbb","scanned_at":"` + nowRFC3339(0) + `","scanner_version":"0.1.6","ttl_days":30}`
	forged := runShieldTool(t, ws, filt.Command, map[string]string{
		"input.deps":            shq([]map[string]any{{"ecosystem": "npm", "name": "evil", "version": "2.0.0", "checksum": "sha512-EVIL"}}),
		"input.cache_raw_jsonl": shqs(republished),
		"input.ttl_days":        "30",
		"input.scanner_version": shqs("0.1.6"),
	})
	pending, _ := forged["pending"].([]any)
	if len(pending) != 1 {
		t.Fatalf("republished package was trusted from cache: %v", forged)
	}
	p0, _ := pending[0].(map[string]any)
	chk, _ := p0["_checksum_changed"].(map[string]any)
	if chk == nil || chk["cached"] != "sha512-bbb" || chk["current"] != "sha512-EVIL" {
		t.Fatalf("republish flag = %v, want cached+current checksums", p0["_checksum_changed"])
	}
}

// nowRFC3339 renders now+offsetDays the way the cache lines carry scanned_at
// (naive UTC ISO, the fromisoformat shape the filter parses).
func nowRFC3339(offsetDays int) string {
	return time.Now().UTC().AddDate(0, 0, offsetDays).Format("2006-01-02T15:04:05")
}

// TestSupplyShield_SarifGenMapsVerdicts pins sarif_gen's emission: one result
// per flag with the rule id namespaced to the bot, severity mapped to SARIF
// levels, the audited file (and line) as the result location — code-scanning
// renders exactly what this file holds, so a shape drifted here is a report
// the operator cannot place.
func TestSupplyShield_SarifGenMapsVerdicts(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	sarifPath := filepath.Join(t.TempDir(), "code", "findings.sarif")
	tool := shieldNode(t, "supply-shield/main.bot", "sarif_gen")
	cmd := strings.ReplaceAll(tool.Command, "{{vars.sarif_dir}}", shq(filepath.Dir(sarifPath)))
	out := runShieldTool(t, ws, cmd, map[string]string{
		"input.verdicts":  shq([]verdict{
			{
				Ecosystem: "npm", Name: "evil", Version: "2.0.0", RiskLevel: "HIGH",
				Summary: "install hook exfil",
				Flags: []verdictFlag{
					{Type: "install-hook", Description: "postinstall opens a socket", EvidenceFile: "node_modules/evil/index.js", EvidenceLine: 3},
				},
				FilesAudited: []string{"node_modules/evil/package.json"},
			},
			{Ecosystem: "npm", Name: "quiet", Version: "1.1.0", RiskLevel: "LOW", Summary: "nothing conclusive"},
		}),
		"input.sarif_path": shqs(sarifPath),
	})
	if n, _ := out["result_count"].(float64); n != 2 {
		t.Fatalf("result_count = %v, want 2 (a flagless verdict still reports its summary)", out["result_count"])
	}
	b, err := os.ReadFile(sarifPath)
	if err != nil {
		t.Fatalf("read sarif: %v", err)
	}
	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name    string `json:"name"`
					Rules   []struct{ ID string `json:"id"` } `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleId  string `json:"ruleId"`
				Level   string `json:"level"`
				Message struct{ Text string `json:"text"` } `json:"message"`
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI string `json:"uri"` } `json:"artifactLocation"`
						Region           struct{ StartLine int `json:"startLine"` } `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("sarif not valid JSON: %v\n%s", err, b)
	}
	if doc.Version != "2.1.0" || len(doc.Runs) != 1 {
		t.Fatalf("sarif version/runs = %s / %d, want 2.1.0 / 1", doc.Version, len(doc.Runs))
	}
	run := doc.Runs[0]
	if run.Tool.Driver.Name != "supply-shield" {
		t.Fatalf("driver name = %q, want supply-shield", run.Tool.Driver.Name)
	}
	var sawHook bool
	for _, r := range run.Results {
		if r.RuleId == "supply-shield/install-hook" {
			sawHook = true
			if r.Level != "error" {
				t.Errorf("install-hook level = %q, want error (HIGH verdict)", r.Level)
			}
			if r.Locations[0].PhysicalLocation.ArtifactLocation.URI != "node_modules/evil/package.json" {
				t.Errorf("location = %q, want the first audited file", r.Locations[0].PhysicalLocation.ArtifactLocation.URI)
			}
			if r.Locations[0].PhysicalLocation.Region.StartLine != 3 {
				t.Errorf("startLine = %d, want the flag's evidence line 3", r.Locations[0].PhysicalLocation.Region.StartLine)
			}
		}
	}
	if !sawHook {
		t.Fatalf("no supply-shield/install-hook result in the sarif: %s", b)
	}
}
