package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestVerifyRunLiftsNotCoveredIntoAGreenTail: verify-build tells every carrier
// to print its NOT COVERED lines FIRST — after the checks, the echo becomes the
// script's exit status and hides the last check's failure — so each compiled
// verify_run lifts those lines into the log tail of a green run (past any
// amount of output), and a failing last check stays red.
func TestVerifyRunLiftsNotCoveredIntoAGreenTail(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	carriers := []struct{ bot, node string }{
		{"instrument/main.bot", "verify_run"}, {"feature-dev/main.bot", "verify_run"},
		{"whole-improve-loop/main.bot", "verify_run"}, {"branch-improve-loop/main.bot", "verify_run"},
		{"feature-gap-fill/main.bot", "verify_run"}, {"adr-cartograph/main.bot", "verify_run"},
		{"app-dev/main.bot", "verify_run"}, {"test-coverage/main.bot", "verify_run"},
		{"e2e-coverage/main.bot", "verify_run"}, {"dep-update-guard/main.bot", "verify_run"},
		{"secured-renovacy/main.bot", "p2_verify_run"},
	}
	const nc = "NOT COVERED: image Dockerfile, built by CI only"
	shapes := []struct {
		name, body string
		green      bool
	}{
		// 900 characters before it (a tool's banner): a carrier whose excerpt
		// keeps the head of the log must still lift it into the tail.
		{"printed first, then 400 lines, green", "#!/bin/sh\nset -e\nprintf '%900s\\n' banner\necho \"" + nc + "\"\ni=0\nwhile [ $i -lt 400 ]; do echo \"ok build line $i padding padding\"; i=$((i+1)); done\ntrue\n", true},
		{"printed first, a failing last check", "#!/bin/sh\nset -e\necho \"" + nc + "\"\ncommand -v no_such_tool_zz && no_such_tool_zz build\n", false},
	}
	for _, c := range carriers {
		c := c
		command := toolCommand(t, c.bot, c.node)
		for _, sh := range shapes {
			sh := sh
			t.Run(strings.Split(c.bot, "/")[0]+"/"+sh.name, func(t *testing.T) {
				ws, scratch := t.TempDir(), t.TempDir()
				gittest.Run(t, ws, "init", "-q")
				if err := os.WriteFile(filepath.Join(ws, "README"), []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				commitFixture(t, ws)
				if err := os.WriteFile(filepath.Join(scratch, "verify.sh"), []byte(sh.body), 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := command
				for k, v := range map[string]string{"{{vars.workspace_dir}}": ws, "{{vars.scratch_dir}}": scratch, "{{input.workspace_dir}}": ws,
					"{{input.scratch_dir}}": scratch, "{{vars.verify_timeout_s}}": "1500", "{{vars.matrix_path}}": "docs/e2e-coverage-matrix.md",
					"{{vars.target}}": "x"} {
					cmd = strings.ReplaceAll(cmd, k, v)
				}
				if i := strings.Index(cmd, "{{"); i >= 0 {
					t.Fatalf("unreplaced placeholder in %s: %q", c.bot, cmd[i:min(i+60, len(cmd))])
				}
				out, err := exec.Command("sh", "-c", cmd).Output()
				if err != nil {
					t.Fatalf("exec: %v %q", err, out)
				}
				var r struct {
					Passed  bool   `json:"passed"`
					LogTail string `json:"log_tail"`
				}
				if err := json.Unmarshal(out, &r); err != nil {
					t.Fatalf("json: %v %q", err, out)
				}
				if r.Passed != sh.green {
					t.Fatalf("%s: passed=%v, want %v (tail %q)", c.bot, r.Passed, sh.green, r.LogTail[max(0, len(r.LogTail)-200):])
				}
				// (Some carriers append their own gate notes after it.)
				if sh.green && !strings.Contains(r.LogTail, nc) {
					t.Fatalf("%s: a green run's tail lost the NOT COVERED line printed first: %q", c.bot, r.LogTail[max(0, len(r.LogTail)-200):])
				}
			})
		}
	}
}
