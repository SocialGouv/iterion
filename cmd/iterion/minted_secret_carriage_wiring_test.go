package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A launcher that spawns `iterion run` hands the run's server-minted secrets
// over in the child's ENVIRONMENT rather than its command line
// (store.ServerMintedSecretEnv, pkg/runview/detached.go). The carriage only
// works if the spawned command TAKES them back out — and taking them out is
// also what keeps them from being inherited by every tool process the run
// spawns. Nothing else in the binary reads that environment, so a `run`
// command that stopped calling for them would lose the grant silently: the
// detached run would compute its review and have no way to publish it.
func TestRunTakesTheMintedSecretsFromItsEnvironment(t *testing.T) {
	root := moduleRoot(t)
	const take = "store.TakeServerMintedSecretVarsFromEnv("
	runCmdSrc, err := os.ReadFile(filepath.Join(root, "cmd", "iterion", "run.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(runCmdSrc), take) {
		t.Errorf("cmd/iterion/run.go does not call %s: a detached launch's publish grant would be dropped on the floor", take)
	}
	producer, err := os.ReadFile(filepath.Join(root, "pkg", "runview", "detached.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(producer), "store.ServerMintedSecretEnv(") {
		t.Error("pkg/runview/detached.go no longer carries the minted secrets by environment: check whether it went back to --var argv")
	}
}
