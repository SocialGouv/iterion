package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// dialRunWS returns a connected websocket client to /api/ws/runs/{id}.
func dialRunWS(t *testing.T, hs *httptest.Server, runID string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/ws/runs/" + runID
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readEnvelope reads one envelope, optionally filtering out unwanted
// types so tests don't have to handle ack/error noise.
func readEnvelope(t *testing.T, c *websocket.Conn, allowedTypes ...string) runWSEnvelope {
	t.Helper()
	return readEnvelopeWithin(t, c, 2*time.Second, allowedTypes...)
}

func writeJSONMessage(t *testing.T, c *websocket.Conn, env runWSEnvelope) {
	t.Helper()
	if err := c.WriteJSON(env); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

// awaitBrokerSubscriber blocks until the server has actually subscribed to the
// broker for runID.
//
// The snapshot frame is NOT that signal. handleSubscribe sends the snapshot
// first and calls SubscribeEvents after, so a test that reads the snapshot and
// publishes immediately is racing the server: when it loses, the event is
// dropped on the floor and the test hangs on a read that never completes.
// That is the whole mechanism behind the intermittent
// TestRunsWS_AlertEventBypassesSnapshotDedup failures on CI.
//
// SubscribeEvents registers with the broker before it does anything expensive,
// and the per-subscriber channel is buffered, so a publish after this returns
// is delivered. This waits for the state the test depends on rather than
// hoping a sleep or a wider read deadline covers it.
//
// The same ordering is a real (narrow) product gap for UNPERSISTED events: a
// client is told it is live-tailing before the tail exists, and an alert event
// (Seq=0, never written to disk) published in that window cannot be replayed
// to it. Tracked on the board; closing it means separating the cheap broker
// subscription from the event-source setup, because simply subscribing before
// the snapshot makes first paint wait for a full replay pass.
func awaitBrokerSubscriber(t *testing.T, srv *Server, runID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if srv.runs.Broker().SubscriberCount(runID) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no broker subscriber for %s after 5s — handleSubscribe never reached SubscribeEvents", runID)
}

func TestRunsWS_SubscribeReceivesSnapshot(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf", store.RunStatusFinished)

	c := dialRunWS(t, hs, "run-1")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})

	env := readEnvelope(t, c, wsTypeSnapshot)
	if env.Type != wsTypeSnapshot {
		t.Fatalf("Type = %q, want snapshot", env.Type)
	}
	var snap runview.RunSnapshot
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Run.ID != "run-1" {
		t.Errorf("Run.ID = %q, want run-1", snap.Run.ID)
	}
	if len(snap.Executions) == 0 {
		t.Errorf("Executions = 0, want > 0 (seeded events should produce executions)")
	}
}

// TestRunsWS_PingElicitsPong guards the application-level heartbeat the
// client's dead-socket watchdog depends on: a client `ping` envelope must
// come back as a `pong`. The browser's automatic control-frame pong is
// invisible to JS, so this JSON-layer reply is the only liveness signal
// the SPA can observe on an idle-but-alive run.
func TestRunsWS_PingElicitsPong(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-ping", "wf", store.RunStatusRunning)

	c := dialRunWS(t, hs, "run-ping")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypePing, AckID: "hb-1"})

	env := readEnvelope(t, c, wsTypePong)
	if env.Type != wsTypePong {
		t.Fatalf("Type = %q, want pong", env.Type)
	}
	if env.AckID != "hb-1" {
		t.Errorf("AckID = %q, want hb-1 (echoed)", env.AckID)
	}
}

// TestRunsWS_TerminatedRunEmitsEventTerminated guards the event-stream
// terminal signal for an external/dispatcher run (a run not produced in
// this process — seedRun persists it without a manager launch). Such a
// run never gets a broker CloseRun, so before the fix the live tail
// blocked forever and the WS never emitted `terminated`, leaving the
// client stuck on "running". Subscribing to an already-finished run must
// now close the event stream promptly.
func TestRunsWS_TerminatedRunEmitsEventTerminated(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-term", "wf", store.RunStatusFinished)

	c := dialRunWS(t, hs, "run-term")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	// The finished run is not Active → the svcSource terminal pre-check
	// fires immediately and ends the stream → terminated.
	env := readEnvelopeWithin(t, c, 6*time.Second, wsTypeEvent, wsTypeEventBatch, wsTypeTerminated)
	if env.Type != wsTypeTerminated {
		t.Fatalf("Type = %q, want terminated", env.Type)
	}
}

