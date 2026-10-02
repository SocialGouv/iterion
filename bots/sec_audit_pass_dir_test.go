package bots

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The scanner scratch used to be ONE directory per workspace. Five writers
// (plan_shards, dispatch_shards, run_generic_scanners, run_lang_scanners,
// run_custom_matchers) put shard-files.json, shards.json, gitleaks.json,
// trivy.json, semgrep-auto.json, the per-language outputs and custom.json
// into it by NAME, and two readers (scan_health, cap_findings) resolved those
// same names back. Two audits of one workspace therefore read bytes that may
// belong to the neighbour — the deepsec half of that class was closed at its
// chokepoint by #1322/#1331, the rest by #1475.
//
// These tests run the REAL node bodies, because the property is what the shell
// leaves on disk, and they assert on CONTENT: a scheme that keys the pass
// directory differently but still keeps two passes apart passes them.

// passScanDir renders one pass's scanner scratch the way the RUNTIME does —
// by evaluating the compute expression the .bot ships against one shared
// vars.scan_dir and this pass's run id. Nothing here re-spells the derivation,
// so a compute pointed back at the shared directory reaches every assertion.
func passScanDir(t *testing.T, wf *ir.Workflow, sharedScanDir, runID string) string {
	t.Helper()
	raw := computeExprRaw(wf, "scan_dir_resolve", "run_scan_dir")
	if raw == "" {
		t.Fatal("compute scan_dir_resolve no longer emits run_scan_dir: every scanner writer is back in one shared directory (#1475)")
	}
	ast, err := expr.Parse(raw)
	if err != nil {
		t.Fatalf("scan_dir_resolve.run_scan_dir does not parse: %v (source %q)", err, raw)
	}
	v, err := ast.Eval(&expr.Context{
		Vars: func(p []string) any {
			if len(p) == 1 && p[0] == "scan_dir" {
				return sharedScanDir
			}
			return nil
		},
		Run: func(p []string) any {
			if len(p) == 1 && p[0] == "id" {
				return runID
			}
			return nil
		},
	})
	if err != nil {
		// concat() is the ARRAY builtin and refuses a string at RUNTIME while
		// iterion validate stays green; string concatenation is `+`.
		t.Fatalf("scan_dir_resolve.run_scan_dir does not evaluate: %v (source %q)", err, raw)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		t.Fatalf("scan_dir_resolve.run_scan_dir is %T %v, want a non-empty string (source %q)", v, v, raw)
	}
	return s
}

// secEdgeScanDir renders the scan_dir one edge carries into one node. A tool
// command resolves {{input.X}} and {{vars.X}} but not {{outputs.X}}
// (catalog_command_refs_test.go pins that), so the edge mapping is the whole
// transport: an edge left at {{vars.scan_dir}} sends its node back to the
// shared directory, and the assertions below see it.
func secEdgeScanDir(t *testing.T, wf *ir.Workflow, from, to string, subs map[string]string) string {
	t.Helper()
	for _, e := range wf.Edges {
		if e.From != from || e.To != to {
			continue
		}
		m := mappingOf(e, "scan_dir")
		if m == nil {
			t.Fatalf("edge %s -> %s carries no scan_dir mapping: %s cannot know which pass's scratch it works in", from, to, to)
		}
		return renderSec(t, m.Raw, subs)
	}
	t.Fatalf("no edge %s -> %s", from, to)
	return ""
}

// renderSec substitutes a body's refs and refuses to hand back a command with
// a ref left in it — an unsubstituted ref would run as literal text.
func renderSec(t *testing.T, body string, subs map[string]string) string {
	t.Helper()
	out := body
	for ref, val := range subs {
		out = strings.ReplaceAll(out, ref, val)
	}
	if i := strings.Index(out, "{{"); i >= 0 {
		t.Fatalf("unsubstituted ref left in the command near %q", out[i:min(len(out), i+80)])
	}
	return out
}

// secPass is one audit: its run id, the tag its stubs write into every output,
// and the shard count its dispatch stub reports.
type secPass struct {
	runID  string
	tag    string
	shards string
}

