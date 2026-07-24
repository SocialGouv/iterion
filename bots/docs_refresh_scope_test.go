package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsRefreshScopeCheckBase executes docs-refresh's scope_check command
// (extracted from the compiled IR) against real fixture repos, pinning the
// one property the writeable-set gate exists for: it must scope to THIS
// run's own commits, never to changes that were already present when the run
// started.
//
// The regression it guards is the amend-on-PR base bug (live run 019f9429):
// the cloud runner clones the base branch (HEAD=main) then checks out the PR
// head, so the OLDEST reflog entry is main — and `git diff <oldest>` then
// contains the PR author's own code, which scope_check flagged as a Doki
// scope violation. With scope_ok stuck false, `converged` never fires and the
// amend run burns every pass. The base must instead be the run-start HEAD:
// the newest reflog entry that is not one of this run's own commits.
func TestDocsRefreshScopeCheckBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	command := toolCommand(t, "docs-refresh/main.bot", "scope_check")

	type scopeOut struct {
		ScopeOK    bool     `json:"scope_ok"`
		OutOfScope []string `json:"out_of_scope"`
		Log        string   `json:"log"`
	}

	git := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return string(out)
	}
	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, dir, msg string) {
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", msg)
	}
	// dokiCommit mirrors how the campaign lands a commit: the mandatory
	// `Bot: docs-refresh` trailer is what marks it as this run's own work.
	dokiCommit := func(t *testing.T, dir, msg string) {
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", msg+"\n\nBot: docs-refresh")
	}
	runScope := func(t *testing.T, ws string) scopeOut {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		if i := strings.Index(cmd, "{{"); i >= 0 {
			t.Fatalf("unresolved template ref in scope_check near %q", cmd[i:min(i+40, len(cmd))])
		}
		out, err := exec.Command("sh", "-c", cmd).Output()
		if err != nil {
			t.Fatalf("scope_check failed to execute: %v (out %q)", err, out)
		}
		var res scopeOut
		if uerr := json.Unmarshal(out, &res); uerr != nil {
			t.Fatalf("scope_check output is not JSON: %v (out %q)", uerr, out)
		}
		return res
	}

	// buildAmendClone reproduces the runner's clone+checkout exactly: an
	// origin repo carrying main (a .go) plus a `pr` branch that adds the PR
	// author's own code + doc, then `git clone` (HEAD=main) and a runner-style
	// fetch+checkout of the PR head. The result: the OLDEST reflog entry is
	// main, the shape that broke reflog[-1].
	buildAmendClone := func(t *testing.T) string {
		origin := t.TempDir()
		git(t, origin, "init", "-q", "-b", "main")
		write(t, origin, "pkg/app.go", "package app\n")
		write(t, origin, "docs/guide.md", "# Guide\nold\n")
		commit(t, origin, "chore: seed main")
		git(t, origin, "checkout", "-q", "-b", "pr")
		write(t, origin, "pkg/app.go", "package app\n// feature\n") // PR author edits code
		write(t, origin, "pkg/new.go", "package app\n")             // PR author adds code
		write(t, origin, "docs/guide.md", "# Guide\nnew\n")         // PR author edits a doc
		commit(t, origin, "feat: the PR author's own change")

		parent := t.TempDir()
		work := filepath.Join(parent, "work")
		git(t, parent, "clone", "-q", origin, work) // HEAD=main → reflog[-1]=main
		git(t, work, "fetch", "-q", "origin", "pr")
		git(t, work, "checkout", "-q", "-B", "pr", "origin/pr") // HEAD → PR head
		return work
	}

	t.Run("amend_scopes_to_own_commits_not_the_pr", func(t *testing.T) {
		work := buildAmendClone(t)
		// Doki aligns docs only — all .md, on top of the PR head.
		write(t, work, "docs/guide.md", "# Guide\nnew (aligned)\n")
		dokiCommit(t, work, "docs(guide): align to the PR change")
		write(t, work, "README.md", "# proj\n")
		dokiCommit(t, work, "docs(readme): note the feature")

		res := runScope(t, work)
		if !res.ScopeOK {
			t.Fatalf("amend run with docs-only commits must be in scope; got violation: %s\nout_of_scope=%v", res.Log, res.OutOfScope)
		}
		// The PR author's own code must never appear as a Doki violation.
		for _, p := range res.OutOfScope {
			if p == "pkg/app.go" || p == "pkg/new.go" {
				t.Fatalf("the PR author's own file %q was flagged as a Doki scope violation (base is the base branch, not the run-start HEAD)", p)
			}
		}
	})

	t.Run("amend_flags_own_non_md_only", func(t *testing.T) {
		work := buildAmendClone(t)
		write(t, work, "docs/guide.md", "# Guide\naligned\n")
		dokiCommit(t, work, "docs(guide): align")
		// Doki (wrongly) touches code — THIS must be the only violation.
		write(t, work, "pkg/app.go", "package app\n// doki should not edit code\n")
		dokiCommit(t, work, "chore: doki edits code (should trip the gate)")

		res := runScope(t, work)
		if res.ScopeOK {
			t.Fatalf("Doki committing a non-.md file must trip scope_check")
		}
		if len(res.OutOfScope) != 1 || res.OutOfScope[0] != "pkg/app.go" {
			t.Fatalf("only Doki's own non-.md edit should be flagged, not the PR's files; got %v", res.OutOfScope)
		}
	})

	t.Run("worktree_auto_no_regression", func(t *testing.T) {
		// The historical (non-repo-targeted) path: a plain tree where Doki
		// commits on top of the starting HEAD. Base must be that HEAD, so a
		// pre-existing .go present at the base is not a violation.
		ws := t.TempDir()
		git(t, ws, "init", "-q", "-b", "main")
		write(t, ws, "pkg/pre.go", "package pkg\n")
		write(t, ws, "docs/x.md", "# x\n")
		commit(t, ws, "chore: pre-existing tree")
		write(t, ws, "docs/x.md", "# x aligned\n")
		dokiCommit(t, ws, "docs(x): align")

		res := runScope(t, ws)
		if !res.ScopeOK {
			t.Fatalf("worktree:auto docs-only run must be in scope; got: %s (%v)", res.Log, res.OutOfScope)
		}
	})

	t.Run("no_own_commits_is_clean", func(t *testing.T) {
		work := buildAmendClone(t)
		// Doki committed nothing (already aligned) → base is HEAD, empty diff.
		res := runScope(t, work)
		if !res.ScopeOK || len(res.OutOfScope) != 0 {
			t.Fatalf("a run with zero own commits must be trivially in scope; got ok=%v out=%v", res.ScopeOK, res.OutOfScope)
		}
	})
}
