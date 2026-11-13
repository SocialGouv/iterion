package lane

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// BranchLockScalingWarningThreshold is the claim count above which an N²
// collision check logs a warning.
const BranchLockScalingWarningThreshold = 50

// BranchLockIntent describes a lane's intent to operate on a branch and modules.
type BranchLockIntent struct {
	LaneID   string   `json:"laneId"`
	Branch   string   `json:"branch"`
	Worktree *string  `json:"worktree,omitempty"`
	Modules  []string `json:"modules,omitempty"`
}

// BranchLockCollision describes a detected collision between lanes.
type BranchLockCollision struct {
	Branch  string   `json:"branch"`
	Module  string   `json:"module"`
	LaneIDs []string `json:"laneIds"`
}

// DetectBranchLockCollisions performs N² pairwise comparison of intents to
// find collisions on the same branch with overlapping modules.
func DetectBranchLockCollisions(intents []BranchLockIntent) []BranchLockCollision {
	var collisions []BranchLockCollision

	for i, left := range intents {
		for _, right := range intents[i+1:] {
			if left.Branch != right.Branch {
				continue
			}
			for _, module := range overlappingModules(left.Modules, right.Modules) {
				collisions = append(collisions, BranchLockCollision{
					Branch:  left.Branch,
					Module:  module,
					LaneIDs: []string{left.LaneID, right.LaneID},
				})
			}
		}
	}

	sort.Slice(collisions, func(i, j int) bool {
		if collisions[i].Branch != collisions[j].Branch {
			return collisions[i].Branch < collisions[j].Branch
		}
		if collisions[i].Module != collisions[j].Module {
			return collisions[i].Module < collisions[j].Module
		}
		return fmt.Sprint(collisions[i].LaneIDs) < fmt.Sprint(collisions[j].LaneIDs)
	})

	// Dedup
	if len(collisions) > 1 {
		j := 0
		for i := 1; i < len(collisions); i++ {
			if collisions[i].Branch != collisions[j].Branch ||
				collisions[i].Module != collisions[j].Module ||
				fmt.Sprint(collisions[i].LaneIDs) != fmt.Sprint(collisions[j].LaneIDs) {
				j++
				collisions[j] = collisions[i]
			}
		}
		collisions = collisions[:j+1]
	}

	return collisions
}

func overlappingModules(left, right []string) []string {
	var overlaps []string
	for _, lm := range left {
		for _, rm := range right {
			if modulesOverlap(lm, rm) {
				overlaps = append(overlaps, sharedScope(lm, rm))
			}
		}
	}
	sort.Strings(overlaps)
	// Dedup
	if len(overlaps) > 1 {
		j := 0
		for i := 1; i < len(overlaps); i++ {
			if overlaps[i] != overlaps[j] {
				j++
				overlaps[j] = overlaps[i]
			}
		}
		overlaps = overlaps[:j+1]
	}
	return overlaps
}

func modulesOverlap(left, right string) bool {
	return left == right ||
		strings.HasPrefix(left, right+"/") ||
		strings.HasPrefix(right, left+"/")
}

func sharedScope(left, right string) string {
	if strings.HasPrefix(left, right+"/") || left == right {
		return right
	}
	return left
}

// ErrBranchCollision is returned when Acquire detects a collision.
var ErrBranchCollision = fmt.Errorf("branch lock collision detected")

// BranchLockManager manages branch lock claims with collision detection.
type BranchLockManager struct {
	mu     sync.RWMutex
	claims []BranchLockIntent
}

// NewBranchLockManager creates a new BranchLockManager.
func NewBranchLockManager() *BranchLockManager {
	return &BranchLockManager{}
}

// Acquire adds a claim after checking for collisions. Returns
// ErrBranchCollision if a collision is detected.
func (m *BranchLockManager) Acquire(intent BranchLockIntent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Defensive copy: avoid aliasing m.claims via append's capacity reuse.
	candidate := make([]BranchLockIntent, len(m.claims)+1)
	copy(candidate, m.claims)
	candidate[len(m.claims)] = intent

	if len(candidate) > BranchLockScalingWarningThreshold {
		slog.Debug("branch lock manager scaling warning", "claims", len(candidate))
	}

	collisions := DetectBranchLockCollisions(candidate)
	if len(collisions) > 0 {
		return fmt.Errorf("%w: %s on branch %s (lanes %v)",
			ErrBranchCollision,
			collisions[0].Module,
			collisions[0].Branch,
			collisions[0].LaneIDs,
		)
	}

	m.claims = candidate
	return nil
}

// Release removes the claim for the given lane ID.
func (m *BranchLockManager) Release(laneID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var remaining []BranchLockIntent
	for _, c := range m.claims {
		if c.LaneID != laneID {
			remaining = append(remaining, c)
		}
	}
	m.claims = remaining
}

// ActiveClaims returns a copy of the current claims.
func (m *BranchLockManager) ActiveClaims() []BranchLockIntent {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]BranchLockIntent, len(m.claims))
	copy(result, m.claims)
	return result
}
