package model

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// clawAgentsTree lays out a repository whose only instruction file is
// AGENTS.md — no CLAUDE.md anywhere in the workspace — under an empty home.
func clawAgentsTree(t *testing.T) (repo, work string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	repo = filepath.Join(tmp, "repo")
	work = filepath.Join(repo, "sub")
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	if out, err := exec.Command(gitBin, git.NoAutoMaintenance("init", "-q", "-b", "main", repo)...).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return repo, work
}

// clawAmbientBlockOf runs one task on the capturing backend and returns the
// joined system-block text.
func clawAmbientBlockOf(t *testing.T, workDir string, policy ambient.Policy) string {
	t.Helper()
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:         "n",
		Model:          "test/test-model",
		SystemPrompt:   "be terse",
		UserPrompt:     "x",
		WorkDir:        workDir,
		AmbientContext: policy,
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
	return ambientBlocks(texts)
}

func TestClawAmbientBlockCarriesAgentsMdWhenNoClaudeMd(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")

	for _, policy := range []ambient.Policy{ambient.Workspace, ambient.All} {
		block := clawAmbientBlockOf(t, work, policy)
		if !strings.Contains(block, "MARKER-REPO-AGENTS-R8") {
			t.Errorf("%s: the repository's AGENTS.md never reached the claw node:\n%s", policy, block)
		}
	}
}

func TestClawAmbientBlockCarriesASubdirectoryAgentsMd(t *testing.T) {
	_, _, _, work := clawTree(t)
	clawWrite(t, filepath.Join(work, "AGENTS.md"), "MARKER-SUB-AGENTS-R9")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if !strings.Contains(block, "MARKER-SUB-AGENTS-R9") {
		t.Errorf("workspace: the working directory's AGENTS.md never reached the claw node:\n%s", block)
	}
	if !strings.Contains(block, "MARKER-REPO-MEMORY-R4") {
		t.Errorf("workspace: the repository's CLAUDE.md went missing alongside it:\n%s", block)
	}
	// Most general first, like the loader renders its own files.
	if sub, root := strings.Index(block, "MARKER-SUB-AGENTS-R9"), strings.Index(block, "MARKER-REPO-MEMORY-R4"); sub < root {
		t.Errorf("workspace: the working directory's AGENTS.md renders before the root's CLAUDE.md:\n%s", block)
	}
}

func TestClawAmbientBlockOrdersAgentsMdRootMostFirst(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")
	clawWrite(t, filepath.Join(work, "AGENTS.md"), "MARKER-SUB-AGENTS-R9")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	for _, m := range []string{"MARKER-REPO-AGENTS-R8", "MARKER-SUB-AGENTS-R9"} {
		if !strings.Contains(block, m) {
			t.Errorf("workspace: %s never reached the claw node:\n%s", m, block)
		}
	}
	if repoIdx, subIdx := strings.Index(block, "MARKER-REPO-AGENTS-R8"), strings.Index(block, "MARKER-SUB-AGENTS-R9"); repoIdx > subIdx {
		t.Errorf("workspace: AGENTS.md files render leaf-first, the loader renders root-most first:\n%s", block)
	}
}

func TestClawAmbientBlockPrefersAClaudeMdOverAGentsMd(t *testing.T) {
	_, _, repo, work := clawTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")

	// The root carries a CLAUDE.md without any import of the AGENTS.md: the
	// fallback stays out, exactly where claude code reads AGENTS.md only when
	// no CLAUDE.md exists.
	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if strings.Contains(block, "MARKER-REPO-AGENTS-R8") {
		t.Errorf("workspace: AGENTS.md loaded beside the CLAUDE.md that shadows it:\n%s", block)
	}
}

// A CLAUDE.md the walk would never load shadows nothing: the shadow exists
// to say where the walk has spoken, and a non-regular file — a FIFO here, a
// socket or a device the same way — is refused by the walk's own readWithin.
func TestClawAmbientBlockShadowsOnlyWhatTheWalkWouldLoad(t *testing.T) {
	_, _, repo, work := clawTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-FIFO-AGENTS")
	// clawTree plants a CLAUDE.md for its own default tree; the FIFO takes
	// its exact place.
	if err := os.Remove(filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, "CLAUDE.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if !strings.Contains(block, "MARKER-FIFO-AGENTS") {
		t.Errorf("workspace: a non-regular CLAUDE.md the walk refuses still shadowed the repo's AGENTS.md:\n%s", block)
	}
}

