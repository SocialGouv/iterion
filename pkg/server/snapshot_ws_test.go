package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// snapshotWSStore replaces run A with B through the native store once the
// snapshot's scan begins, then delegates the scan unchanged. It schedules
// the race; the identity policy under test stays in the native store.
type snapshotWSStore struct {
	ran atomic.Bool
	store.RunStore
	before func() error
	once   sync.Once
	err    error
}

func (s *snapshotWSStore) ScanEvents(c context.Context, id string, v func(*store.Event) bool) error {
	s.ran.Store(true)
	s.once.Do(func() {
		if s.before != nil {
			s.err = s.before()
		}
	})
	if s.err != nil {
		return s.err
	}
	return s.RunStore.ScanEvents(c, id, v)
}

// snapshotWSSource counts subscription attempts and refuses them: it
// witnesses whether the connection subscribes after its snapshot, not
// live delivery.
type snapshotWSSource struct{ calls atomic.Int32 }

func (s *snapshotWSSource) SubscribeEvents(context.Context, string, int64) (runstream.EventSubscription, error) {
	s.calls.Add(1)
	return nil, errors.New("probe subscription reached")
}

func (s *snapshotWSSource) SubscribeLogs(context.Context, string, int64) (runstream.LogSubscription, error) {
	return nil, errors.New("unused")
}

func (s *snapshotWSSource) Close() error { return nil }

// CheckBoundObservationSource endorses the probe itself: it serves no run at
// all, so a bound connection may reach it.
func (s *snapshotWSSource) CheckBoundObservationSource(actual runstream.Source) error {
	if receiver, ok := actual.(*snapshotWSSource); !ok || receiver != s {
		return store.ErrRunDataCapability
	}
	return nil
}

// TestSnapshotWSOriginalBoundary drives the real WS subscribe path: a run
// replaced between the snapshot's load and its scan yields snapshot_failed,
// no snapshot frame and no subscription, instead of A's metadata over B's
// events.
func TestSnapshotWSOriginalBoundary(t *testing.T) {
	for _, f := range []store.ValueFormat{1, 2} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("f%d/replace=%t", f, replace), func(t *testing.T) {
				srv, hs := newTestServer(t)
				native, e := store.New(srv.cfg.StoreDir)
				if e != nil {
					t.Fatal(e)
				}
				seed := func(marker string) error {
					r, e := native.CreateRunWithValueFormat(t.Context(), store.RunCreateSpec{ID: "snap-origin", WorkflowName: marker, Format: f})
					if e != nil {
						return e
					}
					c, e := store.BindRunValueFormat(t.Context(), r)
					if e != nil {
						return e
					}
					_, e = native.AppendEvent(c, r.ID, store.Event{Type: store.EventNodeStarted, NodeID: marker})
					return e
				}
				if e = seed("A"); e != nil {
					t.Fatal(e)
				}
				wrapped := &snapshotWSStore{RunStore: native}
				if replace {
					wrapped.before = func() error {
						if e := native.DeleteRun(t.Context(), "snap-origin"); e != nil {
							return e
						}
						if _, e := native.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); e != nil {
							return e
						}
						return seed("B")
					}
				}
				source := &snapshotWSSource{}
				srv.runs.Stop(t.Context())
				srv.runs = newTestRunviewService(t, t.TempDir(), runview.WithRecoveryPassive(), runview.WithStore(wrapped))
				runview.WithStreamSource(source)(srv.runs)
				c := dialRunWS(t, hs, "snap-origin")
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypeSubscribe})
				first := readEnvelope(t, c)
				if replace {
					if wrapped.before == nil {
						t.Fatal("missing replacement barrier")
					}
					if !wrapped.ran.Load() {
						t.Fatal("native scan barrier was not reached")
					}
					if first.Type != wsTypeError {
						t.Fatalf("successor mixed snapshot escaped: type=%s payload=%s", first.Type, first.Payload)
					}
					var payload struct {
						Code string `json:"code"`
					}
					if e = json.Unmarshal(first.Payload, &payload); e != nil {
						t.Fatal(e)
					}
					if payload.Code != "snapshot_failed" {
						t.Fatal("wrong refusal", payload.Code, string(first.Payload))
					}
					// The origin the upgrade bound is gone: the socket closes
					// scope_lost behind the error instead of serving more.
					ce, texts := awaitWSClose(t, c, 2*time.Second)
					if len(texts) != 0 || ce.Code != 1000 || !strings.HasPrefix(ce.Text, wsCloseScopeLost+":") {
						t.Fatalf("close after a lost origin = %+v, frames %v", ce, texts)
					}
					if source.calls.Load() != 0 {
						t.Fatalf("subscriptions=%d want 0", source.calls.Load())
					}
					return
				} else {
					if first.Type != wsTypeSnapshot {
						t.Fatalf("positive missing snapshot: %s %s", first.Type, first.Payload)
					}
					var snap runview.RunSnapshot
					if e = json.Unmarshal(first.Payload, &snap); e != nil {
						t.Fatal(e)
					}
					if snap.Run.WorkflowName != "A" {
						t.Fatal("wrong positive metadata")
					}
					if next := readEnvelope(t, c); next.Type != wsTypeError {
						t.Fatal("source probe did not return error", next.Type)
					}
				}
				writeJSONMessage(t, c, runWSEnvelope{Type: wsTypePing, AckID: "barrier"})
				if next := readEnvelope(t, c); next.Type != wsTypePong {
					t.Fatal("unexpected post-snapshot envelope", next.Type)
				}
				want := int32(1)
				if replace {
					want = 0
				}
				if source.calls.Load() != want {
					t.Fatalf("subscriptions=%d want%d", source.calls.Load(), want)
				}
			})
		}
	}
}
