package bots

import (
	"bytes"
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

// PR #1473, revi verdict 4 (Rf73c6f) -- cap_findings had a read path to the
// pre-0.1.4 shared slot scan_dir/<basename(vars.deepsec_out)>: its top-level
// glob picked the file up whenever the guards keyed on the producer's
// published path were inert, i.e. exactly when the deep scan was disabled or
// refused. Two consecutive verdicts found that class on two consecutive
// guards. The way out is not a third guard: the glob no longer yields that
// name at all, and the deep scan enters the harvest through ONE route, the
// path its producer published in json_paths.
//
// capHarvest is the slice of the cap_findings envelope these tests read.
type capHarvest struct {
	TotalRaw    int  `json:"total_raw"`
	TotalKept   int  `json:"total_kept"`
	TotalCapped int  `json:"total_capped"`
	Truncated   bool `json:"truncated"`
	PerScanner  []struct {
		File   string `json:"file"`
		Array  string `json:"array"`
		Raw    int    `json:"raw"`
		Kept   int    `json:"kept"`
		Capped int    `json:"capped"`
	} `json:"per_scanner"`
	Inline []struct {
		File     string           `json:"file"`
		Findings []map[string]any `json:"findings"`
	} `json:"inline"`
	InlineTotal    int    `json:"inline_total"`
	DeepsecHarvest string `json:"deepsec_harvest"`
}

// runCapFindingsBody executes the shipped cap_findings python against scanDir
// with the wire the node runs with in production (SCAN_DIR, CAP, INLINE_MAX,
// plus whatever the caller adds: DEEPSEC_PATHS and DEEPSEC_OUT).
func runCapFindingsBody(t *testing.T, scanDir string, env ...string) capHarvest {
	t.Helper()
	scriptPath := filepath.Join(t.TempDir(), "cap.py")
	if err := os.WriteFile(scriptPath, []byte(capFindingsScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", scriptPath)
	cmd.Env = append(os.Environ(), "SCAN_DIR="+scanDir, "CAP=50", "INLINE_MAX=524288")
	cmd.Env = append(cmd.Env, env...)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, raw)
	}
	var got capHarvest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, raw)
	}
	return got
}

