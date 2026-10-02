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
		// A forfait-suppressed env (a facade hint with no key for that
		// facade) names NO session root: its CLI runs with a POISONED
		// CLAUDE_CONFIG_DIR it cannot write transcripts under — the
		// node is refused on "no credential" before any
		// transcript exists — and the ambient default here would point
		// pack/unpack/HasSession at the OPERATOR'S OWN config dir for
		// a session the run never wrote. Every caller degrades on ""
		// (pack ErrNotExist, unpack ErrNotExist, HasSession false).
		if env != nil && isForfaitSuppressed(env) {
			return ""
		}
		if env != nil {
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
