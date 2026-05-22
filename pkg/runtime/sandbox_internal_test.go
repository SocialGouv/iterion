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

	t.Run("auto + no devcontainer + default image -> block Mounts/Env/PostCreate carry through", func(t *testing.T) {
		richWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode: string(sandbox.ModeAuto),
			Mounts: []string{
				"type=bind,source=${localEnv:HOME}/.claude,target=/root/.claude",
			},
			Env:             map[string]string{"CLAUDE_CONFIG_DIR": "/root/.claude"},
			PostCreate:      "npm install -g @anthropic-ai/claude-code@latest",
			User:            "node",
			WorkspaceFolder: "/workspace",
		}}
		spec, source, err := resolveSandboxSpec(richWf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1")
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
		if len(spec.Mounts) != 1 {
			t.Fatalf("Mounts = %v, want 1 entry", spec.Mounts)
		}
		homeDir, _ := os.UserHomeDir()
		wantMount := "type=bind,source=" + homeDir + "/.claude,target=/root/.claude"
		if spec.Mounts[0] != wantMount {
			t.Errorf("Mounts[0] = %q, want %q (expandSandboxSpec should resolve ${localEnv:HOME})", spec.Mounts[0], wantMount)
		}
		if spec.Env["CLAUDE_CONFIG_DIR"] != "/root/.claude" {
			t.Errorf("Env[CLAUDE_CONFIG_DIR] = %q, want /root/.claude", spec.Env["CLAUDE_CONFIG_DIR"])
		}
		if spec.PostCreate != "npm install -g @anthropic-ai/claude-code@latest" {
			t.Errorf("PostCreate = %q, want the npm install string", spec.PostCreate)
		}
		if spec.User != "node" {
			t.Errorf("User = %q, want node", spec.User)
		}
		if spec.WorkspaceFolder != "/workspace" {
			t.Errorf("WorkspaceFolder = %q, want /workspace", spec.WorkspaceFolder)
		}
		if !strings.Contains(source, "default image: ghcr.io/test/sandbox:v1") {
			t.Errorf("source = %q, want it to mention the default image", source)
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

func TestPickHostState(t *testing.T) {
	cases := []struct {
		name       string
		wf         string
		cli        string
		global     string
		wantMode   string
		wantSource string
	}{
		{"cli wins over everything", "auto", "none", "auto", "none", "cli flag --sandbox-host-state"},
		{"workflow wins over env", "none", "", "auto", "none", "workflow sandbox.host_state"},
		{"env when nothing else", "", "", "none", "none", "ITERION_SANDBOX_HOST_STATE"},
		{"default is auto", "", "", "", "auto", "default"},
		{"cli auto over default", "", "auto", "", "auto", "cli flag --sandbox-host-state"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMode, gotSource := pickHostState(c.wf, c.cli, c.global)
			if gotMode != c.wantMode {
				t.Errorf("mode = %q, want %q", gotMode, c.wantMode)
			}
			if !strings.HasPrefix(gotSource, c.wantSource) {
				t.Errorf("source = %q, want prefix %q", gotSource, c.wantSource)
			}
		})
	}
}

func TestPathContains(t *testing.T) {
	tmp := t.TempDir()
	parent := tmp
	child := filepath.Join(tmp, "nested", "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !pathContains(parent, child) {
		t.Errorf("expected parent %q to contain child %q", parent, child)
	}
	if !pathContains(parent, parent) {
		t.Errorf("expected pathContains(x, x) to be true")
	}
	if pathContains(child, parent) {
		t.Errorf("expected child %q to NOT contain parent %q", child, parent)
	}
	if pathContains("", parent) || pathContains(parent, "") {
		t.Errorf("empty-string operands must return false")
	}
}

func TestParseUserUID(t *testing.T) {
	cases := []struct {
		input  string
		want   int
		wantOK bool
	}{
		{"", 0, false},
		{"node", 0, false},
		{"1000", 1000, true},
		{"1000:1000", 1000, true},
		{"500:600", 500, true},
		{"abc:1000", 0, false},
		{"1000abc", 0, false}, // strict-numeric: no trailing junk
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			got, ok := parseUserUID(c.input)
			if ok != c.wantOK {
				t.Errorf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("uid = %d, want %d", got, c.want)
			}
		})
	}
}

