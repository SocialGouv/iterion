package runtime

import (
	"bytes"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestExportForfaitConfigDirs(t *testing.T) {
	t.Run("nothing mounted leaves the env untouched", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, nil, false, false)
		if spec.Env != nil {
			t.Fatalf("env = %v, want nil", spec.Env)
		}
	})

	t.Run("claude forfait alone exports only CLAUDE_CONFIG_DIR", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, nil, true, false)
		if got := spec.Env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
			t.Fatalf("CLAUDE_CONFIG_DIR = %q, want %q", got, secrets.ClaudeCodeSandboxConfigDir)
		}
		if _, set := spec.Env["CODEX_HOME"]; set {
			t.Fatalf("CODEX_HOME must not be set without a codex forfait: %v", spec.Env)
		}
	})

	t.Run("both forfaits export both dirs", func(t *testing.T) {
		var spec sandbox.Spec
		exportForfaitConfigDirs(&spec, nil, true, true)
		if spec.Env["CLAUDE_CONFIG_DIR"] != secrets.ClaudeCodeSandboxConfigDir || spec.Env["CODEX_HOME"] != secrets.CodexSandboxConfigDir {
			t.Fatalf("env = %v", spec.Env)
		}
	})

	// A config dir is the WEAKEST channel the CLI reads: a key on the
	// container env outranks it, and a forfait that silently does not
	// authenticate the work is the failure this path exists to end.
	t.Run("a credential that outranks the dir is said out loud", func(t *testing.T) {
		for _, c := range []struct {
			name, key, want string
			claude, codex   bool
		}{
			{name: "anthropic key beats the claude dir", key: "ANTHROPIC_API_KEY", want: "ANTHROPIC_API_KEY", claude: true},
			{name: "anthropic token beats it too", key: "ANTHROPIC_AUTH_TOKEN", want: "ANTHROPIC_AUTH_TOKEN", claude: true},
			{name: "openai key beats CODEX_HOME", key: "OPENAI_API_KEY", want: "OPENAI_API_KEY", codex: true},
		} {
			t.Run(c.name, func(t *testing.T) {
				var buf bytes.Buffer
				spec := sandbox.Spec{Env: map[string]string{c.key: "secret-by-reference"}}
				exportForfaitConfigDirs(&spec, iterlog.New(iterlog.LevelDebug, &buf), c.claude, c.codex)
				if !strings.Contains(buf.String(), c.want) {
					t.Errorf("the run's forfait is not what authenticates and nothing said so:\n%s", buf.String())
				}
				if strings.Contains(buf.String(), "secret-by-reference") {
					t.Errorf("the credential VALUE reached the log:\n%s", buf.String())
				}
			})
		}
	})

	// The other half: no credential on the env, nothing to warn about. A
	// warning that always fires teaches operators to ignore it.
	t.Run("no competing credential, no warning", func(t *testing.T) {
		var buf bytes.Buffer
		spec := sandbox.Spec{Env: map[string]string{"PATH": "/bin"}}
		exportForfaitConfigDirs(&spec, iterlog.New(iterlog.LevelDebug, &buf), true, true)
		if strings.Contains(buf.String(), "authenticate") {
			t.Errorf("warned with nothing to warn about:\n%s", buf.String())
		}
	})

	t.Run("an operator-declared value wins", func(t *testing.T) {
		spec := sandbox.Spec{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/operator/claude", "PATH": "/bin"}}
		exportForfaitConfigDirs(&spec, nil, true, true)
		if spec.Env["CLAUDE_CONFIG_DIR"] != "/operator/claude" {
			t.Fatalf("operator value overwritten: %v", spec.Env)
		}
		if spec.Env["CODEX_HOME"] != secrets.CodexSandboxConfigDir || spec.Env["PATH"] != "/bin" {
			t.Fatalf("env = %v", spec.Env)
		}
	})
}
