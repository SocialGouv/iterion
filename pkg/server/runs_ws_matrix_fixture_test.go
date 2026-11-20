package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run socket's origin matrix runs on the production local wiring: the
// server's own runview.Service over the native filesystem store (no
// WithStore), served through Server.Handler with DisableAuth, so checked
// dials pass the origin gate. Its stream source is the Service's own, behind
// matrixHookSource, installed after construction: the Service's native live
// streams are built inside NewService and survive it.

// matrixOptions selects the fixture's variant.
type matrixOptions struct {
	// alerts wires the run console's alert manager (Config.Alerts): the
	// registry lane stands behind the socket.
	alerts bool
	// legacy rebuilds the Service over a store wrapper that endorses itself:
	// the legacy stream path. Only W0's control arm asks for it.
	legacy bool
}

// matrixServer is one server of the matrix and its stream hooks.
type matrixServer struct {
	srv    *Server
	hs     *httptest.Server
	native *store.FilesystemRunStore
	hook   *matrixHookSource
}

// newMatrixServer builds the production local wiring and requires, once, that
// it serves the native path.
func newMatrixServer(t *testing.T, opt matrixOptions) *matrixServer {
	t.Helper()
	fx := buildMatrixServer(t, opt)
	requireNativeWiring(t, fx)
	return fx
}

func buildMatrixServer(t *testing.T, opt matrixOptions) *matrixServer {
	t.Helper()
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	cfg := Config{WorkDir: workDir, StoreDir: storeDir, SkipProjectRegistration: true, DisableAuth: true}
	if opt.alerts {
		cfg.Alerts = &runview.AlertSettings{}
	}
	srv := New(cfg, iterlog.New(iterlog.LevelError, os.Stderr))
	if srv.runs == nil {
		t.Fatal("run console service not wired")
	}
	native, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if opt.legacy {
		srv.runs.Stop(t.Context())
		srv.runs = newTestRunviewService(t, storeDir, runview.WithRecoveryPassive(), runview.WithStore(&wsOriginStore{FilesystemRunStore: native}))
	}
	hook := &matrixHookSource{inner: srv.runs.StreamSource()}
	runview.WithStreamSource(hook)(srv.runs)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hook.releaseAll()
		hs.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := hook.join(30 * time.Second); err != nil {
			t.Errorf("join the matrix relays: %v", err)
		}
	})
	return &matrixServer{srv: srv, hs: hs, native: native, hook: hook}
}

// matrixSentinel is the running run the wiring probe streams.
const matrixSentinel = "matrix-sentinel"

// requireNativeWiring is the behavioural shape witness of every FS row: the
// native merge drops a broker payload other than an alert, the legacy tail
// ships any seq past its high-water mark.
func requireNativeWiring(t *testing.T, fx *matrixServer) {
	t.Helper()
	if fx.wiringProbe(t) {
		t.Fatal("wiring probe: a BROKER-PROBE payload reached the socket — the matrix fixture serves the legacy stream path, not the native merge")
	}
}

// wiringProbe streams a dedicated running sentinel run on its own socket,
// publishes a non-alert payload far past any cursor then an unstamped alert
// fence on the run's broker, reads to the fence (the broker subscription is
// FIFO) and reports whether the payload reached the socket.
func (fx *matrixServer) wiringProbe(t *testing.T) bool {
	t.Helper()
	mustWS(t, seedWSOriginRun(t.Context(), fx.native, matrixSentinel, "sentinel", store.LegacyValueFormat, store.RunStatusRunning, 1))
	c := dialRunWS(t, fx.hs, matrixSentinel)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	snapshotOf(t, readEnvelope(t, c))
	awaitBrokerSubscriber(t, fx.srv, matrixSentinel)
	b := fx.srv.runs.Broker()
	b.Publish(store.Event{Seq: 1 << 40, RunID: matrixSentinel, Type: store.EventNodeStarted, NodeID: "BROKER-PROBE"})
	b.Publish(store.Event{RunID: matrixSentinel, Type: store.EventAlert, NodeID: "WIRING-FENCE"})
	sawProbe := false
	for fenced := false; !fenced; {
		for _, ev := range decodeEventEnvelope(t, readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)) {
			switch ev.NodeID {
			case "BROKER-PROBE":
				sawProbe = true
			case "WIRING-FENCE":
				fenced = true
			}
		}
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "probe-done"})
	readUntilAck(t, c, "probe-done")
	_ = c.Close()
	return sawProbe
}

