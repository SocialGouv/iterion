package bots

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// #1473 R485b0c [HIGH] -- moving the deep scan export to a per-run subdir
// (#1322) took it out of reach of cap_findings, which harvests via a
// non-recursive glob over scan_dir/*.json. On claw/codex triage, which reads
// findings_budget.inline as its ONLY transport (see triage_system rule 1a),
// the array-shaped deepsec export then disappeared from `inline`,
// `inline_total`, and `inline_truncated` -- the silent thinning this
// transport exists to end. The class of readers of the export path is:
//
//   - bank_deepsec_findings  (reads from input.json_paths.deepsec)          -- per-run since #1331 + #1322
//   - scan_health            (reads from input.deepsec_paths.deepsec)       -- per-run since #1331 + #1322
//   - triage                 (opens json_paths.deepsec via read_file)       -- per-run since #1322
//   - cap_findings           (globs scan_dir/*.json)                        -- FIXED here: takes deepsec_paths and harvests it beside the glob
//
// No fifth reader exists in the graph (cf. `grep -nE 'glob|listdir|scandir|
// os\.walk' bots/sec-audit-source/main.bot` at the time of writing: the only
// glob hit on scan_dir is cap_findings; all other globs run on
// workspace_dir/patch_dir/records_dir).
//
// This test pins the wire: the cap_findings input schema declares
// deepsec_paths, the tool command reads DEEPSEC_PATHS, the scan_health ->
// cap_findings edge maps deepsec_paths from scan_join, and the harvest is
// keyed on json_paths.deepsec. Mutation: remove DEEPSEC_PATHS -> the
// existing TestInlineFindings_CarryTheArrayShapedExportToo goes red (its
// fixture writes deepsec.json under the per-run subdir; without the
// producer-path harvest cap_findings never sees it).
func TestCapFindingsHarvestsThePerRunDeepsecExport(t *testing.T) {
	wf := compilePlanPhaseBot(t, "sec-audit-source")

	// The tool declares an input schema and it carries deepsec_paths.
	capIn, ok := wf.Schemas["cap_findings_input"]
	if !ok {
		t.Fatal("schema cap_findings_input not found -- cap_findings takes no envelope input, so the deepsec path never reaches it")
	}
	if !schemaHasField(capIn, "deepsec_paths") {
		t.Error("cap_findings_input does not declare deepsec_paths -- the schema would reject the wired value")
	}

	// The command reads DEEPSEC_PATHS from its env, sourced from
	// {{input.deepsec_paths}} rather than {{vars.deepsec_out}} (the base
	// template) or {{outputs.<compute>.deepsec_paths}} (which is not
	// substituted inside a command body, cf. main.bot:425).
	tool, ok := wf.Nodes["cap_findings"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("cap_findings is %T, want *ir.ToolNode", wf.Nodes["cap_findings"])
	}
	if !strings.Contains(tool.Command, "DEEPSEC_PATHS={{input.deepsec_paths}}") {
		t.Error("cap_findings command does not export DEEPSEC_PATHS={{input.deepsec_paths}} -- the harvest path is not plumbed")
	}
	if !strings.Contains(tool.Command, "os.environ.get('DEEPSEC_PATHS'") {
		t.Error("cap_findings python does not read DEEPSEC_PATHS -- the env var is defined but never consulted")
	}

	// The edge scan_health -> cap_findings maps deepsec_paths from scan_join.
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(src),
		`scan_health -> cap_findings with {deepsec_paths: "{{outputs.scan_join.deepsec_paths}}"}`) {
		t.Error("the scan_health -> cap_findings edge does not map deepsec_paths from scan_join -- the input arrives empty and the harvest is a no-op")
	}

	// The glob remains NON-recursive. A recursive glob would sweep other
	// passes' per-run subdirs and reopen #1322 with a neighbour attribution.
	// The scanner is written explicitly `sorted(glob.glob(os.path.join(
	// scan_dir, '*.json')))` -- assert the `**` shape is absent.
	if strings.Contains(tool.Command, `glob.glob(os.path.join(scan_dir, '**`) {
		t.Error("cap_findings widened its glob to recurse -- this reopens #1322 with a neighbour pass's export sweeping into this run's harvest")
	}
	if strings.Contains(tool.Command, `recursive=True`) {
		t.Error("cap_findings uses recursive glob -- same class as widening `**`, same defect")
	}
}

