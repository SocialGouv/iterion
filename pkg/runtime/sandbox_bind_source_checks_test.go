package runtime

// The plan-to-create window (#2069, H-10 round 2): a bind source is
// RESOLVED BY THE DAEMON at container creation, but verified when the plan
// is built — before the driver's pull, which is minutes. A sandboxed run
// alive through that window can swap a verified leaf for a symlink and
// choose where the next creation's mount lands. The plan therefore names
// every source it verified (Spec.BindSourceChecks) and the driver re-asks
// the same question at that border. These witnesses hold the RUNTIME side:
// the naming, and the fact that a swap between the two asks fails the ask.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/safepath"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

func emitNoop(store.EventType, map[string]any) error { return nil }

// Every source the plan verified is NAMED for the driver: the project
// scratch and each cache the plan mounts, each with the store root its
// check compared against. A check naming nothing, or naming a path the plan
// did not mount, would leave the driver re-asking the wrong question — or
// no question at all, which is the open window.
func TestTheBindSourcesThePlanVerifiedAreNamedForTheDriver(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	if iterionHome == "" {
		t.Fatal("no iterion home for the guard test")
	}
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	spec := &sandbox.Spec{}
	applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: t.TempDir()}, guard, emitNoop, iterlog.Nop())

	workspace := t.TempDir()
	if bound := applyScratchMount(spec, workspace, workspace, true, emitNoop, iterlog.Nop()); bound == "" {
		t.Fatalf("no scratch bind emitted — the witness needs the plan that mounts it: %v", spec.Mounts)
	}

	// The scratch: one check, rooted at the iterion home.
	scratch := memory.WorkspaceScratchDir(workspace)
	found := false
	for _, c := range spec.BindSourceChecks {
		if c.Path == scratch {
			found = true
			if c.Root != iterionHome {
				t.Errorf("the scratch check is rooted at %q, not at the iterion home %q", c.Root, iterionHome)
			}
		}
	}
	if !found {
		t.Errorf("the plan verified the scratch %s but did not name it for the driver — the leaf can be swapped after the check with nobody re-asking: %+v", scratch, spec.BindSourceChecks)
	}

	// Each cache the plan mounts: a check rooted at the store. The sources
	// are read back from the MOUNTS, not from a parallel list, so a mount
	// whose source was never verified cannot hide.
	storeDir := SandboxCacheStoreDir(iterionHome)
	caches := 0
	for _, m := range spec.Mounts {
		source := mountSource(m)
		if source == "" || !hasPathPrefix(source, storeDir) || source == storeDir {
			continue
		}
		caches++
		named := false
		for _, c := range spec.BindSourceChecks {
			if c.Path == source {
				named = true
				if c.Root != storeDir {
					t.Errorf("the cache %s is checked against %q, not against the store %q", source, c.Root, storeDir)
				}
			}
		}
		if !named {
			t.Errorf("the plan mounts the cache %s from the store it verified, but does not name it for the driver — the window stays open for it", source)
		}
	}
	if caches == 0 {
		t.Errorf("no cache bind found under the store %s — the witness needs the warmed plan: %v", storeDir, spec.Mounts)
	}

	// Every named check must pass RIGHT NOW: a plan that names a source its
	// own check would refuse is incoherent by construction.
	for _, c := range spec.BindSourceChecks {
		if err := safepath.RealBindSource(c.Root, c.Path); err != nil {
			t.Errorf("the plan named %s for the driver and it does not verify: %v", c.Path, err)
		}
	}
}

// The window, end to end at the unit level: the leaf the plan verified is
// swapped by a concurrent run, and the SAME question the driver re-asks now
// refuses it. The driver-side witness (pkg/sandbox/docker) holds the
// refusal of the creation itself.
func TestASourceSwappedAfterThePlanFailsTheQuestionTheDriverReasks(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	storeDir := SandboxCacheStoreDir(iterionHome)
	// The store root is created by the plan itself (roDirOver lays it empty
	// when absent) before the caches below it are checked.
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(storeDir, ".cache", "go-build")
	if err := safepath.EnsureRealBindSourceDir(storeDir, leaf, 0o700); err != nil {
		t.Fatalf("the plan-time ensure must pass on the real leaf: %v", err)
	}
	if err := safepath.RealBindSource(storeDir, leaf); err != nil {
		t.Fatalf("the plan-time ask must pass: %v", err)
	}
	// The concurrent run swaps the leaf through the read-write home bind.
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, leaf); err != nil {
		t.Fatal(err)
	}
	if err := safepath.EnsureRealBindSourceDir(storeDir, leaf, 0o700); err == nil {
		t.Fatal("the swapped leaf passed the question the driver re-asks — the window is open")
	}
}

