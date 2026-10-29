package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2069 — host_state: auto must not let a sandboxed agent write host
// configuration the host later executes. The guard shadows those paths with
// per-run copies (files the sandbox may write) and read-only overlays
// (directories it never writes), audited on sandbox_host_state_mounted.

// guardTestHome builds a fake operator home carrying every config the guard
// protects, points HOME + ITERION_HOME at it, and returns the paths.
func guardTestHome(t *testing.T) (home, iterionHome string) {
	t.Helper()
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	home = t.TempDir()
	iterionHome = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ITERION_HOME", iterionHome)
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(home, ".gitconfig"), "[user]\n\tname = Op\n")
	mustWrite(filepath.Join(home, ".claude.json"), `{"projects":{}}`)
	mustWrite(filepath.Join(home, ".claude", "settings.json"), `{"hooks":{}}`)
	mustWrite(filepath.Join(home, ".claude", "settings.local.json"), "{}")
	mustWrite(filepath.Join(home, ".claude", "plugins", "good", "plugin.yaml"), "name: good")
	mustWrite(filepath.Join(home, ".codex", "config.toml"), "model = \"gpt\"")
	mustWrite(filepath.Join(iterionHome, "plugins.yaml"), "enable: [rtk]")
	mustWrite(filepath.Join(iterionHome, "plugins", "rtk", "plugin.yaml"), "name: rtk")
	return home, iterionHome
}

// bindEntry finds the mount string whose target= field is exactly path.
func bindEntry(mounts []string, path string) string {
	for _, m := range mounts {
		for _, field := range strings.Split(m, ",") {
			if field == "target="+path {
				return m
			}
		}
	}
	return ""
}

// TestHostStateGuardShadowsExecutableHostConfig is the ticket's witness: none
// of the host-executed configuration is reachable read-WRITE through a bind
// whose source IS the host path — each is a per-run copy or a read-only
// overlay — and the sandbox_host_state_mounted event audits every shadow.
// Without the guard the first assertion set fails on every path.
func TestHostStateGuardShadowsExecutableHostConfig(t *testing.T) {
	home, iterionHome := guardTestHome(t)

	var event map[string]any
	emit := func(et store.EventType, data map[string]any) error {
		if et == store.EventSandboxHostStateMounted {
			event = data
		}
		return nil
	}
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard, emit, iterlog.Nop())

	// Files the sandbox may write bind a per-run COPY: source != host path,
	// content seeded from it, no readonly flag (writes succeed — into the
	// copy, which dies with the sandbox).
	copies := []string{
		filepath.Join(home, ".gitconfig"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".claude", "settings.local.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(iterionHome, "plugins.yaml"),
	}
	for _, hostPath := range copies {
		entry := bindEntry(spec.Mounts, hostPath)
		if entry == "" {
			t.Errorf("no bind targets %q at all", hostPath)
			continue
		}
		if strings.Contains(entry, "source="+hostPath) {
			t.Errorf("%q is bound from the HOST file — a sandboxed write lands on the host's config: %s", hostPath, entry)
		}
		if strings.Contains(entry, "readonly") {
			t.Errorf("%q must stay writable (a copy), not read-only: %s", hostPath, entry)
		}
	}
	// The copy carries the operator's content: reads see the real config.
	gitconfigEntry := bindEntry(spec.Mounts, filepath.Join(home, ".gitconfig"))
	src := strings.Split(strings.Split(gitconfigEntry, "source=")[1], ",")[0]
	content, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("the gitconfig copy does not read: %v", err)
	}
	if !strings.Contains(string(content), "name = Op") {
		t.Errorf("the gitconfig copy holds %q, want the operator's content", content)
	}
	if !strings.HasPrefix(src, guard.dir) {
		t.Errorf("the copy %q does not live in the per-run guard scratch %q", src, guard.dir)
	}

	// Directories the sandbox never writes bind READ-ONLY.
	for _, dir := range []string{
		filepath.Join(home, ".claude", "plugins"),
		filepath.Join(iterionHome, "plugins"),
	} {
		entry := bindEntry(spec.Mounts, dir)
		if entry == "" || !strings.Contains(entry, "readonly") {
			t.Errorf("%q must be a read-only overlay: %q", dir, entry)
		}
		if !strings.Contains(entry, "source="+dir) {
			t.Errorf("%q should still READ the host tree: %q", dir, entry)
		}
	}

	// The audit: every shadow rides the event.
	guarded, _ := event["guarded"].([]string)
	for _, hostPath := range append(copies,
		filepath.Join(home, ".claude", "plugins"),
		filepath.Join(iterionHome, "plugins")) {
		found := false
		for _, g := range guarded {
			if strings.HasSuffix(g, ":"+hostPath) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the audit event does not name %q; guarded=%v", hostPath, guarded)
		}
	}

	// The copies die with the sandbox.
	scratch := guard.dir
	guard.cleanup()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("guard scratch %s survives cleanup: %v", scratch, err)
	}
}

