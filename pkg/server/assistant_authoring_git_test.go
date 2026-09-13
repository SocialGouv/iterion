package server

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func attestedGitFixture(t *testing.T) (string, func() (string, error)) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"selected", "unrelated"} {
		writeGitTestFile(t, filepath.Join(root, name), "before\n", 0o644)
	}
	initAuthoringGit(t, root)
	writeGitTestFile(t, filepath.Join(root, "selected"), "attested\n", 0o644)
	return root, func() (string, error) {
		return commitAttestedAuthoringFiles(t.Context(), root, []string{"selected"}, map[string]string{"selected": contentSHA256("attested\n")}, "subject\n\n# comment\n")
	}
}

func writeGitTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func gitTestRead(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := runAuthoringGit(t.Context(), root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gitTestHook(t *testing.T, root, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX Git hooks")
	}
	path := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "--path-format=absolute", "--git-path", "hooks/"+name))
	writeGitTestFile(t, path, "#!/bin/sh\nset -eu\n"+script+"\n", 0o755)
}

func TestAuthoringGitUsesAttestedTreeDespiteConcurrentEditor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX Git wrapper")
	}
	root, commit := attestedGitFixture(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrappers := t.TempDir()
	// Mutate immediately before the final commit command reads anything.
	// This fails the old commit --only implementation, which re-stages the
	// selected worktree paths after the host's index verification.
	t.Setenv("AUTHORING_REAL_GIT", realGit)
	t.Setenv("AUTHORING_EDITOR_FILE", filepath.Join(root, "selected"))
	writeGitTestFile(t, filepath.Join(wrappers, "git"), `#!/bin/sh
for arg do
  case "$arg" in commit|commit-tree) printf 'editor-newer\n' > "$AUTHORING_EDITOR_FILE"; break;; esac
done
exec "$AUTHORING_REAL_GIT" "$@"
`, 0o755)
	t.Setenv("PATH", wrappers+string(os.PathListSeparator)+os.Getenv("PATH"))
	// An unrelated index flag must survive installation of selected entries.
	gitTestRead(t, root, "update-index", "--assume-unchanged", "unrelated")
	before := gitTestRead(t, root, "ls-files", "-v", "--stage", "--", "unrelated")
	head, err := commit()
	if err != nil || head == "" {
		t.Fatalf("commit = %s, %v", head, err)
	}
	if got := gitTestRead(t, root, "show", head+":selected"); got != "attested\n" {
		t.Fatalf("unattested commit: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "selected")); string(got) != "editor-newer\n" {
		t.Fatalf("editor content lost: %q", got)
	}
	if got := gitTestRead(t, root, "ls-files", "-v", "--stage", "--", "unrelated"); got != before {
		t.Fatalf("unrelated index entry changed: %q -> %q", before, got)
	}
	if got := gitTestRead(t, root, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("index does not match commit: %q", got)
	}
}

func TestAuthoringGitNativeHookPhases(t *testing.T) {
	root, commit := attestedGitFixture(t)
	gitTestRead(t, root, "config", "commit.cleanup", "strip")
	gitTestHook(t, root, "pre-commit", `[ "$(cat selected)" = attested ]
[ "$GIT_EDITOR" = : ]
case "$GIT_INDEX_FILE" in /*) ;; *) exit 9;; esac
printf 'pre\n' >> hook-order`)
	gitTestHook(t, root, "prepare-commit-msg", `[ "$2" = message ]
printf 'prepare\n' >> hook-order
printf '\nprepared\n' >> "$1"`)
	gitTestHook(t, root, "commit-msg", `printf 'message\n' >> hook-order
printf '\nvalidated\n' >> "$1"`)
	gitTestHook(t, root, "post-commit", `git diff --cached --quiet
[ "$(git show HEAD:selected)" = attested ]
printf 'post\n' >> hook-order
exit 7`)
	head, err := commit()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "hook-order")); string(got) != "pre\nprepare\nmessage\npost\n" {
		t.Fatalf("hook order: %q", got)
	}
	if got := gitTestRead(t, root, "show", "-s", "--format=%B", head); got != "subject\n\nprepared\n\nvalidated\n\n" {
		t.Fatalf("message hooks/cleanup: %q", got)
	}
}

