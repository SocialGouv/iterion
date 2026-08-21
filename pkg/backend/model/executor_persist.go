package model

import (
	"context"
	"os"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/sessionpack"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func (e *ClawExecutor) SessionResumeCapability(node ir.Node) (string, bool) {
	name := e.EffectiveBackendName(node)
	switch name {
	case delegate.BackendClaudeCode, delegate.BackendPi, delegate.BackendCodex:
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
	t := e.packerTask()
	root := delegate.SessionFilesRoot(ctx, t, backend)
	if root == "" {
		return os.ErrNotExist
	}
	return sessionpack.Unpack(blob, sessionpack.Header{Backend: backend, SessionID: sessionID}, root)
}

func (e *ClawExecutor) HasSession(ctx context.Context, backend, sessionID string) bool {
	t := e.packerTask()
	root := delegate.SessionFilesRoot(ctx, t, backend)
	if root == "" || sessionID == "" {
		return false
	}
	return sessionpack.HasFile(root, sessionID, backend)
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
