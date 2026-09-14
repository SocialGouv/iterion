package authority

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
	natsclient "github.com/nats-io/nats.go"
)

func authorityAdapterFixture(t *testing.T) (*Authority, *store.FilesystemRunStore) {
	t.Helper()
	record := staticFixture(t)
	filesystem, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.PortStoreIdentity(filesystem)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &DeploymentAdapter{Config: AdapterConfig{
		KubectlBinary: "kubectl", AuthorityRef: "trusted/authority",
		KubernetesNamespaces: []string{"trusted", "worker"}, SystemNATSURL: "nats://sys:password@nats.example",
		QueueClient: &natsclient.Conn{}, ExpectedQueue: record.Queue,
	}}
	adapter.Record = func(context.Context, AdapterConfig) (*Record, error) { return record, nil }
	adapter.Observe = func(context.Context, AdapterConfig) (*DeploymentCorroboration, error) {
		now := time.Now().UTC()
		return &DeploymentCorroboration{AuthoritySecretUID: "authority-uid", AuthoritySecretRevision: "7",
			DeploymentRevision: record.DeploymentRevision, Epoch: record.Epoch, Queue: record.Queue,
			ObservationDigest: strings.Repeat("a", 64), StartedAt: now, CompletedAt: now}, nil
	}
	authority := &Authority{Adapter: adapter, Activations: filesystem, Proofs: filesystem, Candidates: filesystem,
		StoreIdentity: identity, CapabilityDigest: portsactivation.CapabilityDigest(store.PortActivationDistributed)}
	return authority, filesystem
}

func TestAuthorityProbeAndActivateKeepCandidateSeparateFromLiveProof(t *testing.T) {
	authority, filesystem := authorityAdapterFixture(t)
	ctx := context.Background()
	candidate, err := authority.Probe(ctx)
	if err != nil || candidate.PolicyRevision != 1 || candidate.ProofRevision != 1 {
		t.Fatalf("first probe = %+v, %v", candidate, err)
	}
	if proof, err := filesystem.LoadPortDistributedProof(ctx); err != nil || proof != nil {
		t.Fatalf("probe replaced live proof: %+v, %v", proof, err)
	}
	active, err := authority.Activate(ctx, 0)
	if err != nil || active == nil || active.Revision != 1 || !active.Enabled {
		t.Fatalf("first activation = %+v, %v", active, err)
	}
	proof, err := filesystem.LoadPortDistributedProof(ctx)
	if err != nil || proof == nil || proof.PolicyRevision != 1 {
		t.Fatalf("first activation did not publish proof: %+v, %v", proof, err)
	}
	if candidate, err := filesystem.LoadPortDistributedProofCandidate(ctx); err != nil || candidate != nil {
		t.Fatalf("candidate was not cleared after activation: %+v, %v", candidate, err)
	}

	newCandidate, err := authority.Probe(ctx)
	if err != nil || newCandidate.PolicyRevision != 2 || newCandidate.ProofRevision != 2 {
		t.Fatalf("second probe = %+v, %v", newCandidate, err)
	}
	if _, err = authority.Activate(ctx, 0); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale activation request returned %v", err)
	}
	active, err = filesystem.LoadPortActivation(ctx)
	if err != nil || active.Revision != 1 || !active.Enabled {
		t.Fatalf("stale activation changed live policy: %+v, %v", active, err)
	}
	proof, err = filesystem.LoadPortDistributedProof(ctx)
	if err != nil || proof.PolicyRevision != 1 {
		t.Fatalf("stale activation clobbered live proof: %+v, %v", proof, err)
	}
	if _, err := authority.Activate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	active, err = filesystem.LoadPortActivation(ctx)
	if err != nil || active.Revision != 2 {
		t.Fatalf("second activation was not committed: %+v, %v", active, err)
	}
}

func TestAuthorityRefusesSecondActivationWithoutAtomicBackendWriter(t *testing.T) {
	authority, filesystem := authorityAdapterFixture(t)
	// The concrete filesystem store has the writer; wrap only the activation
	// surface to model an older backend that cannot publish both documents.
	if _, err := authority.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	legacy := &activationOnlyStore{PortActivationStore: filesystem}
	authority.Activations = legacy
	if _, err := authority.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(context.Background(), 1); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("activation through a non-atomic backend returned %v", err)
	}
}

func TestAuthorityCanReactivateAfterDisableWithRetainedProof(t *testing.T) {
	authority, filesystem := authorityAdapterFixture(t)
	ctx := context.Background()
	if _, err := authority.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Activate(ctx, 0); err != nil {
		t.Fatal(err)
	}
	disabled, err := filesystem.DisablePortActivation(ctx, 1)
	if err != nil || disabled.Enabled || disabled.Revision != 2 {
		t.Fatalf("disabled activation = %+v, %v", disabled, err)
	}
	candidate, err := authority.Probe(ctx)
	if err != nil || candidate.PolicyRevision != 3 || candidate.ProofRevision != 2 {
		t.Fatalf("reactivation probe = %+v, %v", candidate, err)
	}
	active, err := authority.Activate(ctx, 2)
	if err != nil || active == nil || !active.Enabled || active.Revision != 3 {
		t.Fatalf("reactivation = %+v, %v", active, err)
	}
	proof, err := filesystem.LoadPortDistributedProof(ctx)
	if err != nil || proof == nil || proof.PolicyRevision != 3 || proof.ProofRevision != 2 {
		t.Fatalf("reactivation proof = %+v, %v", proof, err)
	}
}

type activationOnlyStore struct {
	store.PortActivationStore
}
