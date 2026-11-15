package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// wsOriginStore is the native filesystem store with race hooks: each hook
// runs once inside the named read, then the read is forwarded unchanged with
// the caller's context. The write probes only record whether a mutating write
// carried a run binding. The identity policy under test stays in the native
// store, the stream source and the socket.
type wsOriginStore struct {
	*store.FilesystemRunStore
	mu             sync.Mutex
	beforeEvents   func() error
	beforeLogRange func() error
	eventsHit      bool
	logRangeHit    bool
	hookErr        error
	writes         atomic.Int32
	boundWrites    atomic.Int32
}

func (s *wsOriginStore) runHook(hook *func() error, hit *bool) {
	s.mu.Lock()
	h := *hook
	run := h != nil && !*hit
	*hit = true
	s.mu.Unlock()
	if !run {
		return
	}
	if err := h(); err != nil {
		s.mu.Lock()
		s.hookErr = errors.Join(s.hookErr, err)
		s.mu.Unlock()
	}
}

func (s *wsOriginStore) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hookErr
}

func (s *wsOriginStore) LoadEventsRange(ctx context.Context, id string, from, to int64, limit int) ([]*store.Event, error) {
	s.runHook(&s.beforeEvents, &s.eventsHit)
	return s.FilesystemRunStore.LoadEventsRange(ctx, id, from, to, limit)
}

func (s *wsOriginStore) ReadRunLogRange(ctx context.Context, id string, from, until int64) ([]byte, error) {
	s.runHook(&s.beforeLogRange, &s.logRangeHit)
	return s.FilesystemRunStore.ReadRunLogRange(ctx, id, from, until)
}

func (s *wsOriginStore) observeWrite(ctx context.Context) {
	s.writes.Add(1)
	if _, bound := store.RunDataBindingFrom(ctx); bound {
		s.boundWrites.Add(1)
	}
}

func (s *wsOriginStore) AppendQueuedMessage(ctx context.Context, id string, msg store.QueuedUserMessage) error {
	s.observeWrite(ctx)
	return s.FilesystemRunStore.AppendQueuedMessage(ctx, id, msg)
}

func (s *wsOriginStore) UpdateRunOutcome(ctx context.Context, id string, status store.RunStatus, runErr string, meta store.RunOutcomeMeta, expectedFrom []store.RunStatus) (bool, error) {
	s.observeWrite(ctx)
	return s.FilesystemRunStore.UpdateRunOutcome(ctx, id, status, runErr, meta, expectedFrom)
}

// originWSFixture serves a real runview.Service over the wrapped native store
// in the server's own store directory, behind the fixture's joined streams.
type originWSFixture struct {
	srv     *Server
	hs      *httptest.Server
	native  *store.FilesystemRunStore
	store   *wsOriginStore
	streams *testOwnedRunStreams
}

func newOriginWSFixture(t *testing.T) *originWSFixture {
	t.Helper()
	srv, hs := newTestServer(t)
	native, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &wsOriginStore{FilesystemRunStore: native}
	srv.runs.Stop(t.Context())
	svc := newTestRunviewService(t, srv.cfg.StoreDir, runview.WithRecoveryPassive(), runview.WithStore(wrapped))
	streams := &testOwnedRunStreams{Source: svc.StreamSource()}
	runview.WithStreamSource(streams)(svc)
	t.Cleanup(func() {
		if err := streams.Close(); err != nil {
			t.Errorf("join run streams: %v", err)
		}
	})
	srv.runs = svc
	return &originWSFixture{srv: srv, hs: hs, native: native, store: wrapped, streams: streams}
}

func (f *originWSFixture) eventSubscriptions() int {
	f.streams.mu.Lock()
	defer f.streams.mu.Unlock()
	return len(f.streams.events)
}

func (f *originWSFixture) logSubscriptions() int {
	f.streams.mu.Lock()
	defer f.streams.mu.Unlock()
	return len(f.streams.logs)
}

// seedWSOriginRun creates id with marker as workflow name and node ID of its
// n events, then moves it to status.
func seedWSOriginRun(ctx context.Context, st *store.FilesystemRunStore, id, marker string, f store.ValueFormat, status store.RunStatus, n int) error {
	r, err := st.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, WorkflowName: marker, Format: f})
	if err != nil {
		return err
	}
	bound, err := store.BindRunValueFormat(ctx, r)
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if _, err := st.AppendEvent(bound, id, store.Event{Type: store.EventNodeStarted, NodeID: marker}); err != nil {
			return err
		}
	}
	if status != "" && status != r.Status {
		return st.UpdateRunStatus(ctx, id, status, "")
	}
	return nil
}

