package runview

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync/atomic"
	"testing"
)

type reviewBridgeWrappedStore struct{ *store.FilesystemRunStore }

func TestBridgeReviewFactoryAdmission(t *testing.T) {
	s, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	borrowed := &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) {
		t.Error("borrowed source used")
		return nil, nil
	}, close: func() error { t.Error("borrowed source closed"); return nil }}
	for _, owner := range []*Service{{store: &reviewBridgeWrappedStore{s}, broker: NewEventBroker()}, {store: s, broker: NewEventBroker(), streamSrc: borrowed}} {
		if r, e := newObservedEventBridges(t.Context(), owner); r != nil || !errors.Is(e, store.ErrRunDataCapability) {
			t.Fatal("unsupported factory admitted", r, e)
		}
	}
	if _, e := newObservedEventBridges(t.Context(), &Service{store: s, broker: NewEventBroker()}); e != nil {
		t.Fatal("native rejected", e)
	}
}
func TestBridgeReviewConflictingFastJoin(t *testing.T) {
	r, _ := bridgeRegistry(t)
	o := bridgeOrigin(t)
	s := bridgeSub()
	var opens atomic.Int32
	r.open = func(context.Context) (runstream.Source, error) {
		opens.Add(1)
		return &bridgeProbeSource{subscribe: func(context.Context) (runstream.EventSubscription, error) { return s, nil }}, nil
	}
	h := bridgeAcquire(t, r, o)
	b, e := store.CaptureRunDataBinding(store.RunDataHeader{RunID: "r", Format: 1, BirthID: "B"})
	if e != nil {
		t.Fatal(e)
	}
	wrongBirth, e := store.WithRunDataBinding(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	wrongFormat, e := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "r", Format: 2})
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range []context.Context{wrongBirth, wrongFormat} {
		if got, e := r.acquire(c, o); got != nil || !errors.Is(e, store.ErrRunDataIdentityChanged) {
			if got != nil {
				got.Close()
			}
			t.Fatal("fast join ignored original intent", e)
		}
	}
	// Rejections did not close or disturb the existing shared worker.
	s.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, h, o)
	good, e := store.WithRunDataBinding(t.Context(), func() store.RunDataBinding {
		v, err := store.CaptureRunDataBinding(o.Header())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}())
	if e != nil {
		t.Fatal(e)
	}
	second, e := r.acquire(good, o)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	s.events <- []*store.Event{bridgeEvent(t, o)}
	bridgeReceive(t, h, o)
	bridgeReceive(t, second, o)
	if opens.Load() != 1 {
		t.Fatal("duplicate source", opens.Load())
	}
}
func TestBridgeReviewNativeTerminalEOF(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+format)), func(t *testing.T) {
			r, s := bridgeRegistry(t)
			created, e := s.CreateRunWithValueFormat(t.Context(), store.RunCreateSpec{ID: "r", Format: format})
			if e != nil {
				t.Fatal(e)
			}
			c, e := store.BindRunValueFormat(t.Context(), created)
			if e != nil {
				t.Fatal(e)
			}
			data := map[string]any{"v": json.Number("17")}
			if format == 2 {
				data["v"] = json.Number("1e400")
			}
			written, e := s.AppendEvent(c, "r", store.Event{Data: data})
			if e != nil {
				t.Fatal(e)
			}
			o, ok := written.EventObservation()
			if !ok {
				t.Fatal("native stamp absent")
			}
			h := bridgeAcquire(t, r, o)
			bridgeReceive(t, h, o)
			if e = s.UpdateRunStatus(c, "r", store.RunStatusFinished, ""); e != nil {
				t.Fatal(e)
			}
			bridgeWait(t, h.Done())
			if e = h.Err(); e != nil {
				t.Fatal("normal EOF acquired cleanup cancellation", e)
			}
			bridgeClosed(t, h)
			// Reacquire the identical terminal observation while old handle remains.
			next := bridgeAcquire(t, r, o)
			bridgeReceive(t, next, o)
			bridgeWait(t, next.Done())
			if e = next.Err(); e != nil {
				t.Fatal(e)
			}
			h.Close()
			bridgeClosed(t, next)
		})
	}
}
