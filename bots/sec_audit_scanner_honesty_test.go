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

// scan_health decides coverage by looking at the FILESYSTEM: a scanner output
// file that exists and parses counts as "that scanner ran". So a tool that
// runs, FAILS, and still leaves a parseable (empty) artifact reads as full
// coverage over a broken toolchain — healthy:true, missing:[], no banner on
// the report.
//
// Measured on a real audit: deepsec failed its preflight, contributed 0 of the
// 112 findings it produces on that repo, and the run reported
// healthy:true / degraded:false / missing:[] because an empty deepsec.json sat
// in scan_dir. The audit lost its whole deep-analysis layer and said nothing.
//
// The fix is at the PRODUCERS, not at the gate: a subscanner that failed drops
// its output file and stops claiming its json_paths entry, which is what
// run_lang_scanners has always done. These tests run the REAL command bodies
// against stub scanners, because the defect is in what the shell leaves on
// disk — asserting on the source text would only pin a spelling.

// secToolCommand returns the shell body of one tool node of the sec bot.
func secToolCommand(t *testing.T, node string) string {
	t.Helper()
	wf := compilePlanPhaseBot(t, "sec-audit-source")
	tn, ok := wf.Nodes[node].(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is %T, want *ir.ToolNode", node, wf.Nodes[node])
	}
	if strings.TrimSpace(tn.Command) == "" {
		t.Fatalf("%s has an empty command", node)
	}
	return tn.Command
}

// stubBin writes an executable shell stub and returns its directory.
func stubBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runShell executes a rendered command body with a stub PATH and returns its
// stdout. The bodies always exit 0 by contract, so a non-zero status is itself
// a failure worth reporting.
func runShell(t *testing.T, body, stubDir string, env ...string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", body)
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command exited non-zero (%v); these nodes must always exit 0 and report through the envelope. stdout: %q", err, out)
	}
	return string(out)
}

type envelope struct {
	Scanner   string            `json:"scanner"`
	JSONPaths map[string]string `json:"json_paths"`
	Errors    []string          `json:"errors"`
	Findings  int               `json:"finding_count"`
}

func lastJSONLine(t *testing.T, out string) envelope {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var env envelope
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("envelope is not JSON: %v (raw %q)", err, out)
	}
	return env
}

