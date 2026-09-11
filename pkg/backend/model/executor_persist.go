package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/sessionpack"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

const (
	clawPersistVersion      = 1
	clawPersistTargetTokens = 32_000
	clawPersistRecent       = 8
	// clawPersistReplayBlobBytes bounds a persisted claw transcript before it
	// is replayed into the next prompt. The storage limit below deliberately
	// remains larger: a prompt must retain room for its current instruction,
	// system policy, and a handoff from a private planning node.
	clawPersistReplayBlobBytes = 128 << 10
	clawPersistHardBlobBytes   = 512 << 10
)

type clawPersistEnvelope struct {
	Version     int           `json:"version"`
	SessionID   string        `json:"session_id"`
	Slot        string        `json:"slot"`
	Fingerprint string        `json:"fingerprint"`
	Messages    []api.Message `json:"messages"`
}

func (e *ClawExecutor) SessionResumeCapability(node ir.Node) (string, bool) {
	name := e.EffectiveBackendName(node)
	switch name {
	case delegate.BackendClaw, delegate.BackendClaudeCode, delegate.BackendPi, delegate.BackendCodex:
		return name, true
	default:
		return name, false
	}
}

func (e *ClawExecutor) packerTask() delegate.Task {
	return delegate.Task{
		WorkDir:        e.workDir,
		StoreDir:       e.storeDir,
		SharedStateDir: e.sharedStateDir,
		Sandbox:        e.sandbox,
	}
}

func (e *ClawExecutor) PackSession(ctx context.Context, backend, sessionID string) ([]byte, error) {
	t := e.packerTask()
	return e.packSessionFrom(ctx, &t, backend, sessionID)
}

func (e *ClawExecutor) packSessionFrom(ctx context.Context, task *delegate.Task, backend, sessionID string) ([]byte, error) {
	if task == nil {
		t := e.packerTask()
		task = &t
	}
	root := delegate.SessionFilesRoot(ctx, *task, backend)
	if root == "" || sessionID == "" {
		return nil, os.ErrNotExist
	}
	files, err := sessionpack.CollectBySessionID(root, sessionID, backend)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, os.ErrNotExist
	}
	return sessionpack.Pack(sessionpack.Header{Backend: backend, SessionID: sessionID}, files)
}

func (e *ClawExecutor) UnpackSession(ctx context.Context, backend, sessionID string, blob []byte) error {
	if backend == delegate.BackendClaw {
		var env clawPersistEnvelope
		if err := json.Unmarshal(blob, &env); err != nil {
			return fmt.Errorf("claw persisted session: decode: %w", err)
		}
		if env.Version != clawPersistVersion || env.Slot == "" || env.SessionID != sessionID {
			return fmt.Errorf("claw persisted session: incompatible envelope")
		}
		runID := RunIDFromContext(ctx)
		if runID == "" || e.sessions == nil {
			return fmt.Errorf("claw persisted session: missing runtime context")
		}
		repaired, stats := sanitizeToolPairs(env.Messages, nil, true)
		if stats.removedBlocks() > 0 && e.logger != nil {
			e.logger.Warn("persist: repaired claw session slot %q: removed %d orphaned tool block(s), %d provider-specific block(s), and %d empty message(s)",
				env.Slot, stats.ToolUsesRemoved+stats.ToolResultsRemoved, stats.ProviderBlocksRemoved, stats.MessagesRemoved)
		}
		bounded, replay, before, reduced, err := constrainClawPersistReplay(env.Slot, env.SessionID, env.Fingerprint, repaired)
		if err != nil {
			// Named slots deliberately survive a node-id rewind. Refusing an
			// unsafe legacy replay here lets runtime strip it and run this visit
			// fresh instead of issuing an oversized prompt before a later pack can
			// correct it.
			return fmt.Errorf("claw persisted session: constrain replay: %w", err)
		}
		if reduced && e.logger != nil {
			e.logger.Warn("persist: compacted oversized inbound claw replay slot %q (%d -> %d bytes)", env.Slot, before, len(bounded))
		}
		e.sessions.save(runID, env.Slot, replay)
		return nil
	}
	t := e.packerTask()
	root := delegate.SessionFilesRoot(ctx, t, backend)
	if root == "" {
		return os.ErrNotExist
	}
	return sessionpack.Unpack(blob, sessionpack.Header{Backend: backend, SessionID: sessionID}, root)
}

func (e *ClawExecutor) HasSession(ctx context.Context, backend, sessionID string) bool {
	if backend == delegate.BackendClaw {
		return false
	}
	t := e.packerTask()
	root := delegate.SessionFilesRoot(ctx, t, backend)
	if root == "" || sessionID == "" {
		return false
	}
	return sessionpack.HasFile(root, sessionID, backend)
}