// TestTwoPassesInOneScanDirDoNotOverwriteEachOther is the effect test for
// #1475: two audits of the SAME workspace through the SAME vars.scan_dir,
// differing only in their run id. Every writer must land in its own pass
// directory and every reader must come back with the bytes ITS pass wrote.
//
// Mutation: point compute scan_dir_resolve at "vars.scan_dir", or send one
// writer, one reader or one edge mapping back to {{vars.scan_dir}} — the
// FORBIDDEN alternative is the shared directory, not an empty value. Each
// reddens here.
func TestTwoPassesInOneScanDirDoNotOverwriteEachOther(t *testing.T) {
	for _, bin := range []string{"sh", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	wf := compilePlanPhaseBot(t, "sec-audit-source")

	base := t.TempDir()
	sharedScan := filepath.Join(base, "scan") // the ONE vars.scan_dir
	ws := filepath.Join(base, "ws")           // the ONE workspace
	matchers := filepath.Join(base, "matchers")
	if err := os.MkdirAll(filepath.Join(ws, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "src", "early.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One matcher for both passes: it tags its finding from the environment,
	// so the two passes differ in what they WRITE, not in what they run.
	stubBin(t, matchers, "tagger", `cat > /dev/null; printf '{"findings":[{"id":"%s"}]}' "$SEC_PASS_TAG"`)

	planBody := secToolCommand(t, "plan_shards")
	dispatchBody := secToolCommand(t, "dispatch_shards")
	genBody := secToolCommand(t, "run_generic_scanners")
	customBody := secToolCommand(t, "run_custom_matchers")

	passes := []secPass{{"run-alpha", "ALPHA", "2"}, {"run-beta", "BETA", "7"}}
	dirs := map[string]string{}

	for i, p := range passes {
		if i == 1 {
			// A source file that exists only from the second pass on: the two
			// shard manifests must differ, and the first must not be rewritten.
			if err := os.WriteFile(filepath.Join(ws, "src", "late.go"), []byte("package a\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		passDir := passScanDir(t, wf, sharedScan, p.runID)
		dirs[p.runID] = passDir

		// vars.scan_dir keeps its production value — the SHARED parent. The two
		// resolving to two different directories is what lets a node sent back
		// to {{vars.scan_dir}} land in the neighbour's reach.
		edgeSubs := map[string]string{
			"{{outputs.scan_dir_resolve.run_scan_dir}}": passDir,
			"{{vars.scan_dir}}":                         sharedScan,
		}
		common := map[string]string{
			"{{vars.scan_dir}}":          sharedScan,
			"{{vars.workspace_dir}}":     ws,
			"{{vars.bundle_skills_dir}}": filepath.Join(ws, ".claude", "iterion-skills"),
			"{{vars.shard_size}}":        "1",
			"{{vars.matchers_dir}}":      matchers,
			// plan_shards sweeps stale pass-* directories at the writer; its
			// body reads the run id and the TTL for that. 0 disables the
			// sweep, which is what these two passes want — they must both
			// still be on disk when the readers run.
			"{{run.id}}":                 p.runID,
			"{{vars.scan_dir_ttl_days}}": "0",
		}
		subsFor := func(from, to string) map[string]string {
			out := map[string]string{"{{input.scan_dir}}": secEdgeScanDir(t, wf, from, to, edgeSubs)}
			for k, v := range common {
				out[k] = v
			}
			return out
		}

		stubs := filepath.Join(base, "bin", p.runID)
		stubBin(t, stubs, "gitleaks", `for a in "$@"; do case "$a" in --report-path=*) printf '[{"RuleID":"%s"}]' "$SEC_PASS_TAG" > "${a#--report-path=}";; esac; done; exit 0`)
		stubBin(t, stubs, "trivy", `for a in "$@"; do case "$a" in --output=*) echo '{"Results":[]}' > "${a#--output=}";; esac; done; exit 0`)
		stubBin(t, stubs, "semgrep", `for a in "$@"; do case "$a" in --output=*) echo '{"results":[]}' > "${a#--output=}";; esac; done; exit 0`)
		stubBin(t, stubs, "iterion", `printf '{"shard_count":%s,"shards":[{"status":"finished"}]}' "$SEC_PASS_SHARDS"`)
		env := []string{"SEC_PASS_TAG=" + p.tag, "SEC_PASS_SHARDS=" + p.shards}

		planOut := runShell(t, renderSec(t, planBody, subsFor("scan_dir_resolve", "plan_shards")), stubs, env...)
		var plan struct {
			FilesPath string `json:"files_path"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(planOut)), &plan); err != nil {
			t.Fatalf("plan_shards output is not JSON: %v (%q)", err, planOut)
		}
		if !plan.Enabled {
			t.Fatalf("plan_shards did not enable sharding, so dispatch_shards would never run: %q", planOut)
		}

		dsubs := map[string]string{
			"{{input.scan_dir}}":           secEdgeScanDir(t, wf, "plan_shards", "dispatch_shards", edgeSubs),
			"{{input.workspace_dir}}":      ws,
			"{{input.shard_size}}":         "1",
			"{{input.shard_concurrency}}":  "1",
			"{{input.severity_threshold}}": "low",
			"{{input.fp_path}}":            "",
			"{{input.records_dir}}":        "",
			"{{input.matchers_dir}}":       matchers,
			"{{input.files_path}}":         plan.FilesPath,
			"{{input.workflow_path}}":      "sec-audit-source/main.bot",
			"{{run.id}}":                   p.runID,
		}
		runShell(t, renderSec(t, dispatchBody, dsubs), stubs, env...)
		runShell(t, renderSec(t, genBody, subsFor("plan_shards", "run_generic_scanners")), stubs, env...)
		runShell(t, renderSec(t, customBody, subsFor("run_lang_scanners", "run_custom_matchers")), stubs, env...)
	}

	// ── readback, after BOTH passes have written ──────────────────────
	for _, p := range passes {
		t.Run(p.runID, func(t *testing.T) {
			dir := dirs[p.runID]
			other := map[string]string{"ALPHA": "BETA", "BETA": "ALPHA"}[p.tag]
			edgeSubs := map[string]string{
				"{{outputs.scan_dir_resolve.run_scan_dir}}": dir,
				"{{vars.scan_dir}}":                         sharedScan,
			}

			// gitleaks.json, through the REAL reader: cap_findings harvests the
			// scanner JSON of the pass its edge points it at and carries the
			// findings themselves in `inline`.
			harvest := runSecCapFindings(t, secEdgeScanDir(t, wf, "scan_health", "cap_findings", edgeSubs), sharedScan)
			var gitleaks []any
			for _, g := range harvest.Inline {
				if g.File == "gitleaks.json" {
					gitleaks = g.Findings
				}
			}
			if len(gitleaks) == 0 {
				t.Fatalf("%s: cap_findings harvested no gitleaks findings from this pass — the reader and the writer no longer agree on where the scan output is (harvest %+v)", p.runID, harvest.Inline)
			}
			blob, err := json.Marshal(gitleaks)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(blob), p.tag) {
				t.Errorf("%s: its own gitleaks findings are gone, the harvest carries %s", p.runID, blob)
			}
			if strings.Contains(string(blob), other) {
				t.Errorf("%s: it read back the findings of %s — one gitleaks.json for two passes: %s", p.runID, other, blob)
			}

			// custom.json — the matcher envelope this pass wrote.
			var custom struct {
				Matchers map[string]struct {
					Findings []struct {
						ID string `json:"id"`
					} `json:"findings"`
				} `json:"matchers"`
			}
			readSecJSON(t, filepath.Join(dir, "custom.json"), &custom)
			got := ""
			if m, ok := custom.Matchers["tagger"]; ok && len(m.Findings) == 1 {
				got = m.Findings[0].ID
			}
			if got != p.tag {
				t.Errorf("%s: custom.json carries %q, want %q — the two passes share one matcher envelope", p.runID, got, p.tag)
			}

			// shards.json — the dispatch envelope this pass wrote.
			var shards struct {
				ShardCount int `json:"shard_count"`
			}
			readSecJSON(t, filepath.Join(dir, "shards.json"), &shards)
			want := map[string]int{"ALPHA": 2, "BETA": 7}[p.tag]
			if shards.ShardCount != want {
				t.Errorf("%s: shards.json reports shard_count=%d, want %d — the dispatch envelope of the other pass overwrote it", p.runID, shards.ShardCount, want)
			}

			// shard-files.json — the manifest this pass planned. late.go exists
			// from the second pass on, so a first pass that still lists it was
			// rewritten by its neighbour.
			var files []string
			readSecJSON(t, filepath.Join(dir, "shard-files.json"), &files)
			sawLate := false
			for _, f := range files {
				if strings.HasSuffix(f, "late.go") {
					sawLate = true
				}
			}
			if sawLate != (p.tag == "BETA") {
				t.Errorf("%s: shard-files.json lists late.go=%v, want %v — the manifest is not the one this pass planned (%v)", p.runID, sawLate, p.tag == "BETA", files)
			}

			// And the anti-façade gate must find this pass's own outputs where
			// it looks: a reader left pointing at the shared parent reports the
			// whole generic trio missing and hard-fails.
			exit, health := runSecScanHealth(t, secEdgeScanDir(t, wf, "scan_join", "scan_health", edgeSubs), sharedScan, ws)
			if exit != 0 {
				t.Errorf("%s: scan_health hard-failed on a pass whose three generic scanners all wrote (exit %d, %+v)", p.runID, exit, health)
			}
			if health["healthy"] != true {
				t.Errorf("%s: scan_health does not see this pass's own scanner outputs: %+v", p.runID, health)
			}
		})
	}
}

// secHarvest is the cap_findings envelope with findings typed loosely: the
// node harvests both dict-shaped scanner reports and bare arrays (gitleaks
// emits one, and so does the shard manifest), so an element is not always an
// object.
type secHarvest struct {
	Inline []struct {
		File     string `json:"file"`
		Findings []any  `json:"findings"`
	} `json:"inline"`
}

// runSecCapFindings runs the real cap_findings command against the scratch its
// edge points it at.
func runSecCapFindings(t *testing.T, passDir, sharedScan string) secHarvest {
	t.Helper()
	rendered := renderSec(t, secToolCommand(t, "cap_findings"), map[string]string{
		"{{input.scan_dir}}":               passDir,
		"{{vars.scan_dir}}":                sharedScan,
		"{{vars.findings_cap_per_file}}":   "50",
		"{{vars.triage_inline_max_bytes}}": "524288",
		"{{vars.deepsec_out}}":             filepath.Join(sharedScan, "deepsec.json"),
		"{{input.deepsec_paths}}":          shellQuote("{}"),
	})
	out, err := exec.Command("sh", "-c", rendered).Output()
	if err != nil {
		t.Fatalf("cap_findings exited non-zero (%v): %q", err, out)
	}
	var got secHarvest
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("cap_findings output is not JSON: %v (%q)", err, out)
	}
	return got
}

// runSecScanHealth runs the real scan_health command against the scratch its
// edge points it at, deep scan off, and returns its exit code and envelope.
func runSecScanHealth(t *testing.T, passDir, sharedScan, ws string) (int, map[string]any) {
	t.Helper()
	rendered := renderSec(t, secToolCommand(t, "scan_health"), map[string]string{
		"{{input.scan_dir}}":            passDir,
		"{{vars.scan_dir}}":             sharedScan,
		"{{vars.min_generic_scanners}}": "3",
		"{{input.langs}}":               "[]",
		"{{vars.workspace_dir}}":        ws,
		"{{vars.bundle_skills_dir}}":    filepath.Join(ws, ".claude", "iterion-skills"),
		"{{vars.enable_deepsec}}":       "false",
		"{{vars.deepsec_out}}":          filepath.Join(sharedScan, "deepsec.json"),
		"{{input.deepsec_paths}}":       shellQuote("{}"),
	})
	cmd := exec.Command("sh", "-c", rendered)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("harness failed to run scan_health: %v (stderr %q)", err, stderr.String())
		}
		exit = ee.ExitCode()
	}
	var env map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
		t.Fatalf("scan_health stdout is not the JSON envelope alone: %v (%q)", err, stdout.String())
	}
	return exit, env
}

func readSecJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — the writer of this pass did not land it where its pass directory is", filepath.Base(path), err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s is not the JSON its producer writes: %v (%q)", filepath.Base(path), err, raw)
	}
}

// The deep scanner keeps its own state directly under vars.scan_dir, and that
// is deliberate: deepsec-workspace/ holds the data root, the file-record cache
// and the claim mutex, so a second pass skipping what a first already analysed
// is the POINT. #1475 moves the scanner scratch, not that.
func TestDeepsecStateStaysUnderTheSharedScanDir(t *testing.T) {
	body := secToolCommand(t, "run_deepsec_scanner")
	if !strings.Contains(body, "SCAN_DIR={{vars.scan_dir}}") {
		t.Error("run_deepsec_scanner no longer takes the SHARED scan_dir: deepsec-workspace/ is the cross-pass file-record cache and the claim mutex, and the retention sweep in this node prunes the whole parent")
	}
	if !strings.Contains(body, `DSW="$SCAN_DIR/deepsec-workspace"`) {
		t.Error("the deepsec data root moved off $SCAN_DIR: a second pass can no longer skip what a first analysed")
	}
}

// #1475 keys one pass directory per audit under vars.scan_dir. Nothing else
// reclaims it: the engine scratch sweep stales top-level entries of
// PROJECT_SCRATCH_DIR by whole-subtree mtime (pkg/memory/scratch_sweep.go) and
// vars.scan_dir sits two levels below, so a workspace audited regularly never
// ages out. Before #1475 those scanner outputs were rewritten in ONE shared
// slot, constant size; after it, a directory per pass.
//
// The sweep therefore lives at the WRITER. plan_shards creates the directory
// and runs on every pass; run_deepsec_scanner runs only when enable_deepsec is
// true, which is NOT the default (main.bot: `enable_deepsec: bool = false`), so
// a sweep placed there alone would never reclaim pass-* on a default run.
//
// The mutation this reddens on is the forbidden alternative — the sweep back in
// the deep scanner only, i.e. removed from this node.
func TestPassDirIsSweptByTheNodeThatWritesIt(t *testing.T) {
	body := secToolCommand(t, "plan_shards")
	dir := t.TempDir()
	scanDir := filepath.Join(dir, "scan")
	ws := filepath.Join(dir, "ws")
	for _, d := range []string{scanDir, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const runID = "run-CURRENT"
	// Aged past the TTL. Only the pass-* pair is this node's shape.
	agedMine := []string{filepath.Join(scanDir, "pass-run-OLD-A"), filepath.Join(scanDir, "pass-run-OLD-B")}
	// Aged, and NOT this node's shape: another node owns them, or nobody does.
	agedOthers := []string{
		filepath.Join(scanDir, "deepsec-out-run-OLD"),
		filepath.Join(scanDir, "deepsec-logs-run-OLD"),
		filepath.Join(scanDir, "operator-cache"),
	}
	// Must stay whatever happens: this pass, and one inside the window.
	keep := []string{filepath.Join(scanDir, "pass-"+runID), filepath.Join(scanDir, "pass-run-FRESH")}

	all := append(append(append([]string{}, agedMine...), agedOthers...), keep...)
	for _, d := range all {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sentinel"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Every pass directory carries the manifest plan_shards writes into it,
	// and the sweep is gated on exactly that: the name alone would also match
	// a pass-manager an operator dropped under a repointed scan_dir.
	for _, d := range append(append([]string{}, agedMine...), keep...) {
		if err := os.WriteFile(filepath.Join(d, "shard-files.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Aged, pass-prefixed, and NOT written by this bot: it must survive.
	decoy := filepath.Join(scanDir, "pass-manager")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	agedOthers = append(agedOthers, decoy)
	old := time.Now().Add(-40 * 24 * time.Hour)
	for _, d := range append(append([]string{}, agedMine...), agedOthers...) {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	rendered := expandEngineBracedEnv(body)
	for ref, val := range map[string]string{
		"{{input.scan_dir}}":         filepath.Join(scanDir, "pass-"+runID),
		"{{vars.scan_dir}}":          scanDir,
		"{{vars.workspace_dir}}":     ws,
		"{{vars.bundle_skills_dir}}": filepath.Join(ws, ".claude", "iterion-skills"),
		"{{vars.shard_size}}":        "1",
		"{{run.id}}":                 runID,
		"{{vars.scan_dir_ttl_days}}": "30",
	} {
		rendered = strings.ReplaceAll(rendered, ref, val)
	}
	if i := strings.Index(rendered, "{{"); i >= 0 {
		t.Fatalf("unsubstituted ref left in the command near %q", rendered[i:min(i+60, len(rendered))])
	}
	runShell(t, rendered, filepath.Join(dir, "bin"))

	for _, d := range agedMine {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("%s survived: a pass directory older than scan_dir_ttl_days is not reclaimed by the node that writes it, so every default audit leaves one behind for ever", filepath.Base(d))
		}
	}
	for _, d := range append(append([]string{}, agedOthers...), keep...) {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s was pruned and must not be: this sweep is scoped POSITIVELY to pass-*, excludes the current run, and leaves every other shape — including the deep scanner's own and an operator directory — to their owners", filepath.Base(d))
		}
	}
}
