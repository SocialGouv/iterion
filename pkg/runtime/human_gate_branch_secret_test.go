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

const humanGateBranchBot = `prompt review_text:
  Check what the build read before approving.

schema decision:
  approve: bool

schema review_in:
  note: string

schema noted:
  ok: bool

tool readcfg:
  command: "cat creds.txt"

router fork:
  mode: fan_out_all

human review:
  interaction: human
  input: review_in
  output: decision
  instructions: review_text

compute other:
  output: noted
  expr:
    ok: "true"

compute gather:
  output: noted
  await: wait_all
  expr:
    ok: "true"

workflow w:
  worktree: none
  sandbox: none
  entry: readcfg
  budget:
    max_parallel_branches: 4
  readcfg -> fork
  fork -> review with {
    note: "{{outputs.readcfg.result}}",
  }
  fork -> other
  review -> gather
  other -> gather
  gather -> done
`

// The same gate inside a fan-out branch (pauseBranchAtHuman): its
// human_input_requested event carries the questions and the instructions.
func TestABranchHumanGatesEventIsScrubbed(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	wf := compileBotText(t, humanGateBranchBot)
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
	err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "branch-run", nil)
	if err == nil {
		t.Fatal("scenario broken: the run did not pause at the branch's human gate")
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
			if strings.Contains(line, secret) {
				t.Errorf("the branch's human_input_requested event carries the value")
			}
		}
		return nil
	})
	if seen == 0 {
		t.Fatal("scenario broken: no human_input_requested event")
	}
}
