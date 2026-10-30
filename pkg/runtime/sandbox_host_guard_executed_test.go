package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/testfs"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2069, second round. Shadowing ~/.claude/settings.json shadows a POINTER:
// a hook is a `command:` naming a script, and the script — like the local CLI
// install, the skills, the commands, the agents — stayed writable at its own
// host path through the read-write ~/.claude bind. These witnesses pin the
// surface by the verb that defines it: what the HOST executes, or loads as
// instructions, from a mounted home.

// writableAtItsOwnHostPath reports whether path, from inside the container,
// resolves to a bind that can carry a write back to the HOST — the DEEPEST
// bind covering it (docker resolves a path through the deepest mount), not
// read-only, and sourced from the host path itself (a per-run copy's source
// cannot reach the operator's file). It returns the deciding mount entry so a
// failure names what let the write through.
func writableAtItsOwnHostPath(mounts []string, path string) (writable bool, entry string) {
	best := -1
	for _, m := range mounts {
		var source, target string
		readOnly := false
		for _, field := range strings.Split(m, ",") {
			switch {
			case strings.HasPrefix(field, "source="):
				source = strings.TrimPrefix(field, "source=")
			case strings.HasPrefix(field, "target="):
				target = strings.TrimPrefix(field, "target=")
			case field == "readonly":
				readOnly = true
			}
		}
		if target == "" || (path != target && !strings.HasPrefix(path, target+string(filepath.Separator))) {
			continue
		}
		if len(target) > best {
			best = len(target)
			entry = m
			writable = !readOnly && (source == target || strings.HasPrefix(path, source+string(filepath.Separator)))
		}
	}
	return writable, entry
}

// executedSurfaceHome builds a fake operator home carrying every surface the
// HOST executes or loads out of its own home — the pointee of the shadowed
// pointer first — on top of guardTestHome's config files.
func executedSurfaceHome(t *testing.T) (home, iterionHome string) {
	t.Helper()
	home, iterionHome = guardTestHome(t)
	mustWrite := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// The pointee of the shadowed pointer, and the rest of what the host runs
	// or reads as instructions out of its own home.
	mustWrite(filepath.Join(home, ".claude", "hooks", "context-watch.sh"), "#!/bin/sh\necho ok\n", 0o755)
	mustWrite(filepath.Join(home, ".claude", "local", "claude"), "#!/bin/sh\necho cli\n", 0o755)
	mustWrite(filepath.Join(home, ".claude", "skills", "s", "SKILL.md"), "# s\n", 0o644)
	mustWrite(filepath.Join(home, ".claude", "commands", "c.md"), "# c\n", 0o644)
	mustWrite(filepath.Join(home, ".claude", "agents", "a.md"), "# a\n", 0o644)
	mustWrite(filepath.Join(home, ".claude", "output-styles", "o.md"), "# o\n", 0o644)
	mustWrite(filepath.Join(home, ".claude", "rules", "r.md"), "# r\n", 0o644)
	mustWrite(filepath.Join(home, ".claude", "CLAUDE.md"), "# operator memory\n", 0o644)
	mustWrite(filepath.Join(home, ".codex", "AGENTS.md"), "# codex memory\n", 0o644)
	mustWrite(filepath.Join(home, ".codex", "prompts", "p.md"), "# p\n", 0o644)
	// The host's warm caches, whose content the host executes and compiles.
	mustWrite(filepath.Join(home, ".local", "share", "pnpm", "tsc"), "#!/bin/sh\n", 0o755)
	mustWrite(filepath.Join(home, "go", "pkg", "mod", "golang.org", "toolchain@v0.0.1-go1.26.0.linux-amd64", "bin", "go"), "ELF", 0o755)
	mustWrite(filepath.Join(home, ".cache", "go-build", "ab", "abcdef-d"), "object", 0o644)
	mustWrite(filepath.Join(home, ".npm", "_cacache", "index-v5", "x"), "idx", 0o644)
	mustWrite(filepath.Join(home, ".cache", "yarn", "x"), "y", 0o644)
	return home, iterionHome
}

// guardedHomeMounts runs the production wiring, guard on, over that home.
func guardedHomeMounts(t *testing.T) (home, iterionHome string, spec *sandbox.Spec) {
	t.Helper()
	home, iterionHome = executedSurfaceHome(t)
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	spec = &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	return home, iterionHome, spec
}

