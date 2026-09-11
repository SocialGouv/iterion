package git

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ShallowClone clones url into dest with shallow history. A branch or tag ref
// keeps the normal `clone --branch` path. A full lowercase commit SHA instead
// uses fetch + detached checkout because `git clone --branch` does not accept
// commit objects. dest must not already exist (git clone requires an absent
// target). Network access and authentication are git's responsibility: the
// host's configured credential helpers and SSH keys apply, exactly as for a
// manual `git clone`.
//
// url is gated by ValidateCloneSource: only https:// and ssh git URLs are
// accepted, so dangerous transports (ext::, file://, …) are rejected before
// git runs. The `--` sentinel below is kept as additional flag-injection
// defense in depth — it is NOT a transport check.
func ShallowClone(ctx context.Context, url, ref, dest string) error {
	if err := ValidateCloneSource(url); err != nil {
		return err
	}
	if fullCommitSHA.MatchString(ref) {
		return shallowCloneCommit(ctx, url, ref, dest)
	}
	return shallowCloneNamedRef(ctx, url, ref, dest)
}

func shallowCloneNamedRef(ctx context.Context, url, ref, dest string) error {
	args := namedRefCloneArgs(url, ref, dest)
	if err := runCloneGit(ctx, args...); err != nil {
		return fmt.Errorf("git clone %s: %w", url, err)
	}
	return nil
}

func namedRefCloneArgs(url, ref, dest string) []string {
	args := []string{"clone", "--depth", "1", "--single-branch"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	return append(args, "--", url, dest)
}

// shallowCloneCommit first obtains the normal shallow remote envelope without
// checking out its default branch. It then requests exactly the reachable
// object pin, detaches at it and proves that HEAD is byte-for-byte the pin.
// Git hosts that disallow fetching reachable SHA wants report the fetch error;
// silently falling back to a default branch would violate the lock contract.
func shallowCloneCommit(ctx context.Context, url, ref, dest string) error {
	if !fullCommitSHA.MatchString(ref) {
		return fmt.Errorf("commit ref must be a lowercase 40-character SHA")
	}
	if err := runCloneGit(ctx, commitCloneArgs(url, dest)...); err != nil {
		return fmt.Errorf("git clone %s: %w", url, err)
	}
	if err := runCloneGit(ctx, "-C", dest, "fetch", "--depth", "1", "origin", ref); err != nil {
		return fmt.Errorf("git fetch exact commit %s from %s: %w", ref, url, err)
	}
	if err := runCloneGit(ctx, "-C", dest, "checkout", "--detach", "--quiet", ref); err != nil {
		return fmt.Errorf("git checkout exact commit %s: %w", ref, err)
	}
	head, err := cloneGitOutput(ctx, "-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("git verify exact commit %s: %w", ref, err)
	}
	if strings.TrimSpace(head) != ref {
		return fmt.Errorf("git checkout resolved %s, want requested commit %s", strings.TrimSpace(head), ref)
	}
	return nil
}

func commitCloneArgs(url, dest string) []string {
	return []string{"clone", "--depth", "1", "--single-branch", "--no-checkout", "--", url, dest}
}

func runCloneGit(ctx context.Context, args ...string) error {
	_, err := cloneGitOutput(ctx, args...)
	return err
}

func cloneGitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