func hasPathPrefix(s, p string) bool {
	if s == p {
		return true
	}
	return strings.HasPrefix(s, p+string(filepath.Separator))
}

// The WORKSPACE bind: the one mount the DRIVER derives from run metadata
// instead of from this plan — read-write, and for a run on an engine
// worktree under the iterion home the home bind hands every living
// sandboxed run read-write. The guard names it for the border (MustExist:
// the tree is what the engine laid, nothing recreates it) on BOTH
// host_state branches, because the bind happens on both.
func TestTheEngineLaidWorkspaceIsNamedForTheBorderOnBothHostStateBranches(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	if iterionHome == "" {
		t.Fatal("no iterion home for the guard test")
	}
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	wt := filepath.Join(iterionHome, "projects", "k", "worktrees", "run-named")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}

	for name, p := range map[string]SandboxParams{
		"host_state active": {WorkspacePath: wt},
		"host_state none":   {WorkspacePath: wt, HostStateOverride: "none"},
	} {
		spec := &sandbox.Spec{}
		if _, err := applyHostStateMounts(spec, &ir.Workflow{}, p, guard, emitNoop, iterlog.Nop()); err != nil {
			t.Fatalf("%s: the plan refused an honest engine-laid workspace: %v", name, err)
		}
		named := false
		for _, c := range spec.BindSourceChecks {
			if c.Path != wt {
				continue
			}
			named = true
			if c.Root != iterionHome {
				t.Errorf("%s: the workspace check is rooted at %q, not at the store that laid it %q", name, c.Root, iterionHome)
			}
			if !c.MustExist {
				t.Errorf("%s: the workspace check is not MustExist — the border would recreate the run's tree cold instead of refusing", name)
			}
		}
		if !named {
			t.Errorf("%s: the plan hands the engine-laid workspace %s to the driver with no border check — the leaf can be swapped after the plan with nobody re-asking: %+v", name, wt, spec.BindSourceChecks)
		}
		// Every named check must pass RIGHT NOW: a plan that names a source
		// its own question refuses is incoherent by construction.
		for _, c := range spec.BindSourceChecks {
			if err := safepath.RealBindSource(c.Root, c.Path); err != nil {
				t.Errorf("%s: the plan named %s for the driver and it does not verify: %v", name, c.Path, err)
			}
		}
	}
}

// The other shape: a workspace the engine did NOT lay — an operator
// checkout anywhere outside the store — is a source no sandbox chose, and
// the plan names no border check for it.
func TestAnOperatorCheckoutOutsideTheStoreIsNamedByNoBorderCheck(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	_ = iterionHome
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	checkout := t.TempDir()
	spec := &sandbox.Spec{}
	if _, err := applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: checkout}, guard, emitNoop, iterlog.Nop()); err != nil {
		t.Fatalf("the plan refused an honest operator checkout: %v", err)
	}
	for _, c := range spec.BindSourceChecks {
		if c.Path == checkout {
			t.Errorf("the operator checkout %s was named for the border — it is not a source under a store the sandbox writes: %+v", checkout, spec.BindSourceChecks)
		}
	}
}

