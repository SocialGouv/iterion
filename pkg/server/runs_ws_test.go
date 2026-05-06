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
	allowed := map[string]bool{}
	for _, a := range allowedTypes {
		allowed[a] = true
	}
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env runWSEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if len(allowed) == 0 || allowed[env.Type] {
			return env
		}
	}
}

func writeJSONMessage(t *testing.T, c *websocket.Conn, env runWSEnvelope) {
	t.Helper()
	if err := c.WriteJSON(env); err != nil {
		t.Fatalf("write json: %v", err)
	}
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

	// Publish an event through the broker — same path the engine uses.
	srv.runs.Broker().Publish(store.Event{
		Seq:    0,
		Type:   store.EventNodeStarted,
		RunID:  "run-live",
		NodeID: "analyze",
		Data:   map[string]interface{}{"kind": "agent"},
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

func TestRunsWS_FromSeqReplaysHistorical(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "run-replay", "wf", store.RunStatusFinished)
	// seedRun appends 3 events at seq 0,1,2.

	c := dialRunWS(t, hs, "run-replay")
	// Ask to replay starting at seq 1 — should see seq 1 and 2
	// (seedRun's middle and final events) replayed via the stream.
	writeJSONMessage(t, c, runWSEnvelope{
		Type:    wsTypeSubscribe,
		Payload: json.RawMessage(`{"from_seq":1}`),
	})
	_ = readEnvelope(t, c, wsTypeSnapshot)

	got := []int64{}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(got) < 2 {
		env := readEnvelope(t, c, wsTypeEvent, wsTypeTerminated)
		if env.Type == wsTypeTerminated {
			break
		}
		var ev store.Event
		if err := json.Unmarshal(env.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		got = append(got, ev.Seq)
	}
	if len(got) < 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("replayed seqs = %v, want [1 2]", got)
	}
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
