package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Provenance describes the repository state that owns a workflow source.
// Commit and TreeHash identify the committed baseline; Dirty reports whether
// the operator checkout also contains staged, unstaged, or untracked files.
type Provenance struct {
	RepoRoot string
	Commit   string
	TreeHash string
	Dirty    bool
}

// Describe returns the git provenance for path. path may name either a file
// inside a checkout or the checkout itself.
func Describe(path string) (Provenance, error) {
	start := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		start = filepath.Dir(path)
	}
	root := FindRepoRoot(start)
	if root == "" {
		return Provenance{}, ErrNotGitRepo
	}
	head, err := run(root, "rev-parse", "HEAD")
	if err != nil {
		return Provenance{}, err
	}
	tree, err := run(root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return Provenance{}, err
	}
	status, err := run(root, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return Provenance{}, err
	}
	return Provenance{
		RepoRoot: root,
		Commit:   strings.TrimSpace(string(head)),
		TreeHash: strings.TrimSpace(string(tree)),
		Dirty:    len(status) > 0,
	}, nil
}

// SnapshotWorkingTree writes the checkout's current tracked and untracked
// contents to a hidden commit without changing its index, HEAD, or files.
// The returned commit is suitable as the explicit base of a linked worktree.
// namespace must already be a bounded host-derived identifier.
func SnapshotWorkingTree(repoRoot, namespace string) (commit, tree string, err error) {
	if FindRepoRoot(repoRoot) == "" {
		return "", "", ErrNotGitRepo
	}
	namespace = strings.Trim(strings.TrimSpace(namespace), "/")
	if namespace == "" || strings.Contains(namespace, "..") {
		return "", "", fmt.Errorf("git: invalid snapshot namespace")
	}
	tmp, err := os.MkdirTemp("", "iterion-git-index-*")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	index := filepath.Join(tmp, "index")

	runWithIndex := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
		cmd.Env = append(SanitizeEnv(os.Environ()),
			"LC_ALL=C", "LANG=C", "GIT_INDEX_FILE="+index,
			"GIT_AUTHOR_NAME=Iterion", "GIT_AUTHOR_EMAIL=iterion@localhost",
			"GIT_COMMITTER_NAME=Iterion", "GIT_COMMITTER_EMAIL=iterion@localhost")
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			return "", fmt.Errorf("git %s: %w (output: %s)", strings.Join(args, " "), cmdErr, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := runWithIndex("read-tree", "HEAD"); err != nil {
		return "", "", err
	}
	if _, err := runWithIndex("add", "-A", "--", "."); err != nil {
		return "", "", err
	}
	tree, err = runWithIndex("write-tree")
	if err != nil {
		return "", "", err
	}
	head, err := runWithIndex("rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	commit, err = runWithIndex("commit-tree", tree, "-p", head, "-m", "iterion delegated repair snapshot")
	if err != nil {
		return "", "", err
	}
	if _, err := runWithIndex("update-ref", "refs/iterion/delegations/"+namespace+"/base", commit); err != nil {
		return "", "", err
	}
	return commit, tree, nil
}
