package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// gateMongoStore is the native Mongo store with the action probe and the
// one-shot hook after the Nth run load of gateStore. Every call it overrides
// is forwarded with the caller's context, so the binding policy under test
// stays in the native store.
type gateMongoStore struct {
	*mongostore.Store
	probe       actionProbe
	loads       atomic.Int32
	mu          sync.Mutex
	afterLoadAt int32
	afterLoad   func() error
	hookErr     error
}

func (s *gateMongoStore) CheckBoundObservationStore(actual store.RunStore) error {
	if receiver, ok := actual.(*gateMongoStore); !ok || receiver != s {
		return fmt.Errorf("%w: %T is not this test double", store.ErrRunDataCapability, actual)
	}
	return store.RequireBoundObservationStore(s.Store)
}

func (s *gateMongoStore) afterLoadN(n int32, h func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterLoadAt = s.loads.Load() + n
	s.afterLoad = h
}

func (s *gateMongoStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

func (s *gateMongoStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	n := s.loads.Add(1)
	if err := s.probe.observe(ctx, "LoadRun", id); err != nil {
		return nil, err
	}
	r, err := s.Store.LoadRun(ctx, id)
	s.mu.Lock()
	h := s.afterLoad
	fire := h != nil && n == s.afterLoadAt
	if fire {
		s.afterLoad = nil
	}
	s.mu.Unlock()
	if fire {
		if herr := h(); herr != nil {
			s.mu.Lock()
			s.hookErr = errors.Join(s.hookErr, herr)
			s.mu.Unlock()
		}
	}
	return r, err
}

func (s *gateMongoStore) SaveRun(ctx context.Context, r *store.Run) error {
	if err := s.probe.observe(ctx, "SaveRun", r.ID); err != nil {
		return err
	}
	err := s.Store.SaveRun(ctx, r)
	s.probe.land("SaveRun", err, true)
	return err
}

func (s *gateMongoStore) DeleteRun(ctx context.Context, id string) error {
	if err := s.probe.observe(ctx, "DeleteRun", id); err != nil {
		return err
	}
	err := s.Store.DeleteRun(ctx, id)
	s.probe.land("DeleteRun", err, true)
	return err
}

func (s *gateMongoStore) ClaimMerge(ctx context.Context, id string, staleBefore time.Time, expect store.ExpectedBirth) (bool, store.MergeStatus, time.Time, error) {
	if err := s.probe.observe(ctx, "ClaimMerge", id); err != nil {
		return false, "", time.Time{}, err
	}
	ok, prior, at, err := s.Store.ClaimMerge(ctx, id, staleBefore, expect)
	s.probe.land("ClaimMerge", err, ok)
	return ok, prior, at, err
}

func (s *gateMongoStore) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, expectedFrom []store.MergeStatus) (bool, error) {
	if err := s.probe.observe(ctx, "UpdateRunMergeIf", id); err != nil {
		return false, err
	}
	changed, err := s.Store.UpdateRunMergeIf(ctx, id, upd, expectedFrom)
	s.probe.land("UpdateRunMergeIf", err, changed)
	return changed, err
}

func (s *gateMongoStore) AppendQueuedMessage(ctx context.Context, id string, msg store.QueuedUserMessage) error {
	if err := s.probe.observe(ctx, "AppendQueuedMessage", id); err != nil {
		return err
	}
	err := s.Store.AppendQueuedMessage(ctx, id, msg)
	s.probe.land("AppendQueuedMessage", err, true)
	return err
}

func (s *gateMongoStore) AddWatchedIssues(ctx context.Context, runID string, ids []string) ([]string, error) {
	if err := s.probe.observe(ctx, "AddWatchedIssues", runID); err != nil {
		return nil, err
	}
	out, err := s.Store.AddWatchedIssues(ctx, runID, ids)
	s.probe.land("AddWatchedIssues", err, true)
	return out, err
}

func (s *gateMongoStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	if err := s.probe.observe(ctx, "AppendEvent", runID); err != nil {
		return nil, err
	}
	out, err := s.Store.AppendEvent(ctx, runID, evt)
	s.probe.land("AppendEvent", err, true)
	return out, err
}

// mongoActionFixture serves the production chain on the DisableAuth path
// (its synthetic "dev" tenant) over the probed native Mongo store, one run
// born A; ctx carries that tenant for the fixture's own store calls.
type mongoActionFixture struct {
	srv   *Server
	hs    *httptest.Server
	ms    *mongostore.Store
	store *gateMongoStore
	ctx   context.Context
	row   actionRow
	env   actionEnv
	birth string
}