// matrixHookSource is the Service's own stream source behind test hooks: a
// one-shot hook inside an event or log subscribe, before the subscription
// reaches the Service, and an optional hold of an event subscription after its
// first relayed batch. It counts the subscriptions that reached it, per run.
type matrixHookSource struct {
	inner runstream.Source

	mu           sync.Mutex
	beforeEvents map[string]func() error
	beforeLogs   map[string]func() error
	holds        map[string]*matrixHold
	allHolds     []*matrixHold
	hookErr      error
	eventSubs    map[string]int
	logSubs      map[string]int
	relayed      map[string]int
	relays       sync.WaitGroup
}

// CheckBoundObservationSource endorses this hook source itself, and only when
// the source it forwards to honors the binding.
func (h *matrixHookSource) CheckBoundObservationSource(actual runstream.Source) error {
	if receiver, ok := actual.(*matrixHookSource); !ok || receiver != h {
		return fmt.Errorf("%w: %T is not this matrix source", store.ErrRunDataCapability, actual)
	}
	return runstream.RequireBoundObservationSource(h.inner)
}

func (h *matrixHookSource) Close() error { return h.inner.Close() }

func (h *matrixHookSource) SubscribeEvents(ctx context.Context, id string, from int64) (runstream.EventSubscription, error) {
	return h.SubscribeEventsCursor(ctx, id, runstream.EventCursor{Seq: from})
}

func (h *matrixHookSource) SubscribeEventsCursor(ctx context.Context, id string, cursor runstream.EventCursor) (runstream.EventSubscription, error) {
	h.mu.Lock()
	if h.eventSubs == nil {
		h.eventSubs = map[string]int{}
	}
	h.eventSubs[id]++
	hook := h.beforeEvents[id]
	delete(h.beforeEvents, id)
	hold := h.holds[id]
	delete(h.holds, id)
	h.mu.Unlock()
	h.runHook(hook)
	sub, err := runstream.SubscribeEventsCursor(ctx, h.inner, id, cursor)
	if err != nil || hold == nil {
		return sub, err
	}
	out := &matrixHeldSubscription{EventPipe: runstream.NewEventPipe(), inner: sub, done: make(chan struct{})}
	h.relays.Add(1)
	go h.relay(id, sub, out, hold)
	return out, nil
}

func (h *matrixHookSource) SubscribeLogs(ctx context.Context, id string, from int64) (runstream.LogSubscription, error) {
	h.mu.Lock()
	if h.logSubs == nil {
		h.logSubs = map[string]int{}
	}
	h.logSubs[id]++
	hook := h.beforeLogs[id]
	delete(h.beforeLogs, id)
	h.mu.Unlock()
	h.runHook(hook)
	return h.inner.SubscribeLogs(ctx, id, from)
}

func (h *matrixHookSource) runHook(hook func() error) {
	if hook == nil {
		return
	}
	if err := hook(); err != nil {
		h.mu.Lock()
		h.hookErr = errors.Join(h.hookErr, err)
		h.mu.Unlock()
	}
}

// beforeNextEvents runs hook once, inside the next event subscribe of id,
// before it reaches the Service: after the snapshot frame was queued.
func (h *matrixHookSource) beforeNextEvents(id string, hook func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.beforeEvents == nil {
		h.beforeEvents = map[string]func() error{}
	}
	h.beforeEvents[id] = hook
}

