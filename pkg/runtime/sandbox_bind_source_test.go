package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mountReaching returns the first mount of the plan whose SOURCE resolves to
// target (or under it) — the question a bind answers on the host, where the
// kernel follows every symlink in the source path. A mount list that merely
// SPELLS the store is not evidence: the string is exactly what a planted link
// leaves untouched.
func mountReaching(mounts []string, target string) string {
	for _, m := range mounts {
		src := ""
		for _, f := range strings.Split(m, ",") {
			if strings.HasPrefix(f, "source=") {
				src = strings.TrimPrefix(f, "source=")
			}
		}
		if src == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(src)
		if err != nil {
			continue
		}
		if resolved == target || strings.HasPrefix(resolved, target+string(filepath.Separator)) {
			return m
		}
	}
	return ""
}

// The sandbox's own cache store lives under the iterion home, which is bound
// READ-WRITE so the run store survives the run — so the store is a directory
// the sandbox writes, and a symlink a run plants there chooses the NEXT run's
// bind source. Measured on the real plan: the run before plants the link, this
// run's mount plan must not reach its target.
func TestAPlantedSymlinkInTheCacheStoreIsNotMounted(t *testing.T) {
	home, iterionHome := guardTestHome(t)
	// Any host path the engine's uid can write stands for the class; a key
	// directory is the one that makes the stakes plain.
	secret := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "id_ed25519"), []byte("PRIVATE KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(iterionHome, sandboxCacheRoot, ".cache", "go-build")
	if err := os.MkdirAll(filepath.Dir(planted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, planted); err != nil {
		t.Fatal(err)
	}

	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard,
		func(store.EventType, map[string]any) error { return nil }, iterlog.Nop())

	if entry := mountReaching(spec.Mounts, secret); entry != "" {
		t.Errorf("the planted symlink was honoured: %q reaches %s — a run chose the next run's bind source, read-write, through a mount the audit still spells as the store", entry, secret)
	}
	// The audit must not claim a cache it did not mount: a run that reads
	// "private:<path>" and finds no mount has been told the wrong thing.
	for _, line := range guard.audit() {
		if strings.HasPrefix(line, "private:") && strings.HasSuffix(line, filepath.Join(home, ".cache", "go-build")) {
			t.Errorf("the audit claims %q while the bind was refused", line)
		}
	}
	// The sibling cache entries, whose sources were NOT planted, still mount:
	// a check that refuses the whole plan would be a cold run for everyone.
	if entry := bindEntry(spec.Mounts, filepath.Join(home, "go", "pkg", "mod")); entry == "" {
		t.Errorf("the unplanted cache entries must still be warmed: %v", spec.Mounts)
	}
}

// The same class, at the OTHER store under the iterion home: the project
// scratch, whose own comment names the threat ("the repository decides the
// bind source — symlinks included") and which a deterministic tool node later
// EXECUTES (verify.sh).
func TestAPlantedSymlinkInTheProjectScratchIsNotMounted(t *testing.T) {
	home, iterionHome := guardTestHome(t)
	secret := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	// The production resolver, so the test and the code cannot disagree on
	// which path the bind would have used.
	scratch := memory.WorkspaceScratchDir(workspace)
	if scratch == "" {
		t.Fatal("no scratch dir resolved for the workspace")
	}
	if !strings.HasPrefix(scratch, iterionHome) {
		t.Fatalf("the scratch %s is expected under the iterion home %s", scratch, iterionHome)
	}
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, scratch); err != nil {
		t.Fatal(err)
	}

	spec := &sandbox.Spec{HostState: sandbox.HostStateAuto}
	var skipped string
	bound := applyScratchMount(spec, workspace, workspace, true,
		func(_ store.EventType, payload map[string]any) error {
			if enabled, _ := payload["enabled"].(bool); !enabled {
				skipped, _ = payload["reason"].(string)
			}
			return nil
		}, iterlog.Nop())

	if bound != "" {
		t.Errorf("the scratch bind was made from a planted path: %q", bound)
	}
	if entry := mountReaching(spec.Mounts, secret); entry != "" {
		t.Errorf("the planted symlink was honoured: %q reaches %s", entry, secret)
	}
	if skipped == "" {
		t.Errorf("a refused scratch must say so in the run record — an empty ${PROJECT_SCRATCH_DIR} with no event is the silent degradation this skip exists to avoid")
	}
}

// The .git tree is bound read-write too (a commit needs refs and objects), so
// it is the third root of the same class: the hooks overlay's source must be
// the hooks directory, not wherever a run pointed it.
func TestAPlantedSymlinkInTheWorktreeGitTreeIsNotOverlaid(t *testing.T) {
	home, _ := guardTestHome(t)
	secret := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	dotGit := filepath.Join(repo, ".git")
	if err := os.MkdirAll(filepath.Join(dotGit, "worktrees", "run-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotGit, "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dotGit, "hooks")); err != nil {
		t.Fatal(err)
	}

	guard := newHostConfigGuard(iterlog.Nop())
	spec := &sandbox.Spec{}
	guard.guardWorktreeGit(spec, filepath.Join(dotGit, "worktrees", "run-1"))

	if entry := mountReaching(spec.Mounts, secret); entry != "" {
		t.Errorf("the planted symlink was honoured: %q reaches %s", entry, secret)
	}
	// The config copy, whose source is a real file, is unaffected: the check
	// refuses a path, not a tree.
	if len(spec.Mounts) == 0 {
		t.Errorf("the .git/config copy must still be laid: %v", spec.Mounts)
	}
}

// The check is a predicate, so its two halves are exercised directly: a real
// path under the root passes, every shape that makes the source the sandbox's
// choice fails — including the one that does not exist yet, which must pass
// before creation and fail after a link appears.
func TestRealBindSourceRefusesWhatTheSandboxCouldChoose(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	real := filepath.Join(root, "store", "cache")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := realBindSource(root, real); err != nil {
		t.Errorf("a real directory under the root must pass: %v", err)
	}
	if err := realBindSource(root, filepath.Join(root, "store", "absent", "deeper")); err != nil {
		t.Errorf("a path that does not exist yet cannot be a symlink, so it must pass: %v", err)
	}
	if err := realBindSource(root, filepath.Join(outside, "x")); err == nil {
		t.Errorf("a path outside the root must be refused")
	}

	linked := filepath.Join(root, "store", "linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if err := realBindSource(root, linked); err == nil {
		t.Errorf("a symlink whose target leaves the root must be refused")
	}
	if err := realBindSource(root, filepath.Join(linked, "deeper")); err == nil {
		t.Errorf("a path THROUGH a symlink that leaves the root must be refused — the kernel follows it at mount time")
	}

	// ensureRealBindSourceDir asks again after creating, so a link that
	// appears between the check and the MkdirAll cannot survive into the plan.
	ensured := filepath.Join(root, "store", "ensured")
	if err := ensureRealBindSourceDir(root, ensured, 0o700); err != nil {
		t.Errorf("creating a real directory under the root must pass: %v", err)
	}
	if info, err := os.Lstat(ensured); err != nil || !info.IsDir() {
		t.Errorf("the source was not created: %v", err)
	}
	if err := ensureRealBindSourceDir(root, filepath.Join(linked, "x"), 0o700); err == nil {
		t.Errorf("creating through an escaping symlink must be refused, not followed")
	}
}
