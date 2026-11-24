package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SocialGouv/iterion/pkg/store"
)

// wsWireEvent is the part of an event frame these tests compare as bytes.
type wsWireEvent struct {
	NodeID string          `json:"node_id"`
	Data   json.RawMessage `json:"data"`
}

// wireEventsOf decodes the events an event or event_batch envelope carries,
// keeping each event's data as the bytes the server wrote.
func wireEventsOf(t *testing.T, env runWSEnvelope) []wsWireEvent {
	t.Helper()
	switch env.Type {
	case wsTypeEvent:
		var ev wsWireEvent
		mustWS(t, json.Unmarshal(env.Payload, &ev))
		return []wsWireEvent{ev}
	case wsTypeEventBatch:
		var evs []wsWireEvent
		mustWS(t, json.Unmarshal(env.Payload, &evs))
		return evs
	}
	return nil
}

// awaitWireEvent reads frames until the event of nodeID arrives and returns
// its data bytes.
func awaitWireEvent(t *testing.T, c *websocket.Conn, nodeID string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		_ = c.SetReadDeadline(deadline)
		_, raw, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("event %s not delivered within %v: %v", nodeID, within, err)
		}
		var env runWSEnvelope
		mustWS(t, json.Unmarshal(raw, &env))
		if env.Type == wsTypeTerminated {
			t.Fatalf("stream terminated before event %s", nodeID)
		}
		for _, ev := range wireEventsOf(t, env) {
			if ev.NodeID == nodeID {
				return string(ev.Data)
			}
		}
	}
}

// T1-WS: a successor's event published on the run ID never reaches a socket
// bound to its predecessor: the stream fails with the identity refusal and
// the socket closes as scope_lost, with no successor frame before it.
func TestRunWSLiveSuccessorPublicationRefused(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		t.Run(fmt.Sprintf("f%d", f), func(t *testing.T) {
			srv, hs := newTestServer(t)
			ctx := t.Context()
			st, err := store.New(srv.cfg.StoreDir)
			mustWS(t, err)
			mustWS(t, seedWSOriginRun(ctx, st, "r", "A", f, store.RunStatusRunning, 1))
			c := dialRunWS(t, hs, "r")
			writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
			if snap := snapshotOf(t, readEnvelope(t, c)); snap.Run.WorkflowName != "A" {
				t.Fatalf("snapshot names %q", snap.Run.WorkflowName)
			}
			awaitBrokerSubscriber(t, srv, "r")
			// Reach the live tail first: two events appended then published
			// (the in-process order), each awaited on the socket.
			r, err := st.LoadRun(ctx, "r")
			mustWS(t, err)
			vctx, err := store.BindRunValueFormat(ctx, r)
			mustWS(t, err)
			for _, node := range []string{"A1", "A2"} {
				persisted, err := st.AppendEvent(vctx, "r", store.Event{Type: store.EventNodeFinished, NodeID: node})
				mustWS(t, err)
				srv.runs.Broker().Publish(*persisted)
				awaitWireEvent(t, c, node, 5*time.Second)
			}
			mustWS(t, os.Rename(filepath.Join(srv.cfg.StoreDir, "runs", "r"), filepath.Join(srv.cfg.StoreDir, "aside-r")))
			mustWS(t, seedWSOriginRun(ctx, st, "r", "B", f, store.RunStatusRunning, 4))
			successor, err := st.LoadEvents(ctx, "r")
			mustWS(t, err)
			if len(successor) != 4 {
				t.Fatalf("successor events = %+v", successor)
			}
			// Seq 3 is past the socket's cursor: only the run's identity can refuse it.
			srv.runs.Broker().Publish(*successor[3])
			ce, texts := awaitWSClose(t, c, 10*time.Second)
			for _, text := range texts {
				if strings.Contains(text, `"node_id":"B"`) {
					t.Fatalf("successor frame delivered: %s", text)
				}
			}
			if len(texts) != 1 {
				t.Fatalf("frames before the close = %v, want exactly the event_stream_failed error", texts)
			}
			var env runWSEnvelope
			mustWS(t, json.Unmarshal([]byte(texts[0]), &env))
			if env.Type != wsTypeError || errorCodeOf(t, env).Code != "event_stream_failed" {
				t.Fatalf("frame before the close = %s, want event_stream_failed", texts[0])
			}
			if ce.Code != websocket.CloseNormalClosure || !strings.HasPrefix(ce.Text, wsCloseScopeLost+": ") {
				t.Fatalf("close = %d %q, want scope_lost", ce.Code, ce.Text)
			}
		})
	}
}

// wsExactValues are numbers a float64 cannot carry: one past 2^53, and one
// past the float64 range.
var wsExactValues = []struct{ node, literal string }{
	{"int53", "9007199254740993"},
	{"e400", "1e400"},
}

// seedWSExactRun creates a running run of format f with one plain event.
func seedWSExactRun(t *testing.T, st *store.FilesystemRunStore, f store.ValueFormat) context.Context {
	t.Helper()
	r, err := st.CreateRunWithValueFormat(t.Context(), store.RunCreateSpec{ID: "r", WorkflowName: "wf", Format: f})
	mustWS(t, err)
	ctx, err := store.BindRunValueFormat(t.Context(), r)
	mustWS(t, err)
	_, err = st.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeStarted, NodeID: "seed"})
	mustWS(t, err)
	return ctx
}

