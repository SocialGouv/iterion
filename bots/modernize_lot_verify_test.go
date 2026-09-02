package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestModernizeLotVerifyCertifierAbsenceVsSilence pins the ONE distinction the
// extension certifier's refusal term rests on: a net that has no extension
// machinery is not a certifier that refused to answer.
//
// `lot_verify` treats an unusable verdict as a refusal — correctly, because the
// certifier runs on ledger content the constrained party writes, so a crash
// there is reachable from the ledger itself. But a net materialised BEFORE the
// extension feature declares no such mode and is never called at all, and
// collapsing those two cases turns every campaign against a pre-existing net
// into a lot that can neither converge nor stop: build green, oracle green, net
// untouched, and `refs_untouched` false naming a certifier that never ran.
//
// Both directions are pinned here because fixing either one alone is a
// regression in the other: absence must stay silent, and silence must stay a
// refusal.
func TestModernizeLotVerifyCertifierAbsenceVsSilence(t *testing.T) {
	for _, tool := range []string{"python3", "git", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}

	script := toolScript(t, "modernize/main.bot", "lot_verify")

	type lotReport struct {
		GatePassed    bool     `json:"gate_passed"`
		OraclePassed  bool     `json:"oracle_passed"`
		RefsUntouched bool     `json:"refs_untouched"`
		RefsChanged   []string `json:"refs_changed"`
		LogTail       string   `json:"log_tail"`
	}

	// harnessSrc is committed as the net's harness.py. The capability probe is
	// a literal search for the mode string, so what matters is only whether
	// that literal is present — not whether the file is a real harness.
	run := func(t *testing.T, harnessSrc string) lotReport {
		t.Helper()
		ws := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
			// The throwaway repo must not inherit the operator's global or
			// system config: a core.hooksPath or commit.gpgsign there fails
			// `git commit` for reasons unrelated to what this test pins.
			cmd.Env = append(os.Environ(),
				"GIT_CONFIG_GLOBAL=/dev/null",
				"GIT_CONFIG_SYSTEM=/dev/null",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v (%s)", args, err, out)
			}
		}
		git("init", "-q", "-b", "main")
		git("config", "user.email", "t@example.com")
		git("config", "user.name", "t")

		gm := filepath.Join(ws, ".golden-master")
		if err := os.MkdirAll(filepath.Join(gm, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
		write := func(rel, content string, mode os.FileMode) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(ws, rel), []byte(content), mode); err != nil {
				t.Fatal(err)
			}
		}
		write(".golden-master/harness.py", harnessSrc, 0o644)
		write(".golden-master/refs/a.txt", "ref A\n", 0o644)
		write(".golden-master/corpus.json", `{"entries":[]}`+"\n", 0o644)
		// A green oracle that reports no invalid mutants, so the only term
		// left free to move in this test is the extension one.
		write(".golden-master/verify-oracle.sh", "#!/bin/sh\necho '{\"invalid\": []}'\nexit 0\n", 0o755)
		write("app.txt", "v1\n", 0o644)
		git("add", "-A")
		git("commit", "-qm", "base")

		out, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse: %v", err)
		}
		base := strings.TrimSpace(string(out))

		// The lot: an honest source change that never touches the net.
		write("app.txt", "v2\n", 0o644)
		git("add", "-A")
		git("commit", "-qm", "lot: honest source change")

		body := script
		for ref, val := range map[string]string{
			"{{vars.workspace_dir}}": ws,
			"{{input.base_sha}}":     base,
			"{{input.refs_dir}}":     ".golden-master/refs",
			"{{input.exit_gate}}":    "true",
			"{{input.plan_path}}":    ".modernize/plan.yaml",
			"{{input.lot_id}}":       "L1",
		} {
			body = strings.ReplaceAll(body, ref, strconv.Quote(val))
		}
		if i := strings.Index(body, "{{"); i >= 0 {
			t.Fatalf("unresolved template ref in lot_verify near %q", body[i:min(i+40, len(body))])
		}
		scriptPath := filepath.Join(t.TempDir(), "lot_verify.py")
		if err := os.WriteFile(scriptPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, err := exec.Command("python3", scriptPath).Output()
		if err != nil {
			t.Fatalf("lot_verify failed to execute: %v (out %q)", err, stdout)
		}
		var res lotReport
		if uerr := json.Unmarshal(stdout, &res); uerr != nil {
			t.Fatalf("lot_verify output is not JSON: %v (out %q)", uerr, stdout)
		}
		if !res.GatePassed || !res.OraclePassed {
			t.Fatalf("fixture is not the intended one: gate=%v oracle=%v (log %q)",
				res.GatePassed, res.OraclePassed, res.LogTail)
		}
		return res
	}

	t.Run("a net with no extension machinery is not a refusal", func(t *testing.T) {
		// A pre-feature harness: no extension mode anywhere in the file, so
		// the probe never calls it. Calling it would be the bug the probe
		// exists for — its main() would fall through to the full gate path,
		// boot the application and run the mutant campaign.
		res := run(t, "print('pre-feature harness: no extension modes')\n")
		if !res.RefsUntouched {
			t.Fatalf("an honest lot on a pre-feature net was refused: "+
				"refs_untouched=false, changed=%v, log=%q",
				res.RefsChanged, res.LogTail)
		}
		if strings.Contains(res.LogTail, "certifier") {
			t.Fatalf("a net that has no certifier was blamed for one: %q", res.LogTail)
		}
	})

	t.Run("a certifier that cannot answer still refuses", func(t *testing.T) {
		// Declares the capability, so it IS called — and fails. Reading that
		// silence as absolution is what let a rewritten ledger ride a green.
		res := run(t, "import sys\n"+
			"mode = 'unset'\n"+
			"if mode == \"extend-verify\":\n    pass\n"+
			"sys.stderr.write('boom\\n')\n"+
			"raise SystemExit(3)\n")
		if res.RefsUntouched {
			t.Fatalf("a certifier that could not answer was read as absolution "+
				"(log %q)", res.LogTail)
		}
		if !strings.Contains(res.LogTail, "refusal, not an absence") {
			t.Fatalf("the refusal is not named in the log: %q", res.LogTail)
		}
	})
}
