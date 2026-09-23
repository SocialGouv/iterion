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
	// template) or {{outputs.<node>.deepsec_paths}} — an outputs ref in a
	// command body is refused catalogue-wide by
	// TestCatalogToolCommandsResolveTheirRefs, so the edge is the transport.
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
	// Read off the COMPILED edge, not off the source text: the same edge also
	// carries the pass's scan_dir (#1475), and an assertion on the whole line
	// would pin the spelling of its neighbours rather than this mapping.
	var capDeepsec *ir.DataMapping
	for _, e := range wf.Edges {
		if e.From == "scan_health" && e.To == "cap_findings" {
			capDeepsec = mappingOf(e, "deepsec_paths")
		}
	}
	if capDeepsec == nil || capDeepsec.Raw != `{{outputs.scan_join.deepsec_paths}}` {
		t.Errorf("the scan_health -> cap_findings edge does not map deepsec_paths from scan_join (%v) -- the input arrives empty and the harvest is a no-op", capDeepsec)
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
	perRun := filepath.Join(scanDir, "deepsec-out-run-XYZ")
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
		"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"),
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
	mine := filepath.Join(scanDir, "deepsec-out-run-MINE")
	theirs := filepath.Join(scanDir, "deepsec-out-run-THEIRS")
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
		"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"),
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
	// - deepsec-out-*, deepsec-logs-* and pass-* aged must GO (owned shapes,
	//   past TTL). pass-<run.id> is the scanner scratch scan_dir_resolve keys
	//   (#1475): it accumulates one directory per audit whether or not the
	//   deep scan runs, and this sweep is the only thing that reclaims it.
	// - alien aged (unprefixed) must STAY (Re56aa9 positive-scope fix): a
	//   directory the operator or another node dropped is not enrolled in
	//   this sweep by default.
	agedOwned := []string{
		filepath.Join(scanDir, "deepsec-out-run-OLD-A"),
		filepath.Join(scanDir, "deepsec-out-run-OLD-B"),
		filepath.Join(scanDir, "deepsec-logs-run-OLD-A"),
		filepath.Join(scanDir, "pass-run-OLD-A"),
		filepath.Join(scanDir, "pass-run-OLD-B"),
	}
	agedAlien := []string{
		filepath.Join(scanDir, "run-OLD-A"),         // bare run id -- not owned
		filepath.Join(scanDir, "operator-cache"),    // arbitrary operator dir
		filepath.Join(scanDir, "unrelated-scratch"), // another node might drop this
	}
	fresh := []string{
		filepath.Join(scanDir, "deepsec-out-run-FRESH"),
		filepath.Join(scanDir, "pass-run-FRESH"),
		filepath.Join(scanDir, "deepsec-workspace"), // the shared data root
	}
	aged := append([]string{}, agedOwned...)
	aged = append(aged, agedAlien...)
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
	currentDir := filepath.Join(scanDir, "deepsec-out-"+runID)
	currentLogsDir := filepath.Join(scanDir, "deepsec-logs-"+runID)
	currentPassDir := filepath.Join(scanDir, "pass-"+runID)
	for _, d := range []string{currentDir, currentLogsDir, currentPassDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	// The scanner body has this order: RUN_ID guards → retention sweep →
	// node + deepsec + agent probes (each with its own err_envelope) →
	// OUT_JSON derivation + mkdir. The sweep runs on every entry with a
	// usable run id (revi R58b272); the fixture stubs a working node and a
	// no-op deepsec so the pass reaches its envelope.
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

	// The aged OWNED directories (deepsec-out-* and deepsec-logs-*) must be
	// gone. Aged ALIEN directories (no owned prefix) must stay -- the sweep
	// is positively scoped and does not enrol what it does not own (Re56aa9).
	for _, d := range agedOwned {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("aged owned dir %s survived the prune -- scan_dir_ttl_days=30 with mtime 40 days ago should have swept it", d)
		}
	}
	for _, d := range agedAlien {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("alien dir %s was pruned but must stay: %v -- the sweep is scoped to deepsec-out-* / deepsec-logs-* / pass-* only; enrolling anything else would delete operator or foreign-node state after the TTL", d, err)
		}
	}
	// The fresh dirs, current-run dirs, and deepsec-workspace must stay.
	for _, d := range []string{
		filepath.Join(scanDir, "deepsec-out-run-FRESH"),
		filepath.Join(scanDir, "pass-run-FRESH"),
		filepath.Join(scanDir, "deepsec-workspace"),
		currentDir,
		currentLogsDir,
		currentPassDir,
	} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("dir %s was pruned but must stay: %v -- either the current-run guard is missing (%s) or deepsec-workspace is not excluded", d, err, fmt.Sprintf("SCAN_DIR/deepsec-out-%s, SCAN_DIR/deepsec-logs-%s and SCAN_DIR/pass-%s must never be pruned", runID, runID, runID))
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