func TestRunsWS_LiveEventReachesSubscriber(t *testing.T) {
	srv, hs := newTestServer(t)
	// Create the run with an empty event stream so the snapshot is
	// trivial; we'll publish events after subscribe.
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "run-live", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	c := dialRunWS(t, hs, "run-live")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	_ = readEnvelope(t, c, wsTypeSnapshot)
	awaitBrokerSubscriber(t, srv, "run-live")

	// Publish an event through the broker — same path the engine uses.
	srv.runs.Broker().Publish(store.Event{
		Seq:    0,
		Type:   store.EventNodeStarted,
		RunID:  "run-live",
		NodeID: "analyze",
		Data:   map[string]any{"kind": "agent"},
	})

	env := readEnvelope(t, c, wsTypeEvent)
	var ev store.Event
	if err := json.Unmarshal(env.Payload, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.NodeID != "analyze" {
		t.Errorf("NodeID = %q, want analyze", ev.NodeID)
	}
}

// TestRunsWS_AlertEventBypassesSnapshotDedup is the regression guard for
// the run-health alert delivery path. Alert events (store.EventAlert) are
// published straight to the broker with Seq=0 and are never persisted, so
// the live-tail snapshot dedup guard (ev.Seq <= snapshotSeq) would drop
// them once the run has emitted any real event (snapshotSeq past 0) —
// silently breaking the browser toast + notification dot. The server must
// special-case EventAlert and forward it regardless of seq.
func TestRunsWS_AlertEventBypassesSnapshotDedup(t *testing.T) {
	srv, hs := newTestServer(t)
	// A running run with several persisted events so the subscribe-time
	// snapshot's LastSeq is well past 0 (the realistic case: an alert
	// only fires minutes into a run that has emitted many events).
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	const runID = "run-alert"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, evt := range []store.Event{
		{Type: store.EventRunStarted, RunID: runID},
		{Type: store.EventNodeStarted, RunID: runID, NodeID: "analyze"},
		{Type: store.EventNodeFinished, RunID: runID, NodeID: "analyze"},
	} {
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	c := dialRunWS(t, hs, runID)
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	snapEnv := readEnvelope(t, c, wsTypeSnapshot)
	var snap runview.RunSnapshot
	if err := json.Unmarshal(snapEnv.Payload, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.LastSeq <= 0 {
		t.Fatalf("snapshot LastSeq = %d, want > 0 (test needs snapshotSeq past 0 to exercise the guard)", snap.LastSeq)
	}
	awaitBrokerSubscriber(t, srv, runID)

	// Publish an in-process alert event the way pkg/alert's browser sink
	// does: straight to the broker, unpersisted, with Seq=0.
	srv.runs.Broker().Publish(store.Event{
		Seq:    0,
		Type:   store.EventAlert,
		RunID:  runID,
		NodeID: "analyze",
		Data: map[string]any{
			"kind":   "budget_warning",
			"title":  "Budget warning: wf",
			"reason": "tokens budget at 82%",
		},
	})

	env := readEnvelope(t, c, wsTypeEvent)
	var ev store.Event
	if err := json.Unmarshal(env.Payload, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.Type != store.EventAlert {
		t.Fatalf("event Type = %q, want %q (alert must bypass the snapshot dedup guard)", ev.Type, store.EventAlert)
	}
	if ev.NodeID != "analyze" {
		t.Errorf("NodeID = %q, want analyze", ev.NodeID)
	}
}

func TestRunsWS_FromSeqReplaysHistorical(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-replay", "wf", store.RunStatusFinished)
	// seedRun appends 3 events at seq 0,1,2.

	c := dialRunWS(t, hs, "run-replay")
	// Ask to replay starting at seq 1 — should see seq 1 and 2
	// (seedRun's middle and final events) replayed via the stream.
	// Lazy mode is the new default; opt back in with replay_history.
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribe,
		Payload: json.RawMessage(`{"from_seq":1,"replay_history":true}`),
	})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	got := []int64{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(got) < 2 {
		env := readEnvelope(t, c, wsTypeEvent, wsTypeEventBatch, wsTypeTerminated)
		if env.Type == wsTypeTerminated {
			break
		}
		for _, ev := range decodeEventEnvelope(t, env) {
			got = append(got, ev.Seq)
		}
	}
	if len(got) < 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("replayed seqs = %v, want [1 2]", got)
	}
}

// decodeEventEnvelope normalises a server→client event-bearing envelope
// (wsTypeEvent for a single event or wsTypeEventBatch for an array)
// into a flat slice so tests don't have to fork on payload shape.
func decodeEventEnvelope(t *testing.T, env runWSEnvelope) []store.Event {
	t.Helper()
	switch env.Type {
	case wsTypeEvent:
		var ev store.Event
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		return []store.Event{ev}
	case wsTypeEventBatch:
		var evs []store.Event
		if err := json.Unmarshal(env.Payload, &evs); err != nil {
			t.Fatalf("decode event_batch: %v", err)
		}
		return evs
	default:
		t.Fatalf("decodeEventEnvelope: unexpected type %q", env.Type)
		return nil
	}
}

func TestRunsWS_ReplayPaginatesPastMaxEventsPerPage(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	const runID = "run-big-replay"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Seed MaxEventsPerPage+50 events so a single LoadEvents page can't
	// cover the whole replay window. The pre-pagination implementation
	// silently dropped the tail past the cap, including any terminal
	// run_failed/run_finished — leaving the studio's pill stuck on
	// whatever pre-terminal status the last replayed event implied.
	n := runview.MaxEventsPerPage + 50
	for i := 0; i < n; i++ {
		evt := store.Event{Type: store.EventNodeStarted, RunID: runID, NodeID: "x"}
		if _, err := st.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}
	if err := st.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	c := dialRunWS(t, hs, runID)
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribe,
		Payload: json.RawMessage(`{"replay_history":true}`),
	})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	received := 0
	var lastSeq int64 = -1
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && received < n {
		env := readEnvelope(t, c, wsTypeEvent, wsTypeEventBatch, wsTypeTerminated)
		if env.Type == wsTypeTerminated {
			break
		}
		for _, ev := range decodeEventEnvelope(t, env) {
			received++
			lastSeq = ev.Seq
		}
	}
	if received != n {
		t.Errorf("received = %d events, want %d (replay must paginate past MaxEventsPerPage=%d)",
			received, n, runview.MaxEventsPerPage)
	}
	if want := int64(n - 1); lastSeq != want {
		t.Errorf("lastSeq = %d, want %d (terminal event must reach the client)", lastSeq, want)
	}
}

