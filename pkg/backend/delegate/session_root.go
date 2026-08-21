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
		if env := anthropicCredEnvForCLI(ctx, task.ProviderHint, !task.Hostless()); env != nil {
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
