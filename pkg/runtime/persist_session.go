package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/google/uuid"
)

type sessionResumeCapability interface {
	SessionResumeCapability(node ir.Node) (backend string, supported bool)
}

type sessionPacker interface {
	PackSession(ctx context.Context, backend, sessionID string) ([]byte, error)
	UnpackSession(ctx context.Context, backend, sessionID string, blob []byte) error
	HasSession(ctx context.Context, backend, sessionID string) bool
}

func takeSessionStateBlob(output map[string]any) []byte {
	if output == nil {
		return nil
	}
	raw, ok := output[delegate.SessionStateBlobKey]
	delete(output, delegate.SessionStateBlobKey)
	delete(output, delegate.SessionStateKey)
	delete(output, "_session_state_ref")
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

func stripSessionKeys(input map[string]any) {
	if input == nil {
		return
	}
	delete(input, delegate.SessionIDKey)
	delete(input, delegate.SessionFingerprintKey)
	delete(input, delegate.SessionStateKey)
}

func stringMap(v any) string {
	s, _ := v.(string)
	return s
}

func newSessionRef() string {
	return uuid.NewString()
}

func cloneNodeSessions(src map[string]store.NodeSessionSlot) map[string]store.NodeSessionSlot {
	if src == nil {
		return nil
	}
	dst := make(map[string]store.NodeSessionSlot, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (e *Engine) adoptCheckpointSessions(rs *runState) {
	if ev, ok := e.executor.(interface{ EvictRun(string) }); ok && rs != nil {
		ev.EvictRun(rs.runID)
	}
}

func (e *Engine) persistStore() store.BackendSessionStore {
	return store.AsBackendSessionStore(e.store)
}

func (e *Engine) capability(node ir.Node) (string, bool) {
	if c, ok := e.executor.(sessionResumeCapability); ok {
		return c.SessionResumeCapability(node)
	}
	// Test stubs without the seam: do not claim a backend.
	return "", true
}

// injectPersistAndResume mutates nodeInput in place (ADR-089 resolution order).
// Callers that already placed a mid-node pause blob or fork rehydration
// must run this BEFORE those overlays, or pass midNode=true.
func (e *Engine) injectPersistAndResume(ctx context.Context, rs *runState, node ir.Node, nodeInput map[string]any, midNode bool) {
	if nodeInput == nil {
		return
	}
	llm, ok := node.(ir.LLMNode)
	if !ok {
		return
	}
	if midNode {
		return
	}
	if llm.GetSession() != ir.SessionPersist {
		// inherit / fork / inherit_if_available stay on applySessionContinuity.
		return
	}
	backend, supported := e.capability(node)
	if !supported {
		if e.logger != nil {
			e.logger.Warn("[%s/persist] backend %q cannot resume; running fresh", node.NodeID(), backend)
		}
		stripSessionKeys(nodeInput)
		return
	}
	slot, has := rs.nodeSessions[node.NodeID()]
	if has && slot.StateRef != "" && (backend == "" || slot.Backend == "" || slot.Backend == backend) {
		if e.unpackSlot(ctx, rs, nodeInput, slot, backend) {
			return
		}
		// Own-slot Get/unpack failure: strip and run fresh. Do not
		// fall through to HasSession (ADR-089).
		stripSessionKeys(nodeInput)
		return
	}
	e.injectVisit1Seed(ctx, rs, node, nodeInput, backend)
}

func (e *Engine) unpackSlot(ctx context.Context, rs *runState, nodeInput map[string]any, slot store.NodeSessionSlot, backend string) bool {
	bss := e.persistStore()
	if bss == nil || slot.StateRef == "" {
		return false
	}
	blob, err := bss.GetBackendSession(ctx, rs.runID, slot.StateRef)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("persist: get slot %s: %v; running fresh", slot.StateRef, err)
		}
		return false
	}
	packer, ok := e.executor.(sessionPacker)
	if !ok {
		nodeInput[delegate.SessionStateKey] = blob
		nodeInput[delegate.SessionIDKey] = slot.SessionID
		nodeInput[delegate.SessionFingerprintKey] = slot.Fingerprint
		return true
	}
	if err := packer.UnpackSession(ctx, slot.Backend, slot.SessionID, blob); err != nil {
		if e.logger != nil {
			e.logger.Warn("persist: unpack: %v; running fresh", err)
		}
		return false
	}
	nodeInput[delegate.SessionIDKey] = slot.SessionID
	nodeInput[delegate.SessionFingerprintKey] = slot.Fingerprint
	return true
}

func (e *Engine) injectVisit1Seed(ctx context.Context, rs *runState, node ir.Node, nodeInput map[string]any, capBackend string) {
	id := stringMap(nodeInput[delegate.SessionIDKey])
	if id == "" {
		return
	}
	src, ok := visit1EligibleSource(e.workflow, rs, node.NodeID(), id, capBackend)
	if !ok {
		stripSessionKeys(nodeInput)
		return
	}
	if slot, has := rs.nodeSessions[src]; has && slot.SessionID == id && slot.Backend == capBackend && slot.StateRef != "" {
		if e.unpackSlot(ctx, rs, nodeInput, slot, capBackend) {
			return
		}
		stripSessionKeys(nodeInput)
		return
	}
	packer, okp := e.executor.(sessionPacker)
	if okp && packer.HasSession(ctx, capBackend, id) {
		if fp := stringMap(rs.outputs[src][delegate.SessionFingerprintKey]); fp != "" {
			nodeInput[delegate.SessionFingerprintKey] = fp
		} else {
			delete(nodeInput, delegate.SessionFingerprintKey)
		}
		return
	}
	stripSessionKeys(nodeInput)
}

func visit1EligibleSource(wf *ir.Workflow, rs *runState, nodeID, id, capBackend string) (string, bool) {
	if wf == nil || rs == nil || id == "" {
		return "", false
	}
	var found []string
	for _, edge := range wf.Edges {
		if edge.To != nodeID {
			continue
		}
		srcOut := rs.outputs[edge.From]
		if srcOut == nil {
			continue
		}
		if stringMap(srcOut[delegate.SessionIDKey]) != id {
			continue
		}
		srcBackend := stringMap(srcOut[delegate.BackendNameKey])
		// Legacy/stub outputs may omit _backend; when the source stamped
		// one it must match the consuming node's capability backend.
		if capBackend != "" && srcBackend != "" && srcBackend != capBackend {
			continue
		}
		found = append(found, edge.From)
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

func (e *Engine) putSessionBlob(ctx context.Context, runID, ref string, blob []byte) error {
	bss := e.persistStore()
	if bss == nil {
		return fmt.Errorf("persist: no BackendSessionStore")
	}
	return bss.PutBackendSession(ctx, runID, ref, blob)
}

func (e *Engine) deleteSessionBlob(ctx context.Context, runID, ref string) {
	if ref == "" {
		return
	}
	bss := e.persistStore()
	if bss == nil {
		return
	}
	_ = bss.DeleteBackendSession(ctx, runID, ref)
}

// hydratePauseSession Gets the in-flight ask_user pack and unpacks it.
// Get/unpack failure strips session keys so --resume is never issued
// against a missing or corrupt pack (ADR-089).
func (e *Engine) hydratePauseSession(ctx context.Context, rs *runState, nodeInput map[string]any, backend, sessionID, ref string) {
	if ref == "" {
		return
	}
	bss := e.persistStore()
	if bss == nil {
		if e.logger != nil {
			e.logger.Warn("persist: no BackendSessionStore for pause ref; running fresh")
		}
		stripSessionKeys(nodeInput)
		return
	}
	blob, err := bss.GetBackendSession(ctx, rs.runID, ref)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("persist: get pause blob: %v; running fresh", err)
		}
		stripSessionKeys(nodeInput)
		return
	}
	packer, ok := e.executor.(sessionPacker)
	if !ok {
		nodeInput[delegate.SessionStateKey] = blob
		return
	}
	if err := packer.UnpackSession(ctx, backend, sessionID, blob); err != nil {
		if e.logger != nil {
			e.logger.Warn("persist: unpack pause blob: %v; running fresh", err)
		}
		stripSessionKeys(nodeInput)
		return
	}
}

func (e *Engine) commitPersistSlot(ctx context.Context, rs *runState, node ir.Node, output map[string]any) error {
	blob := takeSessionStateBlob(output)
	llm, ok := node.(ir.LLMNode)
	if !ok || llm.GetSession() != ir.SessionPersist {
		return nil
	}
	sid := stringMap(output[delegate.SessionIDKey])
	backend := stringMap(output[delegate.BackendNameKey])
	fp := stringMap(output[delegate.SessionFingerprintKey])
	old := rs.nodeSessions[node.NodeID()]
	pauseRef := rs.pauseSessionRef
	rs.pauseSessionRef = ""

	if sid != "" && len(blob) == 0 {
		if packer, ok := e.executor.(sessionPacker); ok {
			if packed, err := packer.PackSession(ctx, backend, sid); err == nil {
				blob = packed
			} else if e.logger != nil {
				e.logger.Warn("[%s/persist] pack: %v", node.NodeID(), err)
			}
		}
	}
	if sid == "" || len(blob) == 0 {
		delete(rs.nodeSessions, node.NodeID())
		if sid != "" {
			if e.logger != nil {
				e.logger.Warn("[%s/persist] no packed session blob; slot cleared", node.NodeID())
			}
			if err := e.checkpointPersistDeletion(ctx, rs, node.NodeID()); err != nil {
				return err
			}
		}
		if pauseRef != "" {
			e.deleteSessionBlob(ctx, rs.runID, pauseRef)
		}
		return nil
	}
	ref := newSessionRef()
	if err := e.putSessionBlob(ctx, rs.runID, ref, blob); err != nil {
		delete(rs.nodeSessions, node.NodeID())
		if e.logger != nil {
			e.logger.Warn("persist: put slot: %v", err)
		}
		if cpErr := e.checkpointPersistDeletion(ctx, rs, node.NodeID()); cpErr != nil {
			return cpErr
		}
		if pauseRef != "" {
			e.deleteSessionBlob(ctx, rs.runID, pauseRef)
		}
		return nil
	}
	rs.nodeSessions[node.NodeID()] = store.NodeSessionSlot{
		Backend:     backend,
		SessionID:   sid,
		Fingerprint: fp,
		StateRef:    ref,
	}
	cp := buildCheckpoint(rs, node.NodeID())
	if err := e.store.SaveCheckpoint(ctx, rs.runID, cp); err != nil {
		if e.logger != nil {
			e.logger.Error("persist: required checkpoint after slot failed: %v", err)
		}
		return e.failRunErrWithCheckpoint(rs, node.NodeID(), fmt.Errorf("persist checkpoint: %w", err))
	}
	if old.StateRef != "" && old.StateRef != ref {
		e.deleteSessionBlob(ctx, rs.runID, old.StateRef)
	}
	if pauseRef != "" && pauseRef != ref {
		e.deleteSessionBlob(ctx, rs.runID, pauseRef)
	}
	return nil
}

func (e *Engine) checkpointPersistDeletion(ctx context.Context, rs *runState, nodeID string) error {
	cp := buildCheckpoint(rs, nodeID)
	if err := e.store.SaveCheckpoint(ctx, rs.runID, cp); err != nil {
		if e.logger != nil {
			e.logger.Error("persist: required checkpoint of slot deletion failed: %v", err)
		}
		return e.failRunErrWithCheckpoint(rs, nodeID, fmt.Errorf("persist checkpoint: %w", err))
	}
	return nil
}

func (e *Engine) clearPauseRefAfterSuccess(ctx context.Context, rs *runState, nodeID string) {
	ref := rs.pauseSessionRef
	rs.pauseSessionRef = ""
	cp := buildCheckpoint(rs, nodeID)
	if err := e.store.SaveCheckpoint(ctx, rs.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("persist: clear pause ref checkpoint: %v", err)
	}
	e.deleteSessionBlob(ctx, rs.runID, ref)
}