func newMongoActionFixture(t *testing.T, uri string, row actionRow) *mongoActionFixture {
	t.Helper()
	ctx := store.WithIdentity(t.Context(), "dev", "dev")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("action_origin_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	mustWS(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	wrapped := &gateMongoStore{Store: ms}
	workDir := t.TempDir()
	srv := New(Config{WorkDir: workDir, StoreDir: filepath.Join(workDir, ".iterion"), SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(wrapped))
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
	env := actionEnv{}
	if row.env != nil {
		env = row.env(t)
	}
	mustWS(t, seedActionRun(ctx, ms, row, env, "A"))
	return &mongoActionFixture{srv: srv, hs: hs, ms: ms, store: wrapped, ctx: ctx, row: row, env: env, birth: birthOf(t, storeWithCtx{ms, ctx}, actionRunID)}
}

// mongoRunPicture is the successor as the store holds it: its document and
// its inbox.
func mongoRunPicture(ctx context.Context, ms *mongostore.Store) (string, error) {
	r, err := ms.LoadRun(ctx, actionRunID)
	if err != nil {
		return "", err
	}
	msgs, err := ms.ListQueuedMessages(ctx, actionRunID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Run   *store.Run
		Inbox []store.QueuedUserMessage
	}{r, msgs})
	return string(raw), err
}

// W2m + W3 on the native Mongo store, for the rows whose doors differ from
// the filesystem's: the merge claim and CAS, the delete transaction, the
// SaveRun preimage, the inbox append and the metadata predicate of the
// watched set. W2m: A replaced by an actionable B right after the gate's
// read → 409 run_origin_lost, B's document and inbox unchanged, no write
// landed, the repository unchanged, every access of the action bound but
// the confirm read. W3: at the current birth, HEAD's status, the path's run
// only, no bound access behind the response. Then the delete race: another
// delete of A between the service's load and the store's — a delete bound
// to A's tombstone is the Mongo store's idempotent success (204), one bound
// to a reaped A refuses typed (409). Gated on ITERION_TEST_MONGO_URI.
func TestBoundMutationsNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the bound mutations are unverified on Mongo")
	}
	noLLMCredential(t)
	for _, name := range []string{"merge", "delete", "rename", "queue-message", "watch-add"} {
		row := actionRowByName(t, name)
		t.Run("W2m-"+name, func(t *testing.T) {
			fx := newMongoActionFixture(t, uri, row)
			var (
				mu                      sync.Mutex
				picture, envPic, birthB string
			)
			fx.store.afterLoadN(1, func() error {
				if err := replaceActionRun(fx.ctx, fx.ms, row, fx.env, "B"); err != nil {
					return err
				}
				pic, err := mongoRunPicture(fx.ctx, fx.ms)
				if err != nil {
					return err
				}
				e, err := envDigest(fx.env)
				if err != nil {
					return err
				}
				b, err := fx.ms.LoadRun(fx.ctx, actionRunID)
				if err != nil {
					return err
				}
				mu.Lock()
				picture, envPic, birthB = pic, e, b.BirthID
				mu.Unlock()
				fx.store.probe.reset()
				return nil
			})
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			mustWS(t, fx.store.failure())
			wantOriginLost(t, name, got, actionRunID)
			mu.Lock()
			defer mu.Unlock()
			if birthB == "" || birthB == fx.birth {
				t.Fatalf("the replacement did not run: successor birth %q, original %q", birthB, fx.birth)
			}
			after, err := mongoRunPicture(fx.ctx, fx.ms)
			mustWS(t, err)
			if after != picture {
				t.Fatalf("the successor changed:\nbefore %s\nafter  %s", picture, after)
			}
			e, err := envDigest(fx.env)
			mustWS(t, err)
			if e != envPic {
				t.Fatalf("the successor's repository changed:\nbefore:\n%s\nafter:\n%s", envPic, e)
			}
			calls, landed := fx.store.probe.recorded()
			if len(landed) != 0 {
				t.Fatalf("writes landed on the successor: %v", landed)
			}
			var unbound []storeCall
			bound := 0
			for _, c := range calls {
				if c.bound {
					bound++
				} else {
					unbound = append(unbound, c)
				}
			}
			if bound == 0 || len(unbound) != 1 || unbound[0].method != "LoadRun" {
				t.Fatalf("after the replacement: %d bound accesses and the unbound %+v, want bound accesses and one unbound confirm read", bound, unbound)
			}
		})
		t.Run("W3-"+name, func(t *testing.T) {
			fx := newMongoActionFixture(t, uri, row)
			h := fx.srv.Handler()
			hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
				fx.store.probe.after.Store(true)
			}))
			t.Cleanup(hs.Close)
			fx.store.probe.reset()
			if got := actionDo(t, hs.URL, row, fx.birth); got.status != row.ok {
				t.Fatalf("%s at the current birth: %d %s, want %d", name, got.status, got.body, row.ok)
			}
			calls, _ := fx.store.probe.recorded()
			if len(calls) < 2 {
				t.Fatalf("%s: %d probed accesses, want the gate's and the action's", name, len(calls))
			}
			for i, c := range calls {
				if c.runID != actionRunID || c.bound && c.after || i > 0 && !c.bound {
					t.Fatalf("%s access %d %+v: want the path's run, bound after the gate's read, none bound behind the response (all %+v)", name, i, c, calls)
				}
			}
		})
	}
	for _, reaped := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete-raced/reaped=%t", reaped), func(t *testing.T) {
			row := actionRowByName(t, "delete")
			fx := newMongoActionFixture(t, uri, row)
			fx.store.afterLoadN(2, func() error {
				if err := fx.ms.DeleteRun(fx.ctx, actionRunID); err != nil {
					return err
				}
				if reaped {
					if _, err := fx.ms.PruneDeletionMarkers(fx.ctx, time.Now().Add(time.Hour)); err != nil {
						return err
					}
				}
				fx.store.probe.reset()
				return nil
			})
			got := actionDo(t, fx.hs.URL, row, fx.birth)
			mustWS(t, fx.store.failure())
			if reaped {
				wantOriginLost(t, "delete bound to a reaped run", got, actionRunID)
			} else if got.status != http.StatusNoContent {
				t.Fatalf("delete bound to its own tombstone: %d %s, want the idempotent 204", got.status, got.body)
			}
			if _, err := fx.ms.LoadRun(fx.ctx, actionRunID); !errors.Is(err, store.ErrRunDeleted) && !errors.Is(err, store.ErrRunNotFound) {
				t.Fatalf("after the raced delete the run loads: %v", err)
			}
		})
	}
}
