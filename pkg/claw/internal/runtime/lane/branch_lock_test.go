package lane

import (
	"errors"
	"sync"
	"testing"
)

func TestDetectsSameBranchSameModuleCollisions(t *testing.T) {
	wta := "wt-a"
	wtb := "wt-b"
	collisions := DetectBranchLockCollisions([]BranchLockIntent{
		{LaneID: "lane-a", Branch: "feature/lock", Worktree: &wta, Modules: []string{"runtime/mcp"}},
		{LaneID: "lane-b", Branch: "feature/lock", Worktree: &wtb, Modules: []string{"runtime/mcp"}},
	})

	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if collisions[0].Branch != "feature/lock" {
		t.Errorf("branch = %s", collisions[0].Branch)
	}
	if collisions[0].Module != "runtime/mcp" {
		t.Errorf("module = %s", collisions[0].Module)
	}
}

func TestDetectsNestedModuleScopeCollisions(t *testing.T) {
	collisions := DetectBranchLockCollisions([]BranchLockIntent{
		{LaneID: "lane-a", Branch: "feature/lock", Modules: []string{"runtime"}},
		{LaneID: "lane-b", Branch: "feature/lock", Modules: []string{"runtime/mcp"}},
	})

	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if collisions[0].Module != "runtime" {
		t.Errorf("module = %s, want runtime", collisions[0].Module)
	}
}

func TestIgnoresDifferentBranches(t *testing.T) {
	collisions := DetectBranchLockCollisions([]BranchLockIntent{
		{LaneID: "lane-a", Branch: "feature/a", Modules: []string{"runtime/mcp"}},
		{LaneID: "lane-b", Branch: "feature/b", Modules: []string{"runtime/mcp"}},
	})

	if len(collisions) != 0 {
		t.Fatalf("expected 0 collisions, got %d", len(collisions))
	}
}

func TestBranchLockManagerAcquireAndRelease(t *testing.T) {
	mgr := NewBranchLockManager()

	err := mgr.Acquire(BranchLockIntent{
		LaneID:  "lane-a",
		Branch:  "main",
		Modules: []string{"src/api"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Different module on same branch should be fine
	err = mgr.Acquire(BranchLockIntent{
		LaneID:  "lane-b",
		Branch:  "main",
		Modules: []string{"src/web"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Overlapping module should fail
	err = mgr.Acquire(BranchLockIntent{
		LaneID:  "lane-c",
		Branch:  "main",
		Modules: []string{"src/api/v2"},
	})
	if !errors.Is(err, ErrBranchCollision) {
		t.Fatalf("expected ErrBranchCollision, got %v", err)
	}

	// Release lane-a, then lane-c should succeed
	mgr.Release("lane-a")
	err = mgr.Acquire(BranchLockIntent{
		LaneID:  "lane-c",
		Branch:  "main",
		Modules: []string{"src/api/v2"},
	})
	if err != nil {
		t.Fatalf("expected success after release, got %v", err)
	}

	claims := mgr.ActiveClaims()
	if len(claims) != 2 {
		t.Errorf("expected 2 claims, got %d", len(claims))
	}
}

// TestBranchLockAcquireDefensiveCopy verifies that mutating the returned claims
// slice does not affect the manager's internal state (defensive copy in Acquire).
func TestBranchLockAcquireDefensiveCopy(t *testing.T) {
	mgr := NewBranchLockManager()

	// Acquire a claim.
	err := mgr.Acquire(BranchLockIntent{
		LaneID:  "lane-a",
		Branch:  "main",
		Modules: []string{"src/api"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Get claims and mutate the returned slice.
	claims := mgr.ActiveClaims()
	claims[0].LaneID = "MUTATED"
	claims[0].Branch = "MUTATED"

	// Verify the internal state is unaffected.
	internalClaims := mgr.ActiveClaims()
	if internalClaims[0].LaneID != "lane-a" {
		t.Errorf("internal claims corrupted: LaneID = %q, want 'lane-a'", internalClaims[0].LaneID)
	}
	if internalClaims[0].Branch != "main" {
		t.Errorf("internal claims corrupted: Branch = %q, want 'main'", internalClaims[0].Branch)
	}
}

// TestBranchLockAcquireNoAliasing verifies that Acquire does not alias the
// internal claims slice via append capacity reuse.
func TestBranchLockAcquireNoAliasing(t *testing.T) {
	mgr := NewBranchLockManager()

	// Acquire two claims on different branches (no collision).
	_ = mgr.Acquire(BranchLockIntent{LaneID: "lane-a", Branch: "a", Modules: []string{"m1"}})
	_ = mgr.Acquire(BranchLockIntent{LaneID: "lane-b", Branch: "b", Modules: []string{"m2"}})

	// Get the internal claims after 2 acquires.
	before := mgr.ActiveClaims()
	if len(before) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(before))
	}

	// Acquire a third claim. If append aliased, the internal slice might
	// have been mutated by the candidate slice during collision detection.
	_ = mgr.Acquire(BranchLockIntent{LaneID: "lane-c", Branch: "c", Modules: []string{"m3"}})

	// All three must be present and intact.
	after := mgr.ActiveClaims()
	if len(after) != 3 {
		t.Fatalf("expected 3 claims, got %d", len(after))
	}
	for i, want := range []string{"lane-a", "lane-b", "lane-c"} {
		if after[i].LaneID != want {
			t.Errorf("claims[%d].LaneID = %q, want %q", i, after[i].LaneID, want)
		}
	}
}

func TestBranchLockManagerConcurrentSafety(t *testing.T) {
	mgr := NewBranchLockManager()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			laneID := "lane-" + string(rune('a'+id))
			branch := "feature/" + string(rune('a'+id))
			_ = mgr.Acquire(BranchLockIntent{
				LaneID:  laneID,
				Branch:  branch,
				Modules: []string{"src"},
			})
		}(i)
	}
	wg.Wait()

	claims := mgr.ActiveClaims()
	if len(claims) != 20 {
		t.Errorf("expected 20 claims (all different branches), got %d", len(claims))
	}
}
