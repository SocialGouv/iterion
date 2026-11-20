package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.mongodb.org/mongo-driver/v2/bson"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The run socket's origin matrix on the cloud wiring: the native Mongo store
// behind the run console, the resolved change-stream source
// (runstream.NewMongoResolved, as cmd/iterion/server.go builds it) behind the
// matrix hook source, served through Server.Handler on the DisableAuth path,
// whose synthetic "dev" tenant scopes the rows. Gated on
// ITERION_TEST_MONGO_URI: CI's mongo-conformance job runs ./pkg/server/...
// whole, so every test here is collected there.

// matrixMongoTenant is the tenant the DisableAuth upgrade scopes the rows to.
const matrixMongoTenant = "dev"

// matrixMongoURI returns the Mongo URI the matrix runs against, or skips.
func matrixMongoURI(t *testing.T) string {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; the run socket's cloud origin matrix is unverified")
	}
	return uri
}

// matrixMongoServer is one server of the cloud matrix: the Mongo store, its
// run console and the shared change-stream source behind the hook source.
type matrixMongoServer struct {
	hs     *httptest.Server
	ms     *mongostore.Store
	ctx    context.Context
	format store.ValueFormat
	hook   *matrixHookSource
	polls  *matrixTerminalPolls
}

// newMatrixMongoServer builds the cloud wiring over a fresh database. The hook
// source forwards Close to the MongoSource itself, so a socket that closed the
// shared source would end every cloud stream as production's would: untyped,
// MongoSource.Close ending an open stream as a clean terminated. With
// observePolls, the selections the source resolves report their terminal
// reads (matrixPolledSelection).
func newMatrixMongoServer(t *testing.T, uri string, f store.ValueFormat, observePolls bool) *matrixMongoServer {
	t.Helper()
	ctx := store.WithIdentity(t.Context(), matrixMongoTenant, matrixMongoTenant)
	ms, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("ws_matrix_%d", time.Now().UnixNano()), Blob: wsOriginExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
	mustWS(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = ms.DB().Drop(c)
		_ = ms.Close(c)
	})
	var polls *matrixTerminalPolls
	if observePolls {
		polls = &matrixTerminalPolls{notify: make(chan struct{}, 1)}
	}
	workDir := t.TempDir()
	srv := New(Config{WorkDir: workDir, StoreDir: filepath.Join(workDir, ".iterion"), SkipProjectRegistration: true, DisableAuth: true}, iterlog.New(iterlog.LevelError, os.Stderr))
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(ms))
	src := runstream.NewMongoResolved(func(c context.Context, id string) (runstream.MongoSelection, error) {
		sel, err := ms.ResolveStreamSelection(c, id)
		if err != nil {
			return nil, err
		}
		if polls == nil {
			return sel, nil
		}
		return &matrixPolledSelection{StreamSelection: sel, polls: polls}, nil
	}, nil)
	hook := &matrixHookSource{inner: src}
	runview.WithStreamSource(hook)(svc)
	srv.runs = svc
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hook.releaseAll()
		hs.Close()
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
		// MongoSource.Close cancels and joins every producer it started,
		// before the database is dropped.
		if err := src.Close(); err != nil {
			t.Errorf("close the Mongo source: %v", err)
		}
		if err := hook.join(30 * time.Second); err != nil {
			t.Errorf("join the matrix relays: %v", err)
		}
	})
	return &matrixMongoServer{hs: hs, ms: ms, ctx: ctx, format: f, hook: hook, polls: polls}
}

// seed creates run r named marker in the fixture's format, with one event of
// node marker and the log line "<marker> log\n", then sets its status.
func (fx *matrixMongoServer) seed(t *testing.T, marker string, status store.RunStatus) {
	t.Helper()
	r, err := fx.ms.CreateRunWithValueFormat(fx.ctx, store.RunCreateSpec{ID: "r", WorkflowName: marker, Format: fx.format})
	mustWS(t, err)
	write, err := store.BindRunValueFormat(fx.ctx, r)
	mustWS(t, err)
	_, err = fx.ms.AppendEvent(write, "r", store.Event{Type: store.EventNodeStarted, NodeID: marker})
	mustWS(t, err)
	mustWS(t, fx.ms.AppendRunLog(write, "r", 0, []byte(marker+" log\n")))
	mustWS(t, fx.ms.UpdateRunStatus(fx.ctx, "r", status, ""))
}

