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
