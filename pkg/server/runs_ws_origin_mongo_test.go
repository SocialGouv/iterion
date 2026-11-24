package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// wsOriginHookSource runs a hook once inside a subscribe call, then forwards
// it unchanged: it schedules a replacement between the snapshot and the
// subscription. It forwards the bound-observation contract explicitly.
type wsOriginHookSource struct {
	inner                    runstream.Source
	mu                       sync.Mutex
	beforeEvents, beforeLogs func() error
	hookErr                  error
}

func (s *wsOriginHookSource) hook(h *func() error) {
	s.mu.Lock()
	run := *h
	*h = nil
	s.mu.Unlock()
	if run == nil {
		return
	}
	if err := run(); err != nil {
		s.mu.Lock()
		s.hookErr = errors.Join(s.hookErr, err)
		s.mu.Unlock()
	}
}
func (s *wsOriginHookSource) SubscribeEvents(ctx context.Context, id string, from int64) (runstream.EventSubscription, error) {
	return s.SubscribeEventsCursor(ctx, id, runstream.EventCursor{Seq: from})
}
func (s *wsOriginHookSource) SubscribeEventsCursor(ctx context.Context, id string, cursor runstream.EventCursor) (runstream.EventSubscription, error) {
	s.hook(&s.beforeEvents)
	return runstream.SubscribeEventsCursor(ctx, s.inner, id, cursor)
}
func (s *wsOriginHookSource) SubscribeLogs(ctx context.Context, id string, from int64) (runstream.LogSubscription, error) {
	s.hook(&s.beforeLogs)
	return s.inner.SubscribeLogs(ctx, id, from)
}
func (s *wsOriginHookSource) Close() error { return s.inner.Close() }
func (s *wsOriginHookSource) CheckBoundObservationSource(actual runstream.Source) error {
	if receiver, ok := actual.(*wsOriginHookSource); !ok || receiver != s {
		return store.ErrRunDataCapability
	}
	return runstream.RequireBoundObservationSource(s.inner)
}
func (s *wsOriginHookSource) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

// W18: the Service's own source, no injected one, over the native Mongo
// store is admitted for a bound subscription — the store endorses itself —
// and over a wrapper that embeds it is refused typed. CI's mongo-conformance
// job supplies the URI.
func TestRunWSServiceSourceStoreCapabilityNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; Mongo store capability unverified")
	}
	ctx := store.WithIdentity(t.Context(), "ws-origin-tenant", "owner")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("ws_store_capability_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	mustWS(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	native := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms))
	if err := runstream.RequireBoundObservationSource(native.StreamSource()); err != nil {
		t.Fatalf("native Mongo store refused: %v", err)
	}
	embedding := &struct{ *mongostore.Store }{ms}
	wrapped := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(embedding))
	if err := runstream.RequireBoundObservationSource(wrapped.StreamSource()); !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatalf("a wrapper embedding the Mongo store admitted: %v", err)
	}
}

// T4: the socket origin against the native Mongo store and the production
// resolved change-stream source. The upgrade carries a tenant, as every cloud
// upgrade does. CI's mongo-conformance job supplies the URI.
func TestRunWSOriginNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; Mongo socket origin unverified")
	}
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		modes := []string{"positive", "legitimate", "resubscribe", "events", "logs"}
		if f == store.LegacyValueFormat {
			// Only format one predates birth identities.
			modes = append(modes, "empty-birth")
		}
		for _, mode := range modes {
			t.Run(fmt.Sprintf("f%d/%s", f, mode), func(t *testing.T) {
				wsOriginMongoCase(t, uri, f, mode)
			})
		}
	}
}

