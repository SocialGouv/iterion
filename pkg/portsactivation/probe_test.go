package portsactivation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestLocalProbeActivateAndRollback(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := ProbeLocal(ctx, s, false); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("unknown local automation was accepted: %v", err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *proof
	tampered.StoreIdentity += "-other"
	if _, err := ActivateLocal(ctx, s, tampered); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("edited proof accepted: %v", err)
	}
	record, err := ActivateLocal(ctx, s, *proof)
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 1 || record.ProofDigest != proof.Digest {
		t.Fatalf("activation lost proof: %+v", record)
	}
	if err := store.RequirePortActivationCapability(ctx, s, store.PortActivationLocal, CapabilityDigest(store.PortActivationLocal), time.Now()); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := Disable(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Revision != 2 || rolledBack.Enabled {
		t.Fatalf("rollback did not preserve epoch: %+v", rolledBack)
	}
	if err := store.RequirePortActivationCapability(ctx, s, store.PortActivationLocal, CapabilityDigest(store.PortActivationLocal), time.Now()); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("rollback allowed a new run: %v", err)
	}
}