// replaceWSOriginRun ends, deletes and prunes id, then creates its successor
// under the same ID — the only way a new birth takes an existing ID.
func replaceWSOriginRun(ctx context.Context, st *store.FilesystemRunStore, id, marker string, f store.ValueFormat, status store.RunStatus, n int) error {
	r, err := st.LoadRun(ctx, id)
	if err != nil {
		return err
	}
	if !r.Status.IsTerminal() {
		if err := st.UpdateRunStatus(ctx, id, store.RunStatusFinished, ""); err != nil {
			return err
		}
	}
	if err := st.DeleteRun(ctx, id); err != nil {
		return err
	}
	if _, err := st.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	return seedWSOriginRun(ctx, st, id, marker, f, status, n)
}

func mustWS(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// readUntilAck reads envelopes up to the ack carrying ackID and returns the
// ones before it.
func readUntilAck(t *testing.T, c *websocket.Conn, ackID string) []runWSEnvelope {
	t.Helper()
	var seen []runWSEnvelope
	for {
		env := readEnvelopeWithin(t, c, 5*time.Second)
		if env.Type == wsTypeAck && env.AckID == ackID {
			return seen
		}
		seen = append(seen, env)
	}
}

// expectOriginLostClose reads to the close frame and requires exactly one
// error envelope with wantCode before a scope_lost close: nothing of a
// successor, no clean completion.
func expectOriginLostClose(t *testing.T, c *websocket.Conn, wantCode string) {
	t.Helper()
	ce, texts := awaitWSClose(t, c, 10*time.Second)
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
}

func snapshotOf(t *testing.T, env runWSEnvelope) runview.RunSnapshot {
	t.Helper()
	if env.Type != wsTypeSnapshot {
		t.Fatalf("envelope %s %s, want a snapshot", env.Type, env.Payload)
	}
	var snap runview.RunSnapshot
	mustWS(t, json.Unmarshal(env.Payload, &snap))
	return snap
}

// T1: a resubscription on the same socket stays on the run its upgrade
// authorized. A run deleted and recreated under the same ID between two
// subscriptions is refused — snapshot_failed, then scope_lost — never served
// as the successor's snapshot, and no new subscription reaches the source.
func TestRunWSOriginResubscribeRefusesSuccessor(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			fx := newOriginWSFixture(t)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", f, store.RunStatusFinished, 1))
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != "A" {
				t.Fatalf("first snapshot names %q", snap.Run.WorkflowName)
			}
			readEnvelopeWithin(t, c, 10*time.Second, wsTypeTerminated)
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
			readUntilAck(t, c, "u1")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", f, store.RunStatusFinished, 1))
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
			expectOriginLostClose(t, c, "snapshot_failed")
			if n := fx.eventSubscriptions(); n != 1 {
				t.Fatalf("event subscriptions = %d, want only the first", n)
			}
		})
	}
}

// T5: legitimate writes keep the origin. A resubscription after a metadata
// save, a pause and resume (each status change rotates the execution epoch)
// and terminal transitions still serves the same run.
func TestRunWSOriginSurvivesLegitimateWrites(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			fx := newOriginWSFixture(t)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", f, store.RunStatusRunning, 1))
			before, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			c := dialRunWS(t, fx.hs, "r")
			resubscribe := func(step string) {
				t.Helper()
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s-" + step})
				if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.ID != "r" || snap.Run.WorkflowName != "A" {
					t.Fatalf("%s: snapshot %+v", step, snap.Run)
				}
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u-" + step})
				readUntilAck(t, c, "u-"+step)
			}
			resubscribe("start")
			// The writers carry the run's own format intent, as the engine's do.
			write, err := store.BindRunValueFormat(ctx, before)
			mustWS(t, err)
			r, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			r.Name = "renamed"
			mustWS(t, fx.native.SaveRun(write, r))
			resubscribe("save")
			mustWS(t, fx.native.PauseRun(write, "r", &store.Checkpoint{}))
			resubscribe("pause")
			mustWS(t, fx.native.UpdateRunStatus(write, "r", store.RunStatusRunning, ""))
			resubscribe("resume")
			after, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			if after.ExecutionEpoch == before.ExecutionEpoch || after.BirthID != before.BirthID {
				t.Fatalf("fixture did not rotate the epoch under one birth: %q→%q, birth %q→%q", before.ExecutionEpoch, after.ExecutionEpoch, before.BirthID, after.BirthID)
			}
			mustWS(t, fx.native.UpdateRunStatus(write, "r", store.RunStatusFinished, ""))
			resubscribe("finished")
		})
	}
}