func wsOriginMongoCase(t *testing.T, uri string, f store.ValueFormat, mode string) {
	const tenant = "ws-origin-tenant"
	ctx := store.WithIdentity(t.Context(), tenant, "owner")
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("ws_origin_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
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
		data := map[string]any{"marker": marker}
		if f == store.ExactValueFormat {
			data["huge"] = json.Number("1e400")
		}
		if _, err := ms.AppendEvent(bound, "r", store.Event{Type: store.EventNodeStarted, NodeID: marker, Data: data}); err != nil {
			return err
		}
		if err := ms.AppendRunLog(bound, "r", 0, []byte(marker+" bytes\n")); err != nil {
			return err
		}
		return ms.UpdateRunStatus(ctx, "r", store.RunStatusFinished, "")
	}
	replace := func() error {
		if err := ms.DeleteRun(ctx, "r"); err != nil {
			return err
		}
		if _, err := ms.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
			return err
		}
		return seed("B")
	}
	mustWS(t, seed("A"))

	srv, _ := newTestServer(t)
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms))
	mongoSrc := runstream.NewMongoResolved(func(c context.Context, id string) (runstream.MongoSelection, error) {
		return ms.ResolveStreamSelection(c, id)
	}, nil)
	streams := &testOwnedRunStreams{Source: mongoSrc}
	hooks := &wsOriginHookSource{inner: streams}
	runview.WithStreamSource(hooks)(svc)
	srv.runs = svc
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ws/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		srv.handleRunWebSocket(w, r.WithContext(store.WithIdentity(r.Context(), tenant, "owner")))
	})
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		hs.Close()
		if err := streams.Close(); err != nil {
			t.Errorf("join run streams: %v", err)
		}
	})

	if mode == "empty-birth" {
		// A historical row written before birth identities: no birth, no
		// format, the tenant as the old writer stored it.
		doc := map[string]any{"_id": "h", "tenant_id": tenant, "status": string(store.RunStatusFinished), "workflow_name": "H", "created_at": time.Now().UTC(), "updated_at": time.Now().UTC()}
		_, err := ms.DB().Collection("runs").InsertOne(ctx, doc)
		mustWS(t, err)
		h, err := ms.LoadRun(ctx, "h")
		mustWS(t, err)
		if h.BirthID != "" {
			t.Fatalf("fixture is not an empty-birth run: birth %q", h.BirthID)
		}
		hc := dialRunWS(t, hs, "h")
		for i := 0; i < 2; i++ {
			step := fmt.Sprint(i)
			writeJSONMessage(t, hc, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s" + step})
			if snap := snapshotOf(t, readEnvelopeWithin(t, hc, 10*time.Second)); snap.Run.WorkflowName != "H" {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			writeJSONMessage(t, hc, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u" + step})
			readUntilAck(t, hc, "u"+step)
		}
		mustWS(t, hooks.failure())
		return
	}
	c := dialRunWS(t, hs, "r")
	switch mode {
	case "positive":
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: json.RawMessage(`{"replay_history":true}`)})
		if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != "A" {
			t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
		}
		env := readEnvelopeWithin(t, c, 10*time.Second, wsTypeEvent, wsTypeEventBatch)
		// Node IDs only: an exact payload's 1e400 does not fit a float64.
		var node struct {
			NodeID string `json:"node_id"`
		}
		mustWS(t, json.Unmarshal(env.Payload, &node))
		if env.Type != wsTypeEvent || node.NodeID != "A" {
			t.Fatalf("events = %s %s", env.Type, env.Payload)
		}
		if f == store.ExactValueFormat && !strings.Contains(string(env.Payload), `"huge":1e400`) {
			t.Fatalf("exact literal lost: %s", env.Payload)
		}
		readEnvelopeWithin(t, c, 20*time.Second, wsTypeTerminated)
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
		if got := collectLogText(t, c, "", "A bytes\n", 10*time.Second); got != "A bytes\n" {
			t.Fatalf("log = %q", got)
		}
	case "legitimate":
		// Writes that keep the birth keep the origin: a metadata save, a
		// pause and a resume (each status change rotates the epoch), an end.
		a, err := ms.LoadRun(ctx, "r")
		mustWS(t, err)
		write, err := store.BindRunValueFormat(ctx, a)
		mustWS(t, err)
		resubscribe := func(step string) {
			t.Helper()
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s-" + step})
			if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != "A" {
				t.Fatalf("%s: snapshot names %q", step, snap.Run.WorkflowName)
			}
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u-" + step})
			readUntilAck(t, c, "u-"+step)
		}
		mustWS(t, ms.UpdateRunStatus(write, "r", store.RunStatusRunning, ""))
		resubscribe("running")
		r, err := ms.LoadRun(ctx, "r")
		mustWS(t, err)
		r.Name = "renamed"
		mustWS(t, ms.SaveRun(write, r))
		resubscribe("save")
		if f == store.LegacyValueFormat {
			mustWS(t, ms.PauseRun(write, "r", &store.Checkpoint{}))
		} else {
			// An exact checkpoint must carry every exact payload; the pause
			// here is the status change, which is what rotates the epoch.
			mustWS(t, ms.UpdateRunStatus(write, "r", store.RunStatusPausedWaitingHuman, ""))
		}
		resubscribe("pause")
		mustWS(t, ms.UpdateRunStatus(write, "r", store.RunStatusRunning, ""))
		resubscribe("resume")
		after, err := ms.LoadRun(ctx, "r")
		mustWS(t, err)
		if after.ExecutionEpoch == a.ExecutionEpoch || after.BirthID != a.BirthID {
			t.Fatalf("fixture did not rotate the epoch under one birth: %q→%q, birth %q→%q", a.ExecutionEpoch, after.ExecutionEpoch, a.BirthID, after.BirthID)
		}
		mustWS(t, ms.UpdateRunStatus(write, "r", store.RunStatusFinished, ""))
		resubscribe("finished")
	case "resubscribe":
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
		snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second))
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
		readUntilAck(t, c, "u1")
		mustWS(t, replace())
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
		expectOriginLostClose(t, c, "snapshot_failed")
	case "events":
		hooks.beforeEvents = replace
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: json.RawMessage(`{"replay_history":true}`)})
		if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != "A" {
			t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
		}
		expectOriginLostClose(t, c, "event_stream_failed")
	case "logs":
		hooks.beforeLogs = replace
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
		expectOriginLostClose(t, c, "log_stream_failed")
	}
	mustWS(t, hooks.failure())
}

