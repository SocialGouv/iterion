package bots

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repo-root `**/.claude/` rule exists because claude_code
// sub-invocations mirror skills into THEIR cwd, and the campaign bots
// commit with `git add -A` — so any un-ignored `.claude/` subtree gets
// wip-banked into a bot's commit.
//
// third_party/codex-agent-sdk-go/ carries a committed `.claude/rules/`
// of its own (real project content), so `.gitignore` re-includes it.
// That exception is easy to write too broadly: a bare
// `!third_party/codex-agent-sdk-go/.claude/` re-includes the WHOLE
// subtree, reopening the wip-bank hole for exactly the runtime junk the
// rule stops — and the fork is an active work target, so bots do run
// there. The negation must descend into the dir, re-ignore its other
// children, then re-include `rules/`.
//
// Nothing else checks this: an over-broad negation is invisible until a
// run's mirrored skills land in a commit. This test asks git itself.
func TestGitignoreKeepsClaudeRuntimeJunkIgnored(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	root := filepath.Join("..") // bots/ -> repo root
	if out, err := exec.Command(git, "-C", root, "rev-parse", "--is-inside-work-tree").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("not a git work tree (vendored/exported checkout)")
	}

	fork := "third_party/codex-agent-sdk-go/.claude/"
	cases := []struct {
		path        string
		wantIgnored bool
		why         string
	}{
		{fork + "skills/MIRRORED.md", true, "the runtime skill mirror inside the vendored fork"},
		{fork + "settings.local.json", true, "per-machine claude_code settings inside the vendored fork"},
		{fork + "agents/SOME.md", true, "any other runtime artifact inside the vendored fork"},
		{fork + "rules/architecture.md", false, "the fork's committed rules (tracked project content)"},
		{fork + "rules/NEW.md", false, "a new rule added to the fork — must stay addable without -f"},
		{"pkg/runtime/.claude/skills/MIRRORED.md", true, "the baseline **/.claude/ rule elsewhere in the tree"},
	}

	for _, tc := range cases {
		// `git check-ignore` matches patterns only — the path need not exist.
		err := exec.Command(git, "-C", root, "check-ignore", "-q", "--", tc.path).Run()
		ignored := err == nil
		if ignored != tc.wantIgnored {
			t.Errorf("%s: ignored=%v want %v — %s", tc.path, ignored, tc.wantIgnored, tc.why)
		}
	}
}
