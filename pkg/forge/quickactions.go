package forge

import (
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// GitLab runs quick actions — `/merge`, `/approve`, `/close`, `/label`,
// `/assign` … — written at the start of a line of an issue or merge-request
// description or note, with the rights of the account that posted it, API
// posts included (Notes::QuickActionsService; the extractor finds candidate
// paragraphs with its markdown parser, then matches `^/<command>` on their
// raw lines, the name case-folded). Text the server and the board dispatcher
// post to GitLab is model output over untrusted content: a prompt-injected
// finding carrying `/merge` would run with the bot's rights, past the merge
// gate's deterministic surface.
//
// The guard does not depend on how GitLab's parser reads the text. A line
// whose first character is `/` followed by a letter — the only line GitLab
// runs: it matches a command at column 0 of the raw text, the name
// case-folded, Unicode letters included — gets a backslash before its slash.
// In prose CommonMark renders `\/` as `/`, so the reader sees the text as
// written (inside a code or HTML block the backslash shows), and GitLab's
// `^\/` no longer matches. Carriage returns are dropped first,
// as GitLab drops them before matching. An indented line never runs and is
// left as written, so code inside a list item or a footnote keeps its bytes.
// The one text left byte-exact whatever it holds is a one-click suggestion's
// replacement, whose code the click applies: it is fenced only when no line
// of it can run, and shown as text otherwise (GitLabRunsQuickActionLine).

// GitLabQuickActionsEnv names the escape hatch: `live` posts text as given,
// quick actions included — for an operator who drives GitLab through a
// bot's own comments on purpose. Read as INHERITED (internal/envtrust): a
// project `.env` the CLI auto-loads cannot open it. Any other value keeps
// the guard.
const GitLabQuickActionsEnv = "ITERION_FORGE_GITLAB_QUICK_ACTIONS"

// runnableLine is a line GitLab can run: a slash then a letter at column 0.
var runnableLine = regexp.MustCompile(`(?m)^/(\pL)`)

// InertGitLabQuickActions returns text with every line GitLab could run made
// inert (see above). Idempotent; text without such a line only loses its
// carriage returns.
func InertGitLabQuickActions(text string) string {
	if gitlabQuickActionsLive() {
		return text
	}
	return runnableLine.ReplaceAllString(strings.ReplaceAll(text, "\r", ""), `\/$1`)
}

// GitLabRunsQuickActionLine reports whether text holds a line GitLab could
// run — exactly the lines InertGitLabQuickActions rewrites. The test for
// text that must reach GitLab byte-exact: a one-click suggestion's
// replacement (such a line cannot be made inert without changing the code
// the click applies, so the caller shows it as text instead), and a
// finding's own text around the suggestion fences it carries. Always false
// when the escape hatch is open.
func GitLabRunsQuickActionLine(text string) bool {
	if gitlabQuickActionsLive() {
		return false
	}
	return runnableLine.MatchString(strings.ReplaceAll(text, "\r", ""))
}

func gitlabQuickActionsLive() bool {
	return strings.EqualFold(strings.TrimSpace(envtrust.Inherited(GitLabQuickActionsEnv)), "live")
}
