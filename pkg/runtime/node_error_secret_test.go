package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

const leakingToolBot = `tool leak:
  command: "cat creds.txt; exit 3"

workflow w:
  worktree: none
  sandbox: none
  entry: leak
  leak -> done
`

// A tool node fails printing a registered secret: what the engine persists
// (run.json) and returns — the error an error tracker walks link by link —
// carries its placeholder only.
func TestAFailingToolNodesErrorCarriesNoSecretOutOfTheEngine(t *testing.T) {
	const secret = "hunter2-9f8e7d6c5b4a"
	wf := compileBotText(t, leakingToolBot)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "creds.txt"), []byte("db_password: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	exec := model.NewClawExecutor(nil, wf, model.WithWorkDir(ws), model.WithSecretGuard(guard))
	t.Cleanup(func() { _ = exec.Close() })
	s := tmpStore(t)
	err := New(wf, s, exec, WithWorkDir(ws), WithSandboxOverride("none")).Run(t.Context(), "leak-run", nil)
	if err == nil {
		t.Fatal("scenario broken: no error")
	}
	run, _ := s.LoadRun(t.Context(), "leak-run")
	t.Logf("run.Error = %q", run.Error)
	if strings.Contains(run.Error, secret) {
		t.Errorf("run.json carries the secret")
	}
	var walk func(e error, depth int)
	walk = func(e error, depth int) {
		if e == nil || depth > 20 {
			return
		}
		t.Logf("%*slink %T: %q", depth*2, "", e, e.Error())
		if strings.Contains(e.Error(), secret) {
			t.Errorf("a link of the returned error's chain carries the secret: %T", e)
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				walk(c, depth+1)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap(), depth+1)
		}
	}
	walk(err, 0)
}
