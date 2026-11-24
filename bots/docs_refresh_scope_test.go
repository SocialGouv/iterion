package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestDocsRefreshScopeCheckBase executes docs-refresh's entry node
// (record_run_base) and its scope_check command (both extracted from the
// compiled IR) against real fixture repos, pinning the one property the
// writeable-set gate exists for: it must scope to THIS run's own commits,
// never to changes that were already present when the run started.
//
// The regression it guards is the amend-on-PR base bug (live run 019f9429):
// the cloud runner clones the base branch (HEAD=main) then checks out the PR
// head, so a base taken from the OLDEST reflog entry is main — and `git diff
// <oldest>` then contains the PR author's own code, which scope_check flagged
// as a Doki scope violation. With scope_ok stuck false, `converged` never
// fires and the amend run burns every pass. The base is the run-start HEAD:
// what record_run_base reads at the entry, AFTER the runner's checkout and
// before any pass has written, handed to scope_check on its in-edge. It is
// the window delivery_gate measures in too, so the loop asks for a revert on
// exactly what the tail would refuse — a base the loop re-derived from the
// campaign's own commit messages (the newest commit without the trailer)
// was a different window whenever a commit lacked the trailer.
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
		return gittest.Run(t, dir, args...)
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
	// recordBase runs the real entry node on ws: the base a run records
	// before its first pass, which the campaign's commits cannot move.
	recordBase := func(t *testing.T, ws string) string {
		t.Helper()
		var out struct {
			BaseSHA string `json:"base_sha"`
		}
		runJSONIn(t, ws, resolveCommand(t, toolCommand(t, "docs-refresh/main.bot", "record_run_base"), map[string]string{
			"vars.workspace_dir": ws,
		}), &out)
		if out.BaseSHA == "" {
			t.Fatalf("record_run_base recorded no base on %s", ws)
		}
		return out.BaseSHA
	}
	runScope := func(t *testing.T, ws, base string) scopeOut {
		t.Helper()
		cmd := strings.ReplaceAll(command, "{{vars.workspace_dir}}", ws)
		cmd = strings.ReplaceAll(cmd, "{{input.base_sha}}", base)
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
		base := recordBase(t, work)
		if head := strings.TrimSpace(git(t, work, "rev-parse", "HEAD")); base != head {
			t.Fatalf("record_run_base recorded %s, want the PR head %s the runner checked out — the run starts there, not at the base branch the clone began on", base, head)
		}
		// Doki aligns docs only — all .md, on top of the PR head.
		write(t, work, "docs/guide.md", "# Guide\nnew (aligned)\n")
		dokiCommit(t, work, "docs(guide): align to the PR change")
		write(t, work, "README.md", "# proj\n")
		dokiCommit(t, work, "docs(readme): note the feature")

		res := runScope(t, work, base)
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
		base := recordBase(t, work)
		write(t, work, "docs/guide.md", "# Guide\naligned\n")
		dokiCommit(t, work, "docs(guide): align")
		// Doki (wrongly) touches code — THIS must be the only violation.
		write(t, work, "pkg/app.go", "package app\n// doki should not edit code\n")
		dokiCommit(t, work, "chore: doki edits code (should trip the gate)")

		res := runScope(t, work, base)
		if res.ScopeOK {
			t.Fatalf("Doki committing a non-.md file must trip scope_check")
		}
		if len(res.OutOfScope) != 1 || res.OutOfScope[0] != "pkg/app.go" {
			t.Fatalf("only Doki's own non-.md edit should be flagged, not the PR's files; got %v", res.OutOfScope)
		}
	})

	t.Run("a_commit_without_the_trailer_stays_in_the_window", func(t *testing.T) {
		// What `git revert` writes, and what the engine's wip bank writes
		// before a resume: a commit of the run's carrying no trailer. A base
		// walked back from the reflog to the newest trailerless commit made
		// THIS commit the base and emptied the diff; the recorded base keeps
		// it in the window the tail measures in.
		work := buildAmendClone(t)
		base := recordBase(t, work)
		write(t, work, "docs/guide.md", "# Guide\naligned\n")
		dokiCommit(t, work, "docs(guide): align")
		write(t, work, "pkg/app.go", "package app\n// edited without the trailer\n")
		commit(t, work, "chore: a commit of the run's without the trailer")

		res := runScope(t, work, base)
		if res.ScopeOK || len(res.OutOfScope) != 1 || res.OutOfScope[0] != "pkg/app.go" {
			t.Fatalf("a trailerless commit of the run's must stay in the audited window; got ok=%v out=%v log=%q", res.ScopeOK, res.OutOfScope, res.Log)
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
		base := recordBase(t, ws)
		write(t, ws, "docs/x.md", "# x aligned\n")
		dokiCommit(t, ws, "docs(x): align")

		res := runScope(t, ws, base)
		if !res.ScopeOK {
			t.Fatalf("worktree:auto docs-only run must be in scope; got: %s (%v)", res.Log, res.OutOfScope)
		}
	})

	t.Run("no_own_commits_is_clean", func(t *testing.T) {
		work := buildAmendClone(t)
		base := recordBase(t, work)
		// Doki committed nothing (already aligned) → base is HEAD, empty diff.
		res := runScope(t, work, base)
		if !res.ScopeOK || len(res.OutOfScope) != 0 {
			t.Fatalf("a run with zero own commits must be trivially in scope; got ok=%v out=%v", res.ScopeOK, res.OutOfScope)
		}
	})

	t.Run("no_recorded_base_fails_closed", func(t *testing.T) {
		// The loop condition certifies nothing it cannot measure: an empty
		// base is a closed answer, never a fallback to HEAD that would judge
		// the dirty tree alone and certify every commit the run made.
		work := buildAmendClone(t)
		write(t, work, "pkg/app.go", "package app\n// doki edits code\n")
		dokiCommit(t, work, "chore: doki edits code")
		res := runScope(t, work, "")
		if res.ScopeOK || !strings.Contains(res.Log, "SCOPE GATE UNAVAILABLE") {
			t.Fatalf("scope_check with no recorded base must refuse to certify and say why; got ok=%v log=%q", res.ScopeOK, res.Log)
		}
	})
}