// TestHostStateGuardShadowsWhatTheHostExecutesFromItsHome names the paths
// INDEPENDENTLY of hostExecutedClaudeHomeEntries: a fix that shrinks the list
// must fail here, which it cannot do if the test iterates the list it checks.
func TestHostStateGuardShadowsWhatTheHostExecutesFromItsHome(t *testing.T) {
	home, _, spec := guardedHomeMounts(t)

	// Each case is reachable BECAUSE the host runs or loads it, named with
	// the mechanism, so a later reader can refute membership rather than
	// trust the list.
	for _, tc := range []struct{ why, path string }{
		{"the script a settings.json hook `command:` names", filepath.Join(home, ".claude", "hooks", "context-watch.sh")},
		{"the local CLI install iterion's own probe resolves and runs (clilocate.ClaudeLocalFallback)", filepath.Join(home, ".claude", "local", "claude")},
		{"a skill the host's next session loads, scripts included", filepath.Join(home, ".claude", "skills", "s", "SKILL.md")},
		{"a slash command the host's next session loads as instructions", filepath.Join(home, ".claude", "commands", "c.md")},
		{"a subagent definition the host's next session loads", filepath.Join(home, ".claude", "agents", "a.md")},
		{"an output style the host's next session loads", filepath.Join(home, ".claude", "output-styles", "o.md")},
		{"the operator's own memory rules (claude_code_ambient names ~/.claude/rules/**)", filepath.Join(home, ".claude", "rules", "r.md")},
		{"the operator's own memory file (claude_code_ambient names ~/.claude/CLAUDE.md)", filepath.Join(home, ".claude", "CLAUDE.md")},
		{"an installed plugin's hooks and MCP servers", filepath.Join(home, ".claude", "plugins", "good", "plugin.yaml")},
		{"codex's home instruction file (delegate.codexInstructionFiles)", filepath.Join(home, ".codex", "AGENTS.md")},
		{"a codex custom prompt", filepath.Join(home, ".codex", "prompts", "p.md")},
	} {
		writable, entry := writableAtItsOwnHostPath(spec.Mounts, tc.path)
		if entry == "" {
			t.Errorf("%s: no mount covers %s at all — the surface is not even reached", tc.why, tc.path)
			continue
		}
		if writable {
			t.Errorf("%s: %s is writable at its own host path through %q — the host executes or loads what the sandbox writes there", tc.why, tc.path, entry)
		}
	}

	// Read access survives: an in-container CLI keeps the operator's
	// plugins, skills and memory. A read-only overlay, not a withdrawal.
	for _, dir := range []string{"hooks", "skills", "plugins", "rules"} {
		path := filepath.Join(home, ".claude", dir)
		entry := bindEntry(spec.Mounts, path)
		if entry == "" || !strings.Contains(entry, "source="+path) {
			t.Errorf("%s must still READ the host tree (a read-only overlay of it), got %q", path, entry)
		}
	}

	// The list covers the named set: the production enumeration may grow,
	// never shrink below what the cases above prove.
	for _, name := range []string{"agents", "commands", "hooks", "local", "output-styles", "plugins", "rules", "skills", "workflows", "routines"} {
		found := false
		for _, entry := range hostExecutedClaudeHomeEntries {
			if entry == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("hostExecutedClaudeHomeEntries no longer carries %q — the host loads it; removing it reopens the plant", name)
		}
	}
}

// A ~/.claude entry the host executes and the operator does not HAVE is left
// ALONE: the home is ephemeral, so the path the agent plants lands in the
// per-run tmpfs and the operator's home gains nothing — where the previous
// posture had to create the directory on the host to have a mountpoint to
// overlay, and left eight of them behind forever.
func TestHostStateGuardLeavesAnAbsentExecutedDirAbsent(t *testing.T) {
	home, _ := guardTestHome(t)
	for _, name := range hostExecutedClaudeHomeEntries {
		testfs.RemoveAll(t, home, filepath.Join(home, ".claude", name))
	}
	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	// Named independently of the production list: a test that iterates the
	// list it verifies stays green when the list shrinks.
	for _, name := range []string{"hooks", "local", "skills", "commands", "agents", "output-styles", "workflows", "routines", "rules", "plugins", "dev-mods", "cowork_plugins", "mcp-skill-archives"} {
		path := filepath.Join(home, ".claude", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s was CREATED in the operator's home: an ephemeral home needs no mountpoint, and nothing removes what the guard leaves there", path)
		}
		if writable, entry := writableAtItsOwnHostPath(spec.Mounts, filepath.Join(path, "planted")); writable {
			t.Errorf("absent %s stays plantable through %q", path, entry)
		}
	}
	// The claim above rests on the tmpfs: without it the paths would be
	// absent in the container too, and the CLI could not write its home.
	if !hasTmpfsAt(spec.Tmpfs, filepath.Join(home, ".claude")) {
		t.Errorf("~/.claude must be a per-run tmpfs for the in-sandbox CLI to write freely: %v", spec.Tmpfs)
	}
}

