package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// #1322 -- Two runs sharing the workspace scratch, both running deepsec
// successfully, must publish their exports to DISTINCT per-run paths.
// Before the fix, deepsec_out was one shared slot under ${PROJECT_SCRATCH_DIR}
// and the last writer won: the loser read back an export it did not write and
// banked it as its own findings, with no banner. The fix keys the export path
// on {{run.id}} inside the scanner node (a var default cannot carry
// {{run.id}} -- ExpandWithDefault only expands ${ENV}), and both readers
// (bank_deepsec_findings, scan_health) had already been switched to the
// producer's json_paths by #1331.
//
// The property this pins: given two successful passes A and B in the same
// scratch, the file at .../<vars.deepsec_out dir>/<run.id>/<basename> belongs
// to THIS pass alone, and each pass reads back the exact findings it wrote.
// Mutation: revert OUT_JSON to the shared slot -> the second pass overwrites
// the first, and A's readback returns B's findings.
func TestDeepsecExportPathIsPerRun(t *testing.T) {
	dir := t.TempDir()
	scanDir := filepath.Join(dir, "scan")

	writeRunAExport := `[{"id":"A1"},{"id":"A2"},{"id":"A3"}]`
	writeRunBExport := `[{"id":"B1"},{"id":"B2"},{"id":"B3"},{"id":"B4"},{"id":"B5"}]`

	// Both passes exit successfully with real findings; they differ in RUN_ID
	// and in what they export.
	stubA := `
NOW=$(date -u -d "+5 seconds" +%Y-%m-%dT%H:%M:%S.000Z)
mkdir -p data/p/runs
case "$1" in
  scan)
    printf '{"type":"scan","phase":"done","createdAt":"%s","stats":{"filesScanned":10,"candidatesFound":10}}' "$NOW" > data/p/runs/sidA.json
    echo "Run ID: sidA" ;;
  process)
    printf '{"type":"process","phase":"done","createdAt":"%s","stats":{"filesProcessed":10}}' "$NOW" > data/p/runs/ridA.json
    echo "Processing complete. Run: ridA" ;;
  export)
    prev=""; for a in "$@"; do case "$prev" in --out) echo '` + writeRunAExport + `' > "$a";; esac; prev="$a"; done ;;
esac
exit 0`
	stubB := `
NOW=$(date -u -d "+5 seconds" +%Y-%m-%dT%H:%M:%S.000Z)
mkdir -p data/p/runs
case "$1" in
  scan)
    printf '{"type":"scan","phase":"done","createdAt":"%s","stats":{"filesScanned":50,"candidatesFound":80}}' "$NOW" > data/p/runs/sidB.json
    echo "Run ID: sidB" ;;
  process)
    printf '{"type":"process","phase":"done","createdAt":"%s","stats":{"filesProcessed":50}}' "$NOW" > data/p/runs/ridB.json
    echo "Processing complete. Run: ridB" ;;
  export)
    prev=""; for a in "$@"; do case "$prev" in --out) echo '` + writeRunBExport + `' > "$a";; esac; prev="$a"; done ;;
esac
exit 0`

	// A runs first, then B. Both share scan_dir (per-workspace scratch), so
	// the sequential case is the reachable one; simultaneous execution is a
	// stricter property this test does not cover.
	covA, errsA, scanDirA := runDeepsecNodeAgent(t, dir, "run-A", "", "", stubA)
	covB, errsB, scanDirB := runDeepsecNodeAgent(t, dir, "run-B", "", "", stubB)
	if scanDirA != scanDir || scanDirB != scanDir {
		t.Fatalf("both passes must share one scan_dir (%s), got A=%s B=%s", scanDir, scanDirA, scanDirB)
	}
	if covA["source"] != "run_meta" || covB["source"] != "run_meta" {
		t.Fatalf("both passes were meant to be healthy: covA=%v errsA=%v / covB=%v errsB=%v",
			covA["source"], errsA, covB["source"], errsB)
	}

	// A's export file must live at scanDir/<A run.id>/<basename> and carry A's
	// findings; B's must live at scanDir/<B run.id>/<basename> and carry B's.
	// Both must exist SIMULTANEOUSLY -- neither pass destroyed or overwrote
	// the other. The basename is that of vars.deepsec_out (deepsec.json in
	// the harness substitution).
	aPath := filepath.Join(scanDir, "run-A", "deepsec.json")
	bPath := filepath.Join(scanDir, "run-B", "deepsec.json")

	aBody, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("run A export missing at its per-run slot %s: %v -- either the fix regressed to a shared slot, or B destroyed A's file", aPath, err)
	}
	bBody, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("run B export missing at its per-run slot %s: %v", bPath, err)
	}
	if strings.TrimSpace(string(aBody)) != writeRunAExport {
		t.Errorf("run A read back a foreign export at %s: got %q, want %q -- a neighbour pass rewrote A's findings", aPath, aBody, writeRunAExport)
	}
	if strings.TrimSpace(string(bBody)) != writeRunBExport {
		t.Errorf("run B read back a foreign export at %s: got %q, want %q", bPath, bBody, writeRunBExport)
	}

	// The old shared slot at scanDir/deepsec.json must NOT exist: no pass writes
	// to it any more. Its presence would signal a regression where a scanner
	// path was left un-keyed.
	sharedSlot := filepath.Join(scanDir, "deepsec.json")
	if _, err := os.Stat(sharedSlot); !os.IsNotExist(err) {
		t.Errorf("a file sits at the shared slot %s -- some pass wrote there rather than to its per-run subdir, which is the exact regression this test guards against", sharedSlot)
	}
}

