package cli_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// First attempt fails with a needle-free error; the resume (no recovery
// dispatcher on `iterion resume`) meets an HTTP-client wording in stderr.
func TestR3ResumeSurfaceNeedles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	marker := filepath.Join(t.TempDir(), "second")
	bot := `dsl: 2

schema out:
  ok: bool

tool call_api:
  command: "if [ -f ` + marker + ` ]; then echo 'AxiosError: Request failed with status code 400' >&2; else echo 'first attempt broke' >&2; fi; exit 1"
  output: out

workflow api:
  worktree: none
  sandbox: none
  entry: call_api
  call_api -> done
`
	path := r3WriteBot(t, bot)
	storeDir := t.TempDir()
	p := &cli.Printer{W: io.Discard, Format: cli.OutputJSON}
	_ = cli.RunRun(context.Background(), cli.RunOptions{File: path, StoreDir: storeDir, RunID: "rn", NoInteractive: true, MergeInto: "none"}, p)
	st, _ := store.New(storeDir)
	r, _ := st.LoadRun(context.Background(), "rn")
	t.Logf("run:    status=%s code=%s", r.Status, r.FailureCode)
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cli.RunResumeWithFile(context.Background(), path, cli.ResumeOptions{RunID: "rn", StoreDir: storeDir, MergeInto: "none"}, p)
	r, _ = st.LoadRun(context.Background(), "rn")
	if r.FailureCode != store.FailureExecutionFailed || !retrypolicy.AutoResumable(r.FailureCode) {
		t.Errorf("resume reads tool stderr as provider: %+v", r)
	}
	t.Logf("resume: status=%s code=%s autoResumable=%v deterministic=%v", r.Status, r.FailureCode, retrypolicy.AutoResumable(r.FailureCode), retrypolicy.IsDeterministic(r.FailureCode))
}
