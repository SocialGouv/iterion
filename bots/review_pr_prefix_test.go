package bots

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// reviewPrPromptSource reads one `prompt <name>:` block out of the shipped
// bundle source, the way its author wrote it — the guard below names a
// template at its source, not at its render.
func reviewPrPromptSource(t *testing.T, name string) string {
	t.Helper()
	u := unittest.Whole(unit.LoadDir(filepath.Join(botsDir(t), "review-pr", "main.bot")))
	for _, f := range u.Files {
		m := regexp.MustCompile(`(?s)prompt ` + name + `:\n(.*?)(?:\n\nprompt |\z)`).FindSubmatch(f.Source)
		if m != nil {
			return string(m[1])
		}
	}
	t.Fatalf("prompt %s not found in the review-pr bundle source", name)
	return ""
}

// The system prompts of review-pr are the byte-stable half of the cross-run
// prefix (#3283 S6): the provider's prefix cache serves the next run from
// this one's bytes, and a {{vars.*}}/{{outputs.*}} in a system prompt IS a
// per-run byte, on every run — the first one cuts the cache. review_system
// lost its base sha, workspace, tier and floor to the user message's REVIEW
// SCOPE block; emit_system lost its threshold, cap, board flag and report
// path to emit_user, which already carried them. The system prompts carry
// the mission; the user messages carry the run. (The assembled-request byte
// comparison runs in pkg/backend/model; this guard reads the templates the
// way the author wrote them, so a value that sneaks back in is named at its
// source. The four reviewer nodes also read review_system, and both merge
// twins read emit_system — one block per prompt covers every member.)
func TestReviewPrSystemPromptsCarryNoRunSpecificText(t *testing.T) {
	for _, name := range []string{"review_system", "emit_system"} {
		body := reviewPrPromptSource(t, name)
		if hits := regexp.MustCompile(`\{\{[^}]*\}\}`).FindAllString(body, -1); len(hits) > 0 {
			for _, hit := range hits {
				t.Errorf("%s carries the template %s — run-specific bytes cut the cross-run prefix cache; move it to the user message", name, hit)
			}
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s resolved to an empty body — the guard lost its subject", name)
		}
	}
	// The values the system prompts now only point at really ride the user
	// messages: the scope block on both reviewer lanes, and emit_user's
	// pre-existing lines. A system prompt that sends the model to a value
	// nobody carries would teach it to read nothing.
	userClaude := reviewPrPromptSource(t, "review_user_claude")
	userGPT := reviewPrPromptSource(t, "review_user_gpt")
	for _, fact := range []string{
		"outputs.diff_precheck.base_sha", "outputs.diff_precheck.base_is_current",
		"outputs.diff_precheck.changed_files", "outputs.diff_precheck.workspace_anomaly",
		"vars.review_tier", "outputs.tier_expand.severity_threshold",
	} {
		for _, lane := range map[string]string{"review_user_claude": userClaude, "review_user_gpt": userGPT} {
			if !strings.Contains(lane, fact) {
				t.Errorf("%s no longer carries %s — review_system points the reviewer at a value the user message must state", laneName(lane), fact)
			}
		}
	}
	emitUser := reviewPrPromptSource(t, "emit_user")
	for _, fact := range []string{
		"outputs.tier_expand.severity_threshold", "outputs.tier_expand.max_findings",
		"outputs.tier_expand.post_to_board", "vars.report_path", "vars.workspace_dir",
	} {
		if !strings.Contains(emitUser, fact) {
			t.Errorf("emit_user no longer carries %s — emit_system points the merge at a value the user message must state", fact)
		}
	}
}

// laneName names the prompt a body came from, for the error lines above.
func laneName(body string) string {
	if strings.Contains(body, "CLAUDE-family") {
		return "review_user_claude"
	}
	return "review_user_gpt"
}

// The scope block's git commands must reach the reviewer AS COMMANDS, not
// as prose: the system prompt orders it to run THOSE commands, so a user
// message that lost the `git -C` lines leaves the reviewer recomputing a
// scope from a ref name — the stale-base failure the block exists to close.
func TestReviewPrUserScopeCarriesTheGitCommands(t *testing.T) {
	for _, name := range []string{"review_user_claude", "review_user_gpt"} {
		body := reviewPrPromptSource(t, name)
		for _, cmd := range []string{
			"git -C {{vars.workspace_dir}} diff --name-only {{outputs.diff_precheck.base_sha}}",
			"git -C {{vars.workspace_dir}} diff {{outputs.diff_precheck.base_sha}}",
			"git -C {{vars.workspace_dir}} diff {{outputs.diff_precheck.base_sha}} HEAD",
		} {
			if !strings.Contains(body, cmd) {
				t.Errorf("%s lost the command %q — the scope block owes the values spelled in", name, cmd)
			}
		}
	}
}
