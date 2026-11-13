package bots

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// renderReviConverseHealth renders revi-converse's converse_health command
// with its input refs substituted the way the engine passes them: each value
// single-quoted for the shell.
func renderReviConverseHealth(t *testing.T, refs map[string]string) string {
	t.Helper()
	return regexp.MustCompile(`\{\{([^}]+)\}\}`).ReplaceAllStringFunc(toolCommand(t, "revi-converse/main.bot", "converse_health"), func(ref string) string {
		value := refs[ref[2:len(ref)-2]]
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	})
}

// converse_health is the run's only witness that a `/revi <question>` got its
// answer: a reply that did not land must read degraded, whether the agent
// claimed a post it cannot prove or posted nothing without saying why. An
// agent with no tool answers posted=false and an empty skipped_reason — the
// silence that read healthy while the question went unanswered.
func TestReviConverseHealthFlagsAnUnansweredQuestion(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, tc := range []struct {
		name, posted, reply, skipped string
		wantDegraded                 bool
		wantBanner                   string
	}{
		{name: "posted and verified", posted: "true", reply: "https://forge/notes/1", wantDegraded: false, wantBanner: "forge reply OK: https://forge/notes/1"},
		{name: "posted without a reply url", posted: "true", wantDegraded: true, wantBanner: "FORGE REPLY UNVERIFIED"},
		{name: "skipped with a reason", posted: "false", skipped: "forge_token missing; this run is unattended-only", wantDegraded: false, wantBanner: "forge reply skipped: forge_token missing"},
		{name: "nothing posted, no reason", posted: "false", wantDegraded: true, wantBanner: "FORGE REPLY MISSING"},
		{name: "nothing posted, blank reason", posted: "false", skipped: "  ", wantDegraded: true, wantBanner: "FORGE REPLY MISSING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := renderReviConverseHealth(t, map[string]string{
				"input.posted":         tc.posted,
				"input.reply_url":      tc.reply,
				"input.skipped_reason": tc.skipped,
			})
			out, err := exec.Command("sh", "-c", command).Output()
			if err != nil {
				t.Fatalf("converse_health failed: %v (out %q)", err, out)
			}
			var res struct {
				Degraded bool   `json:"degraded"`
				Banner   string `json:"banner"`
			}
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("output is not converse_health_output JSON: %v (%q)", err, out)
			}
			if res.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (banner %q)", res.Degraded, tc.wantDegraded, res.Banner)
			}
			if !strings.Contains(res.Banner, tc.wantBanner) {
				t.Errorf("banner %q does not name %q", res.Banner, tc.wantBanner)
			}
		})
	}
}