// T5: a historical run whose header carries no birth still streams: its
// origin is the empty-birth header, compared like any other.
func TestRunWSOriginEmptyBirthRun(t *testing.T) {
	fx := newOriginWSFixture(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 2))
	path := filepath.Join(fx.srv.cfg.StoreDir, "runs", "r", "run.json")
	raw, err := os.ReadFile(path)
	mustWS(t, err)
	var doc map[string]json.RawMessage
	mustWS(t, json.Unmarshal(raw, &doc))
	delete(doc, "birth_id")
	raw, err = json.Marshal(doc)
	mustWS(t, err)
	mustWS(t, os.WriteFile(path, raw, 0o600))
	if r, err := fx.native.LoadRun(ctx, "r"); err != nil || r.BirthID != "" {
		t.Fatalf("fixture is not an empty-birth run: %+v %v", r, err)
	}
	c := dialRunWS(t, fx.hs, "r")
	for i := 0; i < 2; i++ {
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: json.RawMessage(`{"replay_history":true}`)})
		snapshotOf(t, readEnvelope(t, c))
		got := 0
		for got < 2 {
			got += len(decodeEventEnvelope(t, readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)))
		}
		readEnvelopeWithin(t, c, 10*time.Second, wsTypeTerminated)
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u"})
		readUntilAck(t, c, "u")
	}
}

// T5: an exact run's number outside float64 range reaches the client through
// a bound subscription with its literal intact.
func TestRunWSOriginExactNumberThroughStream(t *testing.T) {
	fx := newOriginWSFixture(t)
	ctx := t.Context()
	r, err := fx.native.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", WorkflowName: "A", Format: store.ExactValueFormat})
	mustWS(t, err)
	bound, err := store.BindRunValueFormat(ctx, r)
	mustWS(t, err)
	_, err = fx.native.AppendEvent(bound, "r", store.Event{Type: store.EventNodeStarted, NodeID: "n", Data: map[string]any{"huge": json.Number("1e400")}})
	mustWS(t, err)
	mustWS(t, fx.native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""))
	c := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: json.RawMessage(`{"replay_history":true}`)})
	snapshotOf(t, readEnvelope(t, c))
	env := readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)
	if !strings.Contains(string(env.Payload), `"huge":1e400`) {
		t.Fatalf("exact literal lost: %s", env.Payload)
	}
}

// T2: the replay of a bound subscription reads the run its socket
// authorized. A successor recreated under the same ID after the snapshot is
// refused — event_stream_failed, then scope_lost — and none of its events
// reach the client.
func TestRunWSOriginReplayRefusesSuccessor(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			fx := newOriginWSFixture(t)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", f, store.RunStatusFinished, 2))
			fx.store.beforeEvents = func() error {
				return replaceWSOriginRun(context.Background(), fx.native, "r", "B", f, store.RunStatusFinished, 3)
			}
			c := dialRunWS(t, fx.hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: json.RawMessage(`{"replay_history":true}`)})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != "A" {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			expectOriginLostClose(t, c, "event_stream_failed")
			mustWS(t, fx.store.failure())
			if !fx.store.eventsHit {
				t.Fatal("the replay barrier was not reached")
			}
		})
	}
}

