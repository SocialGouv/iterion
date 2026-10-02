package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/ambient"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/git"
)

// clawWrite plants a file, creating its directory.
func clawWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// clawTree lays out the measured laptop shape: the operator's home, a
// directory above the repository, the repository with its own memory and
// rules, and a work directory inside it.
func clawTree(t *testing.T) (home, lab, repo, work string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(tmp, "home")
	lab = filepath.Join(home, "lab")
	repo = filepath.Join(lab, "repo")
	work = filepath.Join(repo, "sub")
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	if out, err := exec.Command(gitBin, git.NoAutoMaintenance("init", "-q", "-b", "main", repo)...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	clawWrite(t, filepath.Join(home, ".claude", "CLAUDE.md"), "MARKER-OPERATOR-MEMORY-R1")
	clawWrite(t, filepath.Join(home, ".claude", "rules", "op.md"), "MARKER-OPERATOR-RULE-R2")
	clawWrite(t, filepath.Join(lab, "CLAUDE.md"), "MARKER-ANCESTOR-R3")
	clawWrite(t, filepath.Join(repo, "CLAUDE.md"), "MARKER-REPO-MEMORY-R4")
	clawWrite(t, filepath.Join(repo, ".claude", "rules", "repo.md"), "MARKER-REPO-RULE-R5")
	t.Setenv("HOME", home)
	return home, lab, repo, work
}

// ambientBlocks joins every system block's text: a task without a
// SystemPrompt has the ambient block first, so nothing may be sliced off.
func ambientBlocks(blocks []string) string {
	return strings.Join(blocks, "\n")
}

func TestClawAmbientBlockFollowsThePolicy(t *testing.T) {
	_, _, _, work := clawTree(t)
	operator := []string{"MARKER-OPERATOR-MEMORY-R1", "MARKER-OPERATOR-RULE-R2", "MARKER-ANCESTOR-R3"}
	workspace := []string{"MARKER-REPO-MEMORY-R4", "MARKER-REPO-RULE-R5"}

	for _, c := range []struct {
		policy                ambient.Policy
		wantOperator, wantAll bool
	}{
		{ambient.Workspace, false, false},
		{ambient.Operator, true, false},
		{ambient.All, true, true},
	} {
		t.Run(c.policy.String(), func(t *testing.T) {
			backend, cap := newCapturingBackend()
			if _, err := backend.Execute(context.Background(), delegate.Task{
				NodeID:         "n",
				Model:          "test/test-model",
				SystemPrompt:   "be terse",
				UserPrompt:     "x",
				WorkDir:        work,
				AmbientContext: c.policy,
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(cap.requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(cap.requests))
			}
			var texts []string
			for _, b := range cap.requests[0].SystemBlocks {
				texts = append(texts, b.Text)
			}
			block := ambientBlocks(texts)
			// The repository's files load under workspace and all; under
			// operator they are exactly what the policy keeps out.
			wantRepo := !c.wantOperator || c.wantAll
			for _, m := range workspace {
				if got := strings.Contains(block, m); got != wantRepo {
					t.Errorf("%s present=%v, want %v (the repository) under %s:\n%s", m, got, wantRepo, c.policy, block)
				}
			}
			for _, m := range operator {
				if got := strings.Contains(block, m); got != c.wantOperator {
					t.Errorf("%s present=%v, want %v", m, got, c.wantOperator)
				}
			}
			if c.wantAll {
				if !strings.Contains(block, "Project Instructions") && !strings.Contains(block, "CLAUDE.md") {
					t.Errorf("all: the block does not read like claw's project-instructions section:\n%s", block)
				}
			}
		})
	}
}

func TestClawAmbientBlockIsAbsentForNone(t *testing.T) {
	_, _, _, work := clawTree(t)
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:         "n",
		Model:          "test/test-model",
		SystemPrompt:   "be terse",
		UserPrompt:     "x",
		WorkDir:        work,
		AmbientContext: ambient.None,
	}); err != nil {
		t.Fatal(err)
	}
	for i, b := range cap.requests[0].SystemBlocks {
		if strings.Contains(b.Text, "MARKER-") {
			t.Errorf("none: system block #%d carries instruction content:\n%s", i, b.Text)
		}
	}
	if n := len(cap.requests[0].SystemBlocks); n != 1 {
		t.Errorf("none: system blocks = %d, want only the node's own prompt", n)
	}
}

func TestClawAmbientBlockKeepsANestedWorktreesMainCheckoutOut(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(tmp, "no-home"))
	main := filepath.Join(tmp, "main")
	wt := filepath.Join(main, ".iterion", "worktrees", "run1")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", main},
		{"-C", main, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", "--detach", wt},
	} {
		if out, err := exec.Command(gitBin, git.NoAutoMaintenance(args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	clawWrite(t, filepath.Join(main, "CLAUDE.md"), "MARKER-MAIN-CHECKOUT-R6")
	clawWrite(t, filepath.Join(wt, "CLAUDE.md"), "MARKER-WORKTREE-R7")

	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "n", Model: "test/test-model", UserPrompt: "x",
		WorkDir: wt, AmbientContext: ambient.Workspace,
	}); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, b := range cap.requests[0].SystemBlocks {
		texts = append(texts, b.Text)
	}
	block := ambientBlocks(texts)
	if !strings.Contains(block, "MARKER-WORKTREE-R7") {
		t.Errorf("workspace: the worktree's own CLAUDE.md is missing:\n%s", block)
	}
	if strings.Contains(block, "MARKER-MAIN-CHECKOUT-R6") {
		t.Errorf("workspace: the main checkout's CLAUDE.md leaked through the walk-up:\n%s", block)
	}

	backend, cap = newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "n", Model: "test/test-model", UserPrompt: "x",
		WorkDir: wt, AmbientContext: ambient.Operator,
	}); err != nil {
		t.Fatal(err)
	}
	texts = nil
	for _, b := range cap.requests[0].SystemBlocks {
		texts = append(texts, b.Text)
	}
	if slices.ContainsFunc(texts, func(s string) bool { return strings.Contains(s, "MARKER-MAIN-CHECKOUT-R6") }) {
		t.Error("operator: the main checkout is the same repository, not the operator's")
	}
}
