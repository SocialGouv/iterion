package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isCommitPin reports whether ref names ONE immutable commit — a full
// lowercase object id — rather than a branch or a tag.
//
// It asks the same regexp ShallowClone switches on rather than a second
// "looks like a sha" test: two such predicates disagree the first time
// either is widened, and they are read on opposite sides of one decision.
//
// A sha256 object id (64 characters) is NOT a pin here, because the fetch
// below proves its result against a 40-character id; widening the shape
// without widening the proof would accept a pin nothing checks.
func isCommitPin(ref string) bool {
	return fullCommitSHA.MatchString(strings.TrimSpace(ref))
}

// MaterializePin writes the source tree at exactly pin into dest, as a PLAIN
// directory: the git metadata is dropped once the pin is proved.
//
// It is the install-time half of a pinned install, and it is deliberately not
// usable at resolution time: no repository is left behind, so nothing an
// operator installed can be moved by a later fetch, and a tier that resolves
// an operation never depends on a remote host answering.
//
// The commit is fetched BY OBJECT ID rather than cloned by branch: `clone
// --branch` cannot take a commit at all, and a branch tip moves between the
// operator reading a sha and this process asking for it — which is the whole
// difference between "the bytes that were reviewed" and "whatever the source
// says now".
//
// source is a git URL, gated by ValidateCloneSource, or a path to a repository
// that already exists on this machine — a colleague's clone, a fixture. A path
// is not a transport the operator did not choose, but git reads `a::b` as a
// remote helper wherever it appears, so that marker is refused in a path too.
//
// dest must be an absolute path the CALLER owns and is free to have git
// initialise: it is created when absent, and the `.git` this function makes
// inside it is the only thing it removes.
func MaterializePin(ctx context.Context, source, pin, dest string) error {
	pin = strings.TrimSpace(pin)
	if !isCommitPin(pin) {
		return fmt.Errorf("git: %q is not a commit pin (a full 40-character lowercase object id): a branch or a tag moves, so what it names today is not what was read", pin)
	}
	url, err := pinSource(source)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("git: materialize %s: destination %q must be an absolute path", pin, dest)
	}
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		// A caller that handed this function a live directory — a catalog
		// root, a checkout — would have git check out over it. Refused
		// rather than merged: the contract is "a tree this call owns".
		return fmt.Errorf("git: materialize %s: %s already holds %d entries, and this writes a tree of its own", pin, dest, len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("git: materialize %s: read %s: %w", pin, dest, err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("git: materialize %s: create %s: %w", pin, dest, err)
	}
	if _, err := cloneGitOutput(ctx, "init", "--quiet", dest); err != nil {
		return err
	}
	// The pin as the refspec. A host that refuses to serve a reachable object
	// id reports ITS error here: falling back to a branch would install
	// something nobody asked for under a name that says otherwise.
	if _, err := cloneGitOutput(ctx, "-C", dest, "fetch", "--depth", "1", url, pin); err != nil {
		return err
	}
	if _, err := cloneGitOutput(ctx, "-C", dest, "checkout", "--detach", "--quiet", "FETCH_HEAD"); err != nil {
		return err
	}
	head, err := cloneGitOutput(ctx, "-C", dest, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(head); got != pin {
		return fmt.Errorf("git: materialize resolved %s, want the requested commit %s", got, pin)
	}
	// Dropped AFTER the proof: the tree is a materialization, not a working
	// repository. Only this path, and only the one this function created.
	if err := os.RemoveAll(filepath.Join(dest, ".git")); err != nil {
		return fmt.Errorf("git: materialize %s: drop the fetch metadata: %w", pin, err)
	}
	return nil
}

// pinSource resolves what to hand git as the remote: an absolute path to a
// repository on this machine, or a validated git URL.
func pinSource(source string) (string, error) {
	s := strings.TrimSpace(source)
	if s == "" {
		return "", fmt.Errorf("git: materialize: a git URL or a local repository path is required")
	}
	if strings.Contains(s, "::") || strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("git: source %q uses an unsupported transport (remote-helper transports such as ext:: are not allowed, in a path either)", source)
	}
	if info, statErr := os.Stat(s); statErr == nil && info.IsDir() {
		abs, absErr := filepath.Abs(s)
		if absErr != nil {
			return "", fmt.Errorf("git: source %q: %w", source, absErr)
		}
		if !isRepositoryDir(abs) {
			return "", fmt.Errorf("git: source %s is a directory but not a git repository (no .git, no bare HEAD), so no commit can be read from it", abs)
		}
		return abs, nil
	}
	if err := ValidateCloneSource(s); err != nil {
		return "", err
	}
	return s, nil
}

// isRepositoryDir reports whether dir looks like a repository git can serve —
// a work tree with .git, or a bare repository. Asked before the fetch so the
// refusal names the directory instead of forwarding git's "does not appear to
// be a git repository" from inside a temporary clone.
func isRepositoryDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "objects")); err == nil {
			return true
		}
	}
	return false
}