// hasTmpfsAt reports whether the spec lays a tmpfs exactly at path.
func hasTmpfsAt(tmpfs []string, path string) bool {
	for _, tm := range tmpfs {
		if tm == path || strings.HasPrefix(tm, path+":") {
			return true
		}
	}
	return false
}

// The warm caches are not shadowed, they are REPLACED: the sandbox gets its
// own under the iterion home, so the host's PNPM_HOME bins, its Go toolchain
// binary and its build-cache objects are not writable from inside — and the
// warmth survives, because the same directory serves the next sandboxed run.
func TestSandboxGetsItsOwnWarmCachesNotTheHostsExecutableOnes(t *testing.T) {
	home, iterionHome, spec := guardedHomeMounts(t)

	for _, tc := range []struct{ why, path string }{
		{"a pnpm global bin, which `pnpm setup` puts on the operator's PATH", filepath.Join(home, ".local", "share", "pnpm", "tsc")},
		{"the Go toolchain binary GOTOOLCHAIN=auto executes", filepath.Join(home, "go", "pkg", "mod", "golang.org", "toolchain@v0.0.1-go1.26.0.linux-amd64", "bin", "go")},
		{"a build-cache object `go build` relinks without re-hashing it", filepath.Join(home, ".cache", "go-build", "ab", "abcdef-d")},
		{"an npm cache entry", filepath.Join(home, ".npm", "_cacache", "index-v5", "x")},
		{"a yarn cache entry", filepath.Join(home, ".cache", "yarn", "x")},
	} {
		if writable, entry := writableAtItsOwnHostPath(spec.Mounts, tc.path); writable {
			t.Errorf("%s: %s is writable at its own host path through %q", tc.why, tc.path, entry)
		}
	}

	// Still warm, and warm from a place iterion owns: the cache is served at
	// the container path the tools resolve from $HOME, out of
	// <iterion home>/sandbox-caches.
	for _, rel := range []string{".cache/go-build", "go/pkg/mod", ".npm", ".local/share/pnpm", ".cache/yarn"} {
		target := filepath.Join(home, filepath.FromSlash(rel))
		entry := bindEntry(spec.Mounts, target)
		if entry == "" {
			t.Errorf("no cache is served at %s — every sandboxed run would build cold", target)
			continue
		}
		want := "source=" + filepath.Join(iterionHome, sandboxCacheRoot, filepath.FromSlash(rel))
		if !strings.Contains(entry, want+",") {
			t.Errorf("the cache at %s is not the sandbox's own (%s): %q", target, want, entry)
		}
		src := filepath.Join(iterionHome, sandboxCacheRoot, filepath.FromSlash(rel))
		info, err := os.Stat(src)
		if err != nil || !info.IsDir() {
			t.Errorf("the sandbox cache dir %s was not created: %v", src, err)
		}
	}

	// Persistence across runs, which is the whole reason the caches exist: a
	// file a run leaves in the cache is in the SAME directory the next run's
	// mount serves.
	stamp := filepath.Join(iterionHome, sandboxCacheRoot, ".cache", "go-build", "warm")
	if err := os.WriteFile(stamp, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := &sandbox.Spec{}
	applyHostStateMounts(second, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, newHostConfigGuard(iterlog.Nop()),
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	entry := bindEntry(second.Mounts, filepath.Join(home, ".cache", "go-build"))
	src := strings.Split(strings.Split(entry, "source=")[1], ",")[0]
	if _, err := os.Stat(filepath.Join(src, "warm")); err != nil {
		t.Errorf("the next run's go-build cache does not carry what this run left: %v (source=%s)", err, src)
	}
}

// With the hatch open the operator asked for the pre-guard posture, and that
// includes the host's own caches at their own path.
func TestSandboxCachesFollowTheHostWhenTheGuardIsOff(t *testing.T) {
	t.Setenv(HostStateGuardEnvVar, "off")
	home, _ := executedSurfaceHome(t)
	if g := newHostConfigGuard(iterlog.Nop()); g != nil {
		t.Fatal("the hatch is off — no guard")
	}
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, nil,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	target := filepath.Join(home, ".cache", "go-build")
	entry := bindEntry(spec.Mounts, target)
	if entry == "" || !strings.Contains(entry, "source="+target+",") {
		t.Errorf("guard off: %s must bind the host cache again, got %q", target, entry)
	}
}

// Both isolation hatches are AUTHORITY decisions, so they are read from the
// environment iterion inherited: a `.env` in the repository under review —
// which a sandboxed agent of a previous run had as its workspace, and which
// cmd/iterion loads with no denylist — cannot switch either control off.
func TestPlantedHatchesDoNotDisarmTheIsolationControls(t *testing.T) {
	plant := func(t *testing.T, name string) {
		t.Helper()
		if _, exists := os.LookupEnv(name); exists {
			t.Fatalf("%s is already set — applyDotEnv would not have planted it", name)
		}
		t.Setenv(name, "off")
		envtrust.MarkPlanted(name)
		if !envtrust.Planted(name) {
			t.Fatalf("envtrust does not consider %s planted", name)
		}
	}

	t.Run("the host-state guard stays on, and says so", func(t *testing.T) {
		home, _ := guardTestHome(t)
		plant(t, HostStateGuardEnvVar)
		var logs bytes.Buffer
		guard := newHostConfigGuard(iterlog.New(iterlog.LevelWarn, &logs))
		if guard == nil {
			t.Fatal("a planted ITERION_SANDBOX_HOST_STATE_GUARD=off disarmed the guard")
		}
		if !strings.Contains(logs.String(), HostStateGuardEnvVar) || !strings.Contains(logs.String(), "IGNORED") {
			t.Errorf("the ignored hatch is silent; run log = %q", logs.String())
		}
		spec := &sandbox.Spec{}
		applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
			func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
		gitconfig := filepath.Join(home, ".gitconfig")
		if writable, entry := writableAtItsOwnHostPath(spec.Mounts, gitconfig); writable {
			t.Errorf("a planted hatch handed %s back read-write: %q", gitconfig, entry)
		}
	})

	t.Run("the isolation floor still fires", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		plant(t, SandboxFloorEnvVar)
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeNone)}}
		floored, neutralized := ApplySandboxFloor(wf, store.BotSourceTierTeam, "")
		if !neutralized {
			t.Fatal("a planted ITERION_SANDBOX_FLOOR=off disarmed the floor — a team bot's `sandbox: none` would be honoured")
		}
		if floored.Sandbox.Mode != "" {
			t.Errorf("floored mode = %q, want the opt-out removed", floored.Sandbox.Mode)
		}
	})

	t.Run("the operator's own export still opens both", func(t *testing.T) {
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv(HostStateGuardEnvVar, "off")
		t.Setenv(SandboxFloorEnvVar, "off")
		if g := newHostConfigGuard(iterlog.Nop()); g != nil {
			t.Error("an inherited ITERION_SANDBOX_HOST_STATE_GUARD=off must disable the guard — philosophy §1")
		}
		wf := &ir.Workflow{Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeNone)}}
		if _, neutralized := ApplySandboxFloor(wf, store.BotSourceTierTeam, ""); neutralized {
			t.Error("an inherited ITERION_SANDBOX_FLOOR=off must disable the floor — philosophy §1")
		}
	})
}

