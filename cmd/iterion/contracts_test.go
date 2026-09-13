package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

func TestContractsCLIInspectsProbesActivatesAndRollsBack(t *testing.T) {
	previousStore, previousExclusive, previousProof, previousJSON := contractsStoreDir, contractsExclusiveStore, contractsProofFile, jsonOutput
	t.Cleanup(func() {
		contractsStoreDir, contractsExclusiveStore, contractsProofFile, jsonOutput = previousStore, previousExclusive, previousProof, previousJSON
	})
	contractsStoreDir = t.TempDir()
	jsonOutput = true
	run := func(cmd *cobra.Command) []byte {
		t.Helper()
		var out bytes.Buffer
		cmd.SetContext(context.Background())
		cmd.SetOut(&out)
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	var inspection portsactivation.Inspection
	if err := json.Unmarshal(run(contractsInspectCmd), &inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Activation != nil || inspection.Scope != store.PortActivationLocal {
		t.Fatalf("fresh inspection: %+v", inspection)
	}
	if _, err := os.Stat(filepath.Join(contractsStoreDir, "runs")); !os.IsNotExist(err) {
		t.Fatalf("read-only inspection created runs/: %v", err)
	}
	contractsExclusiveStore = true
	proofBytes := run(contractsProbeCmd)
	var proof portsactivation.Proof
	if err := json.Unmarshal(proofBytes, &proof); err != nil {
		t.Fatal(err)
	}
	if proof.StoreIdentity != inspection.StoreIdentity {
		t.Fatalf("probe changed store identity: %+v", proof)
	}
	contractsProofFile = filepath.Join(t.TempDir(), "proof.json")
	if err := os.WriteFile(contractsProofFile, proofBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var active store.PortActivation
	if err := json.Unmarshal(run(contractsActivateCmd), &active); err != nil || !active.Enabled {
		t.Fatalf("activation: %+v %v", active, err)
	}
	var disabled store.PortActivation
	if err := json.Unmarshal(run(contractsDeactivateCmd), &disabled); err != nil || disabled.Enabled || disabled.Revision != active.Revision+1 {
		t.Fatalf("rollback: %+v %v", disabled, err)
	}
}
