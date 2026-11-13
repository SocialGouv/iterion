package lane

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BranchFreshnessKind identifies the freshness state.
type BranchFreshnessKind string

const (
	FreshnessFresh    BranchFreshnessKind = "fresh"
	FreshnessStale    BranchFreshnessKind = "stale"
	FreshnessDiverged BranchFreshnessKind = "diverged"
)

// BranchFreshness describes the freshness of a branch relative to a reference.
type BranchFreshness struct {
	Kind          BranchFreshnessKind
	CommitsBehind int      // for Stale and Diverged
	CommitsAhead  int      // for Diverged
	MissingFixes  []string // commit subjects in reference not in branch
}

// StaleBranchPolicy describes the enforcement policy for stale branches.
type StaleBranchPolicy string

const (
	PolicyAutoRebase       StaleBranchPolicy = "auto_rebase"
	PolicyAutoMergeForward StaleBranchPolicy = "auto_merge_forward"
	PolicyWarnOnly         StaleBranchPolicy = "warn_only"
	PolicyBlock            StaleBranchPolicy = "block"
)

// StaleBranchAction is the action to take based on freshness and policy.
type StaleBranchAction struct {
	Kind    StaleBranchActionKind
	Message string // for Warn and Block
}

// StaleBranchActionKind identifies the action type.
type StaleBranchActionKind string

const (
	ActionNoop        StaleBranchActionKind = "noop"
	ActionWarn        StaleBranchActionKind = "warn"
	ActionBlockBranch StaleBranchActionKind = "block"
	ActionRebase      StaleBranchActionKind = "rebase"
	ActionMFwd        StaleBranchActionKind = "merge_forward"
)

// GitRunner abstracts git command execution for testability.
type GitRunner interface {
	// RevListCount returns the number of commits in the range b..a.
	RevListCount(ctx context.Context, repoPath, a, b string) int
	// LogSubjects returns commit subjects in the range b..a.
	LogSubjects(ctx context.Context, repoPath, a, b string) []string
}

// ExecGitRunner is the production implementation using exec.CommandContext.
type ExecGitRunner struct{}

func (r *ExecGitRunner) RevListCount(ctx context.Context, repoPath, a, b string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", fmt.Sprintf("%s..%s", b, a))
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

func (r *ExecGitRunner) LogSubjects(ctx context.Context, repoPath, a, b string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "log", "--format=%s", fmt.Sprintf("%s..%s", b, a))
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects
}

// CheckFreshness analyzes the freshness of branch relative to mainRef in
// repoPath using the given GitRunner.
func CheckFreshness(ctx context.Context, runner GitRunner, repoPath, branch, mainRef string) BranchFreshness {
	behind := runner.RevListCount(ctx, repoPath, mainRef, branch)
	ahead := runner.RevListCount(ctx, repoPath, branch, mainRef)

	if behind == 0 {
		return BranchFreshness{Kind: FreshnessFresh}
	}

	missingFixes := runner.LogSubjects(ctx, repoPath, mainRef, branch)

	if ahead > 0 {
		return BranchFreshness{
			Kind:          FreshnessDiverged,
			CommitsAhead:  ahead,
			CommitsBehind: behind,
			MissingFixes:  missingFixes,
		}
	}

	return BranchFreshness{
		Kind:          FreshnessStale,
		CommitsBehind: behind,
		MissingFixes:  missingFixes,
	}
}

// ApplyPolicy returns the action to take given freshness and policy.
func ApplyPolicy(freshness *BranchFreshness, policy StaleBranchPolicy) StaleBranchAction {
	switch freshness.Kind {
	case FreshnessFresh:
		return StaleBranchAction{Kind: ActionNoop}

	case FreshnessStale:
		switch policy {
		case PolicyWarnOnly:
			return StaleBranchAction{
				Kind: ActionWarn,
				Message: fmt.Sprintf(
					"Branch is %d commit(s) behind main. Missing fixes: %s",
					freshness.CommitsBehind,
					formatMissingFixes(freshness.MissingFixes),
				),
			}
		case PolicyBlock:
			return StaleBranchAction{
				Kind: ActionBlockBranch,
				Message: fmt.Sprintf(
					"Branch is %d commit(s) behind main and must be updated before proceeding.",
					freshness.CommitsBehind,
				),
			}
		case PolicyAutoRebase:
			return StaleBranchAction{Kind: ActionRebase}
		case PolicyAutoMergeForward:
			return StaleBranchAction{Kind: ActionMFwd}
		}

	case FreshnessDiverged:
		switch policy {
		case PolicyWarnOnly:
			return StaleBranchAction{
				Kind: ActionWarn,
				Message: fmt.Sprintf(
					"Branch has diverged: %d commit(s) ahead, %d commit(s) behind main. Missing fixes: %s",
					freshness.CommitsAhead,
					freshness.CommitsBehind,
					formatMissingFixes(freshness.MissingFixes),
				),
			}
		case PolicyBlock:
			return StaleBranchAction{
				Kind: ActionBlockBranch,
				Message: fmt.Sprintf(
					"Branch has diverged (%d ahead, %d behind) and must be reconciled before proceeding. Missing fixes: %s",
					freshness.CommitsAhead,
					freshness.CommitsBehind,
					formatMissingFixes(freshness.MissingFixes),
				),
			}
		case PolicyAutoRebase:
			return StaleBranchAction{Kind: ActionRebase}
		case PolicyAutoMergeForward:
			return StaleBranchAction{Kind: ActionMFwd}
		}
	}

	return StaleBranchAction{Kind: ActionNoop}
}

func formatMissingFixes(fixes []string) string {
	if len(fixes) == 0 {
		return "(none)"
	}
	return strings.Join(fixes, "; ")
}