// The copies are per-run state holding the operator's own config, so they are
// owner-only, and they do not survive the process that staged them: a marked
// scratch left by an engine that died without a teardown is swept at the next
// guard, and a scratch that could still belong to a live run is not.
func TestHostGuardScratchIsOwnerOnlyAndSweptWhenAbandoned(t *testing.T) {
	home, _ := guardTestHome(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	if guard.dir == "" {
		t.Fatal("no scratch was staged — the witness would prove nothing")
	}
	if info, err := os.Stat(guard.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("scratch %s mode = %v (err %v), want 0700", guard.dir, info.Mode().Perm(), err)
	}
	copySource := strings.Split(strings.Split(bindEntry(spec.Mounts, filepath.Join(home, ".claude.json")), "source=")[1], ",")[0]
	if info, err := os.Stat(copySource); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the ~/.claude.json copy %s mode = %v (err %v), want 0600 — it carries the account's own state", copySource, info.Mode().Perm(), err)
	}

	// An engine killed mid-run leaves its marked scratch behind; a later run
	// sweeps it, and leaves alone both a young one (a live run's) and a
	// directory iterion never marked.
	abandoned := filepath.Join(tmp, hostGuardScratchPrefix+"dead")
	live := filepath.Join(tmp, hostGuardScratchPrefix+"live")
	foreign := filepath.Join(tmp, "someone-elses-dir")
	for _, dir := range []string{abandoned, live, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * hostGuardScratchStaleAfter)
	if err := os.Chtimes(abandoned, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, old, old); err != nil {
		t.Fatal(err)
	}
	// Driven through the PRODUCTION entry point — the next run's guard —
	// not by calling the sweeper: a test that calls it itself passes while
	// nothing in a real run ever does.
	hostGuardSweepOnce = sync.Once{}
	if next := newHostConfigGuard(iterlog.Nop()); next == nil {
		t.Fatal("the guard is on by default")
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("the abandoned copy scratch %s survives (err %v) — it holds a copy of ~/.claude.json", abandoned, err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("a young scratch was swept (%v) — a live run's copies would vanish mid-run", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a directory iterion did not mark was removed (%v) — the sweep must only ever take its own", err)
	}
}

// The copies are staged on the host BEFORE the container exists, so every
// return between the staging and the live sandbox's teardown must carry them
// away. The forfait-delivery failure is the one the enumeration missed: a run
// whose credentials name a materialised Claude forfait whose file is gone
// returns past the noop gate, with a guard holding copies of ~/.claude.json,
// ~/.gitconfig and ~/.codex/config.toml.
func TestResolveAndStartSandbox_AForfaitFailureTakesTheGuardCopiesWithIt(t *testing.T) {
	t.Setenv("ITERION_MODE", "local")
	guardTestHome(t)
	scratchRoot := t.TempDir()
	t.Setenv("TMPDIR", scratchRoot)

	// A credential scope naming a forfait dir with no .credentials.json in
	// it: addClaudeOAuthSecretFile reads the file and fails.
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()},
	})
	driver := &specCapturingDriver{hostBinds: true}
	p := SandboxParams{
		Workflow: &ir.Workflow{
			Name:    "forfait_fail",
			Sandbox: &ir.SandboxSpec{Mode: string(sandbox.ModeInline), Image: "example.invalid/iterion-sandbox:test"},
		},
		RunID: "run-forfait", WorkspacePath: t.TempDir(), RepoRoot: t.TempDir(),
		DefaultImage: "example.invalid/iterion-sandbox:test",
		Drivers: map[string]sandbox.DriverConstructor{
			"docker": func() (sandbox.Driver, error) { return driver, nil },
		},
		Logger: iterlog.Nop(), EmitEvent: func(store.EventType, map[string]any) error { return nil },
	}
	active, err := resolveAndStartSandbox(ctx, p)
	if err == nil {
		t.Fatalf("the forfait delivery must fail on a missing credentials file; active=%v — the witness would prove nothing", active)
	}
	if !strings.Contains(err.Error(), "forfait") {
		t.Fatalf("err = %v, want the forfait delivery failure — another path returned and this proves nothing about that one", err)
	}
	entries, rerr := os.ReadDir(scratchRoot)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), hostGuardScratchPrefix) {
			t.Errorf("the per-run copy scratch %s survives a failed start — it holds copies of the operator's ~/.claude.json, ~/.gitconfig and ~/.codex/config.toml", filepath.Join(scratchRoot, e.Name()))
		}
	}
}

