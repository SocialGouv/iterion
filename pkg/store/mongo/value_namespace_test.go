package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"sync"
	"testing"
	"time"
)

type namespaceBlob struct {
	*inMemoryBlob
	exact                        *inMemoryBlob
	mu                           sync.Mutex
	puts, gets                   map[int]int
	putEntered, putRelease       chan struct{}
	putOnce                      sync.Once
	deleteEntered, deleteRelease chan struct{}
	deleteOnce                   sync.Once
	blockDelete                  bool
	deleteFirstOnly              bool
	failDeleteOnce               bool
	deleteCalls                  int
}

func newNamespaceBlob() *namespaceBlob {
	return &namespaceBlob{inMemoryBlob: newInMemoryBlob(), exact: newInMemoryBlob(), puts: map[int]int{}, gets: map[int]int{}, putEntered: make(chan struct{}), putRelease: make(chan struct{}), deleteEntered: make(chan struct{}), deleteRelease: make(chan struct{})}
}
func (b *namespaceBlob) target(f int) *inMemoryBlob {
	if f == 1 {
		return b.inMemoryBlob
	}
	if f == 2 {
		return b.exact
	}
	panic("unsupported namespace")
}
func (b *namespaceBlob) put(ctx context.Context, f int, r, n string, v int, body []byte) error {
	b.mu.Lock()
	b.puts[f]++
	b.mu.Unlock()
	if bytes.Contains(body, []byte(`"old"`)) {
		b.putOnce.Do(func() { close(b.putEntered) })
		select {
		case <-b.putRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target(f).PutArtifact(ctx, r, n, v, body)
}
func (b *namespaceBlob) get(ctx context.Context, f int, r, n string, v int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets[f]++
	return b.target(f).GetArtifact(ctx, r, n, v)
}
func (b *namespaceBlob) list(ctx context.Context, f int, r, n string) ([]int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target(f).ListArtifactVersions(ctx, r, n)
}
func (b *namespaceBlob) del(ctx context.Context, f int, r string) error {
	b.mu.Lock()
	b.deleteCalls++
	call := b.deleteCalls
	fail := b.failDeleteOnce
	b.failDeleteOnce = false
	b.mu.Unlock()
	if fail {
		return fmt.Errorf("test cleanup failure")
	}
	if b.blockDelete && (!b.deleteFirstOnly || call == 1) {
		b.deleteOnce.Do(func() { close(b.deleteEntered) })
		select {
		case <-b.deleteRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target(f).DeleteRun(ctx, r)
}
func (b *namespaceBlob) PutArtifact(c context.Context, r, n string, v int, p []byte) error {
	return b.put(c, 1, r, n, v, p)
}
func (b *namespaceBlob) GetArtifact(c context.Context, r, n string, v int) ([]byte, error) {
	return b.get(c, 1, r, n, v)
}
func (b *namespaceBlob) ListArtifactVersions(c context.Context, r, n string) ([]int, error) {
	return b.list(c, 1, r, n)
}
func (b *namespaceBlob) DeleteRun(c context.Context, r string) error { return b.del(c, 1, r) }
func (b *namespaceBlob) PutArtifactForFormat(c context.Context, f int, r, n string, v int, p []byte) error {
	return b.put(c, f, r, n, v, p)
}
func (b *namespaceBlob) GetArtifactForFormat(c context.Context, f int, r, n string, v int) ([]byte, error) {
	return b.get(c, f, r, n, v)
}
func (b *namespaceBlob) ListArtifactVersionsForFormat(c context.Context, f int, r, n string) ([]int, error) {
	return b.list(c, f, r, n)
}
func (b *namespaceBlob) DeleteRunArtifactsForFormat(c context.Context, f int, r string) error {
	return b.del(c, f, r)
}

func (b *namespaceBlob) RunObjectsForFormat(raw int) (blob.RunObjects, error) {
	f, err := store.NormalizeValueFormat(raw)
	if err != nil {
		return nil, err
	}
	return blob.SelectRunObjects(b.target(int(f)), 1)
}

func nsWaitEntered(t *testing.T, entered <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("operation returned before reaching barrier: %v", err)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

var _ blob.ValueFormatArtifacts = (*namespaceBlob)(nil)
var _ blob.ValueFormatRunObjects = (*namespaceBlob)(nil)

func nsctx(t *testing.T) context.Context {
	return store.WithIdentity(t.Context(), "namespace-tenant", "tester")
}
func nsArtifact(value string, v int) *store.Artifact {
	return &store.Artifact{RunID: "r", NodeID: "n", Version: v, WrittenAt: time.Now().UTC(), Data: map[string]any{"value": value}}
}
func nsCreate(t *testing.T, s *Store, f int) {
	t.Helper()
	birth := f
	if birth == 99 {
		birth = 1
	}
	if _, e := s.CreateRunWithValueFormat(nsctx(t), store.RunCreateSpec{ID: "r", Format: store.ValueFormat(birth)}); e != nil {
		t.Fatal(e)
	}
	if f == 99 {
		if _, e := s.runs.UpdateOne(nsctx(t), bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); e != nil {
			t.Fatal(e)
		}
	}
}
func nsPair(t *testing.T, original int) (*Store, *Store, *namespaceBlob) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires Mongo conformance fixture")
	}
	blob := newNamespaceBlob()
	db := "ns_" + bsonNonce(t)
	a, e := New(nsctx(t), Config{URI: uri, Database: db, Blob: blob})
	if e != nil {
		t.Fatal(e)
	}
	b, e := New(nsctx(t), Config{URI: uri, Database: db, Blob: blob})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		select {
		case <-blob.putRelease:
		default:
			close(blob.putRelease)
		}
		select {
		case <-blob.deleteRelease:
		default:
			close(blob.deleteRelease)
		}
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.db.Drop(c)
		_ = a.Close(c)
		_ = b.Close(c)
	})
	nsCreate(t, a, original)
	return a, b, blob
}
func nsReplace(t *testing.T, s *Store, f int) {
	t.Helper()
	if e := s.DeleteRun(nsctx(t), "r"); e != nil {
		t.Fatal(e)
	}
	if n, e := s.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	nsCreate(t, s, f)
}
func nsAssertReaders(t *testing.T, s *Store, want string) {
	t.Helper()
	a, e := s.LoadArtifact(nsctx(t), "r", "n", 1)
	if e != nil || a.Data["value"] != want {
		t.Fatalf("public direct want=%s got=%+v err=%v", want, a, e)
	}
	a, e = s.LoadLatestArtifact(nsctx(t), "r", "n")
	if e != nil || a.Data["value"] != want {
		t.Fatalf("public latest want=%s got=%+v err=%v", want, a, e)
	}
	vs, e := s.ListArtifactVersions(nsctx(t), "r", "n")
	if e != nil || len(vs) != 1 || vs[0].Version != 1 {
		t.Fatal(vs, e)
	}
}
func nsAssertUnknown(t *testing.T, s *Store) {
	t.Helper()
	_, e := s.LoadArtifact(nsctx(t), "r", "n", 1)
	if !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	_, e = s.LoadLatestArtifact(nsctx(t), "r", "n")
	if e == nil {
		t.Fatal("latest accepted99")
	}
	_, e = s.ListArtifactVersions(nsctx(t), "r", "n")
	if e == nil {
		t.Fatal("list accepted99")
	}
}
func TestValueNamespaceMongoPublicLatePUT(t *testing.T) {
	for _, pair := range [][2]int{{1, 2}, {2, 1}, {2, 99}} {
		t.Run(fmt.Sprintf("%d-to-%d", pair[0], pair[1]), func(t *testing.T) {
			old, next := pair[0], pair[1]
			a, b, blob := nsPair(t, old)
			original := mongoValueBind(t, nsctx(t), "r", store.ValueFormat(old))
			done := make(chan error, 1)
			go func() { done <- a.WriteArtifact(original, nsArtifact("old", 1)) }()
			nsWaitEntered(t, blob.putEntered, done)
			nsReplace(t, b, next)
			if next == 99 {
				nsAssertUnknown(t, b)
			} else {
				if e := b.WriteArtifact(mongoValueBind(t, nsctx(t), "r", store.ValueFormat(next)), nsArtifact("new", 1)); e != nil {
					t.Fatal(e)
				}
				nsAssertReaders(t, b, "new")
			}
			close(blob.putRelease)
			e := <-done
			if e == nil {
				t.Fatal("late publication accepted")
			}
			if next == 99 {
				nsAssertUnknown(t, b)
			} else {
				nsAssertReaders(t, b, "new")
			}
			events, ee := b.LoadEvents(nsctx(t), "r")
			expected := 1
			if next == 99 {
				expected = 0
				if !errors.Is(ee, store.ErrUnsupportedValueFormat) {
					t.Fatal(ee)
				}
			} else if ee != nil {
				t.Fatal(ee)
			}
			if len(events) != expected {
				t.Fatal(len(events), ee)
			}
			oldBody, ee := blob.get(nsctx(t), old, "r", "n", 1)
			if ee != nil || !bytes.Contains(oldBody, []byte(`"old"`)) {
				t.Fatal(ee)
			}
			t.Logf("%d→%d late PUT remained namespace%d; publication=%v; successor events=%d; public readers unchanged/refused", old, next, old, e, len(events))
			// Delayed deletion of the old namespace also cannot remove the successor.
			if e = a.deleteValueArtifacts(nsctx(t), store.ValueFormat(old), "r"); e != nil {
				t.Fatal(e)
			}
			if next == 99 {
				nsAssertUnknown(t, b)
			} else {
				nsAssertReaders(t, b, "new")
			}
		})
	}
}
func TestValueNamespaceMongoBeforeGuardRefusesWithoutPUT(t *testing.T) {
	for _, pair := range [][2]int{{1, 2}, {2, 1}, {2, 99}} {
		t.Run(fmt.Sprint(pair), func(t *testing.T) {
			a, b, blob := nsPair(t, pair[0])
			original := mongoValueBind(t, nsctx(t), "r", store.ValueFormat(pair[0]))
			nsReplace(t, b, pair[1])
			e := a.WriteArtifact(original, nsArtifact("old", 1))
			blob.mu.Lock()
			calls := blob.puts[1] + blob.puts[2]
			blob.mu.Unlock()
			if e == nil || calls != 0 {
				t.Fatal(e, calls)
			}
			t.Logf("captured%d successor%d beforeguard refused, PUTcalls0", pair[0], pair[1])
		})
	}
}
func TestValueNamespaceMongoSameFormatMetadataAcquisition(t *testing.T) {
	for _, format := range []int{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			a, b, blob := nsPair(t, format)
			original := mongoValueBind(t, nsctx(t), "r", store.ValueFormat(format))
			done := make(chan error, 1)
			go func() { done <- a.WriteArtifact(original, nsArtifact("old", 1)) }()
			nsWaitEntered(t, blob.putEntered, done)
			r, e := b.LoadRun(nsctx(t), "r")
			if e != nil {
				t.Fatal(e)
			}
			epoch := r.ExecutionEpoch
			r.Name = "metadata"
			if e = b.SaveRun(nsctx(t), r); e != nil {
				t.Fatal(e)
			}
			r, e = b.LoadRun(nsctx(t), "r")
			if e != nil {
				t.Fatal(e)
			}
			g, ok, e := b.AcquireRunExecutionIf(nsctx(t), "r", store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionStart)
			if e != nil || !ok || g.Epoch == epoch {
				t.Fatal(g, ok, e)
			}
			close(blob.putRelease)
			if e = <-done; e != nil {
				t.Fatal(e)
			}
			nsAssertReaders(t, b, "old")
			t.Logf("format%d remains accepted after metadata+epoch rotation", format)
		})
	}
}
func TestValueNamespaceMongoNoCrossFormatFallback(t *testing.T) {
	a, _, blob := nsPair(t, 2)
	if e := blob.PutArtifact(nsctx(t), "r", "n", 9, []byte(`{"run_id":"r","node_id":"n","version":9,"data":{"value":"legacy-only"}}`)); e != nil {
		t.Fatal(e)
	}
	if _, e := a.LoadArtifact(nsctx(t), "r", "n", 9); e == nil {
		t.Fatal("reader2 took legacy body")
	}
	if _, e := a.LoadLatestArtifact(nsctx(t), "r", "n"); e == nil {
		t.Fatal("latest2 took legacy list")
	}
	vs, e := a.ListArtifactVersions(nsctx(t), "r", "n")
	if e != nil || len(vs) != 0 {
		t.Fatal(vs, e)
	}
	blob.mu.Lock()
	gets := blob.gets[1]
	blob.mu.Unlock()
	if gets != 0 {
		t.Fatal("legacy Get used", gets)
	}
	t.Log("reader2 direct/latest/list refuse or empty without legacy fallback; legacy Get count0")
}
func TestValueNamespaceMongoTombstonePrecedesCleanupPruneWaits(t *testing.T) {
	a, b, blob := nsPair(t, 2)
	if e := a.WriteArtifact(mongoValueBind(t, nsctx(t), "r", 2), nsArtifact("prior", 1)); e != nil {
		t.Fatal(e)
	}
	blob.blockDelete = true
	done := make(chan error, 1)
	go func() { done <- a.DeleteRun(nsctx(t), "r") }()
	nsWaitEntered(t, blob.deleteEntered, done)
	if _, e := b.LoadRun(nsctx(t), "r"); e == nil {
		t.Fatal("live parent while cleanup blocked")
	}
	if n, e := b.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 0 {
		t.Fatalf("pending tombstone pruned: count=%d err=%v", n, e)
	}
	if e := b.WriteArtifact(mongoValueBind(t, nsctx(t), "r", 2), nsArtifact("blocked", 2)); e == nil {
		t.Fatal("write admitted after tombstone")
	}
	if _, e := b.CreateRun(nsctx(t), "r", "premature", nil); e == nil {
		t.Fatal("birth before cleanup")
	}
	close(blob.deleteRelease)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if n, e := b.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e := b.CreateRun(nsctx(t), "r", "next1", nil); e != nil {
		t.Fatal(e)
	}
	if e := b.WriteArtifact(mongoValueBind(t, nsctx(t), "r", 1), nsArtifact("new", 1)); e != nil {
		t.Fatal(e)
	}
	if e := blob.DeleteRunArtifactsForFormat(nsctx(t), 2, "r"); e != nil {
		t.Fatal(e)
	}
	nsAssertReaders(t, b, "new")
	t.Log("tombstone visible before cleanup; prune0/create refused; completed cleanup→prune1; old format2 delete cannot affect new1")
	var doc bson.M
	if e := b.runs.FindOne(nsctx(t), bson.M{"_id": "r"}).Decode(&doc); e != nil {
		t.Fatal(e)
	}
	if doc[valueCleanupPendingField] != nil {
		t.Fatal("new run inherited cleanup marker")
	}
}

