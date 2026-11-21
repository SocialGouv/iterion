package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// cloudFlipMongo is the native Mongo store as the cloud publisher holds it,
// with one-shot hooks run on ENTRY of its two cloud flips, before they
// delegate: the queued claim of a resume and the cancel's outcome flip.
// Every call is forwarded with the caller's context, and the double endorses
// itself as the store it wraps.
type cloudFlipMongo struct {
	*mongostore.Store
	mu            sync.Mutex
	beforeClaim   func() error
	beforeOutcome func() error
	hookErr       error
}

func (s *cloudFlipMongo) CheckBoundObservationStore(actual store.RunStore) error {
	if a, ok := actual.(*cloudFlipMongo); !ok || a != s {
		return fmt.Errorf("%w: %T is not this test double", store.ErrRunDataCapability, actual)
	}
	return store.RequireBoundObservationStore(s.Store)
}

func (s *cloudFlipMongo) fire(h *func() error) {
	s.mu.Lock()
	f := *h
	*h = nil
	s.mu.Unlock()
	if f == nil {
		return
	}
	if err := f(); err != nil {
		s.mu.Lock()
		s.hookErr = errors.Join(s.hookErr, err)
		s.mu.Unlock()
	}
}

func (s *cloudFlipMongo) FlipToQueued(ctx context.Context, id string, from store.RunStatus, at time.Time) (store.QueuedFlip, bool, error) {
	s.fire(&s.beforeClaim)
	return s.Store.FlipToQueued(ctx, id, from, at)
}

func (s *cloudFlipMongo) UpdateRunOutcome(ctx context.Context, id string, status store.RunStatus, runErr string, meta store.RunOutcomeMeta, expectedFrom []store.RunStatus) (bool, error) {
	s.fire(&s.beforeOutcome)
	return s.Store.UpdateRunOutcome(ctx, id, status, runErr, meta, expectedFrom)
}

func (s *cloudFlipMongo) arm(h *func() error, f func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*h = f
}

func (s *cloudFlipMongo) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

// cloudMongoFixture is the real publisher over the native Mongo store with
// one run born A in status; publications and NATS cancels are recorded.
type cloudMongoFixture struct {
	ms        *mongostore.Store
	flips     *cloudFlipMongo
	pub       *cloudpublisher.Publisher
	ctx       context.Context
	row       actionRow
	origin    store.RunObservation
	mu        sync.Mutex
	published []string
	cancels   []string
}

func newCloudMongoFixture(t *testing.T, uri string, status store.RunStatus) *cloudMongoFixture {
	t.Helper()
	ctx := store.WithIdentity(t.Context(), "dev", "dev")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("cloud_origin_flip_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	mustWS(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	fx := &cloudMongoFixture{ms: ms, flips: &cloudFlipMongo{Store: ms}, ctx: ctx, row: actionRow{status: status}}
	fx.pub = cloudpublisher.NewForTesting(cloudpublisher.Config{Store: fx.flips}, func(_ context.Context, m *queue.RunMessage) error {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.published = append(fx.published, m.RunID)
		return nil
	})
	fx.pub.SetCancelForTesting(func(id string, _ store.LeaseIdentity) error {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.cancels = append(fx.cancels, id)
		return nil
	})
	mustWS(t, seedActionRun(ctx, ms, fx.row, actionEnv{}, "A"))
	fx.origin, err = runview.CaptureRunOriginIn(ctx, ms, actionRunID)
	mustWS(t, err)
	return fx
}

func (fx *cloudMongoFixture) replace() error {
	return replaceActionRun(fx.ctx, fx.ms, fx.row, actionEnv{}, "B")
}

// recorded returns the publications and the NATS cancels.
func (fx *cloudMongoFixture) recorded() ([]string, []string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]string(nil), fx.published...), append([]string(nil), fx.cancels...)
}

// current is the run behind the ID now: its birth and status.
func (fx *cloudMongoFixture) current(t *testing.T) (string, store.RunStatus) {
	t.Helper()
	r, err := fx.ms.LoadRun(fx.ctx, actionRunID)
	mustWS(t, err)
	return r.BirthID, r.Status
}

func (fx *cloudMongoFixture) submitResume(checked bool) error {
	spec := runview.ResumeSpec{RunID: actionRunID, FilePath: "main.bot", Source: "workflow w:\n  entry: a\n  a -> done\n"}
	if checked {
		spec.ExpectOrigin = runview.ExpectRunOrigin(fx.origin)
	}
	cs := &runview.CompiledSource{Hash: "h", Main: "main.bot", Files: map[string]string{"main.bot": spec.Source}}
	return fx.pub.SubmitResume(fx.ctx, spec, &ir.Workflow{Name: "w"}, cs)
}