func appendWSExactEvent(t *testing.T, st *store.FilesystemRunStore, ctx context.Context, node, literal string) *store.Event {
	t.Helper()
	persisted, err := st.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeFinished, NodeID: node, Data: map[string]any{"v": json.Number(literal)}})
	mustWS(t, err)
	return persisted
}

// D5: numbers in event data reach the socket exactly as written, for both
// value formats: in replay, live from a run another process writes (appended
// only), and live from a run this process produces (appended, then its
// persisted copy published, the engine's order).
func TestRunWSExactNumbersOnTheWire(t *testing.T) {
	for _, f := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		for _, v := range wsExactValues {
			want := `{"v":` + v.literal + `}`
			t.Run(fmt.Sprintf("f%d/replay/%s", f, v.node), func(t *testing.T) {
				srv, hs := newTestServer(t)
				st, err := store.New(srv.cfg.StoreDir)
				mustWS(t, err)
				ctx := seedWSExactRun(t, st, f)
				appendWSExactEvent(t, st, ctx, v.node, v.literal)
				c := dialRunWS(t, hs, "r")
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe, Payload: json.RawMessage(`{"replay_history":true,"from_seq":0}`)})
				snapshotOf(t, readEnvelope(t, c))
				if got := awaitWireEvent(t, c, v.node, 5*time.Second); got != want {
					t.Fatalf("replayed %s data = %s, want %s", v.node, got, want)
				}
			})
			for _, published := range []bool{false, true} {
				mode := "external"
				if published {
					mode = "published"
				}
				t.Run(fmt.Sprintf("f%d/live-%s/%s", f, mode, v.node), func(t *testing.T) {
					srv, hs := newTestServer(t)
					st, err := store.New(srv.cfg.StoreDir)
					mustWS(t, err)
					ctx := seedWSExactRun(t, st, f)
					c := dialRunWS(t, hs, "r")
					writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
					snapshotOf(t, readEnvelope(t, c))
					awaitBrokerSubscriber(t, srv, "r")
					// Reach the live tail first, so no replay can carry the event.
					for _, node := range []string{"step1", "step2"} {
						step, err := st.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeStarted, NodeID: node})
						mustWS(t, err)
						srv.runs.Broker().Publish(*step)
						awaitWireEvent(t, c, node, 5*time.Second)
					}
					persisted := appendWSExactEvent(t, st, ctx, v.node, v.literal)
					if published {
						srv.runs.Broker().Publish(*persisted)
					}
					if got := awaitWireEvent(t, c, v.node, 5*time.Second); got != want {
						t.Fatalf("live %s data = %s, want %s", v.node, got, want)
					}
				})
			}
		}
	}
}

// awaitSubscribers waits until the broker holds n subscribers for runID.
func awaitSubscribers(t *testing.T, srv *Server, runID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for srv.runs.Broker().SubscriberCount(runID) != n {
		if time.Now().After(deadline) {
			t.Fatalf("broker subscribers for %s = %d, want %d", runID, srv.runs.Broker().SubscriberCount(runID), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// T9: the Service's source is shared, never owned by a socket: closing one
// socket leaves another streaming, and a third subscribes normally.
func TestRunWSSharedSourceSurvivesASocketClose(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	mustWS(t, err)
	ctx := seedWSExactRun(t, st, store.LegacyValueFormat)
	subscribe := func() *websocket.Conn {
		c := dialRunWS(t, hs, "r")
		writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
		snapshotOf(t, readEnvelope(t, c))
		return c
	}
	first, second := subscribe(), subscribe()
	awaitSubscribers(t, srv, "r", 2)
	mustWS(t, first.Close())
	awaitSubscribers(t, srv, "r", 1)
	_, err = st.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeStarted, NodeID: "after-close"})
	mustWS(t, err)
	awaitWireEvent(t, second, "after-close", 5*time.Second)
	third := subscribe()
	awaitSubscribers(t, srv, "r", 2)
	_, err = st.AppendEvent(ctx, "r", store.Event{Type: store.EventNodeStarted, NodeID: "third"})
	mustWS(t, err)
	awaitWireEvent(t, third, "third", 5*time.Second)
	awaitWireEvent(t, second, "third", 5*time.Second)
}

// T18: StopBackground ends an open event stream as a failure, not as a
// terminated run, and the socket stays open.
func TestRunWSStopBackgroundFailsTheStream(t *testing.T) {
	srv, hs := newTestServer(t)
	st, err := store.New(srv.cfg.StoreDir)
	mustWS(t, err)
	seedWSExactRun(t, st, store.LegacyValueFormat)
	c := dialRunWS(t, hs, "r")
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
	snapshotOf(t, readEnvelope(t, c))
	awaitBrokerSubscriber(t, srv, "r")
	srv.runs.StopBackground(t.Context())
	env := readEnvelopeWithin(t, c, 5*time.Second, wsTypeError, wsTypeTerminated)
	if env.Type != wsTypeError || errorCodeOf(t, env).Code != "event_stream_failed" {
		t.Fatalf("after StopBackground: %s %s, want event_stream_failed", env.Type, env.Payload)
	}
	writeJSONMessage(t, c, runWSEnvelope{Type: wsTypePing, AckID: "alive"})
	if pong := readEnvelope(t, c, wsTypePong); pong.AckID != "alive" {
		t.Fatalf("pong = %+v", pong)
	}
}
