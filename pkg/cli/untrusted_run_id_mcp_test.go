package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRun_AnExistingForkRunIsRefusedBeforeItsCatalogBoots is the second
// witness of ADR-162's U4 round.
//
// `iterion run --run-id <id>` executes a run whose document may already exist,
// and the engine adopts that document's trust at Run. The compile and the MCP
// health check run BEFORE that adoption, so under the defect they answered to
// the CARRIER's trust — an operator shell is trusted — and a fork run named by
// id had the operator's plugin servers merged into its catalog and booted by
// the health check before the engine refused it. The CLI settles the trust
// before the compile instead (cliRunTrust: the carrier's, lowered by the
// recorded trust of the document the id names), so the fork row compiles with
// the ambient servers withheld and is refused by the admission floor. The
// control is a fresh id: no document, the carrier's trust, the plugin's server
// boots and fails the health check — proving the canary fires and the fixture
// is not inert.
func TestRun_AnExistingForkRunIsRefusedBeforeItsCatalogBoots(t *testing.T) {
	for _, row := range []struct {
		name  string
		exist bool
	}{
		{"a fresh id (the control)", false},
		{"an existing fork run", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("ITERION_HOME", home)
			// The carrier is the operator's own shell: the fork row proves the
			// DOCUMENT lowered the launch, not the environment.
			t.Setenv(envtrust.EnvRunTrust, "")
			canary := filepath.Join(t.TempDir(), "booted")
			plugin := filepath.Join(home, "plugins", "probe")
			if err := os.MkdirAll(plugin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte(
				"name: probe\nversion: 1.0.0\ndescription: indexer\nschema_version: 1\ndefault_enabled: true\n"+
					"contributes:\n  mcp_servers:\n    - name: plug\n      transport: stdio\n      command: /bin/sh\n      args:\n        - -c\n        - \"echo booted > '"+canary+"'; exit 1\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := writeFixture(t, dir, "test.bot", minimalWorkflow)
			storeDir := filepath.Join(dir, "store")
			const runID = "run-by-id"
			if row.exist {
				s, err := store.New(storeDir)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				r, err := s.CreateRun(ctx, runID, "wf", nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Trust = store.RunTrustFork
				if err := s.SaveRun(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			p, _ := newTestPrinter(cli.OutputHuman)
			runErr := cli.RunRun(context.Background(), cli.RunOptions{
				File:     path,
				StoreDir: storeDir,
				RunID:    runID,
				Sandbox:  noSandbox,
			}, p)
			_, statErr := os.Stat(canary)
			booted := statErr == nil

			if !row.exist {
				if !booted {
					t.Fatalf("the control did not boot the plugin server (err=%v) — the fork row proves nothing", runErr)
				}
				if runErr == nil {
					t.Fatal("the canary server exits 1, so the control's health check must fail the launch before the engine runs")
				}
				return
			}

			if booted {
				t.Error("the plugin server booted for a fork run named by id — the catalog was compiled for a trust this run does not hold")
			}
			s, err := store.New(storeDir)
			if err != nil {
				t.Fatal(err)
			}
			r, loadErr := s.LoadRun(context.Background(), runID)
			if runErr == nil || loadErr != nil || r.Admission == nil || r.Admission.Code != "untrusted_without_sandbox" {
				t.Fatalf("run (err=%v, admission=%+v, load=%v), want the typed admission refusal of an untrusted run without a per-run sandbox", runErr, r.Admission, loadErr)
			}
			if r.Trust != store.RunTrustFork {
				t.Fatalf("run document Trust = %q, want fork — the engine keeps the adopted trust", r.Trust)
			}
		})
	}
}