func TestAuthoringGitRejectsBeforePublishing(t *testing.T) {
	for _, kind := range []string{"hook rejects", "hook stages content", "hook changes mode", "head advances", "head changes branch", "head alias changes", "detached head becomes attached", "index locked", "unsupported cleanup", "invalid signing config", "signing failure"} {
		t.Run(kind, func(t *testing.T) {
			root, commit := attestedGitFixture(t)
			parent := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))
			original, err := os.ReadFile(filepath.Join(root, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			gitTestHook(t, root, "post-commit", "touch post-ran")
			switch kind {
			case "hook rejects":
				gitTestHook(t, root, "pre-commit", "exit 3")
			case "hook stages content":
				gitTestHook(t, root, "pre-commit", "printf 'unattested\\n' > selected\ngit add selected")
			case "hook changes mode":
				gitTestHook(t, root, "commit-msg", "git update-index --chmod=+x selected")
			case "head advances":
				gitTestHook(t, root, "pre-commit", `rival=$(git commit-tree HEAD^{tree} -p HEAD -m rival)
git update-ref HEAD "$rival"`)
			case "head changes branch", "detached head becomes attached":
				gitTestRead(t, root, "branch", "other")
				if kind == "detached head becomes attached" {
					gitTestRead(t, root, "checkout", "--detach", "--quiet")
				}
				gitTestHook(t, root, "pre-commit", "git symbolic-ref HEAD refs/heads/other")
			case "head alias changes":
				branch := strings.TrimSpace(gitTestRead(t, root, "symbolic-ref", "HEAD"))
				gitTestRead(t, root, "symbolic-ref", "refs/heads/alias", branch)
				gitTestRead(t, root, "symbolic-ref", "HEAD", "refs/heads/alias")
				gitTestHook(t, root, "pre-commit", `git symbolic-ref HEAD "$(git symbolic-ref refs/heads/alias)"`)
			case "index locked":
				writeGitTestFile(t, filepath.Join(root, ".git", "index.lock"), "editor-owned", 0o600)
			case "unsupported cleanup":
				gitTestRead(t, root, "config", "commit.cleanup", "invented")
			case "invalid signing config":
				gitTestRead(t, root, "config", "commit.gpgsign", "invalid-boolean")
			case "signing failure":
				gitTestRead(t, root, "config", "commit.gpgsign", "true")
				gitTestRead(t, root, "config", "gpg.program", filepath.Join(root, "missing-gpg"))
			}
			// A detached checkout legitimately refreshes its own index beforehand.
			if kind == "detached head becomes attached" {
				original, _ = os.ReadFile(filepath.Join(root, ".git", "index"))
			}
			head, err := commit()
			if err == nil || head != "" {
				t.Fatalf("unexpected publication: %s, %v", head, err)
			}
			current := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))
			if kind == "head advances" {
				if current == parent || strings.TrimSpace(gitTestRead(t, root, "show", "-s", "--format=%s", current)) != "rival" {
					t.Fatal("competing commit was lost")
				}
			} else if current != parent {
				t.Fatal("branch changed on rejection")
			}
			if kind == "head changes branch" || kind == "detached head becomes attached" {
				if got := strings.TrimSpace(gitTestRead(t, root, "symbolic-ref", "HEAD")); got != "refs/heads/other" {
					t.Fatal("competing HEAD binding was lost")
				}
			}
			if got, _ := os.ReadFile(filepath.Join(root, ".git", "index")); !bytes.Equal(got, original) {
				t.Fatal("real index changed on rejection")
			}
			if _, err := os.Stat(filepath.Join(root, "post-ran")); !os.IsNotExist(err) {
				t.Fatal("post-commit ran on rejected publication")
			}
			if kind == "index locked" {
				if got, _ := os.ReadFile(filepath.Join(root, ".git", "index.lock")); string(got) != "editor-owned" {
					t.Fatal("another process's lock was removed")
				}
			} else if _, err := os.Stat(filepath.Join(root, ".git", "index.lock")); !os.IsNotExist(err) {
				t.Fatal("owned index lock leaked")
			}
		})
	}
}

func TestAuthoringGitLinkedAndDetachedWorktrees(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "linked", true: "detached"}[detached], func(t *testing.T) {
			root, _ := attestedGitFixture(t)
			linked := filepath.Join(t.TempDir(), "linked")
			args := []string{"worktree", "add", "--quiet"}
			if detached {
				args = append(args, "--detach")
			} else {
				args = append(args, "-b", "linked")
			}
			gitTestRead(t, root, append(args, linked)...)
			writeGitTestFile(t, filepath.Join(linked, "selected"), "attested\n", 0o644)
			// Inherited redirection cannot redirect either objects or the index.
			t.Setenv("GIT_DIR", filepath.Join(root, ".git"))
			t.Setenv("GIT_WORK_TREE", root)
			t.Setenv("GIT_INDEX_FILE", filepath.Join(root, ".git", "index"))
			head, err := commitAttestedAuthoringFiles(context.Background(), linked, []string{"selected"}, map[string]string{"selected": contentSHA256("attested\n")}, "linked commit")
			if err != nil || head == "" {
				t.Fatalf("linked commit: %s %v", head, err)
			}
			if got := gitTestRead(t, linked, "show", "HEAD:selected"); got != "attested\n" {
				t.Fatal(got)
			}
			if got := gitTestRead(t, root, "show", "HEAD:selected"); got != "before\n" {
				t.Fatal("main worktree branch changed")
			}
		})
	}
}

