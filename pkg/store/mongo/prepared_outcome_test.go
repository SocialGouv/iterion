package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"sync"
	"testing"
)

func preparedMongo(t *testing.T) (*Store, *Store) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("isolated Mongo fixture required")
	}
	cfg := Config{URI: uri, Database: "d1_prepared_" + bsonNonce(t), Blob: newInMemoryBlob()}
	s, e := New(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	peer, e := New(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := mongotest.TeardownCtx()
		defer c()
		_ = s.db.Drop(ctx)
		_ = peer.Close(ctx)
		_ = s.Close(ctx)
	})
	return s, peer
}
func TestPreparedOutcomeMongo(t *testing.T) {
	for _, kind := range []string{"normal", "metadata-refreshed", "version-stale", "epoch-stale", "future", "tenant", "legacy-unconditional"} {
		t.Run(kind, func(t *testing.T) {
			s, peer := preparedMongo(t)
			ctx := store.WithTenant(t.Context(), "tenant-a")
			r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "run", Format: store.LegacyValueFormat, Status: store.RunStatusRunning})
			if e != nil {
				t.Fatal(e)
			}
			ctx, e = store.BindRunValueFormat(ctx, r)
			if e != nil {
				t.Fatal(e)
			}
			expected := store.RunOutcomeExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch}
			want := true
			switch kind {
			case "metadata-refreshed", "version-stale":
				r.Name = "own route stamp"
				if e = peer.SaveRun(ctx, r); e != nil {
					t.Fatal(e)
				}
				if kind == "metadata-refreshed" {
					expected.Version = r.CASVersion
				} else {
					want = false
				}
			case "epoch-stale":
				a, ok, err := peer.AcquireRunExecutionIf(ctx, "run", store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}, store.ExecutionStart)
				if err != nil || !ok || a.Epoch == expected.Epoch {
					t.Fatal("acquire", a, ok, err)
				}
				fresh, err := s.LoadRun(ctx, "run")
				if err != nil {
					t.Fatal(err)
				}
				expected.Version = fresh.CASVersion
				want = false
			case "future":
				_, e = peer.runs.UpdateOne(ctx, bson.M{"_id": "run"}, bson.M{"$set": bson.M{"format_version": 99}})
				if e != nil {
					t.Fatal(e)
				}
				want = false
			case "tenant":
				ctx = store.WithTenant(ctx, "tenant-b")
				want = false
			}
			var ok bool
			if kind == "legacy-unconditional" {
				ok, e = s.UpdateRunOutcome(ctx, "run", store.RunStatusCancelled, "cancel", store.RunOutcomeMeta{Code: store.FailureCancelled}, []store.RunStatus{store.RunStatusRunning})
			} else {
				ok, e = s.UpdateRunOutcomeIf(ctx, "run", expected, store.RunStatusCancelled, "cancel", store.RunOutcomeMeta{Code: store.FailureCancelled}, []store.RunStatus{store.RunStatusRunning})
			}
			t.Logf("kind=%s matched=%v error=%v", kind, ok, e)
			if kind == "future" {
				if ok || !errors.Is(e, store.ErrUnsupportedValueFormat) {
					t.Fatal(ok, e)
				}
				return
			}
			if e != nil || ok != want {
				t.Fatal("CAS", ok, e, "want", want)
			}
			got, e := s.LoadRun(store.WithTenant(t.Context(), "tenant-a"), "run")
			if e != nil {
				t.Fatal(e)
			}
			if want && got.Status != store.RunStatusCancelled || !want && got.Status != store.RunStatusRunning {
				t.Fatal("status", got.Status)
			}
		})
	}
}
func TestPreparedOutcomeMongoContendsWithAcquisition(t *testing.T) {
	s, peer := preparedMongo(t)
	ctx := store.WithTenant(t.Context(), "tenant-race")
	for i := 0; i < 12; i++ {
		id := bson.NewObjectID().Hex()
		r, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, Format: store.LegacyValueFormat, Status: store.RunStatusRunning})
		if e != nil {
			t.Fatal(e)
		}
		bound, e := store.BindRunValueFormat(ctx, r)
		if e != nil {
			t.Fatal(e)
		}
		var claim store.RunExecutionAuthority
		var stopOK, claimOK bool
		var stopErr, claimErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			stopOK, stopErr = s.UpdateRunOutcomeIf(bound, id, store.RunOutcomeExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch}, store.RunStatusCancelled, "cancel", store.RunOutcomeMeta{Code: store.FailureCancelled}, []store.RunStatus{store.RunStatusRunning})
		}()
		go func() {
			defer wg.Done()
			<-start
			claim, claimOK, claimErr = peer.AcquireRunExecutionIf(bound, id, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status}, store.ExecutionStart)
		}()
		close(start)
		wg.Wait()
		if stopErr != nil || claimErr != nil || stopOK == claimOK {
			t.Fatalf("not exclusive: stop %v/%v claim %v/%v", stopOK, stopErr, claimOK, claimErr)
		}
		got, e := s.LoadRun(bound, id)
		if e != nil {
			t.Fatal(e)
		}
		if claimOK && (got.Status != store.RunStatusRunning || got.ExecutionEpoch != claim.Epoch) || stopOK && (got.Status != store.RunStatusCancelled || got.ExecutionEpoch == r.ExecutionEpoch) {
			t.Fatalf("bad winner %+v", got)
		}
		if got.CASVersion != r.CASVersion+1 {
			t.Fatal("not exactly one mutation", got.CASVersion, r.CASVersion)
		}
		t.Logf("iteration=%d stop=%v acquired=%v", i, stopOK, claimOK)
	}
}
func TestPreparedQueuedMongoAbort(t *testing.T) {
	for _, kind := range []string{"exclusive", "own-stamp", "existing", "peer"} {
		t.Run(kind, func(t *testing.T) {
			s, peer := preparedMongo(t)
			tenantCtx := store.WithTenant(t.Context(), "tenant-queued")
			p, e := store.PrepareRunInvocation(tenantCtx, s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run"}})
			if e != nil {
				t.Fatal(e)
			}
			if p.Run.Status != store.RunStatusQueued {
				t.Fatal("not queued", p.Run.Status)
			}
			if kind == "own-stamp" {
				r, err := s.LoadRun(p.Context, "run")
				if err != nil {
					t.Fatal(err)
				}
				r.Name = "own stamp"
				if err = peer.SaveRun(p.Context, r); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "existing" {
				p, e = store.PrepareRunInvocation(p.Context, s, store.ExistingRunInvocation{ID: "run"})
				if e != nil {
					t.Fatal(e)
				}
			}
			if kind == "peer" {
				_, ok, err := peer.AcquireRunExecutionIf(p.Context, "run", store.RunExecutionExpectation{Version: p.Run.CASVersion, Epoch: p.Run.ExecutionEpoch, Status: p.Run.Status, QueuedAt: p.Run.QueuedAt}, store.ExecutionStart)
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			ctx, cancel := context.WithCancel(p.Context)
			cancel()
			err := runtime.AbortPreparedRun(ctx, s, p, context.Canceled)
			r, e := s.LoadRun(tenantCtx, "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("kind=%s status=%s error=%v", kind, r.Status, err)
			switch kind {
			case "existing":
				if err != context.Canceled || r.Status != store.RunStatusQueued {
					t.Fatal(err, r.Status)
				}
			case "peer":
				if !errors.Is(err, runtime.ErrAdmissionStopped) || r.Status != store.RunStatusRunning {
					t.Fatal(err, r.Status)
				}
			default:
				if !errors.Is(err, runtime.ErrRunCancelled) || r.Status != store.RunStatusCancelled {
					t.Fatal(err, r.Status)
				}
			}
		})
	}
}

func TestPreparedQueuedMongoRetirement(t *testing.T) {
	for _, kind := range []string{"exclusive", "own-stamp", "existing", "peer"} {
		t.Run(kind, func(t *testing.T) {
			s, peer := preparedMongo(t)
			tenantCtx := store.WithTenant(t.Context(), "tenant-queued")
			p, e := store.PrepareRunInvocation(tenantCtx, s, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: "run"}})
			if e != nil {
				t.Fatal(e)
			}
			if p.Run.Status != store.RunStatusQueued {
				t.Fatal("not queued", p.Run.Status)
			}
			if kind == "own-stamp" {
				r, err := s.LoadRun(p.Context, "run")
				if err != nil {
					t.Fatal(err)
				}
				r.Name = "own stamp"
				if err = peer.SaveRun(p.Context, r); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "existing" {
				p, e = store.PrepareRunInvocation(p.Context, s, store.ExistingRunInvocation{ID: "run"})
				if e != nil {
					t.Fatal(e)
				}
			}
			if kind == "peer" {
				_, ok, err := peer.AcquireRunExecutionIf(p.Context, "run", store.RunExecutionExpectation{Version: p.Run.CASVersion, Epoch: p.Run.ExecutionEpoch, Status: p.Run.Status, QueuedAt: p.Run.QueuedAt}, store.ExecutionStart)
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
			}
			cause := errors.New("child executor setup refused")
			err := runtime.RetirePreparedRun(p.Context, s, p, cause)
			r, e := s.LoadRun(tenantCtx, "run")
			if e != nil {
				t.Fatal(e)
			}
			t.Logf("kind=%s status=%s error=%v", kind, r.Status, err)
			switch kind {
			case "existing":
				if !errors.Is(err, cause) || r.Status != store.RunStatusQueued {
					t.Fatal(err, r.Status)
				}
			case "peer":
				if !errors.Is(err, runtime.ErrAdmissionStopped) || r.Status != store.RunStatusRunning {
					t.Fatal(err, r.Status)
				}
			default:
				if !errors.Is(err, cause) || r.Status != store.RunStatusFailed {
					t.Fatal(err, r.Status)
				}
			}
		})
	}
}