func TestResolveWorktreeGitDir(t *testing.T) {
	cases := []struct {
		name     string
		repoRoot string
		wtPath   string
		want     string
	}{
		{
			name:     "happy path: derives <repoRoot>/.git/worktrees/<basename>",
			repoRoot: "/srv/repo",
			wtPath:   "/var/iterion/worktrees/run-abc",
			want:     "/srv/repo/.git/worktrees/run-abc",
		},
		{
			name:     "matches git's actual layout from a live run",
			repoRoot: "/home/jo/lab/ai/iterion",
			wtPath:   "/home/jo/.iterion/worktrees/019e4e6c-03b5-7ddb-9c48-f80ec7403fbe",
			want:     "/home/jo/lab/ai/iterion/.git/worktrees/019e4e6c-03b5-7ddb-9c48-f80ec7403fbe",
		},
		{"empty repoRoot returns empty", "", "/wt/x", ""},
		{"empty wtPath returns empty", "/srv/repo", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveWorktreeGitDir(c.repoRoot, c.wtPath)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestAddWorktreeGitMount(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, ".git", "worktrees", "run-abc")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir gitDir: %v", err)
	}

	t.Run("empty gitDir is a silent no-op", func(t *testing.T) {
		spec := &sandbox.Spec{}
		addWorktreeGitMount(spec, "", nil)
		if len(spec.Mounts) != 0 {
			t.Errorf("Mounts = %v, want empty", spec.Mounts)
		}
	})

	t.Run("missing gitDir on disk is a silent no-op", func(t *testing.T) {
		spec := &sandbox.Spec{}
		addWorktreeGitMount(spec, filepath.Join(tmp, "does", "not", "exist"), nil)
		if len(spec.Mounts) != 0 {
			t.Errorf("Mounts = %v, want empty", spec.Mounts)
		}
	})

	t.Run("present gitDir appends a same-path read-write bind", func(t *testing.T) {
		spec := &sandbox.Spec{}
		addWorktreeGitMount(spec, gitDir, nil)
		if len(spec.Mounts) != 1 {
			t.Fatalf("Mounts = %v, want exactly one entry", spec.Mounts)
		}
		entry := spec.Mounts[0]
		// Must mount at the SAME absolute host path so the worktree's
		// `.git` pointer file (which embeds the host path) resolves
		// from inside the container.
		wantSrc := "source=" + gitDir
		wantTgt := "target=" + gitDir
		if !strings.Contains(entry, wantSrc) {
			t.Errorf("Mounts[0] = %q, want substring %q", entry, wantSrc)
		}
		if !strings.Contains(entry, wantTgt) {
			t.Errorf("Mounts[0] = %q, want substring %q", entry, wantTgt)
		}
		if !strings.Contains(entry, "type=bind") {
			t.Errorf("Mounts[0] = %q, want type=bind", entry)
		}
		// Read-write: must NOT carry `readonly`. Git needs to write
		// HEAD, refs, packed-refs, index when committing.
		if strings.Contains(entry, "readonly") {
			t.Errorf("Mounts[0] = %q must be read-write (no readonly token)", entry)
		}
	})

	t.Run("only the per-run gitdir is mounted, not the whole .git", func(t *testing.T) {
		// Regression guard: the previous implementation mounted the
		// entire <repoRoot>/.git tree, exposing every other concurrent
		// run's worktree state. Verify the new behaviour stays scoped.
		spec := &sandbox.Spec{}
		addWorktreeGitMount(spec, gitDir, nil)
		if len(spec.Mounts) != 1 {
			t.Fatalf("Mounts = %v, want one entry", spec.Mounts)
		}
		// The bind target must contain the run-id segment, not bare ".git".
		if !strings.Contains(spec.Mounts[0], "/worktrees/run-abc") {
			t.Errorf("Mounts[0] = %q should be scoped to the per-run gitdir, not the whole .git", spec.Mounts[0])
		}
		// Must NOT contain a bare bind on the parent .git (would expose
		// other runs).
		bareGit := "source=" + filepath.Join(tmp, ".git") + ","
		if strings.Contains(spec.Mounts[0], bareGit) {
			t.Errorf("Mounts[0] = %q must not bind the whole .git", spec.Mounts[0])
		}
	})
}

func TestCollectHostStateMounts(t *testing.T) {
	tmp := t.TempDir()
	iterionHome := filepath.Join(tmp, "iter-home")
	claudeDir := filepath.Join(tmp, "claude-home")
	workspace := filepath.Join(tmp, "workspace")
	for _, d := range []string{iterionHome, claudeDir, workspace} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	t.Run("both present, disjoint workspace -> both mounted", func(t *testing.T) {
		mounts := collectHostStateMounts(workspace, iterionHome, claudeDir)
		if len(mounts) != 2 {
			t.Fatalf("got %d mounts, want 2", len(mounts))
		}
		for _, m := range mounts {
			if m.HostPath != m.ContainerPath {
				t.Errorf("HostPath %q != ContainerPath %q (must mount at same absolute path)", m.HostPath, m.ContainerPath)
			}
		}
	})

	t.Run("missing host dir skipped silently", func(t *testing.T) {
		missing := filepath.Join(tmp, "does-not-exist")
		mounts := collectHostStateMounts(workspace, missing, claudeDir)
		if len(mounts) != 1 {
			t.Errorf("got %d mounts, want 1 (only claude)", len(mounts))
		}
	})

	t.Run("workspace contains iterion home -> skipped", func(t *testing.T) {
		// project-local .iterion case: store lives inside workspace
		nested := filepath.Join(workspace, ".iterion")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir nested: %v", err)
		}
		mounts := collectHostStateMounts(workspace, nested, claudeDir)
		// Only claude should be mounted; nested is shadowed by workspace.
		if len(mounts) != 1 {
			t.Fatalf("got %d mounts, want 1", len(mounts))
		}
		if mounts[0].HostPath != claudeDir {
			t.Errorf("expected claudeDir, got %q", mounts[0].HostPath)
		}
	})

	t.Run("empty paths short-circuit", func(t *testing.T) {
		mounts := collectHostStateMounts(workspace, "", "")
		if len(mounts) != 0 {
			t.Errorf("got %d mounts, want 0", len(mounts))
		}
	})
}
