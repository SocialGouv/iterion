package cli_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// An `iterion run` started by a node of an untrusted run — a script or an
// agent re-entering the CLI in the outsider's checkout, a shard of
// `__scan-shards` — carries that run's trust (envtrust.EnvRunTrust), and the
// run it launches adopts it: its document is marked, and the admission floor
// that keeps an untrusted run out of the runner's trust domain (#1773) judges
// it. The control is the operator's own invocation, which carries nothing.
func TestRun_ACarriedRunTrustLaunchesAnUntrustedRun(t *testing.T) {
	for _, row := range []struct {
		name    string
		carried string
		want    store.RunTrust
	}{
		{"the operator's own run (the control)", "", store.RunTrustDefault},
		{"a run started under a fork run's trust", "fork", store.RunTrustFork},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv(envtrust.EnvRunTrust, row.carried)
			dir := t.TempDir()
			path := writeFixture(t, dir, "test.bot", minimalWorkflow)
			storeDir := filepath.Join(dir, "store")
			p, _ := newTestPrinter(cli.OutputHuman)
			runErr := cli.RunRun(context.Background(), cli.RunOptions{
				File:     path,
				StoreDir: storeDir,
				RunID:    "carrier-run",
				Executor: &approveExecutor{},
				Sandbox:  noSandbox,
			}, p)

			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.LoadRun(context.Background(), "carrier-run")
			if err != nil {
				t.Fatalf("load run: %v (run error: %v)", err, runErr)
			}
			if r.Trust != row.want {
				t.Fatalf("run document Trust = %q, want %q — the run would execute the outsider's checkout as the tenant's own", r.Trust, row.want)
			}
			if row.want.Trusted() {
				if runErr != nil || r.Status != store.RunStatusFinished {
					t.Fatalf("the operator's own run ended %s (%v), want finished", r.Status, runErr)
				}
				return
			}
			if runErr == nil || r.Admission == nil || r.Admission.Code != "untrusted_without_sandbox" {
				t.Fatalf("an untrusted run with no per-run sandbox ran (err=%v, admission=%+v), want the typed admission refusal", runErr, r.Admission)
			}
		})
	}
}
