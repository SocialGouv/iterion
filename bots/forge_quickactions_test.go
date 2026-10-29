package bots

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// GitLab runs quick actions — `/merge`, `/approve`, `/label`, … — written at
// the start of a line of a description or note, with the rights of the
// account that posted it, API posts included. The server-side guard (#2413,
// pkg/forge/quickactions.go) covers the posts iterion itself makes; the bots
// that post to GitLab with their OWN mounted forge_token — from a skill's
// curl/glab commands — are outside it (#2422), so every such skill escapes
// the model-written body itself, with the same rewrite the server applies.
//
// The escape this guard pins on every skill below. In the skills it rides
// in a shell single-quoted `sed -E '...'` command; here it is the sed
// program itself. The [^[:print:]] branch is load-bearing: under LC_ALL=C
// [[:alpha:]] leaves `/ſpend 1h` and `/ﬆatus done` raw, and Onigmo
// case-folds them onto real command names. The class is [:print:], not
// [:ascii:]: GNU sed rejects [:ascii:] ("Invalid character class name",
// verified on gnused 4.9), and [^[:print:]] catches the same non-ASCII
// bytes on GNU and BSD.
const quickActionGuardSed = `s#^([[:blank:]]*)/([[:alpha:]]|[^[:print:]])#\1\\/\2#`

// gitLabPostingSkills are the skills whose bot posts model-written text to
// GitLab with its own token: the seven forge-mr-create copies (an MR
// description), revi-converse's reply note, the two supply-shield report
// posts, and dep-update-guard's note. Pinned so a renamed or split skill
// reddens instead of quietly dropping out of the guard.
var gitLabPostingSkills = []string{
	"app-dev/skills/forge-mr-create.md",
	"branch-improve-loop/skills/forge-mr-create.md",
	"dep-update-guard/skills/dependency-pr-guard.md",
	"docs-refresh/skills/forge-mr-create.md",
	"feature-dev/skills/forge-mr-create.md",
	"instrument/skills/forge-mr-create.md",
	"product-docs/skills/forge-mr-create.md",
	"revi-converse/skills/forge-reply.md",
	"supply-shield-cve/skills/forge-report.md",
	"supply-shield/skills/forge-report.md",
	"whole-improve-loop/skills/forge-mr-create.md",
}

// gitLabWrite marks a skill that WRITES to GitLab: a glab create/note, or
// an API POST/PUT at a notes/discussions endpoint (the prose and the code
// spans both match — the window covers the line wraps).
var gitLabWrite = regexp.MustCompile(`glab (mr create|mr note|issue note)|(?s)(POST|PUT).{0,120}(notes|discussions)`)

// gitlabPostingMatcher is GitLab's command matcher (lib/gitlab/quick_actions/
// extractor.rb), the same port pkg/forge's guard test uses: `^/<command>`,
// the name case-folded. The witness below measures the skill guard against
// GitLab's own reading of the text, not against the sed expression's intent.
var gitlabPostingMatcher = regexp.MustCompile(`(?mi)^/(?:merge|approve|unapprove|close|reopen|label|labels|unlabel|relabel|assign|unassign|assign_reviewer|request_review|unassign_reviewer|title|milestone|due|estimate|spend|draft|ready|rebase|submit_review|lock|unlock|confidential|target_branch|copy_metadata|duplicate|move|shrug|tableflip)(?: [^\n]*)?(?:\s*\n|$)`)