// replace deletes run r, prunes its deletion marker and seeds marker in its
// place: the successor production makes, under a new birth.
func (fx *matrixMongoServer) replace(t *testing.T, marker string, status store.RunStatus) {
	t.Helper()
	before := fx.load(t).BirthID
	mustWS(t, fx.ms.DeleteRun(fx.ctx, "r"))
	_, err := fx.ms.PruneDeletionMarkers(fx.ctx, time.Now().Add(time.Hour))
	mustWS(t, err)
	fx.seed(t, marker, status)
	if after := fx.load(t).BirthID; after == "" || after == before {
		t.Fatalf("fixture: the successor's birth %q, the origin's %q", after, before)
	}
}

func (fx *matrixMongoServer) load(t *testing.T) *store.Run {
	t.Helper()
	r, err := fx.ms.LoadRun(fx.ctx, "r")
	mustWS(t, err)
	return r
}

// writeAs is a writer's context for run r as it stands: its own format, no
// birth binding.
func (fx *matrixMongoServer) writeAs(t *testing.T) context.Context {
	t.Helper()
	w, err := store.BindRunValueFormat(fx.ctx, fx.load(t))
	mustWS(t, err)
	return w
}

// rewriteRun sets one member of run r's document in place, every other kept,
// and requires the canonical header reader to read want back.
func (fx *matrixMongoServer) rewriteRun(t *testing.T, key, value string, want store.RunDataHeader) {
	t.Helper()
	blind := store.WithoutTenantFilter(t.Context())
	res, err := fx.ms.DB().Collection("runs").UpdateOne(blind, bson.M{"_id": "r"}, bson.M{"$set": bson.M{key: value}})
	mustWS(t, err)
	if res.MatchedCount != 1 {
		t.Fatalf("fixture: %s rewrite matched %d run documents", key, res.MatchedCount)
	}
	if got, err := fx.ms.ObserveRunDataHeader(blind, "r"); err != nil || got != want {
		t.Fatalf("fixture: the canonical header after the %s rewrite = %+v (%v), want %+v", key, got, err, want)
	}
}

// forgeSuccessor rewrites run r's birth in place — a successor under the same
// ID, tenant, format and data layout, so its events and log chunks match the
// predecessor's change-stream filter — and returns its writer's context.
func (fx *matrixMongoServer) forgeSuccessor(t *testing.T) context.Context {
	t.Helper()
	h, err := fx.ms.ObserveRunDataHeader(store.WithoutTenantFilter(t.Context()), "r")
	mustWS(t, err)
	h.BirthID = "forged-successor-of-" + h.BirthID
	fx.rewriteRun(t, "birth_id", h.BirthID, h)
	return fx.writeAs(t)
}

// retenant rewrites run r's tenant in place under its birth and requires the
// run to load in that tenant only.
func (fx *matrixMongoServer) retenant(t *testing.T, tenant string) {
	t.Helper()
	h, err := fx.ms.ObserveRunDataHeader(store.WithoutTenantFilter(t.Context()), "r")
	mustWS(t, err)
	fx.rewriteRun(t, "tenant_id", tenant, h)
	if r, err := fx.ms.LoadRun(store.WithIdentity(t.Context(), tenant, matrixMongoTenant), "r"); err != nil || r.TenantID != tenant || r.BirthID != h.BirthID {
		t.Fatalf("fixture: run r in tenant %q after the rewrite = %+v, %v", tenant, r, err)
	}
	if _, err := fx.ms.LoadRun(fx.ctx, "r"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("fixture: run r still loads in tenant %q after the rewrite: %v", matrixMongoTenant, err)
	}
}

