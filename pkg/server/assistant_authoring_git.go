package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// authoringGitIndex owns an alternate index, never the editor's index. Hooks
// see the real worktree, but commit-tree only sees the verified immutable tree.
type authoringGitIndex struct{ root, index string }

func (g authoringGitIndex) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.root}, gitlib.NoAutoMaintenance(args...)...)...)
	cmd.Env = append(gitlib.SanitizeEnv(os.Environ()), "GIT_INDEX_FILE="+g.index, "GIT_EDITOR=:")
	return cmd
}

func (g authoringGitIndex) run(ctx context.Context, input string, args ...string) (string, error) {
	cmd := g.command(ctx, args...)
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (g authoringGitIndex) config(ctx context.Context, key string, boolean bool) (string, error) {
	args := []string{"config", "--get", key}
	if boolean {
		args = []string{"config", "--bool", "--get", key}
	}
	out, err := g.run(ctx, "", args...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return strings.TrimSpace(out), err
}

func (g authoringGitIndex) tree(ctx context.Context, parent string, paths []string, hashes map[string]string) (string, error) {
	changed, err := g.run(ctx, "", "diff", "--cached", "--name-only", "-z", parent)
	if err != nil {
		return "", err
	}
	if !sameGitPathSet(splitGitPathList(changed), paths) {
		return "", authoringConflictError{"staged paths differ from the selected authoring files"}
	}
	for _, path := range paths {
		entry, err := g.run(ctx, "", "ls-files", "--stage", "-z", "--", path)
		if err != nil {
			return "", err
		}
		meta, name, ok := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
		fields := strings.Fields(meta)
		if !ok || name != path || len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755") {
			return "", fmt.Errorf("selected file %q must be a regular, resolved Git blob", path)
		}
		blob, err := g.run(ctx, "", "cat-file", "blob", fields[1])
		if err != nil {
			return "", err
		}
		if contentSHA256(blob) != hashes[path] {
			return "", authoringConflictError{fmt.Sprintf("staged content for %q differs from the host snapshot", path)}
		}
	}
	tree, err := g.run(ctx, "", "write-tree")
	return strings.TrimSpace(tree), err
}

func commitAttestedAuthoringFiles(ctx context.Context, root string, paths []string, hashes map[string]string, message string) (published string, err error) {
	version, err := runAuthoringGit(ctx, root, "version")
	if err != nil {
		return "", err
	}
	var major, minor int
	if _, err := fmt.Sscanf(version, "git version %d.%d", &major, &minor); err != nil || major < 2 || (major == 2 && minor < 46) {
		return "", errors.New("attested authoring commits require Git 2.46 or newer for guarded ref transactions")
	}
	indexPath, err := runAuthoringGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	indexPath = strings.TrimSpace(indexPath)
	lock, err := os.OpenFile(indexPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("acquire authoring Git index lock (another Git operation may be active): %w", err)
	}
	lockOwned := true
	defer func() {
		_ = lock.Close()
		if lockOwned {
			_ = os.Remove(indexPath + ".lock")
		}
	}()
	originalIndex, err := os.ReadFile(indexPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(indexPath)
	if err != nil {
		return "", err
	}
	if err := lock.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(filepath.Dir(indexPath), "iterion-authoring-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	g := authoringGitIndex{root: root, index: filepath.Join(dir, "index")}
	cleanup, err := g.config(ctx, "commit.cleanup", false)
	if err != nil {
		return "", err
	}
	switch cleanup {
	case "", "default", "whitespace", "strip", "verbatim", "scissors":
	default:
		return "", fmt.Errorf("unsupported commit.cleanup %q; nothing was published", cleanup)
	}
	sign, err := g.config(ctx, "commit.gpgsign", true)
	if err != nil {
		return "", err
	}
	parentOut, err := g.run(ctx, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	parent := strings.TrimSpace(parentOut)
	binding, err := authoringHeadBinding(ctx, g)
	if err != nil {
		return "", err
	}
	// Refuse operation states whose native commit has extra semantics (merge
	// parents, sequencer messages). A normal commit can finish those first.
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		path, err := g.run(ctx, "", "rev-parse", "--path-format=absolute", "--git-path", state)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(strings.TrimSpace(path)); err == nil {
			return "", fmt.Errorf("finish the active Git %s operation before an authoring commit", state)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	// Check the real index while holding its conventional lock. All subsequent
	// staging happens in an alternate index; any rejection leaves it intact.
	real := authoringGitIndex{root: root, index: indexPath}
	staged, err := real.run(ctx, "", "diff", "--cached", "--name-only", "-z", parent)
	if err != nil {
		return "", err
	}
	selected := map[string]bool{}
	for _, path := range paths {
		selected[path] = true
	}
	for _, path := range splitGitPathList(staged) {
		if !selected[path] {
			return "", fmt.Errorf("git index already contains non-selected path %q; leave it untouched and commit it separately", path)
		}
	}
	tracked, err := real.run(ctx, "", append([]string{"ls-files", "--error-unmatch", "-z", "--"}, paths...)...)
	if err != nil {
		return "", fmt.Errorf("selected authoring files must already be Git-tracked: %w", err)
	}
	if !sameGitPathSet(splitGitPathList(tracked), paths) {
		return "", errors.New("git did not resolve exactly the selected authoring files")
	}
	if _, err := g.run(ctx, "", "read-tree", parent); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, "", append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	verifiedTree, err := g.tree(ctx, parent, paths, hashes)
	if err != nil {
		return "", err
	}
	// Update only selected entries in a COPY of the editor's index. Preserve
	// unrelated entry flags and cached metadata, not merely their blob bytes.
	prepared := authoringGitIndex{root: root, index: filepath.Join(dir, "final-index")}
	if err := os.WriteFile(prepared.index, originalIndex, info.Mode().Perm()); err != nil {
		return "", err
	}
	entries, err := g.run(ctx, "", append([]string{"ls-files", "--stage", "-z", "--"}, paths...)...)
	if err != nil {
		return "", err
	}
	if _, err := prepared.run(ctx, entries, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	msgPath := filepath.Join(dir, "COMMIT_EDITMSG")
	if err := os.WriteFile(msgPath, []byte(message+"\n"), 0o600); err != nil {
		return "", err
	}
	for _, hook := range [][]string{{"pre-commit"}, {"prepare-commit-msg", "--", msgPath, "message"}, {"commit-msg", "--", msgPath}} {
		if _, err := g.run(ctx, "", append([]string{"hook", "run", "--ignore-missing"}, hook...)...); err != nil {
			return "", err
		}
	}
	afterHooks, err := g.tree(ctx, parent, paths, hashes)
	if err != nil || afterHooks != verifiedTree {
		return "", authoringConflictError{"a Git hook changed staged content; nothing was published; review the worktree and take a fresh host snapshot"}
	}
	msg, err := os.ReadFile(msgPath)
	if err != nil {
		return "", err
	}
	if cleanup != "verbatim" {
		args := []string{"stripspace"}
		if cleanup == "strip" {
			args = append(args, "--strip-comments")
		}
		clean, err := g.run(ctx, string(msg), args...)
		if err != nil {
			return "", err
		}
		msg = []byte(clean)
	}
	if strings.TrimSpace(string(msg)) == "" {
		return "", errors.New("git commit message is empty after hooks and cleanup")
	}
	args := []string{"commit-tree", verifiedTree, "-p", parent, "-F", "-"}
	if sign == "true" {
		args = append(args, "-S")
	}
	commit, err := g.run(ctx, string(msg), args...)
	if err != nil {
		return "", err
	}
	commit = strings.TrimSpace(commit)
	identity, err := g.run(ctx, "", "show", "-s", "--format=%T %P", commit)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(identity) != verifiedTree+" "+parent {
		return "", errors.New("prepared commit does not match the verified tree and parent")
	}
	finalIndex, err := os.ReadFile(prepared.index)
	if err != nil {
		return "", err
	}
	if _, err := lock.Write(finalIndex); err != nil {
		return "", err
	}
	if err := lock.Sync(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Once publication begins, a disconnected request cannot strand the index
	// lock or skip installation after a successful CAS.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	visible, publishErr := publishAuthoringCommit(finishCtx, g, parent, commit, binding)
	if !visible {
		return "", publishErr
	}
	published = commit
	if err := lock.Close(); err != nil {
		return published, fmt.Errorf("commit %s was published but closing its index failed: %w", published, err)
	}
	if err := os.Rename(indexPath+".lock", indexPath); err != nil {
		return published, fmt.Errorf("commit %s was published but installing its index failed: %w", published, err)
	}
	lockOwned = false
	// Native post-commit failures do not roll back or reject a committed
	// change. Run it only once the branch and the editor's index agree.
	_, _ = real.run(finishCtx, "", "hook", "run", "--ignore-missing", "post-commit")
	if publishErr != nil {
		return published, fmt.Errorf("commit %s was published and its index installed, but Git reported a transaction error: %w", published, publishErr)
	}
	return published, nil
}

func authoringHeadBinding(ctx context.Context, g authoringGitIndex) (string, error) {
	out, err := g.run(ctx, "", "symbolic-ref", "--quiet", "HEAD")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", nil
	} // detached HEAD
	return strings.TrimSpace(out), err
}

// update HEAD prepares both the referent and HEAD's own log/ref lock. Checking
// its binding AFTER prepare is protected by Git's locks through commit. A
// symref-verify HEAD plus update <branch> transaction is rejected by Git's
// files backend as a duplicate HEAD update, so do not split those into two
// unlocked transactions. This also protects a detached HEAD becoming attached.
func publishAuthoringCommit(ctx context.Context, g authoringGitIndex, parent, commit, binding string) (bool, error) {
	cmd := g.command(ctx, "update-ref", "-m", "commit: authoring", "--stdin")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return false, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return false, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return false, err
	}
	reader := bufio.NewReader(out)
	send := func(commands, expected string) error {
		if _, err := fmt.Fprint(in, commands); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) != expected {
			return fmt.Errorf("unexpected Git transaction response %q", line)
		}
		return nil
	}
	txErr := send("start\n", "start: ok")
	if txErr == nil {
		txErr = send(fmt.Sprintf("update HEAD %s %s\nprepare\n", commit, parent), "prepare: ok")
	}
	if txErr == nil {
		actual, err := authoringHeadBinding(ctx, g)
		if err != nil {
			txErr = err
		} else if actual != binding {
			txErr = authoringConflictError{"HEAD changed branches during commit preparation; nothing was published"}
		}
	}
	committed := false
	if txErr == nil {
		txErr = send("commit\n", "commit: ok")
		committed = txErr == nil
	}
	// EOF aborts any uncommitted transaction and releases Git-owned locks.
	_ = in.Close()
	waitErr := cmd.Wait()
	if txErr != nil || waitErr != nil {
		// A process can lose its acknowledgement after the ref write. Do not
		// leave the real index stale when publication can still be established.
		if !committed {
			probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			head, err := g.run(probeCtx, "", "rev-parse", "--verify", "HEAD")
			cancel()
			committed = err == nil && strings.TrimSpace(head) == commit
		}
		return committed, fmt.Errorf("publish verified Git commit: %w: %s", errors.Join(txErr, waitErr), strings.TrimSpace(stderr.String()))
	}
	return true, nil
}
