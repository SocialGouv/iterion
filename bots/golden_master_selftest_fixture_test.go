package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A fixture that failed to commit is not evidence about the extension rule.
// Exercise the real selftest, interrupting the sixth extension commit: this
// used to discard Git's failure, then panic at acted[0] on an empty ledger.
func TestGoldenMasterSelftestReportsBrokenGitFixture(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	harness, err := filepath.Abs("golden-master/oracle-harness.py")
	if err != nil {
		t.Fatal(err)
	}
	const program = `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("oracle", sys.argv[1])
oracle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oracle)
real_run, commits = oracle.run, [0]
def broken_git(cmd, cwd, timeout=oracle.CMD_TIMEOUT_S):
    if "commit -qm x" in cmd:
        commits[0] += 1
        if commits[0] == 6:
            return 128, "fatal: injected fixture commit failure"
    return real_run(cmd, cwd, timeout)
oracle.run = broken_git
if sys.argv[2] == "missing-act":
    oracle.extension_verdict = lambda *args: {"acted": [], "problems": ["injected empty verdict"]}
raise SystemExit(oracle._selftest())
`
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"failed-commit", []string{"selftest fixture", "commit -qm x", "exit 128", "injected fixture commit failure"}},
		{"missing-act", []string{"selftest extension fixture expected one acted entry", `"acted": []`, "injected empty verdict"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("python3", "-c", program, harness, tc.name)
			cmd.Dir = t.TempDir()
			cmd.Env = os.Environ()
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a broken fixture must stop the selftest: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("missing %q in fixture failure: %s", want, out)
				}
			}
			if strings.Contains(string(out), "Traceback") || strings.Contains(string(out), "IndexError") {
				t.Errorf("fixture failure was masked by a Python exception: %s", out)
			}
		})
	}
}
