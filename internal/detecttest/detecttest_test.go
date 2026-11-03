package detecttest

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
)

// Detection reads the staged host and nothing else: the claude_code CLI
// installed with no forfait — the fact the claude slot's routes are judged
// on (#1221) — and only the provider key named.
//
// Mutant: the PATH line removed from StageClaudeCodeHost → the stage's own
// resolution assertion fails it on a runner with no claude, and on a
// workstation with one.
func TestStageClaudeCodeHost_DetectionReadsTheStagedHost(t *testing.T) {
	StageClaudeCodeHost(t, "ANTHROPIC_API_KEY")
	rep := detect.Detect(context.Background())

	var claudeCode *detect.BackendStatus
	for i := range rep.Backends {
		if rep.Backends[i].Name == detect.BackendClaudeCode {
			claudeCode = &rep.Backends[i]
		}
	}
	if claudeCode == nil {
		t.Fatalf("detection reports no %s backend at all: %+v", detect.BackendClaudeCode, rep.Backends)
	}
	if !claudeCode.BinaryFound || claudeCode.Available {
		t.Errorf("%s reads BinaryFound=%v Available=%v, want the staged CLI found with no forfait (true, false)", detect.BackendClaudeCode, claudeCode.BinaryFound, claudeCode.Available)
	}
	want := map[string]bool{"anthropic": true, "zai": false, "openai": false}
	for _, p := range rep.Providers {
		if w, ok := want[p.Name]; ok && p.Available != w {
			t.Errorf("provider %s reads Available=%v, want %v — the stage names ANTHROPIC_API_KEY alone", p.Name, p.Available, w)
		}
	}
}

// The staged CLI answers detection's question and refuses every other one
// with its own message, so a code path that starts driving the CLI cannot
// pass on an answer the stage never gave. The probe is `--version`: a real
// CLI answers it with exit 0 and spends nothing, so a stage that did not
// take reds here without reaching any model.
//
// Mutant: the stage's refusal exiting 0 → reds.
func TestStageClaudeCodeHost_TheStagedCLIRefusesWhatItDoesNotAnswer(t *testing.T) {
	StageClaudeCodeHost(t)
	out, err := exec.Command("claude", "--version").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 64 {
		t.Fatalf("the staged CLI answered --version: err=%v output=%q, want exit 64", err, out)
	}
	if !strings.Contains(string(out), `answers only "auth status --json"`) {
		t.Errorf("the refusal does not name what the stage answers: %q", out)
	}
}