// matrixPolledSelection is the store's own stream selection with its terminal
// reads observed: one after each round's replay, then one per tick of
// runstream's terminal poll. It endorses itself as the observation selection
// it forwards; every read is the store's.
type matrixPolledSelection struct {
	*mongostore.StreamSelection
	polls *matrixTerminalPolls
}

func (s *matrixPolledSelection) CheckEventObservationSelection(actual any) error {
	if actual != any(s) {
		return fmt.Errorf("%w: %T is not this polled selection", store.ErrRunDataCapability, actual)
	}
	return s.StreamSelection.CheckEventObservationSelection(s.StreamSelection)
}

func (s *matrixPolledSelection) Terminal(ctx context.Context) (bool, error) {
	terminal, err := s.StreamSelection.Terminal(ctx)
	s.polls.observe(err)
	return terminal, err
}

// matrixTerminalPolls counts the terminal reads that returned and keeps the
// refusals among them, by the read's rank.
type matrixTerminalPolls struct {
	mu       sync.Mutex
	returned int
	refused  map[int]error
	notify   chan struct{}
}

func (p *matrixTerminalPolls) observe(err error) {
	p.mu.Lock()
	p.returned++
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if p.refused == nil {
			p.refused = map[int]error{}
		}
		p.refused[p.returned] = err
	}
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// awaitReturned waits until n terminal reads returned and reports how many
// did.
func (p *matrixTerminalPolls) awaitReturned(t *testing.T, n int, within time.Duration) int {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		p.mu.Lock()
		got := p.returned
		p.mu.Unlock()
		if got >= n {
			return got
		}
		select {
		case <-p.notify:
		case <-deadline.C:
			t.Fatalf("%d terminal reads returned within %v, want %d", got, within, n)
		}
	}
}

// refusalsAfter lists the refusals of the reads ranked after k.
func (p *matrixTerminalPolls) refusalsAfter(k int) []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []error
	for rank, err := range p.refused {
		if rank > k {
			out = append(out, fmt.Errorf("terminal read %d: %w", rank, err))
		}
	}
	return out
}

// forgeWithinThePollInterval waits for the first tick of the terminal poll —
// the second terminal read of the arm's only selection, after the one that
// follows the replay — then runs forge at once: the next tick is a full poll
// interval away, so what forge appends reaches the per-frame Validate before
// any poll reads the forged run. A poll refusal after forge fails the arm as
// unattributable.
func (fx *matrixMongoServer) forgeWithinThePollInterval(t *testing.T, forge func()) (check func()) {
	t.Helper()
	k := fx.polls.awaitReturned(t, 2, 20*time.Second)
	forged := time.Now()
	forge()
	return func() {
		t.Helper()
		if refused := fx.polls.refusalsAfter(k); len(refused) != 0 {
			t.Fatalf("the terminal poll refused the forged run (%v): the refusal is not attributable to the per-frame Validate", refused)
		}
		t.Logf("forge to the socket's close: %v (the next poll tick is one interval after the forge)", time.Since(forged))
	}
}