// A host file that does NOT exist yet must not become plantable through the
// read-write parent bind: the guard seeds it empty on the host (user-owned,
// never docker's root-owned mountpoint) and shadows it with a copy.
func TestHostStateGuardSeedsAbsentConfigFiles(t *testing.T) {
	home, _ := guardTestHome(t)
	// Remove the files a minimal home would not have.
	for _, p := range []string{
		filepath.Join(home, ".claude", "settings.local.json"),
		filepath.Join(home, ".codex", "config.toml"),
	} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	// And a home whose plugins dir never existed.
	if err := os.RemoveAll(filepath.Join(home, ".claude", "plugins")); err != nil {
		t.Fatal(err)
	}

	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, p := range []string{
		filepath.Join(home, ".claude", "settings.local.json"),
		filepath.Join(home, ".codex", "config.toml"),
	} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("the absent %s was not seeded: %v — the path stays plantable", p, err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("seeded %s mode = %o, want 0600", p, info.Mode().Perm())
		}
		entry := bindEntry(spec.Mounts, p)
		if entry == "" || strings.Contains(entry, "source="+p) {
			t.Errorf("absent %s must be shadowed by a copy once seeded: %q", p, entry)
		}
	}
	pluginsDir := filepath.Join(home, ".claude", "plugins")
	if info, err := os.Stat(pluginsDir); err != nil || !info.IsDir() {
		t.Errorf("the absent plugins dir was not created: %v", err)
	}
	if entry := bindEntry(spec.Mounts, pluginsDir); entry == "" || !strings.Contains(entry, "readonly") {
		t.Errorf("absent plugins dir must get an empty read-only overlay: %q", entry)
	}
}

// The escape hatch: ITERION_SANDBOX_HOST_STATE_GUARD=off restores the
// pre-guard all-writable posture, explicitly, for the operator whose bots
// deliberately manage host config.
func TestHostStateGuardEscapeHatch(t *testing.T) {
	t.Setenv(HostStateGuardEnvVar, "off")
	if g := newHostConfigGuard(iterlog.Nop()); g != nil {
		t.Fatal("the hatch is off — no guard")
	}
	home, _ := guardTestHome(t)
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, nil,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	entry := bindEntry(spec.Mounts, filepath.Join(home, ".gitconfig"))
	if entry == "" || !strings.Contains(entry, "source="+filepath.Join(home, ".gitconfig")) {
		t.Errorf("guard off: ~/.gitconfig must bind the host file read-write again, got %q", entry)
	}
}

// The worktree `.git` bind is the same host-execution class (#2069): the
// guard shadows config with a copy and hooks/ with a read-only overlay,
// created empty when the repo has none so the agent cannot plant it.
// Applied from applyHostStateMounts (via SandboxParams.WorktreeGitDir) so
// the shadows ride the one audit event — and AFTER the HOME-tmpfs
// layering, which a $HOME-nested repository would otherwise derail.
func TestWorktreeGitMountGuardsConfigAndHooks(t *testing.T) {
	guardTestHome(t)
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git", "worktrees", "run-1")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dotGit := filepath.Join(repo, ".git")
	if err := os.WriteFile(filepath.Join(dotGit, "config"), []byte("[core]\n\trepositoryformatversion = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately NO hooks dir: the common fresh-clone-after-prune shape.

	guard := newHostConfigGuard(iterlog.Nop())
	var event map[string]any
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{},
		SandboxParams{WorkspacePath: t.TempDir(), WorktreeGitDir: gitDir}, guard,
		func(et store.EventType, data map[string]any) error {
			if et == store.EventSandboxHostStateMounted {
				event = data
			}
			return nil
		}, iterlog.Nop())

	// The tree bind itself is addWorktreeGitMount's, appended later — the
	// overlays are in place regardless, and docker's depth ordering gives
	// them the win (measured).
	cfgPath := filepath.Join(dotGit, "config")
	cfgEntry := bindEntry(spec.Mounts, cfgPath)
	if cfgEntry == "" || strings.Contains(cfgEntry, "source="+cfgPath) {
		t.Errorf(".git/config must bind a per-run copy: %q", cfgEntry)
	}
	hooksDir := filepath.Join(dotGit, "hooks")
	if info, err := os.Stat(hooksDir); err != nil || !info.IsDir() {
		t.Errorf("hooks dir not created for the overlay: %v", err)
	}
	hooksEntry := bindEntry(spec.Mounts, hooksDir)
	if hooksEntry == "" || !strings.Contains(hooksEntry, "readonly") {
		t.Errorf(".git/hooks must be a read-only overlay: %q", hooksEntry)
	}
	// Audited with the host-state shadows, on the one event.
	guarded, _ := event["guarded"].([]string)
	var foundCfg, foundHooks bool
	for _, g := range guarded {
		if strings.HasSuffix(g, ":"+cfgPath) {
			foundCfg = true
		}
		if strings.HasSuffix(g, ":"+hooksDir) {
			foundHooks = true
		}
	}
	if !foundCfg || !foundHooks {
		t.Errorf("event guarded = %v, want entries for .git config and hooks", guarded)
	}
}

// The .git shadows also apply under host_state: none — the bind is not
// host_state-gated — and the disabled event carries them.
func TestWorktreeGitGuardAppliesWithHostStateNone(t *testing.T) {
	guardTestHome(t)
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git", "worktrees", "run-1")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{},
		SandboxParams{WorkspacePath: t.TempDir(), WorktreeGitDir: gitDir, HostStateOverride: "none"},
		newHostConfigGuard(iterlog.Nop()),
		func(et store.EventType, data map[string]any) error {
			if et == store.EventSandboxHostStateMounted {
				event = data
			}
			return nil
		}, iterlog.Nop())
	if event["enabled"] != false {
		t.Fatalf("event enabled = %v, want false (host_state: none)", event["enabled"])
	}
	guarded, _ := event["guarded"].([]string)
	if len(guarded) == 0 {
		t.Fatal("the .git shadows must ride the host_state=none event too")
	}
	if entry := bindEntry(spec.Mounts, filepath.Join(repo, ".git", "config")); entry == "" {
		t.Error("no .git/config shadow under host_state: none")
	}
}