// beforeNextLogs runs hook once, inside the next log subscribe of id, before
// it reaches the Service.
func (h *matrixHookSource) beforeNextLogs(id string, hook func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.beforeLogs == nil {
		h.beforeLogs = map[string]func() error{}
	}
	h.beforeLogs[id] = hook
}

// holdNextEvents holds the next event subscription of id after its first
// relayed batch, until the hold is released.
func (h *matrixHookSource) holdNextEvents(id string) *matrixHold {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holds == nil {
		h.holds = map[string]*matrixHold{}
	}
	hold := &matrixHold{held: make(chan struct{}), release: make(chan struct{})}
	h.holds[id] = hold
	h.allHolds = append(h.allHolds, hold)
	return hold
}

func (h *matrixHookSource) releaseAll() {
	h.mu.Lock()
	holds := h.allHolds
	h.mu.Unlock()
	for _, hold := range holds {
		hold.Release()
	}
}

func (h *matrixHookSource) failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hookErr
}

func (h *matrixHookSource) eventSubscriptions(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.eventSubs[id]
}

func (h *matrixHookSource) logSubscriptions(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logSubs[id]
}

func (h *matrixHookSource) relayedEvents(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relayed[id]
}

// join waits for every relay goroutine the hooks started.
func (h *matrixHookSource) join(within time.Duration) error {
	done := make(chan struct{})
	go func() {
		h.relays.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(within):
		return fmt.Errorf("relays still running after %v", within)
	}
}

// relay forwards a held subscription: data in order, permanent errors as the
// stream's failure, warnings as warnings; it holds after the first batch
// until the hold is released or the consumer closes.
func (h *matrixHookSource) relay(id string, inner runstream.EventSubscription, out *matrixHeldSubscription, hold *matrixHold) {
	defer h.relays.Done()
	defer close(out.done)
	defer out.Finish(nil)
	events, errs := inner.Events(), inner.Errors()
	first := true
	for {
		select {
		case batch, ok := <-events:
			if !ok {
				// errs is nil once its close was seen: ranging over it would
				// block forever.
				if errs != nil {
					for err := range errs {
						forwardMatrixError(out.EventPipe, err)
					}
				}
				return
			}
			if !out.Ship(context.Background(), batch) {
				return
			}
			h.mu.Lock()
			if h.relayed == nil {
				h.relayed = map[string]int{}
			}
			h.relayed[id] += len(batch)
			h.mu.Unlock()
			if first {
				first = false
				close(hold.held)
				select {
				case <-hold.release:
				case <-out.Done():
					return
				}
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			forwardMatrixError(out.EventPipe, err)
		}
	}
}

func forwardMatrixError(out runstream.EventPipe, err error) {
	if runstream.IsPermanentError(err) {
		out.Fail(err)
		return
	}
	out.Warn(err)
}

// matrixHold is one held event subscription: held closes once the relay
// holds, Release lets it go on.
type matrixHold struct {
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *matrixHold) Release() { h.once.Do(func() { close(h.release) }) }

func (h *matrixHold) await(t *testing.T) {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the held event subscription never relayed its first batch")
	}
}

// matrixHeldSubscription is the consumer's side of a held subscription. Close
// cancels and joins the Service's subscription, then joins the relay, before
// it returns.
type matrixHeldSubscription struct {
	runstream.EventPipe
	inner     runstream.EventSubscription
	done      chan struct{}
	closeOnce sync.Once
}

func (s *matrixHeldSubscription) Close() error {
	s.closeOnce.Do(func() {
		_ = s.EventPipe.Close()
		_ = s.inner.Close()
		<-s.done
	})
	return nil
}

// matrixOrigin and matrixSuccessor name the origin run A and its successor B
// in workflow names, node IDs and log bytes: a frame of B is recognizable on
// the wire.
const (
	matrixOrigin    = "origin-A"
	matrixSuccessor = "successor-B"
)

