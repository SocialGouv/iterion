package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestWSCursorActualSourceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		floor         int64
		empty, replay bool
	}{{"lazy-max", math.MaxInt64, false, false}, {"replay-max", math.MaxInt64, false, true}, {"lazy-zero", 0, false, false}, {"empty-zero", 0, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hs := newTestServer(t)
			seedRun(t, srv, "r", "wf", store.RunStatusPausedWaitingHuman)
			st, e := store.New(srv.cfg.StoreDir)
			if e != nil {
				t.Fatal(e)
			}
			var raw []byte
			if !tc.empty {
				line, e := json.Marshal(store.Event{RunID: "r", Seq: tc.floor, Type: store.EventNodeStarted})
				if e != nil {
					t.Fatal(e)
				}
				raw = append(line, '\n')
			}
			if e = os.WriteFile(filepath.Join(srv.cfg.StoreDir, "runs", "r", "events.jsonl"), raw, 0600); e != nil {
				t.Fatal(e)
			}
			ws := dialRunWS(t, hs, "r")
			request := map[string]any{"from_seq": int64(0), "replay_history": tc.replay}
			if tc.replay {
				request["from_seq"] = tc.floor
			}
			payload, _ := json.Marshal(request)
			writeJSONMessage(t, ws, runWSEnvelope{Type: wsTypeSubscribe, AckID: "s", Payload: payload})
			env := readEnvelope(t, ws)
			if env.Type != wsTypeSnapshot {
				t.Fatal(env)
			}
			var snap runview.RunSnapshot
			if e = json.Unmarshal(env.Payload, &snap); e != nil {
				t.Fatal(e)
			}
			want := tc.floor
			if tc.empty {
				want = runview.NoEventsSeq
			}
			if snap.LastSeq != want {
				t.Fatal("snapshot", snap.LastSeq, want)
			}
			awaitBrokerSubscriber(t, srv, "r")
			if tc.replay {
				env = readEnvelope(t, ws)
				rows := decodeEventEnvelope(t, env)
				if len(rows) != 1 || rows[0].Seq != tc.floor {
					t.Fatal("inclusive replay", rows)
				}
			}
			if tc.empty {
				persisted, e := st.AppendEvent(t.Context(), "r", store.Event{Type: store.EventNodeFinished})
				if e != nil {
					t.Fatal(e)
				}
				if persisted.Seq != 0 {
					t.Fatal("fixture first persisted seq", persisted.Seq)
				}
				srv.runs.Broker().Publish(*persisted)
				env = readEnvelope(t, ws)
				rows := decodeEventEnvelope(t, env)
				if len(rows) != 1 || rows[0].Seq != 0 || rows[0].Type == store.EventAlert {
					t.Fatal("empty snapshot hid first persisted event", rows)
				}
			}
			srv.runs.Broker().Publish(store.Event{RunID: "r", Type: store.EventAlert, Seq: 0})
			env = readEnvelopeWithin(t, ws, time.Second)
			rows := decodeEventEnvelope(t, env)
			if len(rows) != 1 || rows[0].Type != store.EventAlert {
				t.Fatal("boundary replayed or alert lost", rows)
			}
			writeJSONMessage(t, ws, runWSEnvelope{Type: wsTypeUnsubscribe, AckID: "u"})
			env = readEnvelope(t, ws)
			if env.Type != wsTypeAck || env.AckID != "u" {
				t.Fatal("unsubscribe boundary", env)
			}
		})
	}
}

func TestWSCursorFixtureJoinsItsStreamProducers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &testHeldRunSource{runstream.NewEventPipe(), runstream.NewLogPipe()}
		source := &testOwnedRunStreams{Source: inner}
		if _, e := source.SubscribeEventsCursor(t.Context(), "r", runstream.EventCursor{}); e != nil {
			t.Fatal(e)
		}
		if _, e := source.SubscribeLogs(t.Context(), "r", 0); e != nil {
			t.Fatal(e)
		}
		finished := make(chan error, 1)
		go func() { finished <- source.Close() }()
		synctest.Wait()
		select {
		case e := <-finished:
			t.Fatalf("cleanup returned before producers ended: %v", e)
		default:
		}
		select {
		case <-inner.event.Done():
		default:
			t.Fatal("event was not canceled")
		}
		select {
		case <-inner.log.Done():
		default:
			t.Fatal("log was not canceled")
		}
		if _, e := source.SubscribeEventsCursor(t.Context(), "late", runstream.EventCursor{}); !errors.Is(e, context.Canceled) {
			t.Fatal("late event admitted", e)
		}
		if _, e := source.SubscribeLogs(t.Context(), "late", 0); !errors.Is(e, context.Canceled) {
			t.Fatal("late log admitted", e)
		}
		inner.event.Finish(nil)
		synctest.Wait()
		select {
		case e := <-finished:
			t.Fatalf("cleanup returned before log ended: %v", e)
		default:
		}
		inner.log.Finish(nil)
		synctest.Wait()
		if e := <-finished; e != nil {
			t.Fatal(e)
		}
		if e := source.Close(); e != nil {
			t.Fatal(e)
		}
	})
}

func TestWSCursorFixtureJoinsSubscriptionBeingAdmitted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := &testAdmissionRunSource{testHeldRunSource: &testHeldRunSource{runstream.NewEventPipe(), runstream.NewLogPipe()}, entered: make(chan struct{}), release: make(chan struct{})}
		defer inner.event.Finish(nil)
		source := &testOwnedRunStreams{Source: inner}
		admitted := make(chan error, 1)
		go func() { _, e := source.SubscribeEventsCursor(t.Context(), "r", runstream.EventCursor{}); admitted <- e }()
		synctest.Wait()
		select {
		case <-inner.entered:
		default:
			t.Fatal("subscription never entered source")
		}
		finished := make(chan error, 1)
		closing := make(chan struct{})
		go func() { close(closing); finished <- source.Close() }()
		<-closing
		// Mutex contention is not durably blocking to synctest. Release
		// admission before Wait; the producer still cannot finish.
		close(inner.release)
		synctest.Wait()
		if e := <-admitted; e != nil {
			t.Fatal(e)
		}
		select {
		case e := <-finished:
			t.Fatalf("cleanup failed to join admitted producer: %v", e)
		default:
		}
		select {
		case <-inner.event.Done():
		default:
			t.Fatal("admitted producer not canceled")
		}
		inner.event.Finish(nil)
		synctest.Wait()
		if e := <-finished; e != nil {
			t.Fatal(e)
		}
	})
}