// TestGenericScannerDropsAFailedToolsOutput pins both faces: a crashed scanner
// leaves nothing behind, and a scanner that merely found nothing keeps its
// file. A gate that reddens on a clean repo is as useless as one that stays
// green on a broken toolchain.
func TestGenericScannerDropsAFailedToolsOutput(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no shell")
	}
	body := secToolCommand(t, "run_generic_scanners")

	render := func(scanDir, ws string) string {
		out := body
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":      scanDir,
			"{{vars.workspace_dir}}": ws,
		} {
			out = strings.ReplaceAll(out, ref, val)
		}
		if strings.Contains(out, "{{") {
			t.Fatalf("unsubstituted ref left in the command: %s", out[strings.Index(out, "{{"):min(len(out), strings.Index(out, "{{")+40)])
		}
		return out
	}

	t.Run("a scanner that writes a file and then fails leaves nothing behind", func(t *testing.T) {
		dir := t.TempDir()
		scanDir, ws, stubs := filepath.Join(dir, "scan"), filepath.Join(dir, "ws"), filepath.Join(dir, "bin")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		stubBin(t, stubs, "gitleaks", `for a in "$@"; do case "$a" in --report-path=*) echo '[]' > "${a#--report-path=}";; esac; done; exit 0`)
		// The measured shape: trivy writes a partial report, then dies. Without
		// the drop, that file is indistinguishable from a clean scan.
		stubBin(t, stubs, "trivy", `for a in "$@"; do case "$a" in --output=*) echo '{"Results":[]}' > "${a#--output=}";; esac; done; exit 1`)
		stubBin(t, stubs, "semgrep", `for a in "$@"; do case "$a" in --output=*) echo '{"results":[]}' > "${a#--output=}";; esac; done; exit 0`)

		env := lastJSONLine(t, runShell(t, render(scanDir, ws), stubs))

		if _, claimed := env.JSONPaths["trivy"]; claimed {
			t.Error("the envelope still claims json_paths.trivy after trivy failed — triage will read the file of a scanner that crashed")
		}
		if _, ok := env.JSONPaths["gitleaks"]; !ok {
			t.Error("gitleaks succeeded but its path was dropped — the drop must be per-subscanner, not per-node")
		}
		if len(env.Errors) != 1 || !strings.Contains(env.Errors[0], "trivy") {
			t.Errorf("errors must name the tool that failed, got %v", env.Errors)
		}
		if _, err := os.Stat(filepath.Join(scanDir, "trivy.json")); !os.IsNotExist(err) {
			t.Error("trivy.json is still on disk after trivy failed — scan_health reads the filesystem, so this file alone restores the facade")
		}
		for _, keep := range []string{"gitleaks.json", "semgrep-auto.json"} {
			if _, err := os.Stat(filepath.Join(scanDir, keep)); err != nil {
				t.Errorf("%s was removed although its scanner succeeded: %v", keep, err)
			}
		}
	})

	t.Run("a clean scan keeps every file and reports NO error", func(t *testing.T) {
		dir := t.TempDir()
		scanDir, ws, stubs := filepath.Join(dir, "scan"), filepath.Join(dir, "ws"), filepath.Join(dir, "bin")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		stubBin(t, stubs, "gitleaks", `for a in "$@"; do case "$a" in --report-path=*) echo '[]' > "${a#--report-path=}";; esac; done; exit 0`)
		stubBin(t, stubs, "trivy", `for a in "$@"; do case "$a" in --output=*) echo '{"Results":[]}' > "${a#--output=}";; esac; done; exit 0`)
		stubBin(t, stubs, "semgrep", `for a in "$@"; do case "$a" in --output=*) echo '{"results":[]}' > "${a#--output=}";; esac; done; exit 0`)

		env := lastJSONLine(t, runShell(t, render(scanDir, ws), stubs))

		if len(env.Errors) != 0 {
			// It used to emit ["%s"] fed by an often-empty ERRS, so errors carried
			// one empty string on every run and no consumer could use it as a
			// signal. %q, not %v: [""] and [] print alike under %v.
			t.Errorf("a clean run must report an EMPTY errors list, got %q", env.Errors)
		}
		if len(env.JSONPaths) != 3 {
			t.Errorf("a clean run must claim all three paths, got %v", env.JSONPaths)
		}
	})
}

