package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

func TestPickMode(t *testing.T) {
	inlineWf := &ir.Workflow{
		Sandbox: &ir.SandboxSpec{
			Mode:  string(sandbox.ModeInline),
			Image: "alpine:3.20",
		},
	}
	autoWf := &ir.Workflow{
		Sandbox: &ir.SandboxSpec{
			Mode: string(sandbox.ModeAuto),
		},
	}
	emptyWf := &ir.Workflow{}

	cases := []struct {
		name       string
		wf         *ir.Workflow
		cli        string
		global     string
		wantMode   string
		wantSource string
	}{
		{"cli none beats workflow", inlineWf, "none", "", "none", "cli flag --sandbox"},
		{"cli auto loses to inline workflow block", inlineWf, "auto", "", "inline", "workflow sandbox: block (overrides --sandbox=auto)"},
		{"cli auto wins over auto workflow (no contradiction)", autoWf, "auto", "", "auto", "cli flag --sandbox"},
		{"cli auto on empty workflow", emptyWf, "auto", "", "auto", "cli flag --sandbox"},
		{"workflow inline wins when no cli", inlineWf, "", "auto", "inline", "workflow sandbox: block"},
		{"global default fallback", emptyWf, "", "auto", "auto", "ITERION_SANDBOX_DEFAULT"},
		{"nil workflow + cli", nil, "auto", "", "auto", "cli flag --sandbox"},
		{"nothing set", emptyWf, "", "", "", "default (no sandbox)"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMode, gotSource := pickMode(c.wf, c.cli, c.global)
			if gotMode != c.wantMode {
				t.Errorf("mode = %q, want %q", gotMode, c.wantMode)
			}
			if !strings.HasPrefix(gotSource, c.wantSource) {
				t.Errorf("source = %q, want prefix %q", gotSource, c.wantSource)
			}
		})
	}
}

func TestResolveSandboxSpecAutoFallbackToDefaultImage(t *testing.T) {
	repoNoDC := t.TempDir()
	repoWithDC := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoWithDC, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(repoWithDC, ".devcontainer", "devcontainer.json"),
		[]byte(`{"image":"alpine:3.20"}`),
		0o644,
	); err != nil {
		t.Fatalf("write: %v", err)
	}

	autoWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}

	t.Run("auto + no devcontainer + default image -> synthetic spec", func(t *testing.T) {
		spec, source, err := resolveSandboxSpec(autoWf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec == nil {
			t.Fatal("expected spec, got nil")
		}
		if spec.Image != "ghcr.io/test/sandbox:v1" {
			t.Errorf("Image = %q, want ghcr.io/test/sandbox:v1", spec.Image)
		}
		if spec.Mode != sandbox.ModeAuto {
			t.Errorf("Mode = %q, want auto", spec.Mode)
		}
		if !strings.Contains(source, "default image: ghcr.io/test/sandbox:v1") {
			t.Errorf("source = %q, want it to mention the default image", source)
		}
	})

	t.Run("auto + no devcontainer + empty default -> historical error", func(t *testing.T) {
		_, _, err := resolveSandboxSpec(autoWf, repoNoDC, "", "", "")
		if err == nil {
			t.Fatal("expected error when no devcontainer and no default image, got nil")
		}
		if !strings.Contains(err.Error(), "no .devcontainer/devcontainer.json found") {
			t.Errorf("error = %q, want it to mention missing devcontainer.json", err.Error())
		}
	})

	t.Run("auto + devcontainer present -> default image is ignored", func(t *testing.T) {
		spec, _, err := resolveSandboxSpec(autoWf, repoWithDC, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec == nil {
			t.Fatal("expected spec, got nil")
		}
		if spec.Image != "alpine:3.20" {
			t.Errorf("Image = %q, want alpine:3.20 (devcontainer wins over default image)", spec.Image)
		}
	})
}

func TestResolveDefaultSandboxImage(t *testing.T) {
	t.Setenv(EnvSandboxDefaultImage, "")

	t.Run("flag override wins", func(t *testing.T) {
		t.Setenv(EnvSandboxDefaultImage, "from-env:tag")
		got := resolveDefaultSandboxImage("from-flag:tag")
		if got != "from-flag:tag" {
			t.Errorf("got %q, want from-flag:tag", got)
		}
	})

	t.Run("env wins over built-in when no flag", func(t *testing.T) {
		t.Setenv(EnvSandboxDefaultImage, "from-env:tag")
		got := resolveDefaultSandboxImage("")
		if got != "from-env:tag" {
			t.Errorf("got %q, want from-env:tag", got)
		}
	})

	t.Run("built-in fallback when neither set", func(t *testing.T) {
		t.Setenv(EnvSandboxDefaultImage, "")
		got := resolveDefaultSandboxImage("")
		if !strings.HasPrefix(got, "ghcr.io/socialgouv/iterion-sandbox-slim:") {
			t.Errorf("got %q, want a ghcr.io/socialgouv/iterion-sandbox-slim:* ref", got)
		}
	})
}