// The shared slot is never a harvest source, whatever the deep scan did on
// this pass. Three states of the producer, one orphan at the slot; in none of
// them does an orphan finding reach inline, and the envelope names where the
// deep-scan group came from -- or that there was none to harvest.
//
// Mutation: restore the unfiltered glob over scan_dir/*.json -> the disabled
// and refused subtests redden on the ORPHAN ids riding into inline.
func TestCapFindingsNeverHarvestsTheLegacySharedSlot(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, tc := range []struct {
		name         string
		deepsecPaths string // what {{input.deepsec_paths}} renders to on this pass
		slash        bool   // DEEPSEC_OUT carries a trailing slash (the shell basename strips it; python must too)
		producer     bool   // this pass published a per-run export
	}{
		{"deep scan disabled: scan_join projects an empty string", "", false, false},
		{"deep scan refused: the producer published json_paths {}", "{}", false, false},
		{"deep scan ran: the producer published its per-run path", "", false, true},
		{"deepsec_out carries a trailing slash: the slot is still excluded", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanDir := filepath.Join(t.TempDir(), "scan")
			if err := os.MkdirAll(scanDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, filepath.Join(scanDir, "semgrep.json"), map[string]any{"results": []map[string]any{
				{"check_id": "s1", "severity": "high"},
				{"check_id": "s2", "severity": "high"},
			}})
			// The pre-0.1.4 orphan, at the exact path vars.deepsec_out names.
			orphan := filepath.Join(scanDir, "deepsec.json")
			writeJSON(t, orphan, []map[string]any{
				{"id": "ORPHAN-1", "severity": "critical"},
				{"id": "ORPHAN-2", "severity": "critical"},
				{"id": "ORPHAN-3", "severity": "critical"},
				{"id": "ORPHAN-4", "severity": "critical"},
				{"id": "ORPHAN-5", "severity": "critical"},
			})
			deepsecPaths := tc.deepsecPaths
			wantTotal := 2
			var producerPath string
			if tc.producer {
				producerPath = filepath.Join(scanDir, "deepsec-out-run-CURRENT", "deepsec.json")
				if err := os.MkdirAll(filepath.Dir(producerPath), 0o755); err != nil {
					t.Fatal(err)
				}
				writeJSON(t, producerPath, []map[string]any{
					{"id": "CURRENT-1", "severity": "high"},
					{"id": "CURRENT-2", "severity": "high"},
				})
				enc, err := json.Marshal(map[string]string{"deepsec": producerPath})
				if err != nil {
					t.Fatal(err)
				}
				deepsecPaths = string(enc)
				wantTotal = 4
			}

			deepsecOut := orphan
			if tc.slash {
				deepsecOut = orphan + "/"
			}
			got := runCapFindingsBody(t, scanDir, "DEEPSEC_PATHS="+deepsecPaths, "DEEPSEC_OUT="+deepsecOut)

			deepsecGroups, deepsecFindings := 0, 0
			for _, g := range got.Inline {
				for _, f := range g.Findings {
					if id, _ := f["id"].(string); strings.HasPrefix(id, "ORPHAN") {
						t.Errorf("an orphan finding from the shared slot %s rode into inline under group %q: %v -- cap_findings still has a read path to the pre-0.1.4 slot", orphan, g.File, f)
					}
				}
				if g.File == "deepsec.json" {
					deepsecGroups++
					deepsecFindings += len(g.Findings)
				}
			}
			if tc.producer {
				if deepsecGroups != 1 || deepsecFindings != 2 {
					t.Errorf("deepsec groups=%d findings=%d, want exactly 1 group with the producer's 2 findings: %+v", deepsecGroups, deepsecFindings, got.Inline)
				}
				if want := "published: " + producerPath + " (2 findings harvested)"; got.DeepsecHarvest != want {
					t.Errorf("deepsec_harvest = %q, want %q", got.DeepsecHarvest, want)
				}
			} else {
				if deepsecGroups != 0 {
					t.Errorf("inline carries %d deepsec.json group(s) although this pass published no deep-scan export: the shared-slot orphan was harvested (%+v)", deepsecGroups, got.Inline)
				}
				if !strings.HasPrefix(got.DeepsecHarvest, "none:") {
					t.Errorf("deepsec_harvest = %q, want the observable fact that no export was published (a note starting with none:)", got.DeepsecHarvest)
				}
			}
			if got.InlineTotal != wantTotal {
				t.Errorf("inline_total = %d, want %d -- the orphan's 5 findings must never count", got.InlineTotal, wantTotal)
			}
			// cap_findings reads; removing the orphan is the scanner's job.
			if _, err := os.Stat(orphan); err != nil {
				t.Errorf("cap_findings touched the orphan file: %v", err)
			}
		})
	}
}

