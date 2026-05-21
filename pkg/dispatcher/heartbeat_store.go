package dispatcher

import (
	"context"
	"io"

	"github.com/SocialGouv/iterion/pkg/store"
)

// heartbeatStore wraps a store.RunStore so the dispatcher's OnEvent
// hook fires on every AppendEvent, not just the engine-level events
// that the runtime engine sees via WithEventObserver. High-frequency
// tool_started / tool_called events flow through the backend's hook
// layer (pkg/backend/model/hooks.go) which calls emitter.AppendEvent
// directly — bypassing the engine's onEvent callback. Without this
// wrapping, the dispatcher's stall heartbeat falls behind real
// activity by ~10min on long reviewer/agent nodes (see 2026-05-21
// dogfood post-mortem).
//
// The wrapper carries the FilesystemRunStore through type-assertion
// methods (WriteAttachment, WriteToolBlob, TurnWriter) so the backend
// hooks' capability probes still match — without these the tool blob
// sidecars and turn snapshots silently degrade to inline-only.
type heartbeatStore struct {
	store.RunStore
	onEvent func(name string)
}

func newHeartbeatStore(s store.RunStore, onEvent func(name string)) *heartbeatStore {
	return &heartbeatStore{RunStore: s, onEvent: onEvent}
}

func (h *heartbeatStore) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	persisted, err := h.RunStore.AppendEvent(ctx, runID, evt)
	if err == nil && persisted != nil && h.onEvent != nil {
		h.onEvent(string(persisted.Type))
	}
	return persisted, err
}

// WriteAttachment forwards to the wrapped store IF it implements the
// optional capability, so hooks.go's type assertion still picks it up.
// Returns nil error / zero values when the underlying store doesn't
// satisfy the interface (mirrors the silent-skip semantics in
// model.NewStoreEventHooks).
func (h *heartbeatStore) WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error {
	if w, ok := h.RunStore.(interface {
		WriteAttachment(ctx context.Context, runID string, rec store.AttachmentRecord, body io.Reader) error
	}); ok {
		return w.WriteAttachment(ctx, runID, rec, body)
	}
	return nil
}

func (h *heartbeatStore) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	if w, ok := h.RunStore.(interface {
		WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error)
	}); ok {
		return w.WriteToolBlob(ctx, runID, toolUseID, kind, body)
	}
	return 0, nil
}

func (h *heartbeatStore) WriteTurn(ctx context.Context, runID string, t *store.TurnCheckpoint) error {
	if w, ok := h.RunStore.(interface {
		WriteTurn(ctx context.Context, runID string, t *store.TurnCheckpoint) error
	}); ok {
		return w.WriteTurn(ctx, runID, t)
	}
	return nil
}
