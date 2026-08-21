package model

import (
	"context"
	"os"
	"path/filepath"

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

func (e *ClawExecutor) sessionRoot(backend string) string {
	switch backend {
	case delegate.BackendClaudeCode:
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return d
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".claude")
	case delegate.BackendCodex:
		if d := os.Getenv("CODEX_HOME"); d != "" {
			return d
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".codex")
	case delegate.BackendPi:
		if e.workDir != "" {
			return filepath.Join(e.workDir, ".iterion", "pi")
		}
		return ""
	default:
		return ""
	}
}

func (e *ClawExecutor) PackSession(_ context.Context, backend, sessionID string) ([]byte, error) {
	root := e.sessionRoot(backend)
	if root == "" || sessionID == "" {
		return nil, os.ErrNotExist
	}
	files, err := sessionpack.CollectBySessionID(root, sessionID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, os.ErrNotExist
	}
	return sessionpack.Pack(sessionpack.Header{Backend: backend, SessionID: sessionID}, files)
}

func (e *ClawExecutor) UnpackSession(_ context.Context, backend, sessionID string, blob []byte) error {
	root := e.sessionRoot(backend)
	if root == "" {
		return os.ErrNotExist
	}
	return sessionpack.Unpack(blob, sessionpack.Header{Backend: backend, SessionID: sessionID}, root)
}

func (e *ClawExecutor) HasSession(_ context.Context, backend, sessionID string) bool {
	root := e.sessionRoot(backend)
	if root == "" || sessionID == "" {
		return false
	}
	return sessionpack.HasFile(root, sessionID)
}

func (e *ClawExecutor) packLiveSession(backend, sessionID string) []byte {
	b, err := e.PackSession(context.Background(), backend, sessionID)
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