// #1322 -- The bank_deepsec_findings node consults the producer's json_paths
// (fixed by #1331), and per-run keying (this PR) means that path now points
// at a per-pass file. So a bank of pass A's envelope returns A's findings,
// and a bank of pass B's envelope returns B's. If a shared slot regressed,
// pass A's json_paths still names the shared filename but its contents are
// pass B's -- so banking A would return B's findings.
func TestDeepsecBankReadsThePassOwnFindings(t *testing.T) {
	dir := t.TempDir()
	scanDir := filepath.Join(dir, "scan")
	aFindings := `[{"id":"A1"},{"id":"A2"}]`
	bFindings := `[{"id":"B1"},{"id":"B2"},{"id":"B3"}]`

	stub := func(payload string, sid, rid string) string {
		return `
NOW=$(date -u -d "+5 seconds" +%Y-%m-%dT%H:%M:%S.000Z)
mkdir -p data/p/runs
case "$1" in
  scan)
    printf '{"type":"scan","phase":"done","createdAt":"%s","stats":{"filesScanned":1,"candidatesFound":1}}' "$NOW" > data/p/runs/` + sid + `.json
    echo "Run ID: ` + sid + `" ;;
  process)
    printf '{"type":"process","phase":"done","createdAt":"%s","stats":{"filesProcessed":1}}' "$NOW" > data/p/runs/` + rid + `.json
    echo "Processing complete. Run: ` + rid + `" ;;
  export)
    prev=""; for a in "$@"; do case "$prev" in --out) echo '` + payload + `' > "$a";; esac; prev="$a"; done ;;
esac
exit 0`
	}

	// Run A, then B (same shared scan_dir).
	_, _, gotDir := runDeepsecNodeAgent(t, dir, "run-A", "", "", stub(aFindings, "sidA", "ridA"))
	if gotDir != scanDir {
		t.Fatalf("scan_dir drift")
	}
	runDeepsecNodeAgent(t, dir, "run-B", "", "", stub(bFindings, "sidB", "ridB"))

	// Reconstruct the json_paths envelope each pass would publish and feed
	// bank_deepsec_findings. Under per-run keying, A's path resolves to
	// scanDir/run-A/deepsec.json -- reading it back returns A's findings.
	bankScript := bankScriptFor(t)
	tempPy := filepath.Join(dir, "bank.py")
	if err := os.WriteFile(tempPy, []byte(bankScript), 0o644); err != nil {
		t.Fatal(err)
	}

	runBank := func(runID, wantFirstID string) {
		t.Helper()
		exportPath := filepath.Join(scanDir, runID, "deepsec.json")
		paths, _ := json.Marshal(map[string]string{"deepsec": exportPath})
		cmd := exec.Command("python3", tempPy)
		cmd.Env = append(os.Environ(),
			"DEEPSEC_PATHS="+string(paths),
			"DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"), // the shared-slot template; irrelevant when json_paths is honoured
			"MAX_BYTES=524288",
		)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("bank exited non-zero: %v (%q)", err, raw)
		}
		var got struct {
			Findings []map[string]any `json:"findings"`
			Note     string           `json:"note"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("bank output is not JSON: %v (%q)", err, raw)
		}
		if len(got.Findings) == 0 {
			t.Fatalf("bank of run %s returned no findings from %s -- the per-run slot has vanished: %s", runID, exportPath, got.Note)
		}
		firstID, _ := got.Findings[0]["id"].(string)
		if firstID != wantFirstID {
			t.Errorf("bank of run %s returned foreign findings: first id %q, want %q -- pass %s read back another pass's export from a slot the two shared", runID, firstID, wantFirstID, runID)
		}
	}

	runBank("run-A", "A1")
	runBank("run-B", "B1")
}

// schemaHasField reports whether a resolved schema declares a field of the
// given name.
func schemaHasField(s *ir.Schema, name string) bool {
	if s == nil {
		return false
	}
	for _, f := range s.Fields {
		if f != nil && f.Name == name {
			return true
		}
	}
	return false
}

// bankScriptFor extracts the python body of bank_deepsec_findings and returns
// a runnable script.
func bankScriptFor(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(raw)
	marker := "tool bank_deepsec_findings:"
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatal("no bank_deepsec_findings node")
	}
	blk := src[i:]
	pyStart := strings.Index(blk, "python3 -c '")
	if pyStart < 0 {
		t.Fatal("bank_deepsec_findings does not use python3 -c '...'")
	}
	pyStart += len("python3 -c '")
	pyEnd := strings.Index(blk[pyStart:], "'`")
	if pyEnd < 0 {
		t.Fatal("bank_deepsec_findings python body never closes")
	}
	return blk[pyStart : pyStart+pyEnd]
}