// revi verdict 5 on #1473 (R58b272) -- the deep-scan export is ARRAY-shaped,
// so it bypassed the capping path that records raw/kept/capped for the
// dict-shaped scanner exports: a 500-finding deep export was announced as
// "50 findings harvested", the totals ignored it, `truncated` stayed false
// and no coverage banner fired -- a silent thinning on the only transport
// (inline) non-sandboxed backends read. The array branch now keeps the same
// accounts the dict branch keeps, and the harvest note names both counts
// when the cap dropped findings.
//
// Mutation: revert the array branch to appending the post-cap list alone ->
// every assertion below reddens; keep the accounting but announce the
// post-cap count only -> the wording assertion reddens alone.
func TestDeepsecHarvestNamesThePreCapCountWhenTheCapDrops(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	scanDir := filepath.Join(t.TempDir(), "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(scanDir, "semgrep.json"), map[string]any{"results": []map[string]any{
		{"check_id": "s1", "severity": "high"},
	}})
	const total = 120
	ds := make([]map[string]any, 0, total)
	for i := 0; i < total; i++ {
		sev := "high"
		if i < 60 {
			sev = "critical"
		}
		ds = append(ds, map[string]any{"id": fmt.Sprintf("D-%03d", i), "severity": sev})
	}
	producerPath := filepath.Join(scanDir, "deepsec-out-run-CURRENT", "deepsec.json")
	if err := os.MkdirAll(filepath.Dir(producerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, producerPath, ds)
	enc, err := json.Marshal(map[string]string{"deepsec": producerPath})
	if err != nil {
		t.Fatal(err)
	}
	got := runCapFindingsBody(t, scanDir, "DEEPSEC_PATHS="+string(enc), "DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"))

	if want := "published: " + producerPath + " (50 of 120 findings harvested; the per-file cap dropped 70 -- narrow scope or raise findings_cap_per_file and re-run)"; got.DeepsecHarvest != want {
		t.Errorf("deepsec_harvest = %q, want %q -- the note must name BOTH counts when the cap dropped findings (R58b272)", got.DeepsecHarvest, want)
	}
	if got.TotalRaw != 121 || got.TotalKept != 51 || got.TotalCapped != 70 {
		t.Errorf("totals = raw %d / kept %d / capped %d, want 121 / 51 / 70 -- the array export's 120 raw findings must move the same accounts a dict export moves", got.TotalRaw, got.TotalKept, got.TotalCapped)
	}
	if !got.Truncated {
		t.Error("truncated = false although the cap dropped 70 deep findings -- no coverage banner would fire on a thinned run")
	}
	dsIdx := -1
	for i := range got.PerScanner {
		if got.PerScanner[i].File == "deepsec.json" {
			dsIdx = i
		}
	}
	if dsIdx < 0 {
		t.Fatalf("per_scanner carries no deepsec.json entry: %+v", got.PerScanner)
	}
	if e := got.PerScanner[dsIdx]; e.Array != "" || e.Raw != 120 || e.Kept != 50 || e.Capped != 70 {
		t.Errorf("per_scanner deepsec entry = %+v, want array \"\" raw 120 kept 50 capped 70", e)
	}
	if got.InlineTotal != 51 {
		t.Errorf("inline_total = %d, want 51 (the 50 kept deep findings + semgrep's 1)", got.InlineTotal)
	}
	// The envelope says the export was thinned, so the file on disk must say
	// the same: on the DEFAULT transport (triage_inline_max_bytes = 0) triage
	// opens this exact file, and a full body under a thinned envelope is the
	// same lie in mirror image (R58b272). The deep bank read its own copy
	// earlier in the graph (bank_deepsec_findings -> scan_join), so capping
	// in place cannot starve it.
	onDisk, err := os.ReadFile(producerPath)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []map[string]any
	if err := json.Unmarshal(onDisk, &remaining); err != nil {
		t.Fatalf("the rewritten export is not a bare JSON array: %v (%q)", err, onDisk)
	}
	if len(remaining) != 50 {
		t.Errorf("the export still carries %d findings on disk after a capped pass -- a triage that opens json_paths on the default transport reads the thinning the envelope announces", len(remaining))
	}
	keptIDs := map[string]bool{}
	for _, f := range remaining {
		id, _ := f["id"].(string)
		keptIDs[id] = true
	}
	for i := 0; i < 50; i++ {
		if !keptIDs[fmt.Sprintf("D-%03d", i)] {
			t.Errorf("severity order lost in the rewritten export: D-%03d missing from disk", i)
			break
		}
	}
}

