package runview

import (
	"context"
	"sync/atomic"
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

// storePassCounter counts the whole-store passes a reconcile makes: ListRuns
// is the call whose price follows the size of the store, LoadRun the one
// paid once per run it returned.
type storePassCounter struct {
	store.RunStore
	listRuns atomic.Int64
	loadRun  atomic.Int64
}

func (c *storePassCounter) ListRuns(ctx context.Context) ([]string, error) {
	c.listRuns.Add(1)
	return c.RunStore.ListRuns(ctx)
}

func (c *storePassCounter) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	c.loadRun.Add(1)
	return c.RunStore.LoadRun(ctx, id)
}

// Capabilities is forwarded explicitly: the embedded interface would answer
// it too, but the wrapped store's verdict is what this counter must report.
func (c *storePassCounter) Capabilities() store.Capabilities { return c.RunStore.Capabilities() }

// TestReconcileWithoutLockAuthorityNeverWalksTheStore pins the other half
// of the no-authority rule (#2636): a reconcile that cannot reap also has no
// reason to LOOK. On the cloud server every one of its passes listed the
// whole runs collection and loaded every document, then dropped each one
// at the `!canReap` guard — ten replicas, every minute, the entire store
// — so the skip has to come before the listing, not after it. The
// filesystem store with its lock authority is the control: the same
// reconcile does walk that one.
func TestReconcileWithoutLockAuthorityNeverWalksTheStore(t *testing.T) {
	logger := iterlog.Nop()
	seed := func(t *testing.T, rs store.RunStore) {
		t.Helper()
		for _, id := range []string{"run-a", "run-b"} {
			if _, err := rs.CreateRun(context.Background(), id, "wf", nil); err != nil {
				t.Fatalf("CreateRun %s: %v", id, err)
			}
		}
	}

	t.Run("no authority: no listing, no loads", func(t *testing.T) {
		fs, err := store.New(t.TempDir(), store.WithLogger(logger))
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		seed(t, fs)
		counter := &storePassCounter{RunStore: noLockAuthorityStore{fs}}
		svc, err := NewService("", WithLogger(logger), WithStore(counter))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		defer svc.Stop(context.Background())
		// Judged on an explicit scan, as a delta: construction runs other
		// store walks of its own (a pipeline-queue rebuild when a cap is
		// configured in the environment), and those are not the reconcile's.
		counter.listRuns.Store(0)
		counter.loadRun.Store(0)
		svc.reconcileOrphans(context.Background())
		if n := counter.listRuns.Load(); n != 0 {
			t.Fatalf("reconcile listed the whole store %d times with no lock authority, want 0 — it cannot act on what it would find", n)
		}
		if n := counter.loadRun.Load(); n != 0 {
			t.Fatalf("reconcile loaded %d run documents with no lock authority, want 0", n)
		}
	})

	t.Run("control: with authority the reconcile does walk the store", func(t *testing.T) {
		fs, err := store.New(t.TempDir(), store.WithLogger(logger))
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		seed(t, fs)
		counter := &storePassCounter{RunStore: fs}
		svc, err := NewService("", WithLogger(logger), WithStore(counter))
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		defer svc.Stop(context.Background())
		counter.listRuns.Store(0)
		svc.reconcileOrphans(context.Background())
		if n := counter.listRuns.Load(); n == 0 {
			t.Fatal("the control reconcile never listed the store: the counter sees nothing, so the no-authority case above proves nothing")
		}
	})
}
