package forge

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// gitlabCommandLine is GitLab's own command matcher (lib/gitlab/quick_actions/
// extractor.rb, commands_regex), ported: a line that starts with `/`, a
// command name — case-folded, as Ruby's /i folds (`ſ` reads as `s`) — then
// an optional argument after a space, then the end of the line. The names
// are a sample of the live definitions, the ones that act on a merge request
// or an issue; the guard under test does not depend on the list.
var gitlabCommandLine = regexp.MustCompile(`(?mi)^/(?:merge|approve|unapprove|close|reopen|label|labels|unlabel|relabel|assign|unassign|assign_reviewer|request_review|unassign_reviewer|title|milestone|due|estimate|spend|draft|ready|rebase|submit_review|lock|unlock|confidential|target_branch|copy_metadata|duplicate|move|shrug|tableflip)(?: [^\n]*)?(?:\s*\n|$)`)

// gitlabExecutes reports the commands GitLab's matcher finds in text after
// dropping carriage returns, as GitLab does before matching.
func gitlabExecutes(text string) []string {
	return gitlabCommandLine.FindAllString(strings.ReplaceAll(text, "\r", ""), -1)
}

// hostileBodies are finding texts a prompt-injected model can write: the
// command alone, after prose, indented, after a CRLF, in a fenced block the
// parser may or may not honour, case-folded through a Unicode letter, with
// an argument, behind a carriage return GitLab deletes before it matches.
var hostileBodies = []string{
	"/merge",
	"Looks fine.\n/approve\n",
	"text\r\n/merge\r\nmore",
	"```\n/merge\n```",
	"> quoted\n/close",
	"/ſpend 1h",
	"/LABEL ~security",
	"/assign_reviewer @mallory",
	"line\n/rebase   \nline",
	"x\n\r/merge",
	"\r/approve",
}

// The guard's promise, measured with GitLab's own matcher: whatever the
// model wrote, the text iterion posts carries no command GitLab would run.
// The raw texts are run through the matcher first — an oracle that finds
// nothing in them proves nothing about the guard.
func TestInertGitLabQuickActions_NoCommandSurvivesGitLabsMatcher(t *testing.T) {
	t.Setenv(GitLabQuickActionsEnv, "")
	for _, raw := range hostileBodies {
		if len(gitlabExecutes(raw)) == 0 {
			t.Fatalf("oracle blind: GitLab's matcher finds no command in %q", raw)
		}
		if got := gitlabExecutes(InertGitLabQuickActions(raw)); len(got) != 0 {
			t.Fatalf("GitLab would still run %q from %q (posted %q)", got, raw, InertGitLabQuickActions(raw))
		}
	}
}

// What the guard writes is what a reader sees: a backslash before the slash
// (CommonMark renders `\/` as `/`), on command-shaped lines only. A code
// comment, a path segment after a slash, a lone slash stay as written; the
// guard is idempotent; carriage returns go, as GitLab drops them anyway.
func TestInertGitLabQuickActions_RewritesOnlyCommandShapedLines(t *testing.T) {
	t.Setenv(GitLabQuickActionsEnv, "")
	cases := map[string]string{
		"/merge": `\/merge`,
		// Indented lines never run: GitLab matches at column 0 only, and
		// code in a list item or a footnote keeps its bytes.
		"  /merge now":                  "  /merge now",
		"\t/close":                      "\t/close",
		"a\r\n/approve\r\nb":            "a\n" + `\/approve` + "\nb",
		"/ſpend 1h":                     `\/ſpend 1h`,
		"/etc/passwd is world-readable": `\/etc/passwd is world-readable`,
		"// a code comment":             "// a code comment",
		"/* block */":                   "/* block */",
		"/":                             "/",
		"see /merge inline":             "see /merge inline",
		`\/merge`:                       `\/merge`,
		// Onigmo folds the ligature onto "st": GitLab runs it as /status.
		// RE2 cannot fold it, so the ported oracle is blind here and the
		// table pins it instead.
		"/ﬆatus done": `\/ﬆatus done`,
		"/_private":   "/_private",
	}
	for in, want := range cases {
		if got := InertGitLabQuickActions(in); got != want {
			t.Errorf("InertGitLabQuickActions(%q) = %q, want %q", in, got, want)
		}
	}
	for _, raw := range hostileBodies {
		once := InertGitLabQuickActions(raw)
		if twice := InertGitLabQuickActions(once); twice != once {
			t.Errorf("not idempotent on %q: %q then %q", raw, once, twice)
		}
	}
}

// GitLabRunsQuickActionLine is the review client's test for text that must
// reach GitLab byte-exact: true exactly where the guard rewrites — a slash
// then a letter at column 0 — and false for indented lines and for code that
// only looks slashy.
func TestGitLabRunsQuickActionLine(t *testing.T) {
	t.Setenv(GitLabQuickActionsEnv, "")
	for _, raw := range hostileBodies {
		if !GitLabRunsQuickActionLine(raw) {
			t.Errorf("GitLabRunsQuickActionLine(%q) = false", raw)
		}
	}
	for _, never := range []string{"  /users/{id}:", "\t/usr/bin/install -m 0755 x", "// comment\nx := 1", "/* a */ b", "a / b", "path := \"/etc\"", ""} {
		if GitLabRunsQuickActionLine(never) {
			t.Errorf("GitLabRunsQuickActionLine(%q) = true", never)
		}
	}
}

// A project `.env` cannot open the hatch: the CLI auto-loads the nearest one
// and fills in every variable the operator left unset, so a repository under
// review could otherwise switch the guard off for the bot reviewing it. The
// hatch reads the environment as inherited (internal/envtrust): a planted
// `live` is no answer.
func TestInertGitLabQuickActions_APlantedHatchStaysShut(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(GitLabQuickActionsEnv, "live")
	envtrust.MarkPlanted(GitLabQuickActionsEnv)
	if got := InertGitLabQuickActions("LGTM\n/merge"); got != "LGTM\n\\/merge" {
		t.Fatalf("a planted hatch opened the guard: %q", got)
	}
	if !GitLabRunsQuickActionLine("/merge") {
		t.Fatalf("a planted hatch opened the suggestion test")
	}
}

// ITERION_FORGE_GITLAB_QUICK_ACTIONS=live is the operator's escape hatch:
// text goes out as written, quick actions included, and no suggestion is
// withheld. Any other value keeps the guard.
func TestInertGitLabQuickActions_LiveEscapeHatch(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(GitLabQuickActionsEnv, "live")
	if got := InertGitLabQuickActions("a\r\n/merge"); got != "a\r\n/merge" {
		t.Fatalf("live must post text as written, got %q", got)
	}
	if GitLabRunsQuickActionLine("/merge") {
		t.Fatalf("live must not withhold a suggestion")
	}
	t.Setenv(GitLabQuickActionsEnv, "on")
	if got := InertGitLabQuickActions("/merge"); got != `\/merge` {
		t.Fatalf("only `live` opens the hatch, got %q", got)
	}
}
