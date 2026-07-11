package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func writeRaw(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// scanHealthResult mirrors the scanner_health_output schema.
type scanHealthResult struct {
	GenericExpected   int              `json:"generic_expected"`
	GenericPresent    int              `json:"generic_present"`
	MinGeneric        int              `json:"min_generic"`
	Present           []string         `json:"present"`
	Missing           []map[string]any `json:"missing"`
	TotalFindingsSeen int              `json:"total_findings_seen"`
	Healthy           bool             `json:"healthy"`
	Degraded          bool             `json:"degraded"`
}

// runScanHealth executes the ACTUAL scan_health command from the bot against
// scanDir, returning the parsed health envelope, whether it exited non-zero
// (the hard-fail smoke assertion), and stderr.
func runScanHealth(t *testing.T, scanDir, minGeneric, langs, workspaceDir string) (scanHealthResult, bool, string) {
	t.Helper()
	wf := compileFixture(t, "sec-audit-source/main.bot")
	node, ok := wf.Nodes["scan_health"]
	if !ok {
		t.Fatal("workflow missing scan_health node (anti-façade gate removed?)")
	}
	tool, ok := node.(*ir.ToolNode)
	if !ok {
		t.Fatalf("scan_health is not a ToolNode (got %T)", node)
	}
	cmd := tool.Command
	cmd = strings.ReplaceAll(cmd, "{{vars.scan_dir}}", scanDir)
	cmd = strings.ReplaceAll(cmd, "{{vars.min_generic_scanners}}", minGeneric)
	// Per-language expected outputs are no longer hardcoded: scan_health
	// derives them from the detected langs ({{input.langs}}) crossed with the
	// `iterion:scanners` blocks in {{vars.workspace_dir}}/.claude/skills/lang-*.md.
	// Substitute both so the gate resolves the same LANG file set it would at
	// runtime.
	cmd = strings.ReplaceAll(cmd, "{{input.langs}}", langs)
	cmd = strings.ReplaceAll(cmd, "{{vars.workspace_dir}}", workspaceDir)

	c := exec.Command("sh", "-c", cmd)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()

	var res scanHealthResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("scan_health stdout not valid health JSON: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	return res, runErr != nil, stderr.String()
}

// setupScanHealthWorkspace builds a workspace whose .claude/skills/ holds the
// real bundle lang-{go,js,python}.md skills, so scan_health derives the same
// per-language expected outputs (gosec.json, go-semgrep.json, js.json,
// py-semgrep.json, bandit.json) it would at runtime — no hardcoded file list
// duplicated in the test. Returns the workspace dir to pass as workspaceDir.
func setupScanHealthWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	skillsDir := filepath.Join(ws, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}
	for _, lang := range []string{"go", "js", "python"} {
		name := "lang-" + lang + ".md"
		body, err := os.ReadFile(filepath.Join("..", "bots", "sec-audit-source", "skills", name))
		if err != nil {
			t.Fatalf("read bundle skill %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(skillsDir, name), body, 0o644); err != nil {
			t.Fatalf("write skill %s: %v", name, err)
		}
	}
	return ws
}

// langsGoJsPython is the detected-language list the two coverage subtests
// feed scan_health; crossed with the lang skills it yields the five expected
// per-language scanner outputs.
const langsGoJsPython = `["go","js","python"]`

// TestSecAuditSource_ScanHealth_GuardsAgainstFacade is the regression test for
// the dispatcher-sandbox "scanners emit no output" façade (native:f3a888dc):
// each scanner command ends in `|| true`, so a crashed/absent scanner leaves
// no file and triage sees zero findings — a broken toolchain masquerading as a
// clean repo. scan_health must hard-fail when the always-on generic trio did
// not run, and surface partial gaps without false-failing a genuinely clean
// repo. Runs the ACTUAL bot command, so a regression in the gate fails here.
func TestSecAuditSource_ScanHealth_GuardsAgainstFacade(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("python3 not in PATH — required in CI; skipping here would silently drop the scan_health façade guard")
		}
		t.Skip("python3 not in PATH — skipping scan_health regression test")
	}

	// Healthy: every scanner produced a valid file (some empty = clean repo,
	// some with findings). No hard-fail, not degraded.
	t.Run("all_present_clean_and_findings", func(t *testing.T) {
		dir := t.TempDir()
		// generic — all three present
		capWriteJSON(t, filepath.Join(dir, "gitleaks.json"), []any{}) // gitleaks: top-level array, empty = clean
		capWriteJSON(t, filepath.Join(dir, "trivy.json"), map[string]any{"Results": []any{}})
		capWriteJSON(t, filepath.Join(dir, "semgrep-auto.json"), map[string]any{"results": []any{}})
		// language — present, one with findings
		capWriteJSON(t, filepath.Join(dir, "js.json"), map[string]any{"results": []any{}})
		capWriteJSON(t, filepath.Join(dir, "go-semgrep.json"), map[string]any{"results": []any{}})
		capWriteJSON(t, filepath.Join(dir, "gosec.json"), map[string]any{"Issues": []any{
			map[string]any{"rule_id": "G101", "severity": "HIGH"},
		}})
		capWriteJSON(t, filepath.Join(dir, "py-semgrep.json"), map[string]any{"results": []any{}})
		capWriteJSON(t, filepath.Join(dir, "bandit.json"), map[string]any{"results": []any{}})
		// custom
		capWriteJSON(t, filepath.Join(dir, "custom.json"), map[string]any{"matchers": map[string]any{}})

		ws := setupScanHealthWorkspace(t)
		res, failed, _ := runScanHealth(t, dir, "2", langsGoJsPython, ws)
		if failed {
			t.Errorf("healthy run hard-failed; scan_health must pass when the generic layer ran")
		}
		if res.GenericPresent != 3 {
			t.Errorf("generic_present = %d, want 3", res.GenericPresent)
		}
		if len(res.Missing) != 0 {
			t.Errorf("missing = %v, want empty", res.Missing)
		}
		if !res.Healthy || res.Degraded {
			t.Errorf("healthy=%v degraded=%v, want healthy=true degraded=false", res.Healthy, res.Degraded)
		}
		if res.TotalFindingsSeen != 1 {
			t.Errorf("total_findings_seen = %d, want 1", res.TotalFindingsSeen)
		}
	})

	// Degraded: generic layer fully ran, but two language scanners produced no
	// file. Must NOT hard-fail (the generic layer is intact) but must report
	// the gap so report_card can banner it.
	t.Run("partial_lang_missing_degraded_not_fatal", func(t *testing.T) {
		dir := t.TempDir()
		capWriteJSON(t, filepath.Join(dir, "gitleaks.json"), []any{})
		capWriteJSON(t, filepath.Join(dir, "trivy.json"), map[string]any{"Results": []any{}})
		capWriteJSON(t, filepath.Join(dir, "semgrep-auto.json"), map[string]any{"results": []any{}})
		// gosec + go-semgrep MISSING (the exact tools that silently failed in f3a888dc)
		capWriteJSON(t, filepath.Join(dir, "custom.json"), map[string]any{"matchers": map[string]any{}})

		ws := setupScanHealthWorkspace(t)
		res, failed, _ := runScanHealth(t, dir, "2", langsGoJsPython, ws)
		if failed {
			t.Errorf("degraded run hard-failed; partial language gaps must not fail the run")
		}
		if !res.Degraded || res.Healthy {
			t.Errorf("degraded=%v healthy=%v, want degraded=true healthy=false", res.Degraded, res.Healthy)
		}
		missingFiles := map[string]bool{}
		for _, m := range res.Missing {
			missingFiles[m["file"].(string)] = true
		}
		for _, want := range []string{"gosec.json", "go-semgrep.json", "js.json", "py-semgrep.json", "bandit.json"} {
			if !missingFiles[want] {
				t.Errorf("missing[] should list %s", want)
			}
		}
	})

	// Façade: only one generic scanner produced output (gitleaks) — the exact
	// f3a888dc scenario where gosec/semgrep/trivy all failed. generic_present=1
	// < min_generic=2 → HARD FAIL so a broken toolchain can't report clean.
	t.Run("generic_layer_failed_hard_fails", func(t *testing.T) {
		dir := t.TempDir()
		capWriteJSON(t, filepath.Join(dir, "gitleaks.json"), []any{})
		capWriteJSON(t, filepath.Join(dir, "bandit.json"), map[string]any{"results": []any{}})
		// trivy.json + semgrep-auto.json absent → generic_present = 1

		res, failed, stderr := runScanHealth(t, dir, "2", "[]", t.TempDir())
		if !failed {
			t.Errorf("generic-layer failure must hard-fail (exit non-zero) — this IS the façade gate")
		}
		if res.GenericPresent != 1 {
			t.Errorf("generic_present = %d, want 1", res.GenericPresent)
		}
		if res.Healthy {
			t.Error("healthy must be false when the generic layer failed")
		}
		if !strings.Contains(stderr, "generic scanners produced output") &&
			!strings.Contains(stderr, "scanner toolchain failed") {
			t.Errorf("stderr should explain the smoke-gate failure; got: %q", stderr)
		}
	})

	// Invalid JSON counts as missing (a scanner that wrote a truncated/garbage
	// file did not really produce usable output).
	t.Run("invalid_json_counts_as_missing", func(t *testing.T) {
		dir := t.TempDir()
		capWriteJSON(t, filepath.Join(dir, "gitleaks.json"), []any{})
		capWriteJSON(t, filepath.Join(dir, "semgrep-auto.json"), map[string]any{"results": []any{}})
		// trivy.json present but unparseable
		if err := writeRaw(filepath.Join(dir, "trivy.json"), "{not json"); err != nil {
			t.Fatal(err)
		}

		res, failed, _ := runScanHealth(t, dir, "2", "[]", t.TempDir())
		if failed {
			t.Errorf("two valid generic scanners (>= min) must not hard-fail")
		}
		if res.GenericPresent != 2 {
			t.Errorf("generic_present = %d, want 2 (invalid trivy excluded)", res.GenericPresent)
		}
		var trivyMissing bool
		for _, m := range res.Missing {
			if m["file"] == "trivy.json" && m["status"] == "invalid" {
				trivyMissing = true
			}
		}
		if !trivyMissing {
			t.Errorf("invalid trivy.json should appear in missing[] with status=invalid; missing=%v", res.Missing)
		}
	})
}