// wsOriginBlob is a no-op blob backend: these runs carry events and logs in
// Mongo only, and deletion must find every object kind it cleans.
type wsOriginBlob struct{}

func (wsOriginBlob) PutArtifact(context.Context, string, string, int, []byte) error { return nil }
func (wsOriginBlob) GetArtifact(context.Context, string, string, int) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (wsOriginBlob) ListArtifactVersions(context.Context, string, string) ([]int, error) {
	return nil, nil
}
func (wsOriginBlob) DeleteRun(context.Context, string) error { return nil }
func (wsOriginBlob) Ping(context.Context) error              { return nil }
func (wsOriginBlob) Close() error                            { return nil }
func (wsOriginBlob) PutAttachment(context.Context, string, string, string, string, []byte) error {
	return nil
}
func (wsOriginBlob) GetAttachment(context.Context, string, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
}
func (wsOriginBlob) PresignAttachment(context.Context, string, string, string, time.Duration) (string, error) {
	return "", blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteAttachment(context.Context, string, string, string) error { return nil }
func (wsOriginBlob) DeleteRunAttachments(context.Context, string) error             { return nil }
func (wsOriginBlob) PutToolBlob(context.Context, string, string, string, []byte) error {
	return nil
}
func (wsOriginBlob) GetToolBlobRange(context.Context, string, string, string, int64, int64) ([]byte, int64, bool, error) {
	return nil, 0, false, blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteRunToolBlobs(context.Context, string) error { return nil }
func (wsOriginBlob) PutRunFile(context.Context, string, string, string, io.Reader, int64) error {
	return nil
}
func (wsOriginBlob) ListRunFiles(context.Context, string) ([]blob.RunFileObject, error) {
	return nil, nil
}
func (wsOriginBlob) GetRunFile(context.Context, string, string) (io.ReadCloser, blob.RunFileObject, error) {
	return nil, blob.RunFileObject{}, blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteRunFiles(context.Context, string) error    { return nil }
func (wsOriginBlob) PutIRBlob(context.Context, string, []byte) error { return nil }
func (wsOriginBlob) GetIRBlob(context.Context, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteRunIR(context.Context, string) error { return nil }
func (wsOriginBlob) PutBackendSession(context.Context, string, string, []byte) error {
	return nil
}
func (wsOriginBlob) GetBackendSession(context.Context, string, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteBackendSession(context.Context, string, string) error { return nil }
func (wsOriginBlob) PutUpload(context.Context, string, string, string, io.ReadSeeker, int64) error {
	return nil
}
func (wsOriginBlob) GetUpload(context.Context, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
}
func (wsOriginBlob) DeleteUpload(context.Context, string) error             { return nil }
func (wsOriginBlob) DeleteRunBackendSessions(context.Context, string) error { return nil }

var _ blob.Client = wsOriginBlob{}

// wsOriginExactBlob adds the exact-format object capabilities a format-two
// run's deletion requires.
type wsOriginExactBlob struct{ wsOriginBlob }

func (b wsOriginExactBlob) PutArtifactForFormat(c context.Context, _ int, r, n string, v int, d []byte) error {
	return b.PutArtifact(c, r, n, v, d)
}
func (b wsOriginExactBlob) GetArtifactForFormat(c context.Context, _ int, r, n string, v int) ([]byte, error) {
	return b.GetArtifact(c, r, n, v)
}
func (b wsOriginExactBlob) ListArtifactVersionsForFormat(c context.Context, _ int, r, n string) ([]int, error) {
	return b.ListArtifactVersions(c, r, n)
}
func (b wsOriginExactBlob) DeleteRunArtifactsForFormat(c context.Context, _ int, r string) error {
	return b.DeleteRun(c, r)
}
func (b wsOriginExactBlob) DeleteRunArtifacts(c context.Context, r string) error {
	return b.DeleteRun(c, r)
}
func (b wsOriginExactBlob) RunObjectsForFormat(int) (blob.RunObjects, error) { return b, nil }
func (b wsOriginExactBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return b, nil
}
func (b wsOriginExactBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return b, nil
}
