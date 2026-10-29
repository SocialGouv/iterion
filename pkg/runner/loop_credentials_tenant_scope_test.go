package runner

import (
	"context"
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Every store call of this loop is scoped by the identity the message
// carries (store.WithIdentity) — loop_lock, loop_gitws, bank_push,
// loop_spend all stamp it, including the ones that detach onto a fresh
// background ctx. The run_secrets pair is the same kind of call on the
// collection that holds the sealed credential bundles, so these tests
// judge the ctx the store actually receives.
//
// What they do NOT claim: `_id` is unique in a Mongo collection, so an
// unscoped read by ref lands on the one document that owns the ref. The
// stamp is the layer that keeps it that way, and the layer a second
// keying scheme (or a helper copied onto a non-unique key) would need.

// tenantRecordingRunSecrets records the tenant each call arrives with and
// delegates to a real store, so the assertion reads the effect rather
// than a flag the test itself set. Fed from the runner's own goroutines,
// hence the mutex.
type tenantRecordingRunSecrets struct {
	inner secrets.RunSecretsStore

	mu        sync.Mutex
	getTenant []string
	delTenant []string
	getScoped []bool
	delScoped []bool
}

func newTenantRecordingRunSecrets(inner secrets.RunSecretsStore) *tenantRecordingRunSecrets {
	return &tenantRecordingRunSecrets{inner: inner}
}

func (s *tenantRecordingRunSecrets) record(ctx context.Context, get bool) {
	tenant, ok := store.TenantFromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if get {
		s.getTenant = append(s.getTenant, tenant)
		s.getScoped = append(s.getScoped, ok && tenant != "")
		return
	}
	s.delTenant = append(s.delTenant, tenant)
	s.delScoped = append(s.delScoped, ok && tenant != "")
}

func (s *tenantRecordingRunSecrets) snapshot() (getTenant, delTenant []string, getScoped, delScoped []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.getTenant...), append([]string(nil), s.delTenant...),
		append([]bool(nil), s.getScoped...), append([]bool(nil), s.delScoped...)
}

func (s *tenantRecordingRunSecrets) Put(ctx context.Context, rec secrets.RunSecretsRecord) error {
	return s.inner.Put(ctx, rec)
}

func (s *tenantRecordingRunSecrets) Get(ctx context.Context, id string) (secrets.RunSecretsRecord, error) {
	s.record(ctx, true)
	return s.inner.Get(ctx, id)
}

func (s *tenantRecordingRunSecrets) Delete(ctx context.Context, id string) error {
	s.record(ctx, false)
	return s.inner.Delete(ctx, id)
}

// seedRunSecretsFor puts one sealed bundle for a team and returns the
// recording store wrapping the memory one.
func seedRunSecretsFor(t *testing.T, tenantID, runID, ref string) *tenantRecordingRunSecrets {
	t.Helper()
	mem := secrets.NewMemoryRunSecretsStore()
	rec := secrets.RunSecretsRecord{ID: ref, TenantID: tenantID, RunID: runID, SealedBundle: []byte("sealed")}
	if err := mem.Put(store.WithTenant(context.Background(), tenantID), rec); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	return newTenantRecordingRunSecrets(mem)
}

// TestTheRunSecretsFetchIsScopedToTheMessagesTenant drives the public
// entry point (injectCredentials) and reads the ctx the store received.
func TestTheRunSecretsFetchIsScopedToTheMessagesTenant(t *testing.T) {
	rs := seedRunSecretsFor(t, "team-a", "run-1", "ref-1")
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}

	// No Sealer is wired, so the fetch happens and the open fails after
	// it: the ctx of the fetch is what this test is about.
	_, _, _ = r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"})

	getTenant, _, getScoped, _ := rs.snapshot()
	if len(getTenant) != 1 {
		t.Fatalf("RunSecrets.Get called %d times, want 1", len(getTenant))
	}
	if !getScoped[0] {
		t.Fatal("RunSecrets.Get ran on a ctx carrying no tenant: the fetch escaped the message's team")
	}
	if getTenant[0] != "team-a" {
		t.Fatalf("RunSecrets.Get tenant = %q, want the message's %q", getTenant[0], "team-a")
	}
}

// TestTheRunSecretsDeleteIsScopedToTheMessagesTenant is the same question
// for the cleanup, which detaches onto context.Background() — the site
// where the stamp is easiest to lose and hardest to notice, since a
// failed delete is logged and non-fatal.
func TestTheRunSecretsDeleteIsScopedToTheMessagesTenant(t *testing.T) {
	rs := seedRunSecretsFor(t, "team-a", "run-1", "ref-1")
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}

	r.deleteRunSecrets(&queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"})

	_, delTenant, _, delScoped := rs.snapshot()
	if len(delTenant) != 1 {
		t.Fatalf("RunSecrets.Delete called %d times, want 1", len(delTenant))
	}
	if !delScoped[0] {
		t.Fatal("RunSecrets.Delete ran on a ctx carrying no tenant: the cleanup delete escaped the message's team")
	}
	if delTenant[0] != "team-a" {
		t.Fatalf("RunSecrets.Delete tenant = %q, want the message's %q", delTenant[0], "team-a")
	}

	// The effect, not the recording: the bundle is gone for its owner.
	if _, err := rs.inner.Get(store.WithTenant(context.Background(), "team-a"), "ref-1"); err == nil {
		t.Fatal("the scoped delete did not remove the owning team's bundle")
	}
}

// TestAForeignTenantsCleanupLeavesTheBundleAlone is the negative witness:
// the delete reaches the document only under the team that owns it.
func TestAForeignTenantsCleanupLeavesTheBundleAlone(t *testing.T) {
	rs := seedRunSecretsFor(t, "team-a", "run-1", "ref-1")
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}

	r.deleteRunSecrets(&queue.RunMessage{RunID: "run-1", TenantID: "team-b", SecretsRef: "ref-1"})

	if _, err := rs.inner.Get(store.WithTenant(context.Background(), "team-a"), "ref-1"); err != nil {
		t.Fatalf("team-b's cleanup deleted team-a's bundle: %v", err)
	}
}