// The THIRD home the sandbox mounts is iterion's own, and it carries the same
// class: what the HOST's iterion loads. It cannot be made ephemeral — the run
// store and the memory tree live under it — so each loaded surface is overlaid
// read-only, and the paths are named here by the mechanism that loads them,
// independently of the production list.
func TestTheIterionHomeInstructionSurfacesAreReadOnly(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	skill := filepath.Join(iterionHome, "skills", "deploy", "SKILL.md")
	mustWrite(skill, "---\nname: deploy\n---\nrun the real deploy\n")
	connector := filepath.Join(iterionHome, "connectors", "jira", "connector.yaml")
	mustWrite(connector, "id: jira\n")

	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, tc := range []struct{ why, path string }{
		{"a library skill mirrorLibrarySkills copies into <workDir>/.claude/skills at every run, where the agent's CLI loads it as instructions", skill},
		{"a local connector package, which decides what a `tool … action:` call reaches and which sealed credential goes with it", connector},
		{"a library skill planted where the operator had none", filepath.Join(iterionHome, "skills", "planted", "SKILL.md")},
		{"an iterion plugin, whose MCP servers and rewriters run in the launcher's own process", filepath.Join(iterionHome, "plugins", "rtk", "plugin.yaml")},
		{"the sandbox's own cache store, which is where a planted symlink would choose the NEXT run's bind sources", filepath.Join(iterionHome, sandboxCacheRoot, "planted")},
	} {
		if writable, entry := writableAtItsOwnHostPath(spec.Mounts, tc.path); writable {
			t.Errorf("WRITABLE at its own host path: %s\n    why it matters: %s\n    deciding mount: %q", tc.path, tc.why, entry)
		}
	}
	// The control: what the home is bound read-write FOR still is.
	if writable, _ := writableAtItsOwnHostPath(spec.Mounts, filepath.Join(iterionHome, "projects", "k", "memory", "x.md")); !writable {
		t.Errorf("the run store and memory tree must stay writable — that is what the iterion home is mounted for: %v", spec.Mounts)
	}
}