// The cloud flips on the native Mongo store, gated on ITERION_TEST_MONGO_URI
// (run by the mongo-conformance line over ./pkg/server/...).
//
//   - W8b Mongo twin: a checked SubmitResume (i) refuses typed a document
//     replaced before its load, (ii) refuses typed a successor recreated
//     inside its claim — the queued flip under the binding matches nothing
//     (the bound metadata predicate) — nothing published, the successor not
//     queued. Controls: the admitted run is claimed and published; an
//     unchecked resume claims the successor, as at HEAD.
//   - W13 Mongo twin: a cancel under the binding the checked surfaces hand
//     the publisher (runview.Service.CancelCtx) refuses typed a successor
//     recreated inside its flip, its status kept, no NATS cancel. Controls:
//     the admitted run is cancelled and signalled once; on
//     context.Background (every unchecked caller) the flip lands on the
//     successor.
func TestCloudFlipsBoundNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the cloud flips are unverified on Mongo")
	}
	for _, tc := range []struct {
		name    string
		checked bool
		replace string // "", "before", "claim"
		want    error
	}{
		{"resume-i-replaced-before-the-load", true, "before", store.ErrRunDataIdentityChanged},
		{"resume-ii-replaced-inside-the-claim", true, "claim", store.ErrRunDataIdentityChanged},
		{"resume-control-the-admitted-run", true, "", nil},
		{"resume-control-unchecked-claims-the-successor", false, "claim", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCloudMongoFixture(t, uri, store.RunStatusFailedResumable)
			switch tc.replace {
			case "before":
				mustWS(t, fx.replace())
			case "claim":
				fx.flips.arm(&fx.flips.beforeClaim, fx.replace)
			}
			err := fx.submitResume(tc.checked)
			mustWS(t, fx.flips.failure())
			birth, status := fx.current(t)
			if tc.replace != "" && birth == fx.origin.Header().BirthID {
				t.Fatal("fixture: the replacement did not run")
			}
			published, _ := fx.recorded()
			if tc.want != nil {
				if !errors.Is(err, tc.want) || len(published) != 0 || status != store.RunStatusFailedResumable {
					t.Fatalf("SubmitResume = %v, published %v, run %s; want %v, nothing published, the run failed_resumable", err, published, status, tc.want)
				}
				return
			}
			if err != nil || len(published) != 1 || status != store.RunStatusQueued {
				t.Fatalf("SubmitResume = %v, published %v, run %s; want one publication and the run queued", err, published, status)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		checked bool
		replace bool
	}{
		{"cancel-ii-replaced-inside-the-flip", true, true},
		{"cancel-control-the-admitted-run", true, false},
		{"cancel-control-unchecked-flips-the-successor", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCloudMongoFixture(t, uri, store.RunStatusRunning)
			hs := fx.serve(t)
			if tc.replace {
				fx.flips.arm(&fx.flips.beforeOutcome, fx.replace)
			}
			birthA := ""
			if tc.checked {
				birthA = fx.origin.Header().BirthID
			}
			got := actionDo(t, hs.URL, cancelRow, birthA)
			mustWS(t, fx.flips.failure())
			birth, status := fx.current(t)
			_, cancels := fx.recorded()
			switch {
			case tc.checked && tc.replace:
				wantOriginLost(t, "cloud cancel on Mongo", got, actionRunID)
				if status != store.RunStatusRunning || len(cancels) != 0 || birth == fx.origin.Header().BirthID {
					t.Fatalf("checked cancel of a replaced run: run %s at %q, cancels %v; want the successor running, no NATS cancel", status, birth, cancels)
				}
			case tc.checked:
				if got.status != http.StatusAccepted || status != store.RunStatusCancelled || len(cancels) != 1 {
					t.Fatalf("checked cancel of the admitted run: %d %s, run %s, cancels %v; want 202, cancelled, signalled once", got.status, got.body, status, cancels)
				}
			default:
				if got.status != http.StatusAccepted || status != store.RunStatusCancelled || len(cancels) != 1 || birth == fx.origin.Header().BirthID {
					t.Fatalf("unchecked cancel: %d %s, run %s at %q, cancels %v; want HEAD's 202 and flip of the successor", got.status, got.body, status, birth, cancels)
				}
			}
		})
	}
}

// serve is the production chain on the DisableAuth path over the probed
// native Mongo store, its run service backed by the fixture's publisher.
func (fx *cloudMongoFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	workDir := t.TempDir()
	srv := New(Config{WorkDir: workDir, StoreDir: filepath.Join(workDir, ".iterion"), SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(&gateMongoStore{Store: fx.ms}), runview.WithLaunchPublisher(fx.pub))
	ms := fx.ms
	streams := &testOwnedRunStreams{Source: runstream.NewMongoResolved(func(c context.Context, id string) (runstream.MongoSelection, error) {
		return ms.ResolveStreamSelection(c, id)
	}, nil)}
	runview.WithStreamSource(streams)(svc)
	srv.runs = svc
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		if err := streams.Close(); err != nil {
			t.Errorf("join run streams: %v", err)
		}
	})
	return hs
}
