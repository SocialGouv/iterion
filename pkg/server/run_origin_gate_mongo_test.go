package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// T8: the gate on the native Mongo store, through the production chain on
// the DisableAuth path (its synthetic "dev" tenant scopes the rows). A run
// deleted, pruned and recreated is refused on every class route for the
// original's birth; the current birth serves it; a run socket dialed with the
// original's birth gets run_origin_lost then scope_lost, with the current
// one the successor's snapshot. Gated on ITERION_TEST_MONGO_URI.
func TestRunOriginGateNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the Mongo run-origin gate is unverified")
	}
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) { gateMongoCase(t, uri, f) })
	}
}

func gateMongoCase(t *testing.T, uri string, f store.ValueFormat) {
	ctx := store.WithIdentity(t.Context(), "dev", "dev")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("gate_origin_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	mustWS(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	seed := func(marker string) error {
		r, err := ms.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: marker, Format: f})
		if err != nil {
			return err
		}
		bound, err := store.BindRunValueFormat(ctx, r)
		if err != nil {
			return err
		}
		if _, err := ms.AppendEvent(bound, "r", store.Event{Type: store.EventNodeStarted, NodeID: marker}); err != nil {
			return err
		}
		return ms.UpdateRunStatus(ctx, "r", store.RunStatusFinished, "")
	}
	mustWS(t, seed("A"))
	stale := birthOf(t, storeWithCtx{ms, ctx}, "r")
	mustWS(t, ms.DeleteRun(ctx, "r"))
	_, err = ms.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
	mustWS(t, err)
	mustWS(t, seed("B"))
	current := birthOf(t, storeWithCtx{ms, ctx}, "r")
	if stale == "" || current == "" || stale == current {
		t.Fatalf("fixture births %q → %q", stale, current)
	}

	workDir := t.TempDir()
	srv := New(Config{WorkDir: workDir, StoreDir: filepath.Join(workDir, ".iterion"), SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms))
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

	routes := classRoutes(srv, "r")
	if len(routes) < 70 {
		t.Fatalf("only %d run-wildcard routes found", len(routes))
	}
	for _, rt := range routes {
		wantOriginLost(t, rt[0]+" "+rt[1], gateDo(t, hs.URL, rt[0], rt[1], stale, "header"), "r")
	}
	after, err := ms.LoadRun(ctx, "r")
	mustWS(t, err)
	if after.BirthID != current || after.WorkflowName != "B" || after.Status != store.RunStatusFinished {
		t.Fatalf("a refused request changed the run: birth %q workflow %q status %s", after.BirthID, after.WorkflowName, after.Status)
	}
	got := gateDo(t, hs.URL, http.MethodGet, "/api/runs/r", current, "query")
	var snap runview.RunSnapshot
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &snap) != nil || snap.Run.BirthID != current || snap.Run.WorkflowName != "B" {
		t.Fatalf("current birth: %d %s", got.status, got.body)
	}

	old := dialGate(t, hs, "/api/ws/runs/r?expected_birth="+stale)
	writeJSONMessage(t, old, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	expectOriginLostClose(t, old, wsErrRunOriginLost)
	cur := dialGate(t, hs, "/api/ws/runs/r?expected_birth="+current)
	writeJSONMessage(t, cur, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s"})
	if snap := snapshotOf(t, readEnvelopeWithin(t, cur, 10*time.Second)); snap.Run.WorkflowName != "B" || snap.Run.BirthID != current {
		t.Fatalf("current socket served %q birth %q", snap.Run.WorkflowName, snap.Run.BirthID)
	}
}

// storeWithCtx reads a store under a fixed context (the Mongo rows' tenant).
type storeWithCtx struct {
	*mongostore.Store
	ctx context.Context
}

func (s storeWithCtx) LoadRun(_ context.Context, id string) (*store.Run, error) {
	return s.Store.LoadRun(s.ctx, id)
}