func (e *ClawExecutor) packClawSession(runID, slot, sessionID, fingerprint string) []byte {
	if e.sessions == nil || runID == "" || slot == "" || sessionID == "" {
		return nil
	}
	messages := e.sessions.load(runID, slot)
	if len(messages) == 0 {
		return nil
	}
	messages, _ = sanitizeToolPairs(messages, nil, false)
	if len(messages) == 0 {
		return nil
	}
	if compacted, ok := forceCompactToTokens(messages, clawPersistTargetTokens, clawPersistRecent); ok {
		messages = compacted
	}
	blob, _, before, reduced, err := constrainClawPersistReplay(slot, sessionID, fingerprint, messages)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("persist: dropping unsafe claw replay slot %q: %v", slot, err)
		}
		return nil
	}
	if reduced && e.logger != nil {
		e.logger.Warn("persist: compacted oversized claw replay slot %q (%d -> %d bytes)", slot, before, len(blob))
	}
	return blob
}

// constrainClawPersistReplay makes a claw session safe to prepend to a new
// prompt. It is used for both newly written and legacy inbound envelopes: a
// persist slot can outlive a node-id rewind, so save-time enforcement alone
// would replay a stale oversized blob once before it was ever repacked.
func constrainClawPersistReplay(slot, sessionID, fingerprint string, messages []api.Message) ([]byte, []api.Message, int, bool, error) {
	blob, err := marshalClawPersistEnvelope(slot, sessionID, fingerprint, messages)
	if err != nil {
		return nil, nil, 0, false, err
	}
	before := len(blob)
	if before <= clawPersistReplayBlobBytes {
		return blob, messages, before, false, nil
	}

	// Token estimates intentionally favour speed and can undercount a short
	// transcript containing very large nested tool_result payloads. Do not let
	// that blind spot replay hundreds of kilobytes into a later agent handoff:
	// compact the whole conversation to a protocol-safe continuation summary.
	compacted := compactMessagesToolSafe(messages, clawrt.CompactionConfig{
		PreserveRecentMessages: 0,
		MaxEstimatedTokens:     1,
	}, nil)
	if compacted == nil {
		return nil, nil, before, false, fmt.Errorf("%d-byte replay could not compact to %d bytes", before, clawPersistReplayBlobBytes)
	}
	replay, _ := sanitizeToolPairs(compacted.CompactedMessages, nil, false)
	blob, err = marshalClawPersistEnvelope(slot, sessionID, fingerprint, replay)
	if err != nil {
		return nil, nil, before, false, err
	}
	if len(blob) > clawPersistReplayBlobBytes {
		return nil, nil, before, false, fmt.Errorf("%d-byte replay remains above %d-byte limit after compaction", len(blob), clawPersistReplayBlobBytes)
	}
	if len(blob) > clawPersistHardBlobBytes {
		return nil, nil, before, false, fmt.Errorf("%d-byte replay exceeds hard %d-byte storage limit", len(blob), clawPersistHardBlobBytes)
	}
	return blob, replay, before, true, nil
}

func marshalClawPersistEnvelope(slot, sessionID, fingerprint string, messages []api.Message) ([]byte, error) {
	return json.Marshal(clawPersistEnvelope{
		Version:     clawPersistVersion,
		SessionID:   sessionID,
		Slot:        slot,
		Fingerprint: fingerprint,
		Messages:    messages,
	})
}

func (e *ClawExecutor) packLiveSession(ctx context.Context, task *delegate.Task, backend, sessionID string) []byte {
	b, err := e.packSessionFrom(ctx, task, backend, sessionID)
	if err != nil {
		return nil
	}
	return b
}

// unpackInboundSession materialises a packed CLI session from the
// runtime's `_session_state` input key. Unpack failure strips the
// session keys so --resume is never issued against a corrupt pack.
func (e *ClawExecutor) unpackInboundSession(ctx context.Context, input map[string]any, backend string) {
	if input == nil {
		return
	}
	raw, ok := input[delegate.SessionStateKey]
	delete(input, delegate.SessionStateKey)
	if !ok {
		return
	}
	var blob []byte
	switch v := raw.(type) {
	case []byte:
		blob = v
	case string:
		blob = []byte(v)
	default:
		delete(input, delegate.SessionIDKey)
		delete(input, delegate.SessionFingerprintKey)
		return
	}
	sid, _ := input[delegate.SessionIDKey].(string)
	if err := e.UnpackSession(ctx, backend, sid, blob); err != nil {
		delete(input, delegate.SessionIDKey)
		delete(input, delegate.SessionFingerprintKey)
		if e.logger != nil {
			e.logger.Warn("persist: unpack inbound session: %v; running fresh", err)
		}
	}
}