// WM1 (R3, R4): a run whose successor takes its place while the cloud
// stream is live — forged in place, its birth rewritten, so the successor's
// frame lands in the predecessor's change stream — is refused by the
// per-frame Validate: event_stream_failed / log_stream_failed, then
// scope_lost, and the successor's frame never reaches the socket.
func TestRunWSMatrixLiveSuccessorNativeMongo(t *testing.T) {
	uri := matrixMongoURI(t)
	const originLog = matrixOrigin + " log\n"
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d/live-events", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, true)
			fx.seed(t, matrixOrigin, store.RunStatusRunning)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: json.RawMessage(`{"replay_history":true,"from_seq":0}`)})
			if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			if evs := decodeEventEnvelope(t, readEnvelopeWithin(t, c, 10*time.Second, wsTypeEvent, wsTypeEventBatch)); len(evs) != 1 || evs[0].NodeID != matrixOrigin {
				t.Fatalf("replayed events = %+v, want the origin's one", evs)
			}
			check := fx.forgeWithinThePollInterval(t, func() {
				_, err := fx.ms.AppendEvent(fx.forgeSuccessor(t), "r", store.Event{Type: store.EventNodeStarted, NodeID: matrixSuccessor})
				mustWS(t, err)
			})
			expectMatrixOriginLost(t, c, "event_stream_failed", matrixSuccessor)
			check()
		})
		t.Run(fmt.Sprintf("f%d/live-logs", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, true)
			fx.seed(t, matrixOrigin, store.RunStatusRunning)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			readUntilAck(t, c, "l1")
			if got := collectLogText(t, c, "", originLog, 10*time.Second); got != originLog {
				t.Fatalf("logs of the origin = %q", got)
			}
			check := fx.forgeWithinThePollInterval(t, func() {
				mustWS(t, fx.ms.AppendRunLog(fx.forgeSuccessor(t), "r", int64(len(originLog)), []byte(matrixSuccessor+" log\n")))
			})
			expectMatrixOriginLost(t, c, "log_stream_failed", matrixSuccessor)
			check()
		})
	}
}

// WM2 (R5): one cloud socket, both orders of the two subscription kinds, the
// run replaced between them: the second kind is refused whichever came first
// — the binding is the upgrade's, not the first subscription's.
func TestRunWSMatrixLogsAndEventsInBothOrdersNativeMongo(t *testing.T) {
	uri := matrixMongoURI(t)
	const originLog = matrixOrigin + " log\n"
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d/logs-then-events", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, false)
			fx.seed(t, matrixOrigin, store.RunStatusFinished)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			readUntilAck(t, c, "l1")
			if got := collectLogText(t, c, "", originLog, 10*time.Second); got != originLog {
				t.Fatalf("logs of the origin = %q", got)
			}
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul1"})
			readUntilAck(t, c, "ul1")
			fx.replace(t, matrixSuccessor, store.RunStatusFinished)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			expectMatrixOriginLost(t, c, "snapshot_failed", matrixSuccessor)
			if n := fx.hook.eventSubscriptions("r"); n != 0 {
				t.Fatalf("event subscriptions of r = %d, want none: a refused snapshot reached the source", n)
			}
		})
		t.Run(fmt.Sprintf("f%d/events-then-logs", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, false)
			fx.seed(t, matrixOrigin, store.RunStatusFinished)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 20*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			fx.replace(t, matrixSuccessor, store.RunStatusFinished)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			expectMatrixOriginLost(t, c, "log_stream_failed", matrixSuccessor)
		})
	}
}

// WM3 (R8): the cloud source is the Service's, shared by every socket and
// never closed by one: once two sockets are live (each received an event),
// the first closes and the second still receives a new event, and a third
// subscribes and receives the next. A closed shared source ends the second's
// stream untyped, as a clean terminated: an error or a terminated envelope
// before an awaited event fails the row at once.
func TestRunWSMatrixSharedSourceNativeMongo(t *testing.T) {
	uri := matrixMongoURI(t)
	fx := newMatrixMongoServer(t, uri, store.LegacyValueFormat, false)
	fx.seed(t, matrixOrigin, store.RunStatusRunning)
	subscribe := func() *websocket.Conn {
		t.Helper()
		c := dialRunWS(t, fx.hs, "r")
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
		if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != matrixOrigin {
			t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
		}
		return c
	}
	appendLive := func(node string) {
		t.Helper()
		_, err := fx.ms.AppendEvent(fx.writeAs(t), "r", store.Event{Type: store.EventNodeFinished, NodeID: node})
		mustWS(t, err)
	}
	first, second := subscribe(), subscribe()
	appendLive("before-close")
	awaitMatrixLiveEvent(t, first, "before-close")
	awaitMatrixLiveEvent(t, second, "before-close")
	closeMatrixSocket(t, first)
	appendLive("after-close")
	awaitMatrixLiveEvent(t, second, "after-close")
	third := subscribe()
	appendLive("third")
	awaitMatrixLiveEvent(t, third, "third")
	awaitMatrixLiveEvent(t, second, "third")
}

