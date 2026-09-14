package authority

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation/natsconfig"
	"github.com/SocialGouv/iterion/pkg/store"
)

type testRefreshLeaseSource struct {
	token uint64
}

func (s *testRefreshLeaseSource) Acquire(_ context.Context, _ uint64, expiresAt time.Time) (store.PortActivationRefreshLease, error) {
	s.token++
	return store.PortActivationRefreshLease{Owner: "server-test", Token: s.token, ExpiresAt: expiresAt}, nil
}

func TestAuthorityRefresherRenewsWithoutChangingPolicy(t *testing.T) {
	authority, filesystem := authorityAdapterFixture(t)
	ctx := context.Background()
	if _, err := authority.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := authority.Activate(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	initialExpiry := initial.ExpiresAt
	refresher := &Refresher{Authority: authority, Leases: &testRefreshLeaseSource{}}
	if err := refresher.refresh(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, err := filesystem.LoadPortActivation(ctx)
	if err != nil || current.Revision != initial.Revision || current.ProofRevision != initial.ProofRevision+1 ||
		!current.Enabled || !current.ExpiresAt.After(initialExpiry) {
		t.Fatalf("refresh changed policy or did not advance freshness: %+v, %v", current, err)
	}
	proof, err := filesystem.LoadPortDistributedProof(ctx)
	if err != nil || proof.ProofRevision != current.ProofRevision || proof.PolicyRevision != current.Revision {
		t.Fatalf("refreshed proof = %+v, %v", proof, err)
	}
}

func TestAuthorityRefresherRequiresStableFingerprint(t *testing.T) {
	authority, filesystem := authorityAdapterFixture(t)
	ctx := context.Background()
	if _, err := authority.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	initial, err := authority.Activate(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	initialExpiry := initial.ExpiresAt
	authority.Adapter.Observe = func(context.Context, AdapterConfig) (*DeploymentCorroboration, error) {
		now := time.Now().UTC()
		return &DeploymentCorroboration{AuthoritySecretUID: "authority-uid", AuthoritySecretRevision: "7",
			DeploymentRevision: "release-17", Epoch: 3, Queue: authorityRecordQueue(t),
			ObservationDigest: strings.Repeat("b", 64), StartedAt: now, CompletedAt: now}, nil
	}
	refresher := &Refresher{Authority: authority, Leases: &testRefreshLeaseSource{}}
	if err := refresher.refresh(ctx, time.Now().UTC()); err == nil {
		t.Fatal("fingerprint change was renewed automatically")
	}
	current, err := filesystem.LoadPortActivation(ctx)
	if err != nil || current.ProofRevision != initial.ProofRevision || !current.ExpiresAt.Equal(initialExpiry) {
		t.Fatalf("fingerprint refusal changed active freshness: %+v, %v", current, err)
	}
	if _, err := authority.Activate(ctx, 1); !errors.Is(err, store.ErrPortActivation) {
		// No new probe candidate exists; this protects the explicit reactivation
		// boundary after a changed deployment fingerprint.
		t.Fatalf("changed fingerprint unexpectedly activated: %v", err)
	}
}

func authorityRecordQueue(t *testing.T) natsconfig.QueueTopology {
	t.Helper()
	return staticFixture(t).Queue
}