// The ephemeral home has exactly three exceptions, and each exists because an
// iterion process OUTSIDE the container reads or writes the same tree. They
// are asserted by their mechanism, not by their name: a future refactor that
// drops one must redden here rather than degrade a feature silently.
func TestTheEphemeralHomeKeepsTheStateThatMustRoundTrip(t *testing.T) {
	home, _ := guardTestHome(t)
	credentials := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.WriteFile(credentials, []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, ".codex", "auth.json")
	if err := os.WriteFile(auth, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, tc := range []struct{ why, path string }{
		{"the CLI's session transcripts, which `session: persist` packs from the HOST path after the run (delegate.SessionFilesRoot + sessionpack.CollectBySessionID)", filepath.Join(home, ".claude", "projects")},
		{"the forfait OAuth token the CLI reads and refreshes", credentials},
		{"codex's ChatGPT-forfait token, the only credential path for a user with no metered key", auth},
		{"codex's session rollouts, which sessionpack packs for a codex `session: persist`", filepath.Join(home, ".codex", "sessions")},
	} {
		writable, entry := writableAtItsOwnHostPath(spec.Mounts, filepath.Join(tc.path, "x"))
		if !writable {
			writable, entry = writableAtItsOwnHostPath(spec.Mounts, tc.path)
		}
		if !writable {
			t.Errorf("%s is NOT bound read-write: %s\n    what breaks: %s", tc.path, entry, tc.why)
		}
	}
	// And the read set is still READ: an ephemeral home that hid the
	// operator's own configuration would be isolation by amputation.
	for _, tc := range []struct{ why, path string }{
		{"the operator's plugins, which an in-sandbox session must still see", filepath.Join(home, ".claude", "plugins", "good", "plugin.yaml")},
		{"the operator's settings, read through a per-run copy", filepath.Join(home, ".claude", "settings.json")},
	} {
		if entry := bindEntryUnder(spec.Mounts, tc.path); entry == "" {
			t.Errorf("%s is not visible in the sandbox at all (%s) — the posture must isolate writes, not reads", tc.path, tc.why)
		}
	}
}

// bindEntryUnder finds the mount whose target is path or an ancestor of it.
func bindEntryUnder(mounts []string, path string) string {
	best := ""
	for _, m := range mounts {
		target := mountTarget(m)
		if target == "" {
			continue
		}
		if target == path || strings.HasPrefix(path, target+string(filepath.Separator)) {
			if len(target) > len(best) {
				best = m
			}
		}
	}
	return best
}

// A container pinning a UID that is not the host's gets a COLD run rather than
// a cache store it cannot read: the store is owner-only 0700, so a foreign UID
// meets "permission denied" inside `go build` with nothing in the run record
// to explain it. The scratch bind already answered this condition this way.
func TestTheSandboxCachesAreSkippedWhenTheContainerPinsAnotherUID(t *testing.T) {
	home, iterionHome := guardTestHome(t)
	guard := newHostConfigGuard(iterlog.Nop())
	foreign := os.Getuid() + 1234
	spec := &sandbox.Spec{User: strconv.Itoa(foreign)}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	for _, rel := range []string{".cache/go-build", "go/pkg/mod", ".npm", ".local/share/pnpm", ".cache/yarn"} {
		target := filepath.Join(home, filepath.FromSlash(rel))
		if entry := bindEntry(spec.Mounts, target); entry != "" {
			t.Errorf("the cache store is mounted at %s for a container pinning UID %d (host %d): %q — owner-only 0700, so the container cannot even read it", target, foreign, os.Getuid(), entry)
		}
	}
	// And nothing was created in the store for a run that gets none.
	if _, err := os.Stat(filepath.Join(iterionHome, sandboxCacheRoot, ".cache", "go-build")); err == nil {
		t.Errorf("the store was populated for a run that cannot use it")
	}
	// The control: the SAME plan with the host's own UID warms the caches.
	warm := &sandbox.Spec{}
	applyHostStateMounts(warm, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, newHostConfigGuard(iterlog.Nop()),
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())
	if entry := bindEntry(warm.Mounts, filepath.Join(home, ".cache", "go-build")); entry == "" {
		t.Errorf("control broke: the host's own UID must still get its warm caches: %v", warm.Mounts)
	}
}

// The ephemeral home is a TMPFS or it is nothing: ephemeralDirOver drops the
// read-write bind of ~/.claude and ~/.codex, so if no tmpfs is laid there
// docker root-creates the mount parent of the read-only overlays nested under
// them and the in-sandbox CLI cannot write its own home. The posture must
// therefore hold where the host-UID remap does NOT run — a non-Linux docker
// host, or an engine running as root — which is why the decision is a function
// this witness can drive for a platform it is not on.
func TestTheEphemeralHomeIsBackedByATmpfsEvenWithoutTheUIDRemap(t *testing.T) {
	home, _, spec := guardedHomeMounts(t)
	tmpfsAt := func(entries []string, dir string) string {
		for _, e := range entries {
			if strings.HasPrefix(e, dir+":") {
				return e
			}
		}
		return ""
	}
	// First on the real plan, where the remap DOES run: both ephemeral homes
	// carry a uid-owned tmpfs.
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
		entry := tmpfsAt(spec.Tmpfs, dir)
		if entry == "" {
			t.Fatalf("%s is neither bound nor backed by a tmpfs — the in-sandbox CLI has no home to write: %v", dir, spec.Tmpfs)
		}
		if !strings.Contains(entry, "uid="+strconv.Itoa(os.Getuid())) {
			t.Errorf("%q is not owned by the remapped uid %d", entry, os.Getuid())
		}
		if !strings.Contains(entry, "exec") {
			t.Errorf("%q is noexec — docker's default — and the CLI installs it runs live under the home", entry)
		}
	}

	// Now the same decision with NO uid to align (uid < 0): the guard's dirs
	// stay, the writable-$HOME layering — whose only job is to re-lay as
	// user-owned what docker root-creates — drops out, and the ephemeral
	// homes are owned by the CONTAINER user: a docker tmpfs without uid= is
	// root:root (measured: `--user 1000 --tmpfs d:mode=0755` → touch
	// Permission denied), so "the image's own user owns what is laid" is
	// something the plan must EMIT — spec.User's uid when it names one,
	// world-writable when it does not.
	ephemeral := []string{filepath.Join(home, ".claude"), filepath.Join(home, ".codex")}
	nested := []string{filepath.Join(home, ".cache"), filepath.Join(home, ".claude")}
	entries := homeTmpfsEntries(home, ephemeral, nested, -1, -1, "1000")
	for _, dir := range ephemeral {
		entry := tmpfsAt(entries, dir)
		if entry == "" {
			t.Errorf("with no uid remap, %s gets no tmpfs — docker root-creates it as the overlays' parent and an image whose user is not root cannot write its own home: %v", dir, entries)
			continue
		}
		if !strings.Contains(entry, "uid=1000") {
			t.Errorf("%q does not hand the ephemeral home to the container's pinned user 1000 — a tmpfs without uid= is root:root and the CLI cannot write its home", entry)
		}
		if !strings.Contains(entry, "exec") {
			t.Errorf("%q is noexec — docker's default — and the CLI installs it runs live under the home", entry)
		}
	}
	// A spec.User that names no uid (empty, or a name) cannot be honoured
	// exactly, so the ephemeral homes go world-writable instead: a tmpfs
	// keeps no byte past the run, and the home's isolation never rested on
	// its mode.
	for _, user := range []string{"", "devbox"} {
		fallback := homeTmpfsEntries(home, ephemeral, nested, -1, -1, user)
		for _, dir := range ephemeral {
			entry := tmpfsAt(fallback, dir)
			if entry == "" {
				t.Errorf("user %q: %s gets no tmpfs at all — the CLI has no home to write", user, dir)
				continue
			}
			if !strings.Contains(entry, "mode=0777") {
				t.Errorf("user %q: %q is not world-writable — with no uid to name, root:root 0755 is a home the image's user cannot write", user, entry)
			}
		}
	}
	if entry := tmpfsAt(entries, home); entry != "" {
		t.Errorf("the writable-$HOME layering is uid-owned by nature and must not be laid without one: %q", entry)
	}
	if entry := tmpfsAt(entries, filepath.Join(home, ".cache")); entry != "" {
		t.Errorf("same for the nested bind parents: %q", entry)
	}
	// One destination, one --tmpfs: ~/.claude is both an ephemeral home and
	// the ancestor of the projects/ bind, and docker cannot order two.
	withUID := homeTmpfsEntries(home, ephemeral, nested, 1000, 1000, "")
	seen := map[string]int{}
	for _, e := range withUID {
		seen[strings.SplitN(e, ":", 2)[0]]++
	}
	for dir, n := range seen {
		if n > 1 {
			t.Errorf("%s is laid %d times: %v", dir, n, withUID)
		}
	}
}