// An override that points json_paths.deepsec at a top-level file the glob
// already yields must not count that file twice: the realpath dedupe before
// the append processes the producer's path at most once.
//
// Mutation: replace the dedupe with a plain append -> the file enters _files
// twice and this test reddens on total_raw 6 != 3 and on the duplicate
// per_scanner entry.
func TestCapFindingsCountsAProducerPathTheGlobAlreadyYieldedExactlyOnce(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	scanDir := filepath.Join(t.TempDir(), "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(scanDir, "deepsec-export.json")
	writeJSON(t, flat, []map[string]any{
		{"id": "X-1", "severity": "high"},
		{"id": "X-2", "severity": "high"},
		{"id": "X-3", "severity": "high"},
	})
	// A custom slot name, so the shared-slot exclusion cannot be what hides
	// the flat file from the glob: the dedupe alone must hold.
	enc, err := json.Marshal(map[string]string{"deepsec": flat})
	if err != nil {
		t.Fatal(err)
	}
	got := runCapFindingsBody(t, scanDir, "DEEPSEC_PATHS="+string(enc), "DEEPSEC_OUT="+filepath.Join(scanDir, "slot.json"))

	if got.TotalRaw != 3 {
		t.Errorf("total_raw = %d, want 3 -- the producer's path was processed more than once", got.TotalRaw)
	}
	entries := 0
	for i := range got.PerScanner {
		if got.PerScanner[i].File == "deepsec-export.json" {
			entries++
		}
	}
	if entries != 1 {
		t.Errorf("per_scanner carries %d entries for %s, want exactly 1", entries, flat)
	}
}

// The dict branch always rewrites its file trimmed; the array branch rewrites
// ONLY when the cap thinned the export. An uncapped export must stay
// byte-identical: rewriting it would apply trim (bulky fields dropped,
// strings cut at MAXSTR) to a body the envelope reports as complete -- a
// disk copy silently poorer than what the envelope announces.
//
// Mutation: drop the capped > 0 guard (always rewrite) -> the 400-char
// message is cut to MAXSTR and this test reddens on the byte comparison.
func TestCapFindingsLeavesAnUncappedArrayExportByteIdentical(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	scanDir := filepath.Join(t.TempDir(), "scan")
	producerDir := filepath.Join(scanDir, "deepsec-out-run-CURRENT")
	if err := os.MkdirAll(producerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	producerPath := filepath.Join(producerDir, "deepsec.json")
	writeJSON(t, producerPath, []map[string]any{
		{"id": "U-1", "severity": "critical", "message": strings.Repeat("m", 400)},
		{"id": "U-2", "severity": "high", "message": strings.Repeat("m", 400)},
	})
	before, err := os.ReadFile(producerPath)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(map[string]string{"deepsec": producerPath})
	if err != nil {
		t.Fatal(err)
	}
	got := runCapFindingsBody(t, scanDir, "DEEPSEC_PATHS="+string(enc), "DEEPSEC_OUT="+filepath.Join(scanDir, "deepsec.json"))

	if got.Truncated {
		t.Fatal("truncated = true although nothing was capped -- the envelope would announce a thinning that did not happen")
	}
	after, err := os.ReadFile(producerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("an uncapped array export was rewritten (trim applied to a complete body): %d bytes -> %d bytes -- the file must stay byte-identical when capped == 0", len(before), len(after))
	}
}

// The wire: the slot name reaches cap_findings from vars.deepsec_out (so the
// exclusion holds whether or not the producer published anything), the
// envelope declares deepsec_harvest, and both readers of the envelope are told
// what it says. Mutation on any one line -> its own assertion reddens.
func TestCapFindingsTakesTheSharedSlotNameFromTheVar(t *testing.T) {
	wf := compilePlanPhaseBot(t, "sec-audit-source")
	tool, ok := wf.Nodes["cap_findings"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("cap_findings is %T, want *ir.ToolNode", wf.Nodes["cap_findings"])
	}
	if !strings.Contains(tool.Command, "DEEPSEC_OUT={{vars.deepsec_out}}") {
		t.Error("cap_findings command does not export DEEPSEC_OUT={{vars.deepsec_out}} -- the shared-slot name is not wired, so the exclusion is inert")
	}
	if !strings.Contains(tool.Command, "os.environ.get('DEEPSEC_OUT'") {
		t.Error("cap_findings python does not read DEEPSEC_OUT -- the env var is defined but never consulted")
	}
	out, ok := wf.Schemas["findings_budget_output"]
	if !ok {
		t.Fatal("schema findings_budget_output not found")
	}
	if !schemaHasField(out, "deepsec_harvest") {
		t.Error("findings_budget_output does not declare deepsec_harvest -- the envelope carries the fact undeclared")
	}
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "`findings_budget.deepsec_harvest`") {
		t.Error("the triage prompt does not name findings_budget.deepsec_harvest -- triage is not told that a missing deep-scan group is a published fact, not a file to go looking for")
	}
	if !strings.Contains(body, "`deepsec_harvest` names the deep-scan export this pass published") {
		t.Error("the report prompt does not explain deepsec_harvest -- report_card would read the field as noise")
	}
}

// The retention sweep is -type d, so the pre-0.1.4 shared-slot FILE would
// survive forever. run_deepsec_scanner removes it once, at entry, whatever
// scan_dir_ttl_days says (the harness renders it 0) -- and only once the pass
// has established its run id: a pass that refuses earlier touches nothing.
//
// Mutations: drop the rm -> the first subtest reddens (orphan survives); move
// it above the run-id guard -> the second reddens (a refusing pass deleted).
func TestDeepsecScannerRemovesTheLegacySharedSlotAtEntry(t *testing.T) {
	dir := t.TempDir()
	scanDir := filepath.Join(dir, "scan")
	if err := os.MkdirAll(scanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The harness renders vars.deepsec_out as scanDir/deepsec.json: this file
	// IS the pre-0.1.4 shared slot.
	orphan := filepath.Join(scanDir, "deepsec.json")
	orphanBody := []byte(`[{"id":"ORPHAN-1"}]`)
	plant := func(t *testing.T) {
		t.Helper()
		if err := os.WriteFile(orphan, orphanBody, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A neighbour pass's export, where a current build writes one.
	neighbour := filepath.Join(scanDir, "deepsec-out-run-OTHER", "deepsec.json")
	neighbourBody := []byte(`[{"id":"OTHER-1"},{"id":"OTHER-2"}]`)
	if err := os.MkdirAll(filepath.Dir(neighbour), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neighbour, neighbourBody, 0o644); err != nil {
		t.Fatal(err)
	}
	neighbourIntact := func(t *testing.T) {
		t.Helper()
		got, err := os.ReadFile(neighbour)
		if err != nil {
			t.Fatalf("the neighbour's per-run export at %s was destroyed: %v", neighbour, err)
		}
		if !bytes.Equal(got, neighbourBody) {
			t.Errorf("the neighbour's per-run export at %s was rewritten: %q", neighbour, got)
		}
	}

	t.Run("a pass that runs removes the orphan and leaves the neighbour", func(t *testing.T) {
		plant(t)
		cov, errs, _ := runDeepsecNodeFull(t, dir, "run-CURRENT", `exit 0`)
		if cov["source"] == "deepsec_unavailable" {
			t.Fatalf("this pass was meant to get past its guards: %v", errs)
		}
		if _, err := os.Stat(orphan); !os.IsNotExist(err) {
			t.Errorf("the pre-0.1.4 shared slot %s survived a pass of this build (stat: %v) -- the retention sweep prunes directories only, so nothing else ever reclaims it", orphan, err)
		}
		neighbourIntact(t)
	})

	t.Run("the removal keys on vars.deepsec_out, not on a hardcoded deepsec.json", func(t *testing.T) {
		// runDeepsecNodeFull renders {{vars.deepsec_out}} as scanDir/deepsec.json,
		// which cannot tell a derived slot from a hardcoded name. This subtest
		// renders the body the way TestDeepsecPrunesStalePerRunSubdirs does and
		// points the var at a different basename.
		dir := t.TempDir()
		scanDir := filepath.Join(dir, "scan")
		if err := os.MkdirAll(scanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(dir, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		stubs := filepath.Join(dir, "bin", "run-CUSTOM")
		stubBin(t, stubs, "node", `echo v22.0.0`)
		stubBin(t, stubs, "deepsec", `exit 0`)
		stubBin(t, stubs, "sleep", `exit 0`)
		custom := filepath.Join(scanDir, "ds-export.json")
		if err := os.WriteFile(custom, []byte(`[{"id":"ORPHAN-CUSTOM"}]`), 0o644); err != nil {
			t.Fatal(err)
		}
		rendered := expandEngineBracedEnv(secToolCommand(t, "run_deepsec_scanner"))
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":              scanDir,
			"{{vars.workspace_dir}}":         ws,
			"{{vars.deepsec_out}}":           custom,
			"{{vars.deepsec_concurrency}}":   "1",
			"{{vars.deepsec_process_limit}}": "0",
			"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
			"{{vars.scan_dir_ttl_days}}":     "0",
			"{{run.id}}":                     "run-CUSTOM",
			"{{vars.deepsec_agent}}":         "''",
			"{{vars.deepsec_model}}":         "''",
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left: %q", rendered[strings.Index(rendered, "{{"):])
		}
		if runShell(t, rendered, stubs) == "" {
			t.Fatal("scanner produced no envelope")
		}
		if _, err := os.Stat(custom); !os.IsNotExist(err) {
			t.Errorf("the orphan at the custom slot %s survived -- the removal is not keyed on vars.deepsec_out", custom)
		}
	})

	t.Run("the retention sweep is live under the engine-faithful rendering", func(t *testing.T) {
		// The sweep's -mtime bound travels through the same body. Rendered
		// engine-faithfully (expandEngineBracedEnv) with a ttl of 30, an aged
		// owned directory must actually go: a mirror that erased or mangled
		// the bound rendered the sweep inert while every test stayed green.
		dir := t.TempDir()
		scanDir := filepath.Join(dir, "scan")
		if err := os.MkdirAll(scanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(dir, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		stubs := filepath.Join(dir, "bin", "run-MIRROR")
		stubBin(t, stubs, "node", `echo v22.0.0`)
		stubBin(t, stubs, "deepsec", `exit 0`)
		stubBin(t, stubs, "sleep", `exit 0`)
		aged := filepath.Join(scanDir, "deepsec-out-run-OLD")
		if err := os.MkdirAll(aged, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(aged, "sentinel"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		stale := time.Now().Add(-40 * 24 * time.Hour)
		if err := os.Chtimes(aged, stale, stale); err != nil {
			t.Fatal(err)
		}
		rendered := expandEngineBracedEnv(secToolCommand(t, "run_deepsec_scanner"))
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":              scanDir,
			"{{vars.workspace_dir}}":         ws,
			"{{vars.deepsec_out}}":           filepath.Join(scanDir, "deepsec.json"),
			"{{vars.deepsec_concurrency}}":   "1",
			"{{vars.deepsec_process_limit}}": "0",
			"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
			"{{vars.scan_dir_ttl_days}}":     "30",
			"{{run.id}}":                     "run-MIRROR",
			"{{vars.deepsec_agent}}":         "''",
			"{{vars.deepsec_model}}":         "''",
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left: %q", rendered[strings.Index(rendered, "{{"):])
		}
		if runShell(t, rendered, stubs) == "" {
			t.Fatal("scanner produced no envelope")
		}
		if _, err := os.Stat(aged); !os.IsNotExist(err) {
			t.Errorf("the aged owned dir %s survived a ttl=30 pass rendered engine-faithfully (%v) -- the sweep line is inert under this rendering", aged, err)
		}
	})

	t.Run("the sweep runs on every entry with a usable run id, before the preflight", func(t *testing.T) {
		// revi R58b272: a workspace whose deepsec later becomes unavailable
		// must still reclaim the per-run dirs earlier passes left. The stub
		// node reports v21, so the pass REFUSES at the node-version probe --
		// deterministically, on any host -- AFTER the sweep had its chance:
		// the aged owned directory must be gone anyway.
		dir := t.TempDir()
		scanDir := filepath.Join(dir, "scan")
		if err := os.MkdirAll(scanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ws := filepath.Join(dir, "ws")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		stubs := filepath.Join(dir, "bin", "run-GONE")
		stubBin(t, stubs, "node", `echo v21.0.0`)
		stubBin(t, stubs, "sleep", `exit 0`)
		aged := filepath.Join(scanDir, "deepsec-out-run-OLD")
		if err := os.MkdirAll(aged, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(aged, "sentinel"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		stale := time.Now().Add(-40 * 24 * time.Hour)
		if err := os.Chtimes(aged, stale, stale); err != nil {
			t.Fatal(err)
		}
		rendered := expandEngineBracedEnv(secToolCommand(t, "run_deepsec_scanner"))
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":              scanDir,
			"{{vars.workspace_dir}}":         ws,
			"{{vars.deepsec_out}}":           filepath.Join(scanDir, "deepsec.json"),
			"{{vars.deepsec_concurrency}}":   "1",
			"{{vars.deepsec_process_limit}}": "0",
			"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
			"{{vars.scan_dir_ttl_days}}":     "30",
			"{{run.id}}":                     "run-GONE",
			"{{vars.deepsec_agent}}":         "''",
			"{{vars.deepsec_model}}":         "''",
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left: %q", rendered[strings.Index(rendered, "{{"):])
		}
		raw := runShell(t, rendered, stubs)
		if !strings.Contains(raw, "22") {
			t.Fatalf("the pass did not refuse at the node-version probe as arranged: %q", raw)
		}
		if _, err := os.Stat(aged); !os.IsNotExist(err) {
			t.Errorf("the aged owned dir %s survived a pass that refused at the preflight (%v) -- the sweep no longer runs on entries whose deepsec is unavailable (revi R58b272)", aged, err)
		}
	})

	t.Run("a pass that refuses before its run id is established touches nothing", func(t *testing.T) {
		plant(t)
		cov, _, _ := runDeepsecNodeFull(t, dir, "", `exit 0`)
		if cov["source"] != "deepsec_unavailable" {
			t.Fatalf("an empty run id was meant to refuse: %v", cov)
		}
		got, err := os.ReadFile(orphan)
		if err != nil {
			t.Fatalf("a pass that refused before establishing its run id removed the shared slot: %v -- the removal sits above the run-id guard", err)
		}
		if !bytes.Equal(got, orphanBody) {
			t.Errorf("a refusing pass rewrote the shared slot: %q", got)
		}
		neighbourIntact(t)
	})
}

// runScanHealthBody renders and executes the shipped scan_health body against
// scanDir with enable_deepsec=true, returning the exit code, the envelope
// (stdout, and nothing but the envelope) and stderr on its own.
func runScanHealthBody(t *testing.T, files map[string]string, minGeneric string, publishDeepsec bool, deepsecOut ...string) (int, map[string]any, string) {
	t.Helper()
	body := secToolCommand(t, "scan_health")
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
	pathsJSON, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	rendered := body
	for ref, val := range map[string]string{
		"{{vars.scan_dir}}":             scanDir,
		"{{vars.min_generic_scanners}}": minGeneric,
		"{{input.langs}}":               "[]",
		"{{vars.workspace_dir}}":        dir,
		"{{vars.enable_deepsec}}":       "true",
		"{{vars.deepsec_out}}":          dsOut(scanDir, deepsecOut),
		"{{input.deepsec_paths}}":       shellQuote(string(pathsJSON)),
	} {
		rendered = strings.ReplaceAll(rendered, ref, val)
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command near %q", rendered[strings.Index(rendered, "{{"):])
	}
	cmd := exec.Command("sh", "-c", rendered)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exit := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("harness failed to run scan_health: %v (stderr %q)", runErr, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("scan_health stdout is not the JSON envelope alone: %v (%q)", err, stdout.String())
	}
	return exit, env, stderr.String()
}

// dsOut returns the deepsec_out var value the caller asked for: the default
// scan_dir/deepsec.json, or that path with a trailing slash (the shell
// basename the producer derives strips it; the python reader must strip it
// too for the two sides to agree on the slot name).
func dsOut(scanDir string, override []string) string {
	base := filepath.Join(scanDir, "deepsec.json")
	if len(override) > 0 && override[0] == "TRAILING-SLASH" {
		return base + string(filepath.Separator)
	}
	return base
}

// Revi verdict 4, question 1 -- the min_generic floor is counted over the
// always-on trio alone (#1333), so an operator who set min_generic_scanners=4
// counting the deep scan as a fourth would hard-fail every run from 0.1.4 on.
// The gate clamps the value to the trio's size and SAYS so: the envelope
// carries the value as set and the clamp flag, stderr carries a diagnostic,
// and report_card renders a NOTE (pinned separately). The setting is never
// replaced in silence.
//
// Mutation: drop the clamp -> the first subtest exits 1 and reddens.
func TestScanHealthClampsMinGenericToTheTrio(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	full := map[string]string{
		"gitleaks.json":     `[]`,
		"trivy.json":        `{"Results":[]}`,
		"semgrep-auto.json": `{"results":[]}`,
		"custom.json":       `{"matchers":{}}`,
		"deepsec.json":      `[{"id":1}]`,
	}

	t.Run("min_generic_scanners=4 with the trio complete runs at a floor of 3 and says so", func(t *testing.T) {
		exit, env, stderr := runScanHealthBody(t, full, "4", true)
		if exit != 0 {
			t.Fatalf("trio complete + deepsec present, min_generic_scanners=4 -> exit %d, want 0: the floor was not clamped to the trio and every run of this configuration hard-fails (%v; stderr %q)", exit, env, stderr)
		}
		if got, _ := env["min_generic"].(float64); got != 3 {
			t.Errorf("min_generic=%v, want 3 (the trio's size, the floor the gate ran with)", env["min_generic"])
		}
		if got, _ := env["min_generic_requested"].(float64); got != 4 {
			t.Errorf("min_generic_requested=%v, want 4 (the operator's setting, kept as set)", env["min_generic_requested"])
		}
		if got, _ := env["min_generic_clamped"].(bool); !got {
			t.Errorf("min_generic_clamped=%v, want true -- the clamp is silent without it", env["min_generic_clamped"])
		}
		if got, _ := env["healthy"].(bool); !got {
			t.Errorf("healthy=%v, want true: every expected file is present", env["healthy"])
		}
		if !strings.Contains(stderr, "clamped to 3") {
			t.Errorf("stderr carries no clamp diagnostic: %q", stderr)
		}
	})

	t.Run("the clamped floor still hard-fails a degraded trio", func(t *testing.T) {
		degraded := map[string]string{}
		for k, v := range full {
			if k != "semgrep-auto.json" {
				degraded[k] = v
			}
		}
		exit, env, stderr := runScanHealthBody(t, degraded, "4", true)
		if exit != 1 {
			t.Fatalf("two of three trio scanners present, min_generic_scanners=4 -> exit %d, want 1: the clamp must not lower the floor below the trio (%v)", exit, env)
		}
		if !strings.Contains(stderr, "only 2 of 3") || !strings.Contains(stderr, "was clamped to 3") {
			t.Errorf("the hard-fail message must name the trio denominator and the clamp: %q", stderr)
		}
	})

	t.Run("min_generic_scanners=3 is the boundary and is not clamped", func(t *testing.T) {
		exit, env, stderr := runScanHealthBody(t, full, "3", true)
		if exit != 0 {
			t.Fatalf("the trio complete at min_generic_scanners=3 -> exit %d, want 0: an off-by-one clamps the strictest legitimate floor (%v; stderr %q)", exit, env, stderr)
		}
		if got, _ := env["min_generic_clamped"].(bool); got {
			t.Errorf("min_generic_clamped=true for min_generic_scanners=3 -- 3 IS the trio size, the one demanding setting the clamp must leave alone")
		}
		if got, _ := env["min_generic"].(float64); got != 3 {
			t.Errorf("min_generic=%v, want 3", env["min_generic"])
		}
		if got, _ := env["min_generic_requested"].(float64); got != 3 {
			t.Errorf("min_generic_requested=%v, want 3", env["min_generic_requested"])
		}
		if strings.Contains(stderr, "clamped") {
			t.Errorf("a clamp diagnostic was written for min_generic_scanners=3: %q", stderr)
		}
	})

	t.Run("a floor within the trio is not clamped", func(t *testing.T) {
		exit, env, stderr := runScanHealthBody(t, full, "2", true)
		if exit != 0 {
			t.Fatalf("exit %d, want 0 (%v)", exit, env)
		}
		if got, _ := env["min_generic_clamped"].(bool); got {
			t.Errorf("min_generic_clamped=true for min_generic_scanners=2 -- the flag fires on a setting the gate honoured as written")
		}
		if got, _ := env["min_generic_requested"].(float64); got != 2 {
			t.Errorf("min_generic_requested=%v, want 2", env["min_generic_requested"])
		}
		if got, _ := env["min_generic"].(float64); got != 2 {
			t.Errorf("min_generic=%v, want 2", env["min_generic"])
		}
		if strings.Contains(stderr, "clamped") {
			t.Errorf("a clamp diagnostic was written for a setting that was not clamped: %q", stderr)
		}
	})
}

// The scan_health reader agrees with the shell-side producer on the slot
// name: the scanner derives it with shell basename (which strips a trailing
// slash), so the python DEEP_ENTRY must strip it too, or an operator var
// with a trailing slash leaves the deep entry resolving against a name no
// producer ever claims. Mutation: drop the rstrip on scan_health's
// DEEPSEC_OUT -> this test reddens (deepsec.json vanishes from present[]).
func TestScanHealthToleratesATrailingSlashOnDeepsecOut(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	files := map[string]string{
		"gitleaks.json":     `[]`,
		"trivy.json":        `{"Results":[]}`,
		"semgrep-auto.json": `{"results":[]}`,
		"custom.json":       `{"matchers":{}}`,
		"deepsec.json":      `[{"id":1}]`,
	}
	exit, env, _ := runScanHealthBody(t, files, "2", true, "TRAILING-SLASH")
	_ = exit
	present, _ := env["present"].([]any)
	saw := false
	for _, p := range present {
		if s, _ := p.(string); s == "deepsec.json" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("deepsec.json absent from present[] %v -- the trailing-slash deepsec_out split the slot name between the producer and this reader", env["present"])
	}
	if got, _ := env["healthy"].(bool); !got {
		t.Errorf("healthy=%v, want true: every expected file is present", env)
	}
}

// The clamp reaches the operator: the two fields are declared on the
// scan_health output schema (so they travel as contract, not as stray keys)
// and report_card is told to render the NOTE.
func TestReportCardRendersTheMinGenericClamp(t *testing.T) {
	wf := compilePlanPhaseBot(t, "sec-audit-source")
	health, ok := wf.Schemas["scanner_health_output"]
	if !ok {
		t.Fatal("schema scanner_health_output not found")
	}
	for _, f := range []string{"min_generic_requested", "min_generic_clamped"} {
		if !schemaHasField(health, f) {
			t.Errorf("scanner_health_output does not declare %s", f)
		}
	}
	src, err := os.ReadFile("sec-audit-source/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "If `min_generic_clamped` is true") {
		t.Error("the report prompt has no rule on min_generic_clamped -- the clamp never reaches the report")
	}
	if !strings.Contains(body, "NOTE: min_generic_scanners=<min_generic_requested>") {
		t.Error("the report prompt does not render the operator's requested value in the NOTE -- the setting is replaced without saying which one")
	}
}