// awaitMatrixLiveEvent reads c until the event of node nodeID: an error or a
// terminated envelope before it — the stream's end — fails at once.
func awaitMatrixLiveEvent(t *testing.T, c *websocket.Conn, nodeID string) {
	t.Helper()
	for {
		env := readEnvelopeWithin(t, c, 10*time.Second)
		switch env.Type {
		case wsTypeError, wsTypeTerminated:
			t.Fatalf("the stream ended before event %s: %s %s", nodeID, env.Type, env.Payload)
		case wsTypeEvent, wsTypeEventBatch:
			for _, ev := range decodeEventEnvelope(t, env) {
				if ev.NodeID == nodeID {
					return
				}
			}
		}
	}
}

// closeMatrixSocket closes c with the close handshake: it returns once the
// server read the close frame and answered it, its connection ending.
func closeMatrixSocket(t *testing.T, c *websocket.Conn) {
	t.Helper()
	mustWS(t, c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second)))
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("the server never answered the close: %v", err)
		}
		break
	}
	_ = c.Close()
}

// WM4 (R6): a cloud log subscription unsubscribed then resubscribed from an
// offset serves exactly the bytes past it — no duplicate, no gap; after a
// replacement the resubscription is refused typed.
func TestRunWSMatrixResubscribeLogsNativeMongo(t *testing.T) {
	uri := matrixMongoURI(t)
	const originLog, secondLog = matrixOrigin + " log\n", "y-bytes\n"
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, false)
			fx.seed(t, matrixOrigin, store.RunStatusRunning)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l1"})
			readUntilAck(t, c, "l1")
			if got := collectLogText(t, c, "", originLog, 10*time.Second); got != originLog {
				t.Fatalf("first log subscription = %q", got)
			}
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul1"})
			readUntilAck(t, c, "ul1")
			mustWS(t, fx.ms.AppendRunLog(fx.writeAs(t), "r", int64(len(originLog)), []byte(secondLog)))
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l2", Payload: json.RawMessage(fmt.Sprintf(`{"from_offset":%d}`, len(originLog)))})
			readUntilAck(t, c, "l2")
			if got := readMatrixLogSpan(t, c, int64(len(originLog)), len(secondLog)); got != secondLog {
				t.Fatalf("log resubscribed from offset %d = %q, want exactly %q", len(originLog), got, secondLog)
			}
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribeLog, AckID: "ul2"})
			readUntilAck(t, c, "ul2")
			mustWS(t, fx.ms.UpdateRunStatus(fx.ctx, "r", store.RunStatusFinished, ""))
			fx.replace(t, matrixSuccessor, store.RunStatusFinished)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l3"})
			expectMatrixOriginLost(t, c, "log_stream_failed", matrixSuccessor)
		})
	}
}

// WM5: a run whose tenant is rewritten under its birth is refused on the
// resubscribe — snapshot_failed, then scope_lost, no snapshot frame, and no
// event subscription reaches the source. The tenant-scoped query answers
// not-found before any binding comparison, so no binding mutant reddens this
// row: it pins the outcome on the cloud wiring.
func TestRunWSMatrixRetenantNativeMongo(t *testing.T) {
	uri := matrixMongoURI(t)
	for _, f := range matrixFormats {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			fx := newMatrixMongoServer(t, uri, f, false)
			fx.seed(t, matrixOrigin, store.RunStatusFinished)
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelopeWithin(t, c, 10*time.Second)); snap.Run.WorkflowName != matrixOrigin {
				t.Fatalf("first snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 20*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			fx.retenant(t, "U")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
			expectMatrixOriginLost(t, c, "snapshot_failed", `"type":"snapshot"`)
			if n := fx.hook.eventSubscriptions("r"); n != 1 {
				t.Fatalf("event subscriptions of r = %d, want only the first", n)
			}
		})
	}
}