// The class the posture inversion closes is the one NO list names: with
// ~/.claude a read-write bind, every path the overlay list did not enumerate
// reached the operator's home — including shell code the host's own Bash tool
// sources at exec, and any name a future CLI invents. These cases are
// deliberately OUTSIDE hostExecutedClaudeHomeEntries: a fix that re-binds the
// home read-write and extends the list instead must redden here.
func TestTheEphemeralHomeLeavesNoUnenumeratedPathWritable(t *testing.T) {
	home, _, spec := guardedHomeMounts(t)
	for _, tc := range []struct{ why, path string }{
		{"shell code the HOST's Bash tool sources at exec — no human in the loop, unlike a skill or a command", filepath.Join(home, ".claude", "shell-snapshots", "snapshot-bash-1.sh")},
		{"the environment file a host session sources", filepath.Join(home, ".claude", "session-env", "env.sh")},
		{"tasks the host harness EXECUTES on a timer", filepath.Join(home, ".claude", "scheduled_tasks.json")},
		{"the host harness's launch configuration", filepath.Join(home, ".claude", "launch.json")},
		{"the host harness's daemon configuration", filepath.Join(home, ".claude", "daemon.json")},
		{"a prompt the host harness loops on", filepath.Join(home, ".claude", "loop.md")},
		{"a name NOBODY enumerated — the whole point of naming the read set instead of the write set", filepath.Join(home, ".claude", "PWNED-NEW")},
		{"the program ~/.codex/config.toml's `notify =` names: shadowing the pointer while its pointee stayed writable shadowed nothing", filepath.Join(home, ".codex", "notify.sh")},
		{"the command a config.toml `mcp_servers.<x>.command` names", filepath.Join(home, ".codex", "mcp", "server.sh")},
	} {
		if writable, entry := writableAtItsOwnHostPath(spec.Mounts, tc.path); writable {
			t.Errorf("%s is writable at its own host path through %s\n    what the host does with it: %s", tc.path, entry, tc.why)
		}
	}
	// The control, so a plan that mounted NOTHING could not pass: the read
	// set is still bound.
	if entry := bindEntryUnder(spec.Mounts, filepath.Join(home, ".claude", "hooks", "context-watch.sh")); entry == "" {
		t.Errorf("control broke: the operator's hooks must still be READABLE in the sandbox: %v", spec.Mounts)
	}
}
