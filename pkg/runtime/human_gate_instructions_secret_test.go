package runtime

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/store"
)

const humanGateInstructionsBot = `prompt review_text:
  Check what the build read before approving:
  {{input.note}}

schema decision:
  approve: bool

schema review_in:
  note: string

tool readcfg:
  command: "cat creds.txt"

human review:
  interaction: human
  input: review_in
  output: decision
  instructions: review_text

workflow w:
  worktree: none
  sandbox: none
  entry: readcfg
  readcfg -> review with {
    note: "{{outputs.readcfg.result}}",
  }
  review -> done
`

// A human gate's instructions render an upstream output that quotes a
// registered value: the human_input_requested event carries it in none of
// its fields.
func TestAHumanGatesInstructionsAreScrubbedInTheEventLog(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	wf := compileBotText(t, humanGateInstructionsBot)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	exec := model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })
	storeDir := t.TempDir()
	s, serr := store.New(storeDir)
	if serr != nil {
		t.Fatal(serr)
	}
	err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "instr-run", nil)
	if err == nil {
		t.Fatal("scenario broken: the run did not pause at the human gate")
	}
	seen := 0
	_ = filepath.WalkDir(storeDir, func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() || filepath.Base(p) != "events.jsonl" {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "human_input_requested") {
				continue
			}
			seen++
			if !strings.Contains(line, "instructions") || !strings.Contains(line, "__ITERION_SECRET_DB__") {
				t.Fatalf("scenario broken: the event has no instructions or no scrubbed question: %s", line)
			}
			if strings.Contains(line, secret) {
				i := strings.Index(line, secret)
				t.Errorf("the human_input_requested event carries the value: ...%s...", line[max(0, i-160):min(len(line), i+40)])
			}
		}
		return nil
	})
	if seen == 0 {
		t.Fatal("scenario broken: no human_input_requested event")
	}
}