// #1473 R6ee3b1 [HIGH] on verdict 3 -- cap_findings was appending the deepsec
// export LAST in the harvest, and the inline loop breaks as soon as
// `size >= inline_max`. With a generic layer that fills the budget the deepsec
// findings reached `inline` with zero items -- the exact gap the harvest
// route (Re6ee3b1 fixed by #1473 revi verdict 1) was supposed to close on
// non-sandboxed backends.
//
// The fix: after appending the per-run deepsec path, sort the whole file
// list by basename, so `deepsec.json` interleaves with the other scanners
// the way the pre-per-run glob already did (`d` sorts between `custom.json`
// and `gitleaks.json`). This test runs the real cap_findings body against
// a scan dir where semgrep alone fills the inline budget and asserts
// deepsec still lands in inline.
func TestCapFindingsInlinesDeepsecUnderAFullBudget(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir := filepath.Join(dir, "scan")
	perRun := filepath.Join(scanDir, "deepsec-out-run-BUDGET")
	if err := os.MkdirAll(perRun, 0o755); err != nil {
		t.Fatal(err)
	}

	// A fat semgrep + a fat trivy fill the top-level glob. Each finding is
	// ~250 bytes; ~60 findings per file → each file is > 15 KB (larger than
	// the 8 KB inline budget). The deepsec export is small (3 findings) --
	// if the sort works and the loop encounters deepsec BEFORE it runs
	// out of budget, deepsec makes it into inline.
	mk := func(n int, prefix string) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{
				"check_id": fmt.Sprintf("%s-%d", prefix, i),
				"severity": "high",
				"path":     "src/file.go",
				"message":  strings.Repeat("y", 200),
			})
		}
		return out
	}
	writeJSON(t, filepath.Join(scanDir, "semgrep.json"), map[string]any{"results": mk(60, "s")})
	writeJSON(t, filepath.Join(scanDir, "trivy.json"), map[string]any{"Issues": mk(60, "t")})
	// deepsec findings are the SAME shape and size as semgrep's (~250 bytes
	// each). Under the pre-fix append-last order, the budget breaks inside
	// semgrep and the next iteration on deepsec sees `size + 250 > 8192`
	// which is TRUE, so deepsec gets zero items in inline. Under the fix
	// (sort by basename), deepsec sits BEFORE semgrep (`d` < `s`) and its
	// three items land while there is still room.
	deepsecPath := filepath.Join(perRun, "deepsec.json")
	writeJSON(t, deepsecPath, mk(3, "d"))

	paths, err := json.Marshal(map[string]string{"deepsec": deepsecPath})
	if err != nil {
		t.Fatal(err)
	}

	// Budget of 8 KB: semgrep alone (60 × ~250 bytes) exceeds it. The
	// pre-fix append-last behaviour would drain the whole budget on semgrep
	// before ever opening deepsec; under the sort-by-basename fix the
	// harvest order is `custom.json < deepsec.json < gitleaks.json <
	// semgrep-auto.json < semgrep.json < trivy.json`, so deepsec comes
	// BEFORE semgrep/trivy and gets its slots before the cut.
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		"SCAN_DIR="+scanDir,
		"CAP=50",
		"INLINE_MAX=8192",
		"DEEPSEC_PATHS="+string(paths),
		"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"),
	)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got struct {
		Inline []struct {
			File     string           `json:"file"`
			Findings []map[string]any `json:"findings"`
		} `json:"inline"`
		InlineEmbedded  int  `json:"inline_embedded"`
		InlineTotal     int  `json:"inline_total"`
		InlineTruncated bool `json:"inline_truncated"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, raw)
	}
	// The budget MUST cut (there are ~120+ findings and only 8 KB), so
	// truncated is expected.
	if !got.InlineTruncated {
		t.Fatalf("expected inline_truncated=true under an 8 KB budget with 120+ findings, got %+v", got)
	}
	// The deepsec array MUST have at least one finding in inline. Under the
	// pre-fix (append-last) behaviour, `sortDeepsecLast := true` would drain
	// the budget on the semgrep+trivy prefix before reaching the tail --
	// deepsec_count == 0.
	deepsecCount := 0
	for _, g := range got.Inline {
		if g.File == "deepsec.json" {
			deepsecCount = len(g.Findings)
		}
	}
	if deepsecCount == 0 {
		t.Errorf("deepsec has 0 items in inline under a full generic budget -- the harvest is placing deepsec LAST and the budget cut orphans it (revi R6ee3b1). Got inline: %+v", got.Inline)
	}
}

// Verify the retention block runs POSITIVELY on operator scan_dirs: a mutation
// that widens the sweep to scan_dir/* (deny-list) reddens the alien-stays
// assertions in TestDeepsecPrunesStalePerRunSubdirs. This is a smoke test of
// the fixture design; the mutation itself is documented and manually run in
// the revi thread.

// #1473 verdict 3 RVA agent-1 [medium] -- cap_findings must not harvest a
// pre-#1322 orphan `scan_dir/deepsec.json` alongside the per-run producer
// path. A workspace scanned by a pre-per-run build left the shared-slot file
// on disk; the per-run derivation writes to `scan_dir/deepsec-out-<run>/
// deepsec.json`. The shared slot is never a harvest source (revi verdict 4,
// Rf73c6f): the glob does not yield its name, and the deep scan enters the
// harvest only through json_paths -- so the producer's findings are the
// only deepsec.json group inline carries.
func TestCapFindingsSkipsOrphanWhenTheProducerPublishesADeepsecPath(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDir := filepath.Join(dir, "scan")
	perRun := filepath.Join(scanDir, "deepsec-out-run-CURRENT")
	if err := os.MkdirAll(perRun, 0o755); err != nil {
		t.Fatal(err)
	}

	// Orphan at the legacy shared slot (a pre-#1322 build's leftover, 5
	// findings with distinctive ids). The retention sweep is `-type d` so
	// it never touches this file; only the harvest-time dedup can prevent
	// the double-count.
	orphan := filepath.Join(scanDir, "deepsec.json")
	writeJSON(t, orphan, []map[string]any{
		{"id": "ORPHAN-1", "severity": "critical"},
		{"id": "ORPHAN-2", "severity": "critical"},
		{"id": "ORPHAN-3", "severity": "critical"},
		{"id": "ORPHAN-4", "severity": "critical"},
		{"id": "ORPHAN-5", "severity": "critical"},
	})
	// The current pass's per-run export.
	current := filepath.Join(perRun, "deepsec.json")
	writeJSON(t, current, []map[string]any{
		{"id": "CURRENT-1", "severity": "high"},
		{"id": "CURRENT-2", "severity": "high"},
	})

	paths, err := json.Marshal(map[string]string{"deepsec": current})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(),
		"SCAN_DIR="+scanDir,
		"CAP=50",
		"INLINE_MAX=524288",
		"DEEPSEC_PATHS="+string(paths),
		"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"),
	)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got struct {
		Inline []struct {
			File     string           `json:"file"`
			Findings []map[string]any `json:"findings"`
		} `json:"inline"`
		InlineTotal    int `json:"inline_total"`
		InlineEmbedded int `json:"inline_embedded"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, raw)
	}
	// The producer's 2 findings must be the ONLY deepsec.json items in inline.
	// The orphan's 5 must be excluded.
	deepsecGroups := 0
	deepsecFindings := 0
	for _, g := range got.Inline {
		if g.File != "deepsec.json" {
			continue
		}
		deepsecGroups++
		deepsecFindings += len(g.Findings)
		for _, f := range g.Findings {
			if id, _ := f["id"].(string); strings.HasPrefix(id, "ORPHAN") {
				t.Errorf("an orphan deepsec finding rode into inline: %v -- the pre-#1322 shared-slot file was harvested alongside the producer's per-run export", f)
			}
		}
	}
	if deepsecGroups > 1 {
		t.Errorf("cap_findings produced %d harvest entries labelled deepsec.json; want at most 1 -- the orphan was not deduped by basename", deepsecGroups)
	}
	if deepsecFindings != 2 {
		t.Errorf("deepsec findings in inline = %d, want 2 (the producer's) -- either the orphan slipped through or the producer was skipped", deepsecFindings)
	}
	// inline_total counts ONLY the producer's findings for deepsec.
	if got.InlineTotal != 2 {
		t.Errorf("inline_total = %d, want 2 -- the orphan's 5 findings inflated the total", got.InlineTotal)
	}
}
