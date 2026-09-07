package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestExportForfaitConfigDirs(t *testing.T) {
	t.Run("nothing mounted leaves the env untouched", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, false, false)
		if spec.Env != nil {
			t.Fatalf("env = %v, want nil", spec.Env)
		}
	})

	t.Run("claude forfait alone exports only CLAUDE_CONFIG_DIR", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, true, false)
		if got := spec.Env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
			t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, secrets.ClaudeCodeSandboxConfigDir)
		}
		if _, set := spec.Env["CODEX_HOME"]; set {
			t.Fatalf("CODEX_HOME must not be set without a codex forfait: %v", spec.Env)
		}
	})

	t.Run("both forfaits export both dirs", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, true, true)
		if spec.Env["CLAUDE_CONFIG_DIR"] != secrets.ClaudeCodeSandboxConfigDir || spec.Env["CODEX_HOME"] != secrets.CodexSandboxConfigDir {
			t.Fatalf("env = %v", spec.Env)
		}
	})

	t.Run("an operator-declared value wins", func(t *testing.T) {
		spec := sandbox.Spec{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/operator/claude", "PATH": "/bin"}}
		exportForfaitConfigDirs(&spec, true, true)
		if spec.Env["CLAUDE_CONFIG_DIR"] != "/operator/claude" {
			t.Fatalf("operator value overwritten: %v", spec.Env)
		}
		if spec.Env["CODEX_HOME"] != secrets.CodexSandboxConfigDir || spec.Env["PATH"] != "/bin" {
			t.Fatalf("env = %v", spec.Env)
		}
	})
}
