package delegate

import (
	"context"
	"os"
	"path/filepath"
)

// SessionFilesRoot is the on-disk directory the CLI backend actually wrote
// (or will write) its session transcripts into. Pack/unpack/HasSession must
// use this, not the iterion process environment: CLAUDE_CONFIG_DIR and
// CODEX_HOME are set on the subprocess only (forfait temp dir, sandbox
// config dir), and pi lives under Task.StateDir.
func SessionFilesRoot(ctx context.Context, task Task, backend string) string {
	switch backend {
	case BackendClaudeCode:
		env := anthropicCredEnvForCLI(ctx, task.ProviderHint, !task.Hostless())
		// A forfait-suppressed env carries a POISONED CLAUDE_CONFIG_DIR
		// that must NOT become a session root: writing transcripts
		// under the path we chose because it does not exist is a
		// filesystem error at best, a surprise directory on a laptop
		// that happens to have `/nonexistent/…` at worst. Fall through
		// to the ambient / home default (R0a39d6).
		if env != nil && !isForfaitSuppressed(env) {
			if d := env["CLAUDE_CONFIG_DIR"]; d != "" {
				return d
			}
		}
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return d
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".claude")
	case BackendCodex:
		if env := codexCredEnvForCLI(ctx); env != nil {
			if d := env["CODEX_HOME"]; d != "" {
				return d
			}
		}
		if d := os.Getenv("CODEX_HOME"); d != "" {
			return d
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".codex")
	case BackendPi:
		root, _ := task.StateDir(BackendPi)
		return root
	default:
		return ""
	}
}
