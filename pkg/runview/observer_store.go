package runview

import (
	"context"
	"io"

	"github.com/SocialGouv/iterion/pkg/store"
)

// observerStore wraps a store.RunStore so LaunchSpec.ExtraObservers fire
// on every AppendEvent, not just the engine-level events the runtime
// engine surfaces via WithEventObserver. High-frequency
// tool_started / tool_called events flow through the backend's hook
// layer (pkg/backend/model/hooks.go) which calls AppendEvent directly,
// bypassing the engine's onEvent callback. This is the runview twin of
// the dispatcher's heartbeatStore — routing EngineRunner.Dispatch
// through runview.Service.Launch (ADR-046) keeps stall detection
// byte-identical only because the dispatcher's watermark observer sees
// the SAME store-level event stream here.
//
// The wrapper forwards the optional FilesystemRunStore capabilities
// (WriteAttachment, WriteToolBlob, WriteTurn) through type-assertion so
// the backend hooks' capability probes still match — without them the
// tool-blob sidecars and per-turn checkpoints silently degrade to
// inline-only (studio timeline + Fork API blind).
type observerStore struct {
	store.RunStore
	observers []func(store.Event)
}

// wrapWithObservers returns s wrapped so each observer fires on every
// AppendEvent. Returns s unchanged when there are no observers, so the
// non-dispatcher hot path pays nothing.
func wrapWithObservers(s store.RunStore, observers []func(store.Event)) store.RunStore {
	if len(observers) == 0 {
		return s
	}
	return &observerStore{RunStore: s, observers: observers}
}

func (o *observerStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	persisted, err := o.RunStore.AppendEvent(ctx, runID, evt)
	if err == nil && persisted != nil {
		for _, obs := range o.observers {
			obs(*persisted)
		}
	}
	return persisted, err
}

func (o *observerStore) WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error {
	if w, ok := o.RunStore.(interface {
		WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error
	}); ok {
		return w.WriteAttachment(ctx, runID, rec, body)
	}
	return nil
}

func (o *observerStore) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	if w, ok := o.RunStore.(interface {
		WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error)
	}); ok {
		return w.WriteToolBlob(ctx, runID, toolUseID, kind, body)
	}
	return 0, nil
}

func (o *observerStore) WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error {
	if w, ok := o.RunStore.(interface {
		WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error
	}); ok {
		return w.WriteTurn(ctx, t)
	}
	return nil
}
