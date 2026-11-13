package bots

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// renderReviConverseHealth renders revi-converse's converse_health command
// over input with the engine's own renderer and the shapes the executor
// reads (the workflow's declarations plus the node's input schema): each
// value shell-escaped, an absent input kept as its `{{input.X}}` text. It
// returns the command and the references left unresolved.
func renderReviConverseHealth(t *testing.T, input map[string]any) (string, []string) {
	t.Helper()
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	raw, ok := wf.Nodes["converse_health"]
	if !ok {
		t.Fatal("revi-converse has no converse_health node")
	}
	tn, ok := raw.(*ir.ToolNode)
	if !ok {
		t.Fatalf("converse_health is %T, want *ir.ToolNode", raw)
	}
	shapes := model.WorkflowShapes(wf).WithInputSchema(wf.Schemas[tn.InputSchema])
	var unresolved []string
	command := model.RenderCommand(tn.Command, tn.CommandRefs, input, nil, nil, "run-health", shapes, func(ref string) {
		unresolved = append(unresolved, ref)
	})
	return command, unresolved
}

// converse_health is the run's only witness that a `/revi <question>` got its
// answer, so a question reads answered only when the agent posted AND handed
// back an http(s) URL on the pull request's own host — where every note the
// forge creates lives (GitHub's html_url, GitLab's pr_url#note_<id>). Every
// other shape is degraded: a claimed post with no URL, with text that is not
// one ("placeholder"), or with a URL elsewhere, and any run that posted
// nothing — the agent's skipped_reason, when it gave one, is the banner, but
// it explains the silence, it does not answer the question. An agent that ran
// out of tool steps, or held no tool, says so in exactly that field.
func TestReviConverseHealthFlagsAnUnansweredQuestion(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	// on builds a full input: the agent's three fields beside the PR URL.
	on := func(prURL string, posted bool, reply, skipped string) map[string]any {
		return map[string]any{"posted": posted, "reply_url": reply, "skipped_reason": skipped, "pr_url": prURL}
	}
	const pr = "https://forge/o/r/pull/1"
	for _, tc := range []struct {
		name           string
		input          map[string]any
		wantUnresolved []string
		wantDegraded   bool
		wantBanner     string
	}{
		{name: "posted and verified", input: on(pr, true, "https://forge/notes/1", ""), wantDegraded: false, wantBanner: "forge reply OK: https://forge/notes/1"},
		{name: "posted over plain http", input: on("http://forge.example/o/r/pull/1", true, "http://forge.example/notes/1", ""), wantDegraded: false, wantBanner: "forge reply OK: http://forge.example/notes/1"},
		{name: "posted with a url wrapped in whitespace", input: on(pr, true, " https://forge/notes/1\n", ""), wantDegraded: false, wantBanner: "forge reply OK: https://forge/notes/1"},
		{name: "posted on the PR's host, a GitHub html_url", input: on("https://github.com/o/r/pull/42", true, "https://github.com/o/r/pull/42#discussion_r7", ""), wantDegraded: false, wantBanner: "forge reply OK: https://github.com/o/r/pull/42#discussion_r7"},
		{name: "posted on the PR's host in another case, a GitLab note", input: on("https://gitlab.example/g/p/-/merge_requests/7", true, "https://GitLab.Example/g/p/-/merge_requests/7#note_9", ""), wantDegraded: false, wantBanner: "forge reply OK: https://GitLab.Example/g/p/-/merge_requests/7#note_9"},
		{name: "posted on the PR's host, a GitHub issue comment", input: on("https://github.com/o/r/pull/1", true, "https://github.com/o/r/pull/1#issuecomment-123", ""), wantDegraded: false, wantBanner: "forge reply OK: https://github.com/o/r/pull/1#issuecomment-123"},
		{name: "posted on the PR's host, a GitLab note", input: on("https://pic.example/g/p/-/merge_requests/7", true, "https://pic.example/g/p/-/merge_requests/7#note_95537", ""), wantDegraded: false, wantBanner: "forge reply OK: https://pic.example/g/p/-/merge_requests/7#note_95537"},
		{name: "posted on the PR's host, the pr_url in upper case", input: on("https://GitHub.COM/o/r/pull/1", true, "https://github.com/o/r/pull/1#issuecomment-123", ""), wantDegraded: false, wantBanner: "forge reply OK: https://github.com/o/r/pull/1#issuecomment-123"},
		{name: "posted on the PR's host and port", input: on("https://forge.example:8443/g/p/-/merge_requests/7", true, "https://forge.example:8443/g/p/-/merge_requests/7#note_3", ""), wantDegraded: false, wantBanner: "forge reply OK: https://forge.example:8443/g/p/-/merge_requests/7#note_3"},
		{name: "posted on another host", input: on("https://github.com/o/r/pull/42", true, "https://evil.example/o/r/pull/42#discussion_r7", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted on the forge's API host, not the PR's", input: on("https://github.com/o/r/pull/1", true, "https://api.github.com/repos/o/r/issues/comments/123", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted on the PR's host name, another port", input: on(pr, true, "https://forge:8443/notes/1", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted on the bare host of a pr_url on a port", input: on("https://forge.example:8443/g/p/-/merge_requests/7", true, "https://forge.example/g/p/-/merge_requests/7#note_3", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with no pr_url to place it", input: on("", true, "https://forge/notes/1", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with the pr_url absent", input: map[string]any{"posted": true, "reply_url": "https://forge/notes/1", "skipped_reason": ""}, wantUnresolved: []string{"input.pr_url"}, wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted without a reply url", input: on(pr, true, "", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted without a reply url, a skipped_reason beside it", input: on(pr, true, "", "x"), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with a placeholder reply url", input: on(pr, true, "placeholder", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with a relative reply url", input: on(pr, true, "forge/notes/1", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with a non-web scheme", input: on(pr, true, "ftp://forge/notes/1", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with a scheme and no host", input: on(pr, true, "https://", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with a url and trailing prose", input: on(pr, true, "https://forge/notes/1 placeholder", ""), wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "posted with the reply url absent", input: map[string]any{"posted": true, "skipped_reason": "", "pr_url": pr}, wantUnresolved: []string{"input.reply_url"}, wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "skipped with a reason", input: on(pr, false, "", "forge_token missing; this run is unattended-only"), wantDegraded: true, wantBanner: "forge reply skipped: forge_token missing; this run is unattended-only"},
		{name: "skipped out of tool steps", input: on(pr, false, "", "Ran out of tool steps"), wantDegraded: true, wantBanner: "forge reply skipped: Ran out of tool steps"},
		{name: "skipped with a placeholder reason", input: on(pr, false, "", "placeholder"), wantDegraded: true, wantBanner: "forge reply skipped: placeholder"},
		{name: "nothing posted, no reason", input: on(pr, false, "", ""), wantDegraded: true, wantBanner: "FORGE REPLY MISSING"},
		{name: "nothing posted, blank reason", input: on(pr, false, "", "  "), wantDegraded: true, wantBanner: "FORGE REPLY MISSING"},
		{name: "nothing posted, a url beside it", input: on(pr, false, "https://forge/notes/1", ""), wantDegraded: true, wantBanner: "FORGE REPLY MISSING"},
		{name: "nothing posted, reason absent", input: map[string]any{"posted": false, "reply_url": "", "pr_url": pr}, wantUnresolved: []string{"input.skipped_reason"}, wantDegraded: true, wantBanner: "forge reply skipped: "},
		{name: "every input absent", input: map[string]any{}, wantUnresolved: []string{"input.posted", "input.pr_url", "input.reply_url", "input.skipped_reason"}, wantDegraded: true, wantBanner: "forge reply skipped: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, unresolved := renderReviConverseHealth(t, tc.input)
			slices.Sort(unresolved)
			if !slices.Equal(unresolved, tc.wantUnresolved) {
				t.Fatalf("unresolved refs = %v, want %v — the case does not drive the input it names", unresolved, tc.wantUnresolved)
			}
			// bash is the executor's shell; sh proves the command stays POSIX.
			for _, shell := range []string{"bash", "sh"} {
				if _, err := exec.LookPath(shell); err != nil {
					continue
				}
				out, err := exec.Command(shell, "-c", command).Output()
				if err != nil {
					t.Fatalf("%s: converse_health failed: %v (out %q)", shell, err, out)
				}
				var res struct {
					Degraded *bool  `json:"degraded"`
					Banner   string `json:"banner"`
				}
				if err := json.Unmarshal(out, &res); err != nil {
					t.Fatalf("%s: output is not converse_health_output JSON: %v (%q)", shell, err, out)
				}
				if res.Degraded == nil {
					t.Fatalf("%s: output carries no degraded field: %q", shell, out)
				}
				if *res.Degraded != tc.wantDegraded {
					t.Errorf("%s: degraded = %v, want %v (banner %q)", shell, *res.Degraded, tc.wantDegraded, res.Banner)
				}
				if !strings.Contains(res.Banner, tc.wantBanner) {
					t.Errorf("%s: banner %q does not name %q", shell, res.Banner, tc.wantBanner)
				}
			}
		})
	}
}