// T3: a log subscription is admitted against the socket's origin before any
// buffer or tailer is looked up. The successor's bytes never reach the old
// socket — neither through its own tailer nor through the live buffer another
// socket, legitimately opened on the successor, already stands up.
func TestRunWSOriginLogsRefuseSuccessor(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor-buffer=%t", shared), func(t *testing.T) {
			fx := newOriginWSFixture(t)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
			old := dialRunWS(t, fx.hs, "r")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusRunning, 0))
			writeRunLog(t, fx.srv.cfg.StoreDir, "r", "successor bytes\n")
			if shared {
				fresh := dialRunWS(t, fx.hs, "r")
				writeJSONMessage(t, fresh, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
				if got := collectLogText(t, fresh, "", "successor bytes\n", 5*time.Second); got != "successor bytes\n" {
					t.Fatalf("socket opened on the successor read %q", got)
				}
				if fx.srv.runs.GetLogBuffer("r") == nil {
					t.Fatal("fixture: no live buffer for the successor")
				}
			}
			writeJSONMessage(t, old, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l-old"})
			expectOriginLostClose(t, old, "log_stream_failed")
			want := 0
			if shared {
				want = 1
			}
			if n := fx.logSubscriptions(); n != want {
				t.Fatalf("log subscriptions = %d, want %d: the old socket's was admitted", n, want)
			}
		})
	}
}

// T3b: the persisted-log read of a bound subscription is made under its
// origin. A successor recreated between the admission and the range read is
// refused — log_stream_failed, then scope_lost — never a clean log_terminated
// nor the successor's bytes.
func TestRunWSOriginLogRangeRefusesSuccessor(t *testing.T) {
	fx := newOriginWSFixture(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
	writeRunLog(t, fx.srv.cfg.StoreDir, "r", "original bytes\n")
	fx.store.beforeLogRange = func() error {
		if err := replaceWSOriginRun(context.Background(), fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 0); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(fx.srv.cfg.StoreDir, "runs", "r", "run.log"), []byte("successor bytes\n"), 0o600)
	}
	c := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribeLogs, AckID: "l"})
	ce, texts := awaitWSClose(t, c, 10*time.Second)
	mustWS(t, fx.store.failure())
	if !fx.store.logRangeHit {
		t.Fatal("the log range barrier was not reached")
	}
	if len(texts) != 2 {
		t.Fatalf("frames = %v, want the admission ack then the refusal", texts)
	}
	var ack, refusal runWSEnvelope
	mustWS(t, json.Unmarshal([]byte(texts[0]), &ack))
	mustWS(t, json.Unmarshal([]byte(texts[1]), &refusal))
	if ack.Type != wsTypeAck || refusal.Type != wsTypeError || errorCodeOf(t, refusal).Code != "log_stream_failed" {
		t.Fatalf("frames = %v", texts)
	}
	if !strings.HasPrefix(ce.Text, wsCloseScopeLost+": ") {
		t.Fatalf("close = %q", ce.Text)
	}
}

// T6: for a live origin the mutating envelopes behave as before, and their
// actions run under the unbound context — the binding is for reads only, so a
// resume or a child run never inherits it.
func TestRunWSOriginMutationsServeLiveOrigin(t *testing.T) {
	fx := newOriginWSFixture(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusPausedWaitingHuman, 0))
	c := dialRunWS(t, fx.hs, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeQueueMessage, AckID: "q", Payload: json.RawMessage(`{"text":"hello"}`)})
	if env := readEnvelope(t, c); env.Type != wsTypeAck || env.AckID != "q" {
		t.Fatalf("queue_message = %s %s", env.Type, env.Payload)
	}
	msgs, err := fx.native.ListQueuedMessages(ctx, "r")
	mustWS(t, err)
	if len(msgs) != 1 || msgs[0].Text != "hello" {
		t.Fatalf("queued = %+v", msgs)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeCancel, AckID: "c"})
	if env := readEnvelope(t, c); env.Type != wsTypeAck || env.AckID != "c" {
		t.Fatalf("cancel = %s %s", env.Type, env.Payload)
	}
	r, err := fx.native.LoadRun(ctx, "r")
	mustWS(t, err)
	if r.Status != store.RunStatusCancelled {
		t.Fatalf("status = %s, want cancelled", r.Status)
	}
	if fx.store.writes.Load() < 2 {
		t.Fatalf("write probes saw %d writes", fx.store.writes.Load())
	}
	if n := fx.store.boundWrites.Load(); n != 0 {
		t.Fatalf("%d mutating writes ran under the socket's read binding", n)
	}
}

