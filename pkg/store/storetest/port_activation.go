package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func RunPortActivation(t *testing.T, factory Factory) {
	s := factory(t)
	capability := store.AsPortActivationStore(s)
	if capability == nil {
		t.Fatal("store lacks durable native activation capability")
	}
	ctx := store.WithoutTenantFilter(context.Background())
	now := time.Now().UTC()
	if err := store.RequirePortActivation(ctx, s, store.PortActivationLocal, now); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("fresh store allowed native execution: %v", err)
	}
	first := &store.PortActivation{Version: store.PortActivationVersion, Revision: 1, Enabled: true, Scope: store.PortActivationLocal,
		ProofDigest: strings.Repeat("a", 64), CapabilityDigest: strings.Repeat("b", 64), VerifiedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := capability.SavePortActivation(ctx, 0, first); err != nil {
		t.Fatal(err)
	}
	if err := store.RequirePortActivation(ctx, s, store.PortActivationLocal, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RequirePortActivationCapability(ctx, s, store.PortActivationLocal, strings.Repeat("c", 64), now); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("stale binary capability activated: %v", err)
	}
	if err := store.RequirePortActivation(ctx, s, store.PortActivationDistributed, now); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("local proof activated distributed scope: %v", err)
	}
	if err := store.RequirePortActivation(ctx, s, store.PortActivationLocal, now.Add(2*time.Hour)); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("expired proof activated native execution: %v", err)
	}
	second := *first
	second.Revision, second.Enabled = 2, false
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- capability.SavePortActivation(ctx, 1, &second) }()
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, store.ErrRunConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent activation CAS: %d winners, %d conflicts", wins, conflicts)
	}
	if err := store.RequirePortActivation(ctx, s, store.PortActivationLocal, now); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("disabled activation still allowed new runs: %v", err)
	}
	loaded, err := capability.LoadPortActivation(ctx)
	if err != nil || loaded == nil || loaded.Revision != 2 || loaded.Enabled {
		t.Fatalf("activation record: %+v %v", loaded, err)
	}
}