// TestDeepsecDropsAnUnusableExport is the node the measured facade came from.
// It runs the real body against a stub deepsec, so what is asserted is what
// the shell actually leaves in scan_dir.
func TestDeepsecDropsAnUnusableExport(t *testing.T) {
	body := secToolCommand(t, "run_deepsec_scanner")

	run := func(t *testing.T, deepsecStub string) (envelope, string) {
		t.Helper()
		dir := t.TempDir()
		scanDir, ws, stubs := filepath.Join(dir, "scan"), filepath.Join(dir, "ws"), filepath.Join(dir, "bin")
		out := filepath.Join(scanDir, "deepsec.json")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		// node is probed for a version >= 22 before deepsec is even looked up.
		stubBin(t, stubs, "node", `echo v22.0.0`)
		stubBin(t, stubs, "deepsec", deepsecStub)

		rendered := body
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":              scanDir,
			"{{vars.workspace_dir}}":         ws,
			"{{vars.deepsec_out}}":           out,
			"{{vars.deepsec_concurrency}}":   "1",
			"{{vars.deepsec_process_limit}}": "0",
			"{{vars.deepsec_root}}":          filepath.Join(dir, "absent"),
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left in the command near %q", rendered[strings.Index(rendered, "{{"):])
		}
		return lastJSONLine(t, runShell(t, rendered, stubs)), out
	}

	t.Run("a failed step with nothing exported drops the file and the claim", func(t *testing.T) {
		// export writes an empty array AND fails: exactly what sat in scan_dir on
		// the run that reported healthy:true with zero deepsec findings.
		env, out := run(t, `case "$1" in export) for a in "$@"; do case "$prev" in --out) echo '[]' > "$a";; esac; prev="$a"; done; exit 1;; *) exit 0;; esac`)

		if len(env.JSONPaths) != 0 {
			t.Errorf("the envelope still claims %v after an unusable export — triage would read an empty file as a completed deep scan", env.JSONPaths)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Error("deepsec.json is still on disk — scan_health counts it as present and the run reads healthy, which is the exact facade this guards")
		}
		if len(env.Errors) == 0 {
			t.Error("the envelope reports no error although a step failed")
		}
	})

	t.Run("findings survive a failed step", func(t *testing.T) {
		// scan fails (no retry, no backoff) but export still yields findings: the
		// results are real and must NOT be thrown away. A guard that also fires
		// here would be hysterical, and would silently thin audits itself.
		env, out := run(t, `case "$1" in scan) exit 1;; export) for a in "$@"; do case "$prev" in --out) echo '[{"id":1},{"id":2}]' > "$a";; esac; prev="$a"; done; exit 0;; *) exit 0;; esac`)

		if env.JSONPaths["deepsec"] == "" {
			t.Error("the envelope dropped a path whose file holds real findings — a failed step is not a reason to discard results")
		}
		if _, err := os.Stat(out); err != nil {
			t.Errorf("deepsec.json was deleted although it holds 2 findings: %v", err)
		}
		if env.Findings != 2 {
			t.Errorf("finding_count = %d, want 2", env.Findings)
		}
	})
}

// TestScanHealthReddensWhenAScannerOutputIsGone closes the loop: the drop above
// only matters if the gate downstream actually reacts to it. Both faces, on the
// real scan_health body.
func TestScanHealthReddensWhenAScannerOutputIsGone(t *testing.T) {
	body := secToolCommand(t, "scan_health")

	type health struct {
		Healthy  bool             `json:"healthy"`
		Degraded bool             `json:"degraded"`
		Missing  []map[string]any `json:"missing"`
		Present  []string         `json:"present"`
	}

	run := func(t *testing.T, files map[string]string) health {
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
		rendered := body
		for ref, val := range map[string]string{
			"{{vars.scan_dir}}":             scanDir,
			"{{vars.min_generic_scanners}}": "2",
			"{{input.langs}}":               "[]",
			"{{vars.workspace_dir}}":        dir,
			"{{vars.enable_deepsec}}":       "true",
			"{{vars.deepsec_out}}":          filepath.Join(scanDir, "deepsec.json"),
		} {
			rendered = strings.ReplaceAll(rendered, ref, val)
		}
		if strings.Contains(rendered, "{{") {
			t.Fatalf("unsubstituted ref left in the command near %q", rendered[strings.Index(rendered, "{{"):])
		}
		out, err := exec.Command("sh", "-c", rendered).Output()
		if err != nil {
			t.Fatalf("scan_health exited non-zero: %v (out %q)", err, out)
		}
		var h health
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &h); err != nil {
			t.Fatalf("scan_health output is not JSON: %v (%q)", err, out)
		}
		return h
	}

	full := map[string]string{
		"gitleaks.json":     `[]`,
		"trivy.json":        `{"Results":[]}`,
		"semgrep-auto.json": `{"results":[]}`,
		"custom.json":       `{"matchers":{}}`,
		"deepsec.json":      `[]`,
	}

	t.Run("every output present reads healthy", func(t *testing.T) {
		h := run(t, full)
		if !h.Healthy || h.Degraded {
			t.Errorf("a complete scan_dir must read healthy, got healthy=%v degraded=%v missing=%v", h.Healthy, h.Degraded, h.Missing)
		}
	})

	t.Run("a dropped deepsec output makes the run degraded, not healthy", func(t *testing.T) {
		partial := map[string]string{}
		for k, v := range full {
			if k == "deepsec.json" {
				continue
			}
			partial[k] = v
		}
		h := run(t, partial)
		if h.Healthy {
			t.Error("deepsec.json absent yet the run reads healthy — the deep-analysis layer can then vanish without a word on the report")
		}
		if !h.Degraded {
			t.Error("deepsec.json absent yet degraded=false — report_card prints no coverage banner")
		}
		found := false
		for _, m := range h.Missing {
			if f, _ := m["file"].(string); f == "deepsec.json" {
				found = true
			}
		}
		if !found {
			t.Errorf("missing[] does not name deepsec.json, got %v", h.Missing)
		}
	})
}
