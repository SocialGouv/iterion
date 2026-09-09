package dispatcher

import (
	"context"
	"fmt"
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

var (
	_ store.QueuedMessageInsertOnceStore = (*heartbeatStore)(nil)
	_ store.WatcherCursorStore           = (*heartbeatStore)(nil)
	_ store.OutputCorrectionStore        = (*heartbeatStore)(nil)
)

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

// AppendQueuedMessageOnce preserves the store's idempotent inbox capability
// through the dispatcher decorator. The fallback is deliberately at-least-once:
// an intervention is more useful than silently losing it when a custom backend
// implements only the core RunStore contract.
func (h *heartbeatStore) AppendQueuedMessageOnce(ctx context.Context, runID string, msg store.QueuedUserMessage) (bool, error) {
	if once := store.AsQueuedMessageInsertOnceStore(h.RunStore); once != nil {
		return once.AppendQueuedMessageOnce(ctx, runID, msg)
	}
	if err := h.RunStore.AppendQueuedMessage(ctx, runID, msg); err != nil {
		return false, err
	}
	return true, nil
}

// SetWatcherCursor preserves the granular anti-loop persistence capability.
func (h *heartbeatStore) SetWatcherCursor(ctx context.Context, runID, watcherID string, cursor store.WatcherCursor) error {
	if cursors := store.AsWatcherCursorStore(h.RunStore); cursors != nil {
		return cursors.SetWatcherCursor(ctx, runID, watcherID, cursor)
	}
	return fmt.Errorf("dispatcher: wrapped store does not support watcher cursors")
}

// SetRunOutputCorrection preserves bounded correction ledgers through the
// same decorator so dispatcher-launched runs do not lose correction support.
func (h *heartbeatStore) SetRunOutputCorrection(ctx context.Context, runID, ledgerKey string, episode store.OutputCorrectionEpisode) error {
	if corrections := store.AsOutputCorrectionStore(h.RunStore); corrections != nil {
		return corrections.SetRunOutputCorrection(ctx, runID, ledgerKey, episode)
	}
	return fmt.Errorf("dispatcher: wrapped store does not support output correction ledgers")
}

// CreateChildRun forwards to the wrapped store IF it implements the
// optional store.ParentedRunCreator capability. The engine probes the
// store it is handed with store.AsParentedRunCreator, and an embedded
// interface promotes only the RunStore methods — so without this
// forward every subbot child a dispatched bot spawns is created by
// CreateRun with no ParentRunID, and only the engine's later stamping
// write links it to its parent. In that window the child reads as a
// top-level run to the orphan reconciler and to any run listing.
func (h *heartbeatStore) CreateChildRun(ctx context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*store.Run, error) {
	if pc := store.AsParentedRunCreator(h.RunStore); pc != nil {
		return pc.CreateChildRun(ctx, id, workflowName, parentRunID, inputs)
	}
	r, err := h.CreateRun(ctx, id, workflowName, inputs)
	if err != nil {
		return nil, err
	}
	r.ParentRunID = parentRunID
	if err := h.SaveRun(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
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

// WriteTurn must match store.TurnStore / model.TurnWriter exactly —
// (ctx, t), two args, NOT (ctx, runID, t); the TurnCheckpoint already
// carries RunID. An earlier 3-arg signature here meant the type
// assertion below never matched the wrapped FilesystemRunStore AND
// *heartbeatStore failed to satisfy model.TurnWriter, so the hook
// layer's `emitter.(TurnWriter)` probe came back nil and every
// dispatcher-launched run silently dropped its per-turn checkpoints
// (studio timeline + Fork API blind).
func (h *heartbeatStore) WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error {
	if w, ok := h.RunStore.(interface {
		WriteTurn(ctx context.Context, t *store.TurnCheckpoint) error
	}); ok {
		return w.WriteTurn(ctx, t)
	}
	return nil
}
