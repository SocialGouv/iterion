package runview

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// noLockAuthorityStore is a RunStore that behaves exactly like the
// filesystem store but reports CrossProcessLock=false — the shape of the
// cloud SERVER store (no NATS lock provider), whose LockRun is a noop.
type noLockAuthorityStore struct{ *store.FilesystemRunStore }

func (s noLockAuthorityStore) Capabilities() store.Capabilities {
	c := s.FilesystemRunStore.Capabilities()
	c.CrossProcessLock = false
	return c
}

// TestReconcileSkipsReapWithoutLockAuthority pins the cloud-server fix: a
// store that cannot prove run liveness (noop lock) must NEVER reap a
// `running` run — else the server's 60s reconcile tick fails healthy
// runner-owned cloud runs mid-flight. A genuinely dead runner is recovered
// by the NATS lease + JetStream redelivery, not by the server.
func TestReconcileSkipsReapWithoutLockAuthority(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "20ms")
	dir := t.TempDir()
	logger := iterlog.Nop()

	fs, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if !fs.Capabilities().CrossProcessLock {
		t.Fatal("precondition: filesystem store should have CrossProcessLock=true")
	}
	svc, err := NewService("", WithLogger(logger), WithStore(noLockAuthorityStore{fs}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Stop(context.Background())

	const id = "run-cloud-live"
	if _, err := fs.CreateRun(context.Background(), id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Give several reconcile ticks a chance to (wrongly) reap it.
	time.Sleep(300 * time.Millisecond)
	r, err := svc.store.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusRunning {
		t.Fatalf("run was reaped to %q — the server reconciler must not reap without lock authority", r.Status)
	}
}
