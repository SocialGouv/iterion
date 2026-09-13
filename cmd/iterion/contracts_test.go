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

func TestContractsCLIProbeCreatesFreshPrivateLocalStore(t *testing.T) {
	previousStore, previousExclusive := contractsStoreDir, contractsExclusiveStore
	t.Cleanup(func() {
		contractsStoreDir, contractsExclusiveStore = previousStore, previousExclusive
	})
	contractsStoreDir = filepath.Join(t.TempDir(), "fresh", "store")
	contractsExclusiveStore = true
	if _, err := os.Stat(contractsStoreDir); !os.IsNotExist(err) {
		t.Fatalf("fixture already has a store: %v", err)
	}
	var out bytes.Buffer
	contractsProbeCmd.SetContext(context.Background())
	contractsProbeCmd.SetOut(&out)
	if err := contractsProbeCmd.RunE(contractsProbeCmd, nil); err != nil {
		t.Fatal(err)
	}
	var proof portsactivation.Proof
	if err := json.Unmarshal(out.Bytes(), &proof); err != nil || proof.StoreIdentity == "" {
		t.Fatalf("fresh store has no proof: %+v %v", proof, err)
	}
	info, err := os.Stat(contractsStoreDir)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("new store is missing or not private: %v %v", info, err)
	}
}
