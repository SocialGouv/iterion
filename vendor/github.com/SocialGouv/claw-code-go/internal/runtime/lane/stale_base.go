package lane

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BaseCommitState describes the outcome of comparing HEAD against an expected base.
type BaseCommitState struct {
	Kind     BaseCommitStateKind
	Expected string // for Diverged
	Actual   string // for Diverged
}

// BaseCommitStateKind identifies the base commit state.
type BaseCommitStateKind string

const (
	BaseMatches     BaseCommitStateKind = "matches"
	BaseDiverged    BaseCommitStateKind = "diverged"
	BaseNoExpected  BaseCommitStateKind = "no_expected_base"
	BaseNotAGitRepo BaseCommitStateKind = "not_a_git_repo"
)

// BaseCommitSource identifies where the expected base commit came from.
type BaseCommitSource struct {
	Kind  BaseCommitSourceKind
	Value string
}

// BaseCommitSourceKind identifies the source type.
type BaseCommitSourceKind string

const (
	BaseSourceFlag BaseCommitSourceKind = "flag"
	BaseSourceFile BaseCommitSourceKind = "file"
)

// ReadClawBaseFile reads the .claw-base file from cwd and returns the
// trimmed commit hash. Returns ("", false) when absent or empty.
func ReadClawBaseFile(cwd string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(cwd, ".claw-base"))
	if err != nil {
		return "", false
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// ResolveExpectedBase resolves the expected base commit. Prefers the flag
// value; falls back to reading .claw-base from cwd.
func ResolveExpectedBase(flagValue *string, cwd string) *BaseCommitSource {
	if flagValue != nil {
		trimmed := strings.TrimSpace(*flagValue)
		if trimmed != "" {
			return &BaseCommitSource{Kind: BaseSourceFlag, Value: trimmed}
		}
	}
	if value, ok := ReadClawBaseFile(cwd); ok {
		return &BaseCommitSource{Kind: BaseSourceFile, Value: value}
	}
	return nil
}

// CheckBaseCommit verifies that HEAD matches the expected base commit.
func CheckBaseCommit(cwd string, expectedBase *BaseCommitSource) BaseCommitState {
	if expectedBase == nil {
		return BaseCommitState{Kind: BaseNoExpected}
	}

	expectedRaw := expectedBase.Value

	headSHA, ok := resolveHeadSHA(cwd)
	if !ok {
		return BaseCommitState{Kind: BaseNotAGitRepo}
	}

	expectedSHA, resolved := resolveRev(cwd, expectedRaw)
	if !resolved {
		// Partial SHA fallback: compare prefixes
		if strings.HasPrefix(headSHA, expectedRaw) || strings.HasPrefix(expectedRaw, headSHA) {
			return BaseCommitState{Kind: BaseMatches}
		}
		return BaseCommitState{
			Kind:     BaseDiverged,
			Expected: expectedRaw,
			Actual:   headSHA,
		}
	}

	if headSHA == expectedSHA {
		return BaseCommitState{Kind: BaseMatches}
	}
	return BaseCommitState{
		Kind:     BaseDiverged,
		Expected: expectedSHA,
		Actual:   headSHA,
	}
}

// FormatStaleBaseWarning returns a human-readable warning for non-match states.
// Returns ("", false) for Matches and NoExpectedBase.
func FormatStaleBaseWarning(state *BaseCommitState) (string, bool) {
	switch state.Kind {
	case BaseDiverged:
		return fmt.Sprintf(
			"warning: worktree HEAD (%s) does not match expected base commit (%s). Session may run against a stale codebase.",
			state.Actual, state.Expected,
		), true
	case BaseNotAGitRepo:
		return "warning: stale-base check skipped — not inside a git repository.", true
	default:
		return "", false
	}
}

func resolveHeadSHA(cwd string) (string, bool) {
	return resolveRev(cwd, "HEAD")
}

func resolveRev(cwd, rev string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", rev)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", false
	}
	return sha, true
}