// #1333 -- min_generic is the always-on trio's hard-fail floor: gitleaks +
// trivy + semgrep-auto. A missing deep scan gracefully degrades (banner) but
// must never substitute for a broken always-on scanner -- otherwise a
// toolchain broken enough to fail two of three trio tools crosses the floor
// as soon as the deep scan produces its export, and the run reports findings
// without a hard-fail.
//
// The property: with min_generic=2 and only ONE trio member present + deepsec
// export present, scan_health exits 1. With the trio complete but deepsec
// absent, scan_health does not exit 1 (degraded, not hard-fail).
func TestScanHealthMinGenericIgnoresDeepsec(t *testing.T) {
	body := secToolCommand(t, "scan_health")

	run := func(t *testing.T, files map[string]string, publishDeepsec bool) (int, map[string]any) {
		t.Helper()
		dir := t.TempDir()
		scanDir := filepath.Join(dir, "scan")
		if err := os.MkdirAll(scanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(scanDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		paths := map[string]string{}
		if publishDeepsec {
			paths["deepsec"] = filepath.Join(scanDir, "deepsec.json")
		}
		pathsJSON, _ := json.Marshal(paths)

		rendered := body
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":             scanDir,
			"{{vars.min_generic_scanners}}": "2",
			"{{input.langs}}":               "[]",
			"{{vars.workspace_dir}}":        dir,
			"{{vars.enable_deepsec}}":       "true",
			"{{vars.deepsec_out}}":          filepath.Join(scanDir, "deepsec.json"),
			"{{input.deepsec_paths}}":       shellQuote(string(pathsJSON)),
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left in the command near %q", rendered[strings.Index(rendered, "{{"):])
		}
		cmd := exec.Command("sh", "-c", rendered)
		out, err := cmd.CombinedOutput()
		exit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("harness failed to run scan_health: %v (out %q)", err, out)
		}
		// stdout / stderr are combined; the first line before any 'scan_health:'
		// stderr message is the JSON envelope. Fish it out.
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		var envelope map[string]any
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "{") {
				_ = json.Unmarshal([]byte(ln), &envelope)
				break
			}
		}
		return exit, envelope
	}

	t.Run("two dead trio scanners with deepsec present must still hard-fail", func(t *testing.T) {
		// gitleaks alone in the trio, semgrep-auto/trivy both missing, deepsec
		// present. Before #1333 generic_present crossed min_generic=2 as soon
		// as deepsec produced its export -- the run passed the gate with a
		// broken toolchain.
		exit, env := run(t, map[string]string{
			"gitleaks.json": `[]`,
			"deepsec.json":  `[{"id":1},{"id":2}]`,
			"custom.json":   `{"matchers":{}}`,
		}, true)
		if exit != 1 {
			t.Fatalf("min_generic=2, one trio present + deepsec present -> exit %d, want 1 (hard-fail): %v", exit, env)
		}
		// generic_present must count TRIO only. With one trio present it must be 1.
		if got, _ := env["generic_present"].(float64); got != 1 {
			t.Errorf("generic_present=%v, want 1 (trio-only count); deepsec must not contribute", env["generic_present"])
		}
		if got, _ := env["generic_expected"].(float64); got != 3 {
			t.Errorf("generic_expected=%v, want 3 (size of the trio, not the reported list including deepsec)", env["generic_expected"])
		}
	})

	t.Run("trio complete with deepsec absent must not hard-fail", func(t *testing.T) {
		// The trio is fully covered; deepsec never wrote its export (json_paths
		// carries no deepsec entry, so PRODUCED["deepsec.json"] = "" and
		// classify returns missing). Under the fix this is degraded but never
		// exit 1.
		exit, env := run(t, map[string]string{
			"gitleaks.json":     `[]`,
			"trivy.json":        `{"Results":[]}`,
			"semgrep-auto.json": `{"results":[]}`,
			"custom.json":       `{"matchers":{}}`,
		}, false) // no deepsec entry in json_paths
		if exit != 0 {
			t.Fatalf("trio healthy, deepsec absent -> exit %d, want 0 (degraded, not hard-fail): %v", exit, env)
		}
		if got, _ := env["healthy"].(bool); got {
			t.Errorf("healthy=true even though deepsec is missing; want false (degraded): %v", env)
		}
		if got, _ := env["degraded"].(bool); !got {
			t.Errorf("degraded=false even though deepsec is missing; want true: %v", env)
		}
		if got, _ := env["generic_present"].(float64); got != 3 {
			t.Errorf("generic_present=%v, want 3 (trio complete)", env["generic_present"])
		}
	})
}