func TestValueNamespaceMongoOldCleanupCannotSweepSuccessor(t *testing.T) {
	a, b, blob := nsPair(t, 2)
	blob.blockDelete = true
	blob.deleteFirstOnly = true
	done := make(chan error, 1)
	go func() { done <- a.DeleteRun(nsctx(t), "r") }()
	nsWaitEntered(t, blob.deleteEntered, done)
	if e := b.DeleteRun(nsctx(t), "r"); e != nil {
		t.Fatal(e)
	}
	if n, e := b.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	nsCreate(t, b, 1)
	if e := b.WriteArtifact(nsctx(t), nsArtifact("new", 1)); e != nil {
		t.Fatal(e)
	}
	if e := b.WriteInteraction(nsctx(t), &store.Interaction{RunID: "r", ID: "new"}); e != nil {
		t.Fatal(e)
	}
	close(blob.deleteRelease)
	if e := <-done; !errors.Is(e, store.ErrRunConflict) {
		t.Fatal("retired cleanup accepted", e)
	}
	nsAssertReaders(t, b, "new")
	ids, e := b.ListInteractions(nsctx(t), "r")
	if e != nil || len(ids) != 1 || ids[0] != "new" {
		t.Fatal("successor child swept", ids, e)
	}
}
func TestValueNamespaceMongoCleanupFailureRetried(t *testing.T) {
	a, b, blob := nsPair(t, 2)
	blob.failDeleteOnce = true
	if e := a.DeleteRun(nsctx(t), "r"); e == nil {
		t.Fatal("cleanup failure swallowed")
	}
	if n, e := b.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 0 {
		t.Fatal("pending tombstone pruned", n, e)
	}
	if e := b.DeleteRun(nsctx(t), "r"); e != nil {
		t.Fatal(e)
	}
	if n, e := b.PruneDeletionMarkers(nsctx(t), time.Now().Add(time.Hour)); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
