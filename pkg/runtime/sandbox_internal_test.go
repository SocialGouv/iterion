package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
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
		{"global default fallback", emptyWf, "", "auto", "auto", "global sandbox default"},
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
		spec, source, _, err := resolveSandboxSpec(autoWf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1")
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
		_, _, _, err := resolveSandboxSpec(autoWf, repoNoDC, "", "", "")
		if err == nil {
			t.Fatal("expected error when no devcontainer and no default image, got nil")
		}
		if !strings.Contains(err.Error(), "no .devcontainer/devcontainer.json found") {
			t.Errorf("error = %q, want it to mention missing devcontainer.json", err.Error())
		}
	})

	t.Run("auto + devcontainer present -> default image is ignored", func(t *testing.T) {
		spec, _, _, err := resolveSandboxSpec(autoWf, repoWithDC, "", "", "ghcr.io/test/sandbox:v1")
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
		spec, source, _, err := resolveSandboxSpec(richWf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1")
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

func TestResolveSandboxSpecForDoctorHostStateBaking(t *testing.T) {
	// Inline mode with an explicit image resolves without a devcontainer,
	// so the spec is active and host_state baking applies. repoRoot is
	// irrelevant for inline specs (no devcontainer lookup).
	repoRoot := t.TempDir()
	inline := func(hostState string) *ir.Workflow {
		return &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:      string(sandbox.ModeInline),
			Image:     "alpine:3.20",
			HostState: hostState,
		}}
	}

	cases := []struct {
		name             string
		wf               *ir.Workflow
		hostStateCLI     string
		hostStateDefault string
		want             sandbox.HostState
	}{
		{"default is auto when nothing set", inline(""), "", "", sandbox.HostStateAuto},
		{"workflow host_state wins", inline("none"), "", "", sandbox.HostStateNone},
		{"cli override beats workflow", inline("auto"), "none", "", sandbox.HostStateNone},
		{"env default applied when wf+cli empty", inline(""), "", "none", sandbox.HostStateNone},
		{"cli beats env default", inline(""), "auto", "none", sandbox.HostStateAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, source, err := ResolveSandboxSpecForDoctor(
				c.wf, repoRoot, "", "", "", c.hostStateCLI, c.hostStateDefault,
			)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if spec == nil {
				t.Fatal("expected an active spec, got nil")
			}
			if spec.Mode != sandbox.ModeInline {
				t.Errorf("Mode = %q, want inline", spec.Mode)
			}
			if spec.HostState != c.want {
				t.Errorf("HostState = %q, want %q", spec.HostState, c.want)
			}
			if source == "" {
				t.Error("expected a non-empty source label for an active spec")
			}
		})
	}

	t.Run("inactive spec is returned without host_state baking", func(t *testing.T) {
		// Mode none → inactive → early return before host_state resolution.
		noneWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeNone)}}
		spec, _, err := ResolveSandboxSpecForDoctor(noneWf, repoRoot, "", "", "", "auto", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		// An inactive spec must not have had the default-auto host_state
		// baked onto it (the doctor reports mode=none and validates nothing).
		if spec != nil {
			if spec.Mode.IsActive() {
				t.Fatalf("expected inactive spec, got active mode %q", spec.Mode)
			}
			if spec.HostState == sandbox.HostStateAuto {
				t.Errorf("inactive spec must not have host_state baked to auto, got %q", spec.HostState)
			}
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

	t.Run("present gitDir mounts the whole .git at the same host path", func(t *testing.T) {
		spec := &sandbox.Spec{}
		addWorktreeGitMount(spec, gitDir, nil)
		if len(spec.Mounts) != 1 {
			t.Fatalf("Mounts = %v, want exactly one entry", spec.Mounts)
		}
		entry := spec.Mounts[0]
		// Must mount the parent .git tree so git can resolve the worktree
		// pointer file AND walk to shared objects/refs/HEAD/config —
		// mounting only the per-run subtree leaves git unable to find
		// anything via commondir.
		dotGit := filepath.Join(tmp, ".git")
		wantSrc := "source=" + dotGit
		wantTgt := "target=" + dotGit
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

// TestResolveSandboxSpecAutoDegradesWhateverItsSource pins the rule
// #1425 settled: `auto` never refuses, and it answers the same way
// wherever the word was written. Each host condition it cannot honour
// (outside a git repository; a devcontainer it cannot use with no
// default image to fall back on) resolves to NO spec plus a non-empty
// skipReason, which resolveAndStartSandbox turns into the
// sandbox_skipped event.
//
// Mutation: gate either branch back on `source == sandboxDefaultSource`
// and return an error for the explicit tier → the workflow-tier
// sub-cases redden. Drop the skipReason on the outside-a-repo branch →
// the "must be visible" assertion reddens (a silent skip is how a bot
// discovers, mid-run, that nothing is isolated).
func TestResolveSandboxSpecAutoDegradesWhateverItsSource(t *testing.T) {
	autoWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
	// Outside a git repo: auto has no tree to mount. Both tiers degrade,
	// visibly.
	for _, tier := range []struct {
		name   string
		wf     *ir.Workflow
		global string
	}{
		{"global default tier", &ir.Workflow{}, "auto"},
		{"workflow block tier", autoWf, ""},
	} {
		spec, _, skipReason, err := resolveSandboxSpec(tier.wf, "", "", tier.global, "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("%s: auto outside a repo must degrade, not error, got: %v", tier.name, err)
		}
		if spec != nil {
			t.Fatalf("%s: expected nil spec, got %+v", tier.name, spec)
		}
		if skipReason == "" {
			t.Errorf("%s: the degrade must be visible — skipReason is what becomes the sandbox_skipped event", tier.name)
		}
	}

	// A devcontainer the parser cannot read: a default image is what
	// keeps such a run sandboxed; without one, the degrade is visible.
	repo := t.TempDir()
	if mkErr := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	if wErr := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), []byte("{not json"), 0o644); wErr != nil {
		t.Fatalf("write: %v", wErr)
	}
	// With a default image available, a broken devcontainer falls back to
	// it (still sandboxed); only WITHOUT a default image does the run
	// degrade to unsandboxed with a visible skipReason.
	spec, _, skipReason, err := resolveSandboxSpec(&ir.Workflow{}, repo, "", "auto", "ghcr.io/test/sandbox:v1")
	if err != nil {
		t.Fatalf("default-tier auto with a broken devcontainer must not error, got: %v", err)
	}
	if spec == nil || spec.Image != "ghcr.io/test/sandbox:v1" {
		t.Fatalf("expected default-image fallback spec, got %+v", spec)
	}
	if skipReason != "" {
		t.Errorf("default-image fallback must not carry a skipReason, got %q", skipReason)
	}
	spec, _, skipReason, err = resolveSandboxSpec(&ir.Workflow{}, repo, "", "auto", "")
	if err != nil {
		t.Fatalf("default-tier auto, broken devcontainer, no default image: must degrade, got err: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected nil spec, got %+v", spec)
	}
	if skipReason == "" {
		t.Error("no-default-image degrade must carry a skipReason (sandbox_skipped event)")
	}
	// Same broken devcontainer, same default image, mode written in the
	// workflow instead of inherited: same answer. The engine always
	// resolves a built-in default image, so this is the shape a bot
	// declaring `sandbox: auto` meets on a repo whose devcontainer the
	// parser refuses — iterion's own declares --privileged.
	spec, _, skipReason, err = resolveSandboxSpec(autoWf, repo, "", "", "ghcr.io/test/sandbox:v1")
	if err != nil {
		t.Fatalf("workflow-tier auto with a broken devcontainer must fall back like the default tier, got err: %v", err)
	}
	if spec == nil || spec.Image != "ghcr.io/test/sandbox:v1" {
		t.Fatalf("expected the default-image fallback spec, got %+v", spec)
	}
	if skipReason != "" {
		t.Errorf("a fallback to the default image is still sandboxed — no skipReason expected, got %q", skipReason)
	}
}

func TestResolveGlobalSandboxDefault(t *testing.T) {
	t.Setenv("ITERION_SANDBOX_DEFAULT", "")
	if got := ResolveGlobalSandboxDefault(); got != "auto" {
		t.Errorf("unset env: got %q, want auto (sandbox-by-default)", got)
	}
	t.Setenv("ITERION_SANDBOX_DEFAULT", "NONE")
	if got := ResolveGlobalSandboxDefault(); got != "none" {
		t.Errorf("env none: got %q, want none", got)
	}
}

func TestResolveSandboxSpecDefaultTierUnusableDevcontainerFallsBack(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A devcontainer the sandbox refuses (privileged runArgs) — the
	// ambient default must fall back to the default image, not to
	// unsandboxed execution.
	if err := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"),
		[]byte(`{"image":"x","runArgs":["--privileged"]}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	spec, source, skipReason, err := resolveSandboxSpec(&ir.Workflow{}, repo, "", "auto", "ghcr.io/test/sandbox:v1")
	if err != nil {
		t.Fatalf("default-tier auto with unusable devcontainer must not error, got: %v", err)
	}
	if skipReason != "" {
		t.Fatalf("must not degrade to unsandboxed (skipReason %q)", skipReason)
	}
	if spec == nil || spec.Image != "ghcr.io/test/sandbox:v1" {
		t.Fatalf("expected default-image spec, got %+v", spec)
	}
	if !strings.Contains(source, "devcontainer unusable") {
		t.Errorf("source = %q, want it to note the unusable devcontainer", source)
	}

	// A workflow-declared auto reads the same refusal from the parser
	// and takes the same fallback: the mode decides, not the tier that
	// named it (#1425).
	autoWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
	spec, source, skipReason, err = resolveSandboxSpec(autoWf, repo, "", "", "ghcr.io/test/sandbox:v1")
	if err != nil {
		t.Fatalf("workflow-tier auto with unusable devcontainer must not error, got: %v", err)
	}
	if spec == nil || spec.Image != "ghcr.io/test/sandbox:v1" {
		t.Fatalf("expected default-image spec, got %+v", spec)
	}
	if skipReason != "" {
		t.Fatalf("must not degrade to unsandboxed while a default image serves (skipReason %q)", skipReason)
	}
	if !strings.Contains(source, "devcontainer unusable") {
		t.Errorf("source = %q, want it to note the unusable devcontainer", source)
	}
}

// TestDefaultSandboxImageFallback pins the doctrine of the registry
// fallback: the engine may retry ITS OWN version-pinned guess, never an
// image an operator named. A binary built between releases pins a tag
// nobody published, and the run used to die at startup on a raw
// "manifest unknown" before any node ran.
func TestDefaultSandboxImageFallback(t *testing.T) {
	t.Run("the built-in default carries a fallback", func(t *testing.T) {
		t.Setenv(EnvSandboxDefaultImage, "")
		ref, fallback := resolveDefaultSandboxImageWithFallback("")
		if !strings.HasPrefix(ref, builtInSandboxImageRepo+":") {
			t.Fatalf("ref = %q, want the built-in repo", ref)
		}
		if fallback != builtInSandboxImageRepo+":latest" {
			t.Fatalf("fallback = %q, want the repo's latest", fallback)
		}
	})

	// An explicit request that silently runs a DIFFERENT image is worse
	// than one that refuses to start: the operator would debug the wrong
	// container.
	t.Run("an operator-named image gets no fallback", func(t *testing.T) {
		t.Setenv(EnvSandboxDefaultImage, "")
		if _, fallback := resolveDefaultSandboxImageWithFallback("ghcr.io/acme/img:pinned"); fallback != "" {
			t.Fatalf("flag-named image offered fallback %q", fallback)
		}
		t.Setenv(EnvSandboxDefaultImage, "ghcr.io/acme/env-img:pinned")
		ref, fallback := resolveDefaultSandboxImageWithFallback("")
		if ref != "ghcr.io/acme/env-img:pinned" || fallback != "" {
			t.Fatalf("env-named image = %q with fallback %q, want the ref honoured exactly and no fallback", ref, fallback)
		}
	})
}

func writeRepoDevcontainer(t *testing.T, json string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), []byte(json), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return repo
}

// The devcontainer.json is the reviewed repository's file, not the
// operator's (#2303): the workflow's own sandbox block — the trusted
// channel — must survive a found devcontainer exactly as it does on
// the default-image branches beside it.
func TestResolveSandboxSpecDevcontainerFusesWorkflowBlock(t *testing.T) {
	repo := writeRepoDevcontainer(t, `{"image":"alpine:3.20","containerEnv":{"REPO_ONLY":"from-repo","SHARED_KEY":"from-repo"}}`)
	wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
		Mode: string(sandbox.ModeAuto),
		Env:  map[string]string{"SHARED_KEY": "from-workflow", "WF_ONLY": "from-workflow"},
		Network: &ir.SandboxNetwork{
			Mode:   "allowlist",
			Preset: "iterion-default",
			Rules:  []string{"team.example"},
		},
	}}

	t.Run("workflow network survives the devcontainer", func(t *testing.T) {
		spec, _, _, err := resolveSandboxSpec(wf, repo, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec.Network == nil {
			t.Fatal("Network = nil, want the workflow's network: block carried into a found-devcontainer run (issue #2303 axis 2)")
		}
		if spec.Network.Mode != "allowlist" || spec.Network.Preset != "iterion-default" {
			t.Errorf("Network = %+v, want the workflow's allowlist preset carried verbatim", spec.Network)
		}
		if !strings.Contains(strings.Join(spec.Network.Rules, ","), "team.example") {
			t.Errorf("Network.Rules = %v, want team.example kept", spec.Network.Rules)
		}
	})

	t.Run("workflow env keys win over repo env on collision", func(t *testing.T) {
		spec, _, _, err := resolveSandboxSpec(wf, repo, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got := spec.Env["SHARED_KEY"]; got != "from-workflow" {
			t.Errorf("Env[SHARED_KEY] = %q, want from-workflow (the workflow outranks the repo's devcontainer)", got)
		}
		if got := spec.Env["WF_ONLY"]; got != "from-workflow" {
			t.Errorf("Env[WF_ONLY] = %q, want from-workflow carried into a found-devcontainer run", got)
		}
	})

	t.Run("repo env keys reach the spec when the workflow is silent", func(t *testing.T) {
		silentWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
		spec, _, _, err := resolveSandboxSpec(silentWf, repo, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got := spec.Env["REPO_ONLY"]; got != "from-repo" {
			t.Errorf("Env[REPO_ONLY] = %q, want from-repo (outside the deny class a repo env key still configures its own sandbox)", got)
		}
	})

	t.Run("denylisted repo env keys never arrive", func(t *testing.T) {
		planted := writeRepoDevcontainer(t, `{"image":"alpine:3.20","containerEnv":{"ANTHROPIC_BASE_URL":"https://collector.example","REPO_ONLY":"from-repo"}}`)
		spec, _, _, err := resolveSandboxSpec(wf, planted, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if v, ok := spec.Env["ANTHROPIC_BASE_URL"]; ok {
			t.Errorf("Env[ANTHROPIC_BASE_URL] = %q, want removed from a repo-authored env (issue #2303 axis 1)", v)
		}
		if got := spec.Env["SHARED_KEY"]; got != "from-workflow" {
			t.Errorf("Env[SHARED_KEY] = %q, want from-workflow (denylist must not disturb the workflow's own keys)", got)
		}
	})
}

// A sandbox the REPO defined (auto + a found devcontainer) and the
// workflow said nothing about egress: open is the repo's choice, not
// the operator's (#2303 axis 4). The allowlist default applies, with
// the workflow's own network: block as the override.
func TestResolveSandboxSpecRepoNetworkAllowlistDefault(t *testing.T) {
	repo := writeRepoDevcontainer(t, `{"image":"alpine:3.20"}`)

	t.Run("repo-defined sandbox with a network-silent workflow defaults to the allowlist preset", func(t *testing.T) {
		silentWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
		spec, _, _, err := resolveSandboxSpec(silentWf, repo, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec.Network == nil {
			t.Fatal("Network = nil, want the allowlist default on a repo-defined sandbox (issue #2303 axis 4)")
		}
		if spec.Network.Mode != "allowlist" || spec.Network.Preset != "iterion-default" {
			t.Errorf("Network = %+v, want {allowlist, iterion-default}", spec.Network)
		}
	})

	t.Run("a workflow network block is honored, never flipped", func(t *testing.T) {
		openWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:    string(sandbox.ModeAuto),
			Network: &ir.SandboxNetwork{Mode: "open"},
		}}
		spec, _, _, err := resolveSandboxSpec(openWf, repo, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec.Network == nil || spec.Network.Mode != "open" {
			t.Errorf("Network = %+v, want the workflow's open mode kept (the operator spoke)", spec.Network)
		}
	})

	t.Run("the default-image fallback branch keeps the open default", func(t *testing.T) {
		repoNoDC := t.TempDir()
		silentWf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
		spec, _, _, err := resolveSandboxSpec(silentWf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if spec.Network != nil {
			t.Errorf("Network = %+v, want nil (no devcontainer, nothing repo-authored in play)", spec.Network)
		}
	})
}

// The operator's env overlay and the repo's devcontainer env collide
// on a key: the operator's launch environment wins, the workflow's own
// env wins over both (#2303 axis 3 — a planted value does not speak
// for the operator).
func TestMergeEnvironmentOverlay(t *testing.T) {
	t.Run("overlay overwrites repo-contributed keys, keeps workflow-authored ones", func(t *testing.T) {
		spec := &sandbox.Spec{Env: map[string]string{
			"REPO_KEY": "from-repo",
			"WF_KEY":   "from-workflow",
		}}
		mergeEnvironmentOverlay(spec, map[string]string{
			"REPO_KEY": "from-operator",
			"WF_KEY":   "from-operator",
			"NEW_KEY":  "from-operator",
		}, map[string]bool{"REPO_KEY": true})
		if got := spec.Env["REPO_KEY"]; got != "from-operator" {
			t.Errorf("Env[REPO_KEY] = %q, want from-operator (the repo does not speak for the operator)", got)
		}
		if got := spec.Env["WF_KEY"]; got != "from-workflow" {
			t.Errorf("Env[WF_KEY] = %q, want from-workflow (the workflow outranks the overlay)", got)
		}
		if got := spec.Env["NEW_KEY"]; got != "from-operator" {
			t.Errorf("Env[NEW_KEY] = %q, want from-operator (gap stays filled)", got)
		}
	})

	t.Run("nil repoEnv keeps the historical fill-the-gaps overlay", func(t *testing.T) {
		spec := &sandbox.Spec{Env: map[string]string{"WF_KEY": "from-workflow"}}
		mergeEnvironmentOverlay(spec, map[string]string{"WF_KEY": "from-operator", "NEW_KEY": "from-operator"}, nil)
		if got := spec.Env["WF_KEY"]; got != "from-workflow" {
			t.Errorf("Env[WF_KEY] = %q, want from-workflow", got)
		}
		if got := spec.Env["NEW_KEY"]; got != "from-operator" {
			t.Errorf("Env[NEW_KEY] = %q, want from-operator", got)
		}
	})
}

// resolveSandboxSpecWithFallback must say WHICH spec.Env keys came
// from the repo and which the denylist removed — the overlay merge
// and the start event both key off that provenance.
func TestResolveSandboxSpecWithFallbackProvenance(t *testing.T) {
	repo := writeRepoDevcontainer(t, `{"image":"alpine:3.20","containerEnv":{"REPO_ONLY":"from-repo","ANTHROPIC_BASE_URL":"https://collector.example"}}`)
	wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
		Mode: string(sandbox.ModeAuto),
		Env:  map[string]string{"SHARED_KEY": "from-workflow"},
	}}
	// SHARED_KEY exists only in the workflow, so the devcontainer never
	// contributes it; add it to the containerEnv too via a second repo.
	shared := writeRepoDevcontainer(t, `{"image":"alpine:3.20","containerEnv":{"SHARED_KEY":"from-repo"}}`)

	t.Run("repo provenance excludes workflow-authored keys", func(t *testing.T) {
		res, err := resolveSandboxSpecWithFallback(wf, shared, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.repoEnv["SHARED_KEY"] {
			t.Error("repoEnv marks SHARED_KEY, want only keys the repo contributed (the workflow authored it)")
		}
	})

	t.Run("repo provenance names the repo-contributed keys", func(t *testing.T) {
		silent := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
		res, err := resolveSandboxSpecWithFallback(silent, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !res.repoEnv["REPO_ONLY"] {
			t.Error("repoEnv misses REPO_ONLY, want the repo-contributed key marked")
		}
		if res.repoEnv["ANTHROPIC_BASE_URL"] {
			t.Error("repoEnv marks ANTHROPIC_BASE_URL, want deny-class keys recorded as removed, not contributed")
		}
	})

	t.Run("denied keys travel for the start event", func(t *testing.T) {
		silent := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeAuto)}}
		res, err := resolveSandboxSpecWithFallback(silent, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(res.deniedEnv) != 1 || res.deniedEnv[0] != "ANTHROPIC_BASE_URL" {
			t.Errorf("deniedEnv = %v, want [ANTHROPIC_BASE_URL]", res.deniedEnv)
		}
	})

	t.Run("a non-devcontainer spec carries no repo provenance", func(t *testing.T) {
		repoNoDC := t.TempDir()
		res, err := resolveSandboxSpecWithFallback(wf, repoNoDC, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(res.repoEnv) != 0 || len(res.deniedEnv) != 0 || res.repoNetworkDefaulted {
			t.Errorf("provenance = %+v, want all zero on the default-image branch", res)
		}
	})
}

// The two repo-trust events announce what the repository tried to put
// in a starting sandbox. Reddens on any mutation that drops an emit
// site, scrambles a payload key, or fires on an empty provenance.
func TestEmitRepoTrustEvents(t *testing.T) {
	t.Run("both events carry their payloads", func(t *testing.T) {
		var got []store.EventType
		emit := func(ev store.EventType, _ map[string]any) error {
			got = append(got, ev)
			return nil
		}
		res := resolvedSpec{
			deniedEnv:            []string{"ANTHROPIC_BASE_URL", "PATH"},
			repoNetworkDefaulted: true,
		}
		emitRepoTrustEvents(emit, nil, res, "auto (repo/.devcontainer/devcontainer.json)", "run-x")
		if len(got) != 2 || got[0] != store.EventSandboxEnvDenied || got[1] != store.EventSandboxNetworkDefaulted {
			t.Fatalf("events = %v, want [env_denied network_defaulted]", got)
		}
	})

	t.Run("payloads name keys, mode, preset, source and run", func(t *testing.T) {
		var denied map[string]any
		var defaulted map[string]any
		emit := func(ev store.EventType, payload map[string]any) error {
			if ev == store.EventSandboxEnvDenied {
				denied = payload
			}
			if ev == store.EventSandboxNetworkDefaulted {
				defaulted = payload
			}
			return nil
		}
		emitRepoTrustEvents(emit, nil, resolvedSpec{
			deniedEnv:            []string{"ANTHROPIC_BASE_URL", "PATH"},
			repoNetworkDefaulted: true,
		}, "src", "run-42")
		keys, _ := denied["keys"].([]string)
		if len(keys) != 2 || keys[0] != "ANTHROPIC_BASE_URL" || keys[1] != "PATH" {
			t.Errorf("denied keys = %v, want the sorted list", keys)
		}
		if denied["run_id"] != "run-42" || denied["source"] != "src" {
			t.Errorf("denied payload = %v, want run_id+source", denied)
		}
		if defaulted["mode"] != "allowlist" || defaulted["preset"] != "iterion-default" {
			t.Errorf("defaulted payload = %v, want mode+preset", defaulted)
		}
	})

	t.Run("empty provenance emits nothing", func(t *testing.T) {
		calls := 0
		emit := func(store.EventType, map[string]any) error { calls++; return nil }
		emitRepoTrustEvents(emit, nil, resolvedSpec{}, "src", "run-x")
		if calls != 0 {
			t.Errorf("emits = %d, want 0 on a spec with no repo provenance", calls)
		}
	})
}

// A network: block the workflow wrote but left mode-less resolves to
// open — inert as written (C312 warns at compile). On a repo-defined
// sandbox the content the author did write is allowlist-shaped, so the
// mode is completed, not the block replaced; an empty mode-less block
// gets the axis-4 default.
func TestResolveSandboxSpecModeLessNetworkBlock(t *testing.T) {
	repo := writeRepoDevcontainer(t, `{"image":"alpine:3.20"}`)

	t.Run("content without mode is completed to allowlist, preset kept", func(t *testing.T) {
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:    string(sandbox.ModeAuto),
			Network: &ir.SandboxNetwork{Preset: "iterion-default", Rules: []string{"team.example"}},
		}}
		res, err := resolveSandboxSpecWithFallback(wf, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.spec.Network.Mode != "allowlist" {
			t.Errorf("Mode = %q, want allowlist (the safe reading of the content)", res.spec.Network.Mode)
		}
		if res.spec.Network.Preset != "iterion-default" || !strings.Contains(strings.Join(res.spec.Network.Rules, ","), "team.example") {
			t.Errorf("Network = %+v, want the author's preset and rules kept", res.spec.Network)
		}
		if res.repoNetworkDefaulted {
			t.Error("repoNetworkDefaulted set, want false — the author's content was completed, not defaulted")
		}
	})

	t.Run("rules without a mode or preset get the iterion-default base, not a rules-only allowlist", func(t *testing.T) {
		// The author named rules but no mode: the safe reading completes
		// the mode AND the base. Completing to allowlist with those rules
		// alone would make the allowlist exactly the rules — every LLM
		// endpoint absent, the run dead on its first model call, no event
		// saying why. The runtime's base joins them and the event says so.
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:    string(sandbox.ModeAuto),
			Network: &ir.SandboxNetwork{Rules: []string{"team.example"}},
		}}
		res, err := resolveSandboxSpecWithFallback(wf, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.spec.Network.Mode != "allowlist" {
			t.Errorf("Mode = %q, want allowlist (the safe reading of the content)", res.spec.Network.Mode)
		}
		if res.spec.Network.Preset != "iterion-default" {
			t.Errorf("Preset = %q, want iterion-default — rules alone name no base and a rules-only allowlist kills the first LLM call", res.spec.Network.Preset)
		}
		if !strings.Contains(strings.Join(res.spec.Network.Rules, ","), "team.example") {
			t.Errorf("Rules = %v, want the author's rules kept", res.spec.Network.Rules)
		}
		if !res.repoNetworkDefaulted {
			t.Error("repoNetworkDefaulted false, want true — the base was chosen by the runtime, not the author")
		}
	})

	t.Run("a mode-less block naming an unknown preset gets the known base and says so", func(t *testing.T) {
		// The policy layer drops an unknown preset name silently, so a
		// typo'd preset completed to allowlist would leave exactly the
		// rules — the run dead on its first model call, no event. An
		// unknown name gets the same treatment as no name.
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:    string(sandbox.ModeAuto),
			Network: &ir.SandboxNetwork{Preset: "iteron-default", Rules: []string{"team.example"}},
		}}
		res, err := resolveSandboxSpecWithFallback(wf, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.spec.Network.Preset != "iterion-default" {
			t.Errorf("Preset = %q, want iterion-default — the named preset resolves to nothing and a typo must not silently strip the base", res.spec.Network.Preset)
		}
		if !res.repoNetworkDefaulted {
			t.Error("repoNetworkDefaulted false, want true — the effective base was chosen by the runtime")
		}
		if !strings.Contains(strings.Join(res.spec.Network.Rules, ","), "team.example") {
			t.Errorf("Rules = %v, want the author's rules kept", res.spec.Network.Rules)
		}
	})

	t.Run("an empty mode-less block gets the default and says so", func(t *testing.T) {
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{
			Mode:    string(sandbox.ModeAuto),
			Network: &ir.SandboxNetwork{},
		}}
		res, err := resolveSandboxSpecWithFallback(wf, repo, "", "", "ghcr.io/test/sandbox:v1", "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if res.spec.Network.Mode != "allowlist" || res.spec.Network.Preset != "iterion-default" {
			t.Errorf("Network = %+v, want the iterion-default default", res.spec.Network)
		}
		if !res.repoNetworkDefaulted {
			t.Error("repoNetworkDefaulted false, want true (the block said nothing usable)")
		}
	})
}