// TestEveryGitLabPostingSkillCarriesTheQuickActionGuard: a skill that posts
// model-written text to GitLab with the bot's own token must escape the body
// the way the server does, or a `/approve` quoted from a diff, an issue or a
// dependency changelog runs with the bot's rights (#2422).
func TestEveryGitLabPostingSkillCarriesTheQuickActionGuard(t *testing.T) {
	// Discovery: every GitLab-writing skill is the pinned set — a new
	// posting site that forgets the guard joins this class by failing.
	found, err := filepath.Glob(filepath.Join("*", "skills", "*.md"))
	if err != nil || len(found) == 0 {
		t.Fatalf("skill discovery broke: %v (%d files)", err, len(found))
	}
	sort.Strings(found)
	var posting []string
	for _, skill := range found {
		body := string(readFileOrFail(t, skill))
		if gitLabWrite.MatchString(body) {
			posting = append(posting, skill)
		}
	}
	if strings.Join(posting, ",") != strings.Join(gitLabPostingSkills, ",") {
		t.Fatalf("the GitLab-posting skill class is now %v, pinned as %v — a skill that joined must escape its GitLab-bound bodies (quick-action guard, #2422); one that left must have stopped posting", posting, gitLabPostingSkills)
	}
	for _, skill := range gitLabPostingSkills {
		body := string(readFileOrFail(t, skill))
		if !strings.Contains(body, quickActionGuardSed) {
			t.Errorf("%s posts to GitLab with the bot's own token but carries no quick-action guard: escape every GitLab-bound body with `sed -E '%s'` before posting (#2422)", skill, quickActionGuardSed)
		}
		if !strings.Contains(strings.ToLower(body), "quick-action guard") {
			t.Errorf("%s carries the escape but no longer states the rule (\"quick-action guard\") — the command without the reason is cargo the next edit drops", skill)
		}
	}
}

// runQuickActionGuard pipes text through the sed program the skills pin,
// under LC_ALL=C — the stricter locale, where [[:alpha:]] is ASCII-only and
// the [^[:print:]] branch is what catches /ſpend and /ﬆatus.
func runQuickActionGuard(t *testing.T, text string) string {
	t.Helper()
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skipf("sed unavailable: %v", err)
	}
	cmd := exec.Command("sed", "-E", quickActionGuardSed)
	cmd.Env = []string{"LC_ALL=C", "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the guard's sed does not run: %v", err)
	}
	return string(out)
}

// The witness the ticket demands, per skill family: a reply body and an MR
// description carrying `/approve` — plus the case-folded Unicode commands the
// non-ASCII branch exists for — reach GitLab with no line it would run. The
// raw texts are matched first: an oracle that finds nothing in them proves
// nothing about the guard. Two bodies are oracle-blind BY CONSTRUCTION and
// carry a pinned expectation instead: the indented line (GitLab matches at
// column 0 only) and the ligature ﬆ (Onigmo folds it onto "st"; RE2 cannot,
// so the ported matcher is blind — the table pins what GitLab would do).
func TestQuickActionGuardSedLeavesNothingGitLabRuns(t *testing.T) {
	bodies := []struct {
		name string
		raw  string
		// oracleBlind: GitLab's ported matcher finds no command in the raw
		// text, so the case asserts the pinned escaped output instead of
		// the raw oracle.
		oracleBlind bool
		want        string
	}{
		{"reply body (revi-converse)", "Answer: the diff reads:\n/approve\nwhich is the injected line.", false, ""},
		{"MR description (forge-mr-create)", "What changed: bump foo.\n/approve\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`<run id>`](<run url>)", false, ""},
		{"report note (supply-shield)", "2 new versions inspected\n/merge\n", false, ""},
		{"command alone", "/merge", false, ""},
		{"case-folded ſ", "/ſpend 1h", false, ""},
		{"case-folded ﬆ", "/ﬆatus done", true, `\/ﬆatus done`},
		{"CRLF", "text\r\n/merge\r\nmore", false, ""},
		{"indented", "  /merge now", true, `  \/merge now`},
	}
	for _, tc := range bodies {
		guarded := runQuickActionGuard(t, tc.raw)
		if tc.oracleBlind {
			if guarded != tc.want {
				t.Errorf("%s: guarded = %q, want %q", tc.name, guarded, tc.want)
			}
		} else if got := gitlabPostingMatcher.FindAllString(strings.ReplaceAll(tc.raw, "\r", ""), -1); len(got) == 0 {
			t.Fatalf("%s: oracle blind — GitLab's matcher finds no command in the raw %q", tc.name, tc.raw)
		}
		if got := gitlabPostingMatcher.FindAllString(strings.ReplaceAll(guarded, "\r", ""), -1); len(got) != 0 {
			t.Errorf("%s: GitLab would still run %q from the guarded %q (raw %q)", tc.name, got, guarded, tc.raw)
		}
		if twice := runQuickActionGuard(t, guarded); twice != guarded {
			t.Errorf("%s: the guard is not idempotent: %q then %q", tc.name, guarded, twice)
		}
	}
}