// The #2069 shadows must never feed the HOME-tmpfs layering: with the
// repository nested under $HOME, a tmpfs at the workspace bind's own
// destination (or any ancestor of it) would shadow the workspace itself —
// tmpfs beats bind at one destination in either argument order (measured,
// docker 29).
func TestWorktreeGitGuardLaysNoTmpfsOverTheWorkspace(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the HOME tmpfs layering exists on Linux only")
	}
	if os.Getuid() == 0 {
		t.Skip("the uid-owned tmpfs is only laid for a non-root host user")
	}
	home, _ := guardTestHome(t)
	// The repository nested TWO levels under $HOME — the operator's own
	// layout (~/lab/ai/iterion) is exactly this shape.
	repo := filepath.Join(home, "lab", "proj")
	gitDir := filepath.Join(repo, ".git", "worktrees", "run-1")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{},
		SandboxParams{WorkspacePath: repo, WorktreeGitDir: gitDir},
		newHostConfigGuard(iterlog.Nop()),
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	for _, tm := range spec.Tmpfs {
		entry := strings.SplitN(tm, ":", 2)[0]
		if entry == repo || strings.HasPrefix(repo, entry+"/") {
			if entry != home {
				t.Errorf("tmpfs %q shadows the workspace bind at %q (or one of its parents)", tm, repo)
			}
		}
	}
}

// Without the guard the .git bind is exactly what it was: the whole tree,
// read-write, and nothing else.
func TestWorktreeGitMountUnguardedIsUnchanged(t *testing.T) {
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git", "worktrees", "run-1")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := &sandbox.Spec{}
	addWorktreeGitMount(spec, gitDir, iterlog.Nop())
	if len(spec.Mounts) != 1 {
		t.Fatalf("mounts = %v, want the single .git bind", spec.Mounts)
	}
	if strings.Contains(spec.Mounts[0], "readonly") {
		t.Errorf("the unguarded .git bind is read-write: %q", spec.Mounts[0])
	}
}

// The guard's audit list is nil-safe and empty until something is shadowed.
func TestHostConfigGuardNilIsANoOp(t *testing.T) {
	var guard *hostConfigGuard
	mounts := guard.copyFileOver([]hostStateMount{{HostPath: "/x", ContainerPath: "/x"}}, "/x", "")
	if len(mounts) != 1 {
		t.Errorf("nil guard changed the mount list: %v", mounts)
	}
	if got := guard.audit(); got != nil {
		t.Errorf("nil guard audit = %v, want nil", got)
	}
	guard.cleanup() // must not panic
}

// Regression: a config overlay nested under a bound dir
// ($HOME/.claude/settings.json over the $HOME/.claude bind) must NOT cause
// homeNestedBindParents to lay a tmpfs at the bound dir — the docker driver
// passes --tmpfs AFTER --mount, so the tmpfs would shadow the bind and the
// container would see an empty ~/.claude (sessions and credentials gone).
func TestHostStateGuardOverlaysDoNotShadowTheirParentBind(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the HOME tmpfs layering exists on Linux only")
	}
	if os.Getuid() == 0 {
		t.Skip("the uid-owned tmpfs is only laid for a non-root host user")
	}
	home, _ := guardTestHome(t)
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()},
		newHostConfigGuard(iterlog.Nop()), func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	for _, tm := range spec.Tmpfs {
		for _, bound := range []string{
			filepath.Join(home, ".claude"),
			filepath.Join(home, ".codex"),
		} {
			if strings.HasPrefix(tm, bound+":") {
				t.Errorf("tmpfs %q shadows the %q bind — the container would see an empty dir", tm, bound)
			}
		}
	}
}