// #1473 R485b0c [HIGH] -- end-to-end proof that the harvest wire actually
// pulls the per-run export in. Runs the shipped cap_findings python body
// against a scan dir where semgrep.json + trivy.json live at the top level
// and deepsec.json lives at scan_dir/<runid>/deepsec.json (per #1322),
// feeding DEEPSEC_PATHS with that per-run path. Under the fix, inline
// carries all three; without the DEEPSEC_PATHS handling, inline is missing
// the deepsec array (12+8 findings, not 25).
func TestCapFindingsHarvestReachesTheDeepsecFile(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir := filepath.Join(dir, "scan")
	perRun := filepath.Join(scanDir, "run-XYZ")
	if err := os.MkdirAll(perRun, 0o755); err != nil {
		t.Fatal(err)
	}

	// Object-shaped scanners at the top level; array-shaped deepsec in the
	// per-run subdir (the shape #1322 introduced).
	semgrep := map[string]any{"results": []map[string]any{
		{"check_id": "s1", "severity": "high"},
		{"check_id": "s2", "severity": "high"},
	}}
	trivy := map[string]any{"Issues": []map[string]any{
		{"check_id": "t1", "severity": "medium"},
	}}
	deepsec := []map[string]any{
		{"id": "d1", "severity": "critical"},
		{"id": "d2", "severity": "critical"},
		{"id": "d3", "severity": "critical"},
	}
	writeJSON(t, filepath.Join(scanDir, "semgrep.json"), semgrep)
	writeJSON(t, filepath.Join(scanDir, "trivy.json"), trivy)
	deepsecPath := filepath.Join(perRun, "deepsec.json")
	writeJSON(t, deepsecPath, deepsec)

	paths, err := json.Marshal(map[string]string{"deepsec": deepsecPath})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		"SCAN_DIR="+scanDir,
		"CAP=50",
		"INLINE_MAX=524288",
		"DEEPSEC_PATHS="+string(paths),
	)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got struct {
		InlineTotal    int `json:"inline_total"`
		InlineEmbedded int `json:"inline_embedded"`
		Inline         []struct {
			File     string           `json:"file"`
			Array    string           `json:"array"`
			Findings []map[string]any `json:"findings"`
		} `json:"inline"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, raw)
	}
	if got.InlineTotal != 6 {
		t.Errorf("inline_total = %d, want 2+1+3=6 -- the deepsec array was NOT harvested from the per-run subdir, so triage on claw/codex would miss the whole deep scan silently", got.InlineTotal)
	}
	if got.InlineEmbedded != got.InlineTotal {
		t.Errorf("with a 512 KiB budget nothing must be dropped: embedded=%d total=%d", got.InlineEmbedded, got.InlineTotal)
	}
	var sawDeepsec bool
	for _, g := range got.Inline {
		if g.File == "deepsec.json" && len(g.Findings) == 3 {
			sawDeepsec = true
		}
	}
	if !sawDeepsec {
		t.Errorf("the deepsec array is not in inline[]: %v -- the harvest missed the per-run subdir", got.Inline)
	}
}

// #1473 R485b0c [HIGH] -- confirm the harvest does NOT sweep neighbour
// per-run subdirs. A recursive glob over scan_dir would find every pass's
// deepsec.json under scanDir/<other-run-id>/deepsec.json; this test writes
// a neighbour and asserts cap_findings ignores it, honouring only the path
// DEEPSEC_PATHS names.
func TestCapFindingsIgnoresNeighbourPerRunSubdirs(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir := filepath.Join(dir, "scan")
	mine := filepath.Join(scanDir, "run-MINE")
	theirs := filepath.Join(scanDir, "run-THEIRS")
	for _, d := range []string{mine, theirs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(t, filepath.Join(mine, "deepsec.json"), []map[string]any{{"id": "MINE-1"}})
	writeJSON(t, filepath.Join(theirs, "deepsec.json"), []map[string]any{
		{"id": "THEIRS-1"}, {"id": "THEIRS-2"}, {"id": "THEIRS-3"},
	})

	paths, _ := json.Marshal(map[string]string{"deepsec": filepath.Join(mine, "deepsec.json")})
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		"SCAN_DIR="+scanDir,
		"CAP=50",
		"INLINE_MAX=524288",
		"DEEPSEC_PATHS="+string(paths),
	)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got struct {
		InlineTotal int `json:"inline_total"`
		Inline      []struct {
			Findings []map[string]any `json:"findings"`
		} `json:"inline"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, raw)
	}
	if got.InlineTotal != 1 {
		t.Errorf("inline_total = %d, want 1 -- the harvest is sweeping other passes' per-run subdirs, which is exactly the class #1322 closes. Only json_paths.deepsec is authoritative", got.InlineTotal)
	}
	for _, g := range got.Inline {
		for _, f := range g.Findings {
			if id, _ := f["id"].(string); strings.HasPrefix(id, "THEIRS") {
				t.Errorf("a neighbour pass's finding rode into this pass's inline harvest: %v", f)
			}
		}
	}
}

