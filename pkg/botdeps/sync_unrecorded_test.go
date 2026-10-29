package botdeps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// `bots.lock` IS the record of management for `.botz/<name>`: the pin names
// the dependency in a committed file, and Sync's whole contract is to make
// `.botz` match that pin. So a dependency copy carrying no origin record — a
// workspace materialized before origins existed, or one a hand edit drifted
// — is still re-materialized, even though the installer's own --force
// refusal (#2689 item 2) would otherwise refuse to replace a target it has
// no record of installing. The refusal guards a DIFFERENT destination: an
// operator pointing --dest at authored bundles.
func TestSyncRematerializesADependencyCopyCarryingNoInstallRecord(t *testing.T) {
	workdir := t.TempDir()
	lockOneLocalDependency(t, workdir, "shared-planner")

	// A pre-origins materialization: the bundle is in place, drifted from
	// its pin, and nothing records who put it there.
	target := filepath.Join(workdir, ".botz", "shared-planner")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"manifest.yaml": "name: shared-planner\nschema_version: 1\nversion: 0.0.1\n",
		"main.bot":      "workflow drifted {}\n",
	} {
		if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", ".origins", "shared-planner.json")); err == nil {
		t.Fatal("the fixture is not the pre-origins shape this test exists for")
	}

	results, err := Sync(context.Background(), workdir)
	if err != nil {
		t.Fatalf("Sync over an unrecorded dependency copy: %v", err)
	}
	if len(results) != 1 || !results[0].Changed {
		t.Fatalf("Sync did not re-materialize the drifted copy: %+v", results)
	}
	body, err := os.ReadFile(filepath.Join(target, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got == "workflow drifted {}\n" {
		t.Fatalf("the drifted copy survived the sync: %q", got)
	}
}