// The plan-time half of the workspace's double question: a workspace
// swapped BEFORE the plan — planted by a run that held the store
// read-write and is still alive — refuses the RUN at the plan, the only
// posture that has no cold skip to fall back on.
func TestTheWorkspaceSwappedAtPlanTimeRefusesTheRun(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	guard := newHostConfigGuard(iterlog.Nop())
	if guard == nil {
		t.Fatal("the guard is on by default")
	}
	wt := filepath.Join(iterionHome, "projects", "k", "worktrees", "run-swapped")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, wt); err != nil {
		t.Fatal(err)
	}

	spec := &sandbox.Spec{}
	_, err := applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{WorkspacePath: wt}, guard, emitNoop, iterlog.Nop())
	if err == nil {
		t.Fatalf("the plan accepted a workspace that is not the tree the engine laid: %+v", spec.BindSourceChecks)
	}
	if !strings.Contains(err.Error(), "refusing the run") || !strings.Contains(err.Error(), outside) {
		t.Errorf("the refusal must name the swap and where the workspace now points: %v", err)
	}
}

// The diff's invariant, held over EVERY mount whose source lives under a
// root whose content the sandbox writes — the iterion home (registered by
// applyHostStateMounts) and the worktree .git tree (registered by
// guardWorktreeGit): the plan names it for the driver. The roots themselves
// are exempt — a root IS its own containment root (its realness is what
// RealBindSource resolves every child check against, and the home's
// operator-chosen-ness is operatorsIterionHome's question, not a
// BindSourceCheck's).
//
// Both workspace shapes: one whose home bind the workspace bind overlaps
// away (an engine worktree lives under the home), and one on an operator
// checkout where the home bind — and with it the overlays under it —
// survives. The second is the shape the invariant was first caught lying
// about: five verified sources (connectors, plugins, skills, the store's
// own ro overlay, the .git hooks) named by no entry.
func TestEverySourceUnderASandboxWritableRootIsNamedForTheDriver(t *testing.T) {
	_, iterionHome := guardTestHome(t)
	if iterionHome == "" {
		t.Fatal("no iterion home for the guard test")
	}

	// A fake repository with a .git tree, as a worktree run carries.
	repo := t.TempDir()
	dotGit := filepath.Join(repo, ".git")
	if err := os.MkdirAll(filepath.Join(dotGit, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dotGit, "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, workspace := range map[string]string{
		"engine-laid worktree under the store": filepath.Join(iterionHome, "projects", "k", "worktrees", "run-every"),
		"operator checkout away from the home": t.TempDir(),
	} {
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			t.Fatal(err)
		}
		guard := newHostConfigGuard(iterlog.Nop())
		if guard == nil {
			t.Fatal("the guard is on by default")
		}
		spec := &sandbox.Spec{}
		if _, err := applyHostStateMounts(spec, &ir.Workflow{}, SandboxParams{
			WorkspacePath:  workspace,
			WorktreeGitDir: filepath.Join(dotGit, "worktrees", "run-every"),
		}, guard, emitNoop, iterlog.Nop()); err != nil {
			t.Fatalf("%s: the plan refused an honest tree: %v", name, err)
		}
		if bound := applyScratchMount(spec, repo, workspace, true, emitNoop, iterlog.Nop()); bound == "" {
			t.Fatalf("%s: no scratch bind emitted — the witness needs the plan that mounts it: %v", name, spec.Mounts)
		}

		roots := []string{iterionHome, dotGit}
		named := func(src string) bool {
			for _, c := range spec.BindSourceChecks {
				if c.Path == src {
					return true
				}
			}
			return false
		}
		offenders := []string{}
		for _, m := range spec.Mounts {
			src := mountSource(m)
			if src == "" {
				continue
			}
			for _, r := range roots {
				// STRICTLY under the root: the root itself is exempt, as the
				// comment above says — a check of a root against itself
				// resolves the root FIRST, so a home swapped for a link
				// moves its own check's root with it and the ask answers
				// itself. Only the children pin the tree.
				if src != r && hasPathPrefix(src, r) && !named(src) {
					offenders = append(offenders, src+"   ["+m+"]")
				}
			}
		}
		if len(offenders) > 0 {
			t.Errorf("%s: %d bind source(s) under a sandbox-writable root are NOT named in Spec.BindSourceChecks — the plan-time verification ran but no border re-ask covers them, so the plan-to-create window stays open for exactly these:\n  %s",
				name, len(offenders), strings.Join(offenders, "\n  "))
		}
	}
}