// #1473 R9c0940 [medium] -- the RUN_ID guard admits "." and "..", which are
// legal filenames but not usable as path SEGMENTS: "." recreates the shared
// slot (dirname/./basename resolves back to scan_dir/deepsec.json) and ".."
// places the export and its rm one directory above scan_dir. Reject both
// explicitly before deriving OUT_JSON.
//
// Verified by rendering the deepsec scanner command with RUN_ID = "." and
// RUN_ID = "..", asserting the envelope's errors[] names the refusal and
// that neither the shared slot nor the parent-of-scan_dir carries a
// deepsec.json afterwards.
func TestDeepsecRefusesRunIDDotAndDotDot(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, tc := range []struct{ name, runID string }{
		{"single-dot", "."},
		{"double-dot", ".."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Poison the two locations we care about: what would land at
			// scan_dir/deepsec.json under RUN_ID="." (shared slot) or at
			// scan_dir/../deepsec.json under RUN_ID=".." (parent of scan_dir).
			// If the guard were absent, the rm below the check would DELETE
			// these files. The guard fires before the derivation, so both
			// stay put.
			dir := t.TempDir()
			scanDir := filepath.Join(dir, "scan")
			parent := dir
			if err := os.MkdirAll(scanDir, 0o755); err != nil {
				t.Fatal(err)
			}
			slot := filepath.Join(scanDir, "deepsec.json")
			parentSlot := filepath.Join(parent, "deepsec.json")
			for _, p := range []string{slot, parentSlot} {
				if err := os.WriteFile(p, []byte(`[{"id":"WITNESS"}]`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			_, errs, _ := runDeepsecNodeFull(t, dir, tc.runID, `exit 0`)
			// The refusal ships in errors[] with the "path-navigation
			// segment" phrase; a generic path-segment refusal would be a
			// spelling regression.
			joined := strings.Join(errs, " || ")
			wantSubstr := "path-navigation segment"
			if !strings.Contains(joined, wantSubstr) {
				t.Errorf("RUN_ID=%q did not surface the explicit refusal %q; errs=%q -- the guard is not distinguishing . / .. from the generic invalid-charset case", tc.runID, wantSubstr, joined)
			}
			// The witness files must be untouched.
			for _, p := range []string{slot, parentSlot} {
				body, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("witness file %s was deleted by the RUN_ID=%q refusal: %v -- the rm ran despite the guard", p, tc.runID, err)
				}
				if !strings.Contains(string(body), "WITNESS") {
					t.Errorf("witness file %s was rewritten by the RUN_ID=%q refusal: %q", p, tc.runID, body)
				}
			}
		})
	}
}

// #1473 R6be92b [medium] -- the per-run subdir accumulates unbounded in the
// persistent workspace scratch. Under scan_dir_ttl_days > 0, entries whose
// mtime is older than the TTL are pruned at scanner entry; this run's own
// two directories always stay.
//
// Runs the shipped scanner body against a scan dir seeded with three fake
// per-run subdirs (one aged, one fresh, one deepsec-workspace) plus a
// deepsec-logs-* dir. The scanner refuses fast (no deepsec binary in PATH)
// but the retention block runs BEFORE the refusal probes. Assertions on the
// filesystem afterwards.
func TestDeepsecPrunesStalePerRunSubdirs(t *testing.T) {
	body := secToolCommand(t, "run_deepsec_scanner")
	dir := t.TempDir()
	scanDir := filepath.Join(dir, "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Aged directories: mtime 40 days ago. Fresh: mtime now.
	aged := []string{
		filepath.Join(scanDir, "run-OLD-A"),
		filepath.Join(scanDir, "run-OLD-B"),
		filepath.Join(scanDir, "deepsec-logs-run-OLD-A"),
	}
	fresh := []string{
		filepath.Join(scanDir, "run-FRESH"),
		filepath.Join(scanDir, "deepsec-workspace"), // the shared data root
	}
	for _, d := range append(aged, fresh...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sentinel"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, d := range aged {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// Also seed a fake current-run directory so we can prove it stays.
	const runID = "run-CURRENT"
	currentDir := filepath.Join(scanDir, runID)
	currentLogsDir := filepath.Join(scanDir, "deepsec-logs-"+runID)
	for _, d := range []string{currentDir, currentLogsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(currentDir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(currentLogsDir, old, old); err != nil {
		t.Fatal(err)
	}

	// Render the scanner body: stub node so the version probe passes, but do
	// NOT stub deepsec -- the scanner refuses through err_envelope. The
	// prune block runs BEFORE the deepsec-binary probe (it is inside the
	// "state dirs under scan_dir" section, which follows the RUN_ID guard).
	//
	// Wait, the prune block IS after the deepsec probe in the current shape.
	// Let me re-inspect.
	//
	// Structure: node/deepsec probes and agent charset checks come FIRST
	// (each with its own err_envelope). Then RUN_ID guard. Then OUT_JSON
	// derivation + mkdir + prune. So the prune runs only if the deepsec
	// binary is on PATH. Provide a no-op deepsec.
	ws := filepath.Join(dir, "ws")
	stubs := filepath.Join(dir, "bin", runID)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	stubBin(t, stubs, "node", `echo v22.0.0`)
	stubBin(t, stubs, "deepsec", `exit 0`)
	stubBin(t, stubs, "sleep", `exit 0`)

	rendered := body
	repls := map[string]string{
		"{{vars.scan_dir}}":              scanDir,
		"{{vars.workspace_dir}}":         ws,
		"{{vars.deepsec_out}}":           filepath.Join(scanDir, "deepsec.json"),
		"{{vars.deepsec_concurrency}}":   "1",
		"{{vars.deepsec_process_limit}}": "0",
		"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
		"{{vars.scan_dir_ttl_days}}":     "30",
		"{{run.id}}":                     runID,
		"{{vars.deepsec_agent}}":         "''",
		"{{vars.deepsec_model}}":         "''",
	}
	for k, v := range repls {
		rendered = strings.ReplaceAll(rendered, k, v)
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left: %q", rendered[strings.Index(rendered, "{{"):])
	}
	if out := runShell(t, rendered, stubs); out == "" {
		t.Fatal("scanner produced no envelope")
	}

	// The aged directories must be gone.
	for _, d := range aged {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("aged dir %s survived the prune -- scan_dir_ttl_days=30 with mtime 40 days ago should have swept it", d)
		}
	}
	// The fresh dirs, current-run dirs, and deepsec-workspace must stay.
	for _, d := range []string{
		filepath.Join(scanDir, "run-FRESH"),
		filepath.Join(scanDir, "deepsec-workspace"),
		currentDir,
		currentLogsDir,
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("dir %s was pruned but must stay: %v -- either the current-run guard is missing (%s) or deepsec-workspace is not excluded", d, err, fmt.Sprintf("SCAN_DIR/%s and SCAN_DIR/deepsec-logs-%s must never be pruned", runID, runID))
		}
	}
}

// #1473 answer to the reviewer's question -- `findings_budget.inline` is the
// transport triage reads on the non-sandboxed backends. The prompt names it
// as transport (a) in rule 1, and the OTHER transport (json_paths) is what
// the sandboxed claude_code backend uses. This test pins the prompt so a
// silent rewording that changes the transport contract goes red.
func TestTriageDeclaresInlineAsTheNonSandboxTransport(t *testing.T) {
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// The prompt language names findings_budget.inline explicitly.
	if !strings.Contains(body, "findings_budget.inline") {
		t.Fatal("triage_system no longer names findings_budget.inline as a transport -- either the transport was removed or was renamed silently")
	}
	// And states the ordering: inline first, then json_paths.
	// The marker phrase is the operator-facing contract, not a full
	// sentence, so a copy-edit that keeps the meaning stays green.
	if !strings.Contains(body, "Otherwise read every scanner JSON listed in *.json_paths") {
		t.Error("the fall-through from inline to json_paths is not spelled -- triage may pick json_paths first and then never read inline, missing the deepsec harvest on claw/codex")
	}
}