// T7: a mutating envelope of a socket whose origin was replaced is refused
// before it acts — run_origin_lost, then scope_lost — and the successor is
// left untouched.
func TestRunWSOriginMutationRefusedAfterReplacement(t *testing.T) {
	for _, kind := range []string{wsTypeQueueMessage, wsTypeCancel} {
		t.Run(kind, func(t *testing.T) {
			fx := newOriginWSFixture(t)
			ctx := t.Context()
			mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 0))
			c := dialRunWS(t, fx.hs, "r")
			mustWS(t, replaceWSOriginRun(ctx, fx.native, "r", "B", store.LegacyValueFormat, store.RunStatusPausedWaitingHuman, 0))
			env := runWSEnvelope{Type: kind, AckID: "m"}
			if kind == wsTypeQueueMessage {
				env.Payload = json.RawMessage(`{"text":"meant for A"}`)
			}
			writeJSONMessage(t, c, env)
			expectOriginLostClose(t, c, wsErrRunOriginLost)
			b, err := fx.native.LoadRun(ctx, "r")
			mustWS(t, err)
			if b.WorkflowName != "B" || b.Status != store.RunStatusPausedWaitingHuman {
				t.Fatalf("successor changed: %s %s", b.WorkflowName, b.Status)
			}
			msgs, err := fx.native.ListQueuedMessages(ctx, "r")
			mustWS(t, err)
			if len(msgs) != 0 || fx.store.writes.Load() != 0 {
				t.Fatalf("successor received %d messages, %d writes", len(msgs), fx.store.writes.Load())
			}
		})
	}
}

// W2: a deletion is an origin loss too, whether its tombstone still stands
// (ErrRunDeleted) or was pruned (ErrRunNotFound): each surface answers its
// typed error, then the socket closes scope_lost.
func TestRunWSOriginDeletionClosesLikeReplacement(t *testing.T) {
	for _, kind := range []string{wsTypeSubscribe, wsTypeSubscribeLogs, wsTypeQueueMessage} {
		for _, prune := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pruned=%t", kind, prune), func(t *testing.T) {
				fx := newOriginWSFixture(t)
				ctx := t.Context()
				mustWS(t, seedWSOriginRun(ctx, fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
				c := dialRunWS(t, fx.hs, "r")
				mustWS(t, fx.native.DeleteRun(ctx, "r"))
				if prune {
					_, err := fx.native.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour))
					mustWS(t, err)
				}
				env := runWSEnvelope{Type: kind, AckID: "m"}
				want := map[string]string{wsTypeSubscribe: "snapshot_failed", wsTypeSubscribeLogs: "log_stream_failed", wsTypeQueueMessage: wsErrRunOriginLost}[kind]
				if kind == wsTypeQueueMessage {
					env.Payload = json.RawMessage(`{"text":"meant for A"}`)
				}
				writeJSONMessage(t, c, env)
				expectOriginLostClose(t, c, want)
			})
		}
	}
}

// T7 over the credentialed gate: the origin is compared once the credential
// stands and before the team is resolved — the successor's team must not
// answer for an envelope the socket's original run was opened for.
func TestRunWSOriginCredentialedMutationRefusedAfterReplacement(t *testing.T) {
	srv, ts, guard := standingStack(t, store.RunStatusPausedWaitingHuman)
	conn := dialWithTicket(t, srv, ts, "/api/ws/runs/run-1", caller("u-member-a", "tenant-A"))
	native := guard.FilesystemRunStore
	ctx := context.Background()
	mustWS(t, replaceWSOriginRun(ctx, native, "run-1", "B", store.LegacyValueFormat, store.RunStatusPausedWaitingHuman, 0))
	b, err := native.LoadRun(ctx, "run-1")
	mustWS(t, err)
	b.TenantID = "tenant-A"
	mustWS(t, native.SaveRun(ctx, b))
	mustWS(t, conn.WriteJSON(runWSEnvelope{Type: wsTypeCancel, AckID: "c1"}))
	expectOriginLostClose(t, conn, wsErrRunOriginLost)
	if got := runStatusOf(t, srv, "run-1"); got != store.RunStatusPausedWaitingHuman {
		t.Fatalf("successor status = %q, want it untouched", got)
	}
}

// discardingRunSource never sees its subscription context: it cannot honor a
// binding, so a bound connection must refuse it before subscribing.
type discardingRunSource struct{ calls atomic.Int32 }