func TestClawAmbientBlockDoesNotDoubleLoadAnImportedAgentsMd(t *testing.T) {
	_, _, repo, work := clawTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")
	clawWrite(t, filepath.Join(repo, "CLAUDE.md"), "MARKER-REPO-MEMORY-R4\n\n@AGENTS.md")

	// The import already pulls the AGENTS.md in; the suppressed fallback must
	// not add a second copy.
	if n := strings.Count(clawAmbientBlockOf(t, work, ambient.Workspace), "MARKER-REPO-AGENTS-R8"); n != 1 {
		t.Errorf("workspace: AGENTS.md content appears %d times, want exactly once", n)
	}
}

func TestClawAmbientBlockKeepsAgentsMdOutOfExcludedWorkspaces(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")

	for _, policy := range []ambient.Policy{ambient.None, ambient.Operator} {
		if block := clawAmbientBlockOf(t, work, policy); strings.Contains(block, "MARKER-REPO-AGENTS-R8") {
			t.Errorf("%s: the repository's AGENTS.md leaked into the node:\n%s", policy, block)
		}
	}
}

func TestClawAmbientBlockTruncatesAHugeAgentsMdAtTheStatedBound(t *testing.T) {
	repo, work := clawAgentsTree(t)
	huge := "MARKER-AGENTS-HEAD-R10\n" + strings.Repeat("x", clawAmbientMaxBytes) + "\nMARKER-AGENTS-TAIL-R11"
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), huge)

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if !strings.Contains(block, "MARKER-AGENTS-HEAD-R10") {
		t.Errorf("workspace: the AGENTS.md head went missing:\n%s", block[:min(len(block), 2000)])
	}
	if !strings.Contains(block, "... (truncated)") {
		t.Error("workspace: a huge AGENTS.md is cut without the truncation notice")
	}
	if strings.Contains(block, "MARKER-AGENTS-TAIL-R11") {
		t.Error("workspace: content past the stated bound reached the node anyway")
	}
}

// TestClawAmbientBlockSharesItsBoundAcrossTheWalkAndTheFallback pins the one
// stated bound: the fallback receives what the walk leaves of it, so a block
// whose walk already spent 3 MiB does not double the budget for AGENTS.md.
func TestClawAmbientBlockSharesItsBoundAcrossTheWalkAndTheFallback(t *testing.T) {
	_, _, repo, work := clawTree(t)
	clawWrite(t, filepath.Join(repo, "CLAUDE.md"), strings.Repeat("y", 3<<20))
	clawWrite(t, filepath.Join(work, "AGENTS.md"), strings.Repeat("z", 3<<20))

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if len(block) > clawAmbientMaxBytes+128 {
		t.Errorf("workspace: the block is %d bytes, past the stated %d bound", len(block), clawAmbientMaxBytes)
	}
	if !strings.Contains(block, "... (truncated)") {
		t.Error("workspace: the fallback did not hand the walk's remainder to AGENTS.md")
	}
}