// #1328 -- report_card_user distinguishes "the backlog is exhausted, this
// pass added nothing new" (files_processed=0 AND deepsec findings > 0) from
// "nothing analysed and nothing came out" (files_processed=0 AND count = 0):
// the healthy steady state gets a plain note, not a ⚠, so the operator is
// not trained to ignore coverage warnings.
//
// The discriminator lives on the envelope: report_input carries a
// deepsec_finding_count scalar (0 when the deep pass did not run), populated
// by the scan_join compute from outputs.run_deepsec_scanner.finding_count.
// This test pins the wiring:
//
//   - scan_join_output declares deepsec_finding_count (int).
//   - report_input declares deepsec_finding_count (int).
//   - The merge_with_cache -> report_card edge maps it.
//   - report_card_user prompt reads it AND names the healthy-steady-state
//     note (a marker phrase, not a regex over language, per the fleet
//     contract's rule against spelling-enumeration guards).
func TestDeepsecFindingCountReachesReportCard(t *testing.T) {
	wf := compilePlanPhaseBot(t, "sec-audit-source")

	// scan_join_output schema declares the projection field.
	scanJoin, ok := wf.Schemas["scan_join_output"]
	if !ok {
		t.Fatal("schema scan_join_output not found")
	}
	if !schemaHasField(scanJoin, "deepsec_finding_count") {
		t.Error("scan_join_output does not declare deepsec_finding_count -- the projection is missing, so report_card cannot read it")
	}

	// scan_join compute expression populates the field.
	compute, ok := wf.Nodes["scan_join"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("scan_join is %T, want *ir.ComputeNode", wf.Nodes["scan_join"])
	}
	sawFindingCount := false
	for _, ex := range compute.Exprs {
		if ex.Key != "deepsec_finding_count" {
			continue
		}
		sawFindingCount = true
		if !strings.Contains(ex.Raw, "outputs.run_deepsec_scanner.finding_count") {
			t.Errorf("scan_join.deepsec_finding_count does not project the scanner's finding_count: %s", ex.Raw)
		}
		if !strings.Contains(ex.Raw, "vars.enable_deepsec") {
			t.Errorf("scan_join.deepsec_finding_count is not gated on enable_deepsec: %s", ex.Raw)
		}
	}
	if !sawFindingCount {
		t.Error("scan_join has no deepsec_finding_count expr -- report_card would see a bare template placeholder")
	}

	// report_input schema declares the field.
	reportIn, ok := wf.Schemas["report_input"]
	if !ok {
		t.Fatal("schema report_input not found")
	}
	if !schemaHasField(reportIn, "deepsec_finding_count") {
		t.Error("report_input does not declare deepsec_finding_count -- the schema would reject the wired value")
	}

	// The edge merge_with_cache -> report_card maps the field.
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(src), `deepsec_finding_count: "{{outputs.scan_join.deepsec_finding_count}}"`) {
		t.Error("the merge_with_cache -> report_card edge does not map deepsec_finding_count -- the value never reaches report_card_user")
	}

	// The prompt reads it AND names the healthy-steady-state note. The marker
	// phrase is what the operator sees on a fully-analysed workspace; a spelling
	// change of the surrounding language is fine, but this exact clause is what
	// tells them the finding is a REAL earlier-pass finding, not a coverage gap.
	if !strings.Contains(string(src), "{{input.deepsec_finding_count}}") {
		t.Error("report_card_user does not read {{input.deepsec_finding_count}} -- the prompt cannot discriminate the healthy steady state from a genuine coverage gap")
	}
	const markerPhrase = "the persistent backlog is exhausted"
	if !strings.Contains(string(src), markerPhrase) {
		t.Errorf("report_card_user does not carry the healthy-steady-state marker phrase %q -- either the note was removed or it was rephrased in a way that no longer distinguishes it from the ⚠ banner it replaces", markerPhrase)
	}
}