func TestAuthoringGitFinishesIndexAfterRequestCancellation(t *testing.T) {
	root, _ := attestedGitFixture(t)
	gitTestHook(t, root, "reference-transaction", `if [ "$1" = committed ]; then
  touch published
  while [ ! -f release-hook ]; do sleep 0.01; done
fi`)
	gitTestHook(t, root, "post-commit", "git diff --cached --quiet\ntouch post-ran")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		hash string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		hash, err := commitAttestedAuthoringFiles(ctx, root, []string{"selected"}, map[string]string{"selected": contentSHA256("attested\n")}, "cancel after CAS")
		done <- result{hash, err}
	}()
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "published")); err == nil {
			break
		}
		select {
		case early := <-done:
			t.Fatalf("commit ended before publication: %+v", early)
		case <-deadline:
			t.Fatal("publication hook never ran")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	writeGitTestFile(t, filepath.Join(root, "release-hook"), "", 0o600)
	select {
	case got := <-done:
		if got.err != nil || got.hash == "" {
			t.Fatalf("cancelled finalization: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("finalization stuck after cancellation")
	}
	if got := gitTestRead(t, root, "diff", "--cached", "--name-only"); got != "" {
		t.Fatal("index was not installed")
	}
	if _, err := os.Stat(filepath.Join(root, "post-ran")); err != nil {
		t.Fatal("post-commit was skipped after publication")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatal("index lock leaked")
	}
}

func TestAuthoringGitReportsPublishedHashOnIndexFailure(t *testing.T) {
	root, commit := attestedGitFixture(t)
	gitTestHook(t, root, "reference-transaction", `if [ "$1" = committed ]; then
  mv .git/index .git/saved-index
  mkdir .git/index
fi`)
	gitTestHook(t, root, "post-commit", "touch post-ran")
	head, err := commit()
	if head == "" || err == nil || !strings.Contains(err.Error(), head) {
		t.Fatalf("publication not reported accurately: %s %v", head, err)
	}
	if got := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD")); got != head {
		t.Fatal("reported hash is not published")
	}
	if _, err := os.Stat(filepath.Join(root, "post-ran")); !os.IsNotExist(err) {
		t.Fatal("post-commit ran before index installation")
	}
}

func TestAuthoringGitHonorsSSHSigning(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen unavailable")
	}
	root, commit := attestedGitFixture(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Git executable: %s; %s; exec-path: %s", gitPath,
		strings.TrimSpace(gitTestRead(t, root, "--version")), strings.TrimSpace(gitTestRead(t, root, "--exec-path")))
	key := filepath.Join(t.TempDir(), "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("generate signing key: %v: %s", err, out)
	}
	gitTestRead(t, root, "config", "gpg.format", "ssh")
	gitTestRead(t, root, "config", "user.signingkey", key)
	gitTestRead(t, root, "config", "commit.gpgsign", "true")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed-signers")
	writeGitTestFile(t, allowed, "authoring@example.invalid "+string(pub), 0o600)
	gitTestRead(t, root, "config", "gpg.ssh.allowedSignersFile", allowed)
	head, err := commit()
	if err != nil {
		t.Fatal(err)
	}
	gitTestRead(t, root, "verify-commit", head)
}

func TestAuthoringGitBooleanConfig(t *testing.T) {
	for _, tc := range []struct {
		value, want string
	}{
		{"unset", ""}, {"true", "true"}, {"yes", "true"}, {"on", "true"},
		{"1", "true"}, {"false", "false"}, {"0", "false"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			root, _ := attestedGitFixture(t)
			if tc.value != "unset" {
				gitTestRead(t, root, "config", "commit.gpgsign", tc.value)
			}
			g := authoringGitIndex{root: root, index: filepath.Join(root, ".git", "index")}
			got, err := g.config(t.Context(), "commit.gpgsign", true)
			if err != nil || got != tc.want {
				t.Fatalf("boolean config %q: got %q, %v; want %q", tc.value, got, err, tc.want)
			}
		})
	}
}
