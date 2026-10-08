package runner

import (
	"context"
	"os"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Plan v2 §P5-c: the delivery context carries the run's admitted
// identity from the moment the delivery is admitted, so every lease it
// takes — the run's own lock and the subbot children the engine locks
// under it — is stamped with what this pod actually admitted.

// leaseCaptureStore records the context LockRun was called with.
type leaseCaptureStore struct {
	store.RunStore
	gotCtx context.Context
}

func (s *leaseCaptureStore) LockRun(ctx context.Context, runID string) (store.RunLock, error) {
	s.gotCtx = ctx
	return leaseCaptureLock{}, nil
}

type leaseCaptureLock struct{}

func (leaseCaptureLock) Unlock() error { return nil }

func TestAcquireRunLockCarriesAdmittedIdentity(t *testing.T) {
	r := &Runner{cfg: Config{Store: &leaseCaptureStore{}, Logger: iterlog.Nop()}}
	msg := &queue.RunMessage{RunID: "run-p5c", TenantID: "team-a", RunnerPool: "pool-honorabilite"}

	lock, ok, status := r.acquireRunLock(context.Background(), msg, nil, iterlog.Nop())
	if !ok || status != "" {
		t.Fatalf("acquire failed: ok=%v status=%q", ok, status)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	ident, got := store.LeaseIdentityFromContext(r.cfg.Store.(*leaseCaptureStore).gotCtx)
	if !got {
		t.Fatal("acquireRunLock received no admitted identity: the delivery did not stamp it before the lock")
	}
	if ident.TenantID != "team-a" || ident.Pool != "pool-honorabilite" {
		t.Fatalf("stamped identity: tenant %q pool %q, want team-a / pool-honorabilite", ident.TenantID, ident.Pool)
	}
}

// The pin: the engine path carries the admitted identity too, so the
// subbot children's own LockRun takes a lease stamped with the same
// admitted identity. The chain is stamp → engine ctx → child ctx →
// LockRun; both ends are pinned (a behavioral witness for the whole
// chain needs a subbot harness — the fail-closed guard makes a broken
// link loud in production instead). Without the stamp the child's
// distributed lease fails closed (ErrLeaseUnattributed) — a dropped
// stamp is a production break, not a silent empty field.
func TestEnginePathStampsLeaseIdentity(t *testing.T) {
	lockSrc, err := os.ReadFile("loop_lock.go")
	if err != nil {
		t.Fatal(err)
	}
	stamp := "runCtx = store.WithLeaseIdentity(runCtx, msg.TenantID, msg.RunnerPool)"
	if !strings.Contains(string(lockSrc), stamp) {
		t.Fatal("executeHoldingLease no longer stamps the admitted identity onto the engine context (plan v2 §P5-c)")
	}
	if !strings.Contains(string(lockSrc), "r.cfg.Store.LockRun(store.WithLeaseIdentity(runCtx, msg.TenantID, msg.RunnerPool), msg.RunID)") {
		t.Fatal("acquireRunLock no longer stamps the admitted identity onto the lease context (plan v2 §P5-c)")
	}
	subbotSrc, err := os.ReadFile("subbot.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(subbotSrc), "r.cfg.Store.LockRun(childCtx, childRunID)") {
		t.Fatal("the subbot child's lock no longer takes the inherited delivery context — the child's lease would fail closed on acquire")
	}
}

// The control-plane commands (cancel, steer) subscribe with the same
// admitted identity the delivery was admitted under: the handlers
// refuse any command whose stamp disagrees. Both subscription sites
// are pinned — dropping the stamp would make this pod act on another
// boundary's commands.
func TestControlPlaneSubscriptionsCarryAdmittedIdentity(t *testing.T) {
	src, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatal(err)
	}
	cancelStamp := "SubscribeCancel(runCtx, msg.RunID, store.LeaseIdentity{TenantID: msg.TenantID, Pool: msg.RunnerPool}"
	steerStamp := "SubscribeSteer(runCtx, msg.RunID, store.LeaseIdentity{TenantID: msg.TenantID, Pool: msg.RunnerPool}"
	for _, pin := range []string{cancelStamp, steerStamp} {
		if !strings.Contains(string(src), pin) {
			t.Fatalf("the control-plane subscription lost its admitted-identity stamp (plan v2 §P6): %s", pin)
		}
	}
}