// TestRunsWS_ReplayUsesEventBatch asserts the replay path emits a
// wsTypeEventBatch envelope (the bulk shape) rather than N individual
// wsTypeEvent envelopes. Catches accidental regressions to the
// per-event send pattern, which would re-introduce O(events) WS
// frames + marshal overhead on every reconnect.
func TestRunsWS_ReplayUsesEventBatch(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-batch", "wf", store.RunStatusFinished)
	// seedRun appends 3 events.

	c := dialRunWS(t, hs, "run-batch")
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribe,
		Payload: json.RawMessage(`{"replay_history":true}`),
	})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	env := readEnvelope(t, c, wsTypeEvent, wsTypeEventBatch, wsTypeTerminated)
	if env.Type != wsTypeEventBatch {
		t.Fatalf("Type = %q, want %q (replay must batch events)", env.Type, wsTypeEventBatch)
	}
	evs := decodeEventEnvelope(t, env)
	if len(evs) != 3 {
		t.Errorf("batch length = %d, want 3", len(evs))
	}
}

// TestRunsWS_LazyModeSkipsHistoricalReplay asserts the default subscribe
// (no replay_history flag, or replay_history:false) only sends the
// snapshot — no event envelopes for events already persisted on disk.
// The frontend pulls history on demand via the REST /events endpoint.
func TestRunsWS_LazyModeSkipsHistoricalReplay(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-lazy", "wf", store.RunStatusFinished)
	// seedRun appends 3 events at seq 0,1,2.

	c := dialRunWS(t, hs, "run-lazy")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	// Try to read the next envelope with a short timeout. If lazy mode
	// works, no event/event_batch envelope should arrive — the read
	// either times out (broker quiet on a finished run) or returns
	// terminated. Either outcome is fine; an event envelope means the
	// replay path leaked.
	_ = c.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	_, raw, err := c.ReadMessage()
	if err != nil {
		// Timeout or close — both are acceptable in lazy mode.
		return
	}
	var env runWSEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type == wsTypeTerminated {
		return
	}
	t.Fatalf("lazy mode leaked %q envelope; expected only snapshot then quiet/terminated", env.Type)
}

func TestRunsWS_AckOnUnsubscribe(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf", store.RunStatusFinished)

	c := dialRunWS(t, hs, "run-1")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u1"})
	env := readEnvelope(t, c, wsTypeAck)
	if env.AckID != "u1" {
		t.Errorf("AckID = %q, want u1", env.AckID)
	}
}

func TestRunsWS_UnknownTypeProducesError(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-1", "wf", store.RunStatusFinished)

	c := dialRunWS(t, hs, "run-1")
	writeJSONMessage(t, c, runWSEnvelope{Type: "frobnicate", AckID: "x1"})

	env := readEnvelope(t, c, wsTypeError)
	var p wsErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("decode error payload: %v", err)
	}
	if p.Code != "unknown_type" {
		t.Errorf("Code = %q, want unknown_type", p.Code)
	}
	if env.AckID != "x1" {
		t.Errorf("AckID = %q, want x1", env.AckID)
	}
}
