package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/deeplink"
)

func TestReviewPRRunDetails(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	t.Setenv("ITERION_VIBE_EFFORT_GPT", "xhigh")
	t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "")
	t.Setenv("ITERION_VIBE_EFFORT_EMIT", "")
	for _, tc := range []struct {
		name         string
		endpointPath string
		refs         map[string]string
		want         []string
		absent       []string
	}{
		{
			name:         "served model and harness, total includes other run work",
			endpointPath: "/api/v1/forge/publish-review?unused=private#fragment",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "225800",
				"input.ai_reviewer_gpt_model":   "openai/served-model",
				"input.ai_reviewer_gpt_backend": "claw",
				"input.ai_reviewer_gpt_tokens":  "205000",
				"input.ai_converge_model":       "openai/summary-model",
				"input.ai_converge_backend":     "another-harness",
				"input.ai_converge_tokens":      "20000",
			},
			want:   []string{"<code>run-123</code>", "**225 800**", "| Revue GPT | openai/served-model | claw | xhigh | 205 000 |", "| Synthèse | openai/summary-model | another-harness | medium | 20 000 |"},
			absent: []string{"Revue Claude", "glance", "**225 000**", "private", "fragment", "test-token"},
		},
		{
			name:         "instance public base path is preserved",
			endpointPath: "/iterion/api/v1/forge/publish-review",
			refs:         map[string]string{"run.id": "run-123"},
		},
		{
			name:         "unknown callback path leaves a plain identifier",
			endpointPath: "/custom-publisher",
			refs:         map[string]string{"run.id": "run-123"},
			want:         []string{"Run : <code>run-123</code>"},
			absent:       []string{"<a href="},
		},
		{
			name: "both reviewers and a measured zero",
			refs: map[string]string{
				"input.ai_run_tokens": "700", "input.ai_reviewer_claude_model": "anthropic/served-model",
				"input.ai_reviewer_claude_backend": "claude_code", "input.ai_reviewer_claude_tokens": "700",
				"input.ai_reviewer_gpt_model": "openai/served-model", "input.ai_reviewer_gpt_tokens": "0",
			},
			want: []string{"| Revue Claude | anthropic/served-model | claude_code | high | 700 |", "| Revue GPT | openai/served-model | indisponible | xhigh | 0 |"},
		},
		{
			name: "missing telemetry stays unavailable and HTML stays text",
			refs: map[string]string{
				"run.id": "<details>|unsafe", "input.ai_run_tokens": "{{run.tokens}}",
				"input.ai_reviewer_gpt_model":  "<b>model</b>|extra\nrow",
				"input.ai_reviewer_gpt_tokens": "-1",
			},
			want:   []string{"**indisponible**", "&lt;details&gt;&#124;unsafe", "&lt;b&gt;model&lt;/b&gt;&#124;extra row", "| indisponible | xhigh | indisponible |"},
			absent: []string{"{{run.tokens}}", "<b>model</b>", "**0**", "<a href="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Summary  string           `json:"summary"`
				Comments []map[string]any `json:"comments"`
				Gate     struct {
					BlockingCount int `json:"blocking_count"`
				} `json:"gate"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode publish: %v", err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "comments_posted": len(got.Comments)})
			}))
			defer srv.Close()
			endpointPath := tc.endpointPath
			if endpointPath == "" {
				endpointPath = "/api/v1/forge/publish-review"
			}
			refs := map[string]string{
				"vars.forge_publish_url": srv.URL + endpointPath, "vars.forge_publish_token": "test-token",
				"input.pr_url": "https://github.com/acme/repo/pull/1", "input.effective_review_mode": "mono",
				"input.findings":    `[{"file":"a.go","line":1,"title":"A real finding","severity":"high"}]`,
				"vars.gate_enabled": "true", "vars.gate_severity": "high",
			}
			for k, v := range tc.refs {
				refs[k] = v
			}
			body := regexp.MustCompile(`\{\{([^}]+)\}\}`).ReplaceAllStringFunc(toolCommand(t, "review-pr/main.bot", "publish_review"), func(ref string) string {
				value := refs[ref[2:len(ref)-2]]
				return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
			})
			out, err := exec.Command("sh", "-c", body).CombinedOutput()
			if err != nil {
				t.Fatalf("publish command: %v: %s", err, out)
			}
			if len(got.Comments) != 1 || got.Gate.BlockingCount != 1 {
				t.Fatalf("footer changed existing findings/gate: %+v", got)
			}
			if strings.Count(got.Summary, "<details>") != 1 || strings.Contains(got.Summary, "<details open") || !strings.HasSuffix(got.Summary, "</details>") {
				t.Fatalf("details must be collapsed and last: %s", got.Summary)
			}
			if tc.refs["run.id"] == "run-123" && endpointPath != "/custom-publisher" {
				basePath, _, _ := strings.Cut(endpointPath, "/api/v1/forge/publish-review")
				// The address the studio actually answers on, read from the
				// engine's constant rather than spelled out here: a comment
				// published on a pull request that points at a redirect, or at
				// nothing, is not something a reviewer can be asked to notice.
				want := `<a href="` + srv.URL + basePath + deeplink.StudioBase + `/runs/run-123"><code>run-123</code></a>`
				if !strings.Contains(got.Summary, want) {
					t.Errorf("missing authenticated run link %q in %s", want, got.Summary)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Summary, want) {
					t.Errorf("missing %q in %s", want, got.Summary)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got.Summary, absent) {
					t.Errorf("unexpected %q in %s", absent, got.Summary)
				}
			}
		})
	}
}