func (s *discardingRunSource) SubscribeEvents(context.Context, string, int64) (runstream.EventSubscription, error) {
	s.calls.Add(1)
	return runstream.NewEventPipe(), nil
}
func (s *discardingRunSource) SubscribeLogs(context.Context, string, int64) (runstream.LogSubscription, error) {
	s.calls.Add(1)
	return runstream.NewLogPipe(), nil
}
func (s *discardingRunSource) Close() error { return nil }

// F4: a stream source lacking the bound-observation capability is refused
// with a typed error before any snapshot or subscription.
func TestRunWSOriginRefusesSourceWithoutCapability(t *testing.T) {
	fx := newOriginWSFixture(t)
	mustWS(t, seedWSOriginRun(t.Context(), fx.native, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
	src := &discardingRunSource{}
	runview.WithStreamSource(src)(fx.srv.runs)
	c := dialRunWS(t, fx.hs, "r")
	for _, tc := range []struct{ kind, code string }{{wsTypeSubscribe, "event_stream_failed"}, {wsTypeSubscribeLogs, "log_stream_failed"}} {
		writeJSONMessage(t, c, runWSEnvelope{Type: tc.kind, AckID: tc.kind})
		env := readEnvelope(t, c)
		if env.Type != wsTypeError || env.AckID != tc.kind {
			t.Fatalf("%s answered %s %s", tc.kind, env.Type, env.Payload)
		}
		if p := errorCodeOf(t, env); p.Code != tc.code || !strings.Contains(p.Message, "does not honor a bound run observation") {
			t.Fatalf("%s refusal = %+v", tc.kind, p)
		}
	}
	if src.calls.Load() != 0 {
		t.Fatalf("source reached %d times", src.calls.Load())
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypePing, AckID: "p"})
	if env := readEnvelope(t, c); env.Type != wsTypePong {
		t.Fatalf("a capability refusal closed the socket: %s", env.Type)
	}
}

// D2: a cross-store socket binds the run its upgrade read in the foreign
// store; the same delete-and-recreate is refused there too.
func TestRunWSOriginCrossStoreRefusesSuccessor(t *testing.T) {
	_, hs := newTestServer(t)
	xs, xdir := newCrossStore(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, xs, "r", "A", store.LegacyValueFormat, store.RunStatusFinished, 1))
	c := dialRunWSStore(t, hs, "r", xdir)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s1"})
	if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != "A" {
		t.Fatalf("first snapshot names %q", snap.Run.WorkflowName)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
	readUntilAck(t, c, "u1")
	mustWS(t, replaceWSOriginRun(ctx, xs, "r", "B", store.LegacyValueFormat, store.RunStatusFinished, 1))
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s2"})
	expectOriginLostClose(t, c, "snapshot_failed")
}

// D2: a run absent from the foreign store at the upgrade stays unbound, as
// before: one created afterwards is admitted and streamed.
func TestRunWSOriginCrossStoreAbsentRunAdmittedLater(t *testing.T) {
	_, hs := newTestServer(t)
	xs, xdir := newCrossStore(t)
	ctx := t.Context()
	c := dialRunWSStore(t, hs, "late", xdir)
	mustWS(t, seedWSOriginRun(ctx, xs, "late", "L", store.LegacyValueFormat, store.RunStatusFinished, 2))
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: json.RawMessage(`{"replay_history":true}`)})
	if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != "L" {
		t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
	}
	got := 0
	for got < 2 {
		got += len(decodeEventEnvelope(t, readEnvelopeWithin(t, c, 5*time.Second, wsTypeEvent, wsTypeEventBatch)))
	}
	readEnvelopeWithin(t, c, 15*time.Second, wsTypeTerminated)
}

// D2: a cross-store run the foreign store reports deleted at the upgrade is
// an HTTP refusal, not an unbound socket.
func TestRunWSOriginCrossStoreDeletedRunRefusedAtUpgrade(t *testing.T) {
	_, hs := newTestServer(t)
	xs, xdir := newCrossStore(t)
	ctx := t.Context()
	mustWS(t, seedWSOriginRun(ctx, xs, "gone", "G", store.LegacyValueFormat, store.RunStatusFinished, 0))
	mustWS(t, xs.DeleteRun(ctx, "gone"))
	target := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/ws/runs/gone?store=" + url.QueryEscape(xdir)
	conn, resp, err := websocket.DefaultDialer.Dial(target, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusGone {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial a deleted cross-store run: err=%v status=%d, want %d", err, code, http.StatusGone)
	}
}