// TestClawAmbientBlockKeepsAgentsMdOutOfTheRepositoryItIsLinkedFrom is the
// round-1 regression witness: a repository AGENTS.md that is itself a link
// out of the repository is not loaded — the walk's refusal of workspace
// files that leave the boundary, one discipline for the block's two halves.
// Symlink targets live only under the test's own home in t.TempDir().
func TestClawAmbientBlockKeepsAgentsMdOutOfTheRepositoryItIsLinkedFrom(t *testing.T) {
	repo, work := clawAgentsTree(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	clawWrite(t, filepath.Join(home, "outside.md"), "MARKER-OUTSIDE-R1A")
	if err := os.Symlink(filepath.Join(home, "outside.md"), filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if strings.Contains(block, "MARKER-OUTSIDE-R1A") {
		t.Errorf("workspace: the out-of-repo AGENTS.md link target reached the claw node:\n%s", block)
	}
}

// A CLAUDE.md that is itself a link out of the workspace is not a CLAUDE.md
// the walk would load, so it shadows nothing: the repository's AGENTS.md
// still loads, and the link target stays out.
func TestClawAmbientBlockLetsAnOutOfRepoClaudeMdLinkShadowNothing(t *testing.T) {
	repo, work := clawAgentsTree(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	clawWrite(t, filepath.Join(home, "outside-claude.md"), "MARKER-OUTSIDE-R1B")
	if err := os.Symlink(filepath.Join(home, "outside-claude.md"), filepath.Join(repo, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if strings.Contains(block, "MARKER-OUTSIDE-R1B") {
		t.Errorf("workspace: the out-of-repo CLAUDE.md link target reached the claw node:\n%s", block)
	}
	if !strings.Contains(block, "MARKER-REPO-AGENTS-R8") {
		t.Errorf("workspace: an out-of-workspace CLAUDE.md link suppressed the repository's AGENTS.md:\n%s", block)
	}
}

// The control of the two above: a link INSIDE the repository still loads.
func TestClawAmbientBlockLoadsAnAgentsMdLinkedInsideTheRepository(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "agents-body.md"), "MARKER-INSIDE-R1C")
	if err := os.Symlink(filepath.Join(repo, "agents-body.md"), filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if !strings.Contains(block, "MARKER-INSIDE-R1C") {
		t.Errorf("workspace: an in-repo AGENTS.md link stopped loading:\n%s", block)
	}
}

// The mirror of the confinement: where the walk is unconfined (`all`), the
// fallback is unconfined with it.
func TestClawAmbientBlockFollowsTheWalksUnconfinedScope(t *testing.T) {
	repo, work := clawAgentsTree(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	clawWrite(t, filepath.Join(home, "outside.md"), "MARKER-OUTSIDE-R1D")
	if err := os.Symlink(filepath.Join(home, "outside.md"), filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	block := clawAmbientBlockOf(t, work, ambient.All)
	if !strings.Contains(block, "MARKER-OUTSIDE-R1D") {
		t.Errorf("all: the walk is unconfined, the fallback must follow it:\n%s", block)
	}
}

// A dangling AGENTS.md link is skipped, not an error.
func TestClawAmbientBlockSkipsADanglingAgentsMdLink(t *testing.T) {
	repo, work := clawAgentsTree(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "never-there.md"), filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if strings.Contains(block, "## AGENTS.md (") {
		t.Errorf("workspace: a dangling AGENTS.md link rendered a section:\n%s", block)
	}
}

// The block-level order the ADR states: the fallback's parts follow the
// walk's whole output, so a parent's AGENTS.md renders after a child's
// CLAUDE.md — most-general-first holds within each pass, not across them.
func TestClawAmbientBlockRendersTheWalkBeforeTheFallback(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "MARKER-REPO-AGENTS-R8")
	clawWrite(t, filepath.Join(work, "CLAUDE.md"), "MARKER-SUB-CLAUDE-R12")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	sub, root := strings.Index(block, "MARKER-SUB-CLAUDE-R12"), strings.Index(block, "MARKER-REPO-AGENTS-R8")
	if sub < 0 || root < 0 {
		t.Fatalf("workspace: markers missing:\n%s", block)
	}
	if sub > root {
		t.Errorf("workspace: the walk's output must precede the fallback's parts:\n%s", block)
	}
}

// The import-free convention the ADR states: an AGENTS.md body is injected
// verbatim — @path references reach the node as text, where the same file
// as CLAUDE.md expands them.
func TestClawAmbientBlockInjectsAgentsMdImportFree(t *testing.T) {
	repo, work := clawAgentsTree(t)
	clawWrite(t, filepath.Join(repo, "docs", "x.md"), "MARKER-IMPORT-TARGET-R13")
	clawWrite(t, filepath.Join(repo, "AGENTS.md"), "see @docs/x.md for the contract MARKER-RAW-REF-R14")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	if !strings.Contains(block, "MARKER-RAW-REF-R14") {
		t.Fatalf("workspace: the repository's AGENTS.md never reached the node:\n%s", block)
	}
	if strings.Contains(block, "### Imported:") || strings.Contains(block, "MARKER-IMPORT-TARGET-R13") {
		t.Errorf("workspace: AGENTS.md references expanded, the stated divergence is gone:\n%s", block)
	}
}

// The section shape parity, down to the leading newline the walk's parser
// strips: the body starts exactly one blank line under the heading.
func TestClawAmbientBlockStripsAgentsMdFrontmatterLikeTheWalk(t *testing.T) {
	repo, work := clawAgentsTree(t)
	path := filepath.Join(repo, "AGENTS.md")
	clawWrite(t, path, "---\ntitle: x\n---\nMARKER-BODY-R15")

	block := clawAmbientBlockOf(t, work, ambient.Workspace)
	want := "## AGENTS.md (" + path + ")\n\nMARKER-BODY-R15\n"
	if !strings.Contains(block, want) {
		t.Errorf("workspace: frontmatter body not rendered in the walk's shape, want %q in:\n%s", want, block)
	}
}