// matrixFormats are both value formats every replacement row runs under.
var matrixFormats = []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat}

// expectMatrixOriginLost reads to the close frame: a text frame containing
// forbidden — a mark of the successor, or a snapshot frame — fails at once;
// then exactly one error envelope with wantCode before a scope_lost close.
func expectMatrixOriginLost(t *testing.T, c *websocket.Conn, wantCode, forbidden string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var texts []string
	for {
		kind, raw, err := c.ReadMessage()
		if err == nil {
			if kind == websocket.TextMessage {
				if strings.Contains(string(raw), forbidden) {
					t.Fatalf("a frame of another origin than the upgrade's reached the socket (%q): %s", forbidden, raw)
				}
				texts = append(texts, string(raw))
			}
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("read until the scope_lost close: %v (text frames seen: %v)", err, texts)
		}
		if len(texts) != 1 {
			t.Fatalf("frames before the close = %v, want exactly the %s error", texts, wantCode)
		}
		var env runWSEnvelope
		mustWS(t, json.Unmarshal([]byte(texts[0]), &env))
		if env.Type != wsTypeError {
			t.Fatalf("frame before the close = %s, want the %s error", texts[0], wantCode)
		}
		if p := errorCodeOf(t, env); p.Code != wantCode {
			t.Fatalf("error = %+v, want code %s", p, wantCode)
		}
		if ce.Code != websocket.CloseNormalClosure || !strings.HasPrefix(ce.Text, wsCloseScopeLost+": ") {
			t.Fatalf("close = %d %q, want %d %s", ce.Code, ce.Text, websocket.CloseNormalClosure, wsCloseScopeLost)
		}
		return
	}
}

// retenantMatrixRun sets tenant_id in id's run.json, every other member kept,
// and asserts the store's canonical parsers read header h and the tenant back.
func retenantMatrixRun(t *testing.T, fx *matrixServer, id, tenant string, h store.RunDataHeader) {
	t.Helper()
	path := filepath.Join(fx.srv.cfg.StoreDir, "runs", id, "run.json")
	b, err := os.ReadFile(path)
	mustWS(t, err)
	var raw map[string]json.RawMessage
	mustWS(t, json.Unmarshal(b, &raw))
	raw["tenant_id"], err = json.Marshal(tenant)
	mustWS(t, err)
	b, err = json.Marshal(raw)
	mustWS(t, err)
	mustWS(t, os.WriteFile(path, b, 0o600))
	sel, observed, err := fx.native.OpenFilesystemStream(t.Context(), id, store.FilesystemStreamOpenOptions{})
	mustWS(t, err)
	defer func() { _ = sel.Close() }()
	o, ok, err := store.CaptureEventStreamObservation(sel)
	if err != nil || !ok || !observed || o.Header() != h || o.TenantID() != tenant {
		t.Fatalf("fixture: canonical observation after the rewrite = %+v tenant %q (%t %t %v); want header %+v tenant %q", o.Header(), o.TenantID(), ok, observed, err, h, tenant)
	}
}

// matrixAlert is an ephemeral alert frame of runID named nodeID; stamped
// with obs when obs was observed.
func matrixAlert(t *testing.T, runID, nodeID string, obs store.RunObservation) store.Event {
	t.Helper()
	ev := store.Event{RunID: runID, Type: store.EventAlert, NodeID: nodeID}
	if !obs.Observed() {
		return ev
	}
	stamped, err := store.WithEventObservation(ev, obs)
	mustWS(t, err)
	return stamped
}

// matrixSuccessorObservation is obs's header under another birth: the stamp
// of a run that took the ID after obs.
func matrixSuccessorObservation(t *testing.T, obs store.RunObservation) store.RunObservation {
	t.Helper()
	h := obs.Header()
	h.BirthID = "successor-of-" + h.BirthID
	succ, err := store.CaptureRunObservation(h, obs.TenantID(), true)
	mustWS(t, err)
	return succ
}
