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
		// note is a substring the merge-gate note must carry ("" = unchecked).
		note string
	}{
		{
			name:         "served model and harness, total includes other run work",
			endpointPath: "/api/v1/forge/publish-review?unused=private#fragment",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "225800",
				"input.ai_reviewer_gpt_model":   "openai/gpt-6-astra",
				"input.ai_reviewer_gpt_backend": "claw",
				"input.ai_reviewer_gpt_tokens":  "205000",
				"input.ai_converge_model":       "openai/summary-model",
				"input.ai_converge_backend":     "another-harness",
				"input.ai_converge_tokens":      "20000",
			},
			want:   []string{"<code>run-123</code>", "**225 800**", "| Revue GPT | openai/gpt-6-astra | claw | xhigh | 205 000 |", "| Synthèse · GPT | openai/summary-model | another-harness | medium | 20 000 |"},
			absent: []string{"Revue Claude", "Revue GLM", "glance", "**225 000**", "private", "fragment", "test-token"},
		},
		{
			// The run-details table names the family that SERVED: the gpt
			// slot's model is a dial, and an instance pointing it at a claude
			// model (the documented cross-vendor outage dial) must not publish
			// "Revue GPT" over a claude run — PR #1924's gate comment did
			// exactly that.
			name: "a gpt slot fed a claude model is labelled by what served",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "800",
				"input.ai_reviewer_gpt_model":   "claude-sonnet-4-6",
				"input.ai_reviewer_gpt_backend": "claude_code",
				"input.ai_reviewer_gpt_tokens":  "760",
			},
			want:   []string{"| Revue Claude | claude-sonnet-4-6 | claude_code | xhigh | 760 |"},
			absent: []string{"Revue GPT"},
		},
		{
			name: "a glm model labels the row Revue GLM",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "900",
				"input.ai_reviewer_claude_model":   "glm-5.3",
				"input.ai_reviewer_claude_backend": "claude_code",
				"input.ai_reviewer_claude_tokens":  "860",
			},
			want:   []string{"| Revue GLM | glm-5.3 | claude_code | high | 860 |"},
			absent: []string{"Revue Claude", "Revue GPT"},
		},
		{
			// PR #1924 exactly: the gpt slot sent claude-sonnet-4-6 to the z.ai
			// facade, which ACCEPTED the claude id and served GLM. The model id
			// alone would say "Revue Claude"; only the session's routing label
			// knows. The label itself is never published — only the family.
			name: "a claude id served by the z.ai facade is labelled GLM",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "72128",
				"input.ai_reviewer_gpt_model":   "claude-sonnet-4-6",
				"input.ai_reviewer_gpt_backend": "claude_code",
				"input.ai_reviewer_gpt_tokens":  "72128",
				"input.ai_reviewer_gpt_wire":    "facade:zai:https://api.z.ai/api/anthropic",
				"input.ai_converge_model":       "claude-opus-5-5",
				"input.ai_converge_backend":     "claude_code",
				"input.ai_converge_tokens":      "4316",
				"input.ai_converge_wire":        "facade:zai:https://api.z.ai/api/anthropic",
			},
			want:   []string{"| Revue GLM | claude-sonnet-4-6 | claude_code | xhigh | 72 128 |", "| Synthèse · GLM | claude-opus-5-5 | claude_code | medium | 4 316 |"},
			absent: []string{"Revue Claude", "Revue GPT", "Synthèse · Claude", "api.z.ai", "facade:"},
		},
		{
			// A facade the label names no vendor for claims no family: the
			// model id it was sent is exactly what cannot be trusted there.
			name: "an unrecognised facade claims no family",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "500",
				"input.ai_reviewer_claude_model":   "claude-opus-5-5",
				"input.ai_reviewer_claude_backend": "claude_code",
				"input.ai_reviewer_claude_tokens":  "500",
				"input.ai_reviewer_claude_wire":    "facade:https://gateway.example",
			},
			want:   []string{"| Revue | claude-opus-5-5 | claude_code | high | 500 |"},
			absent: []string{"Revue Claude", "gateway.example"},
		},
		{
			name: "the gate note names a claude reviewer served by its fallback",
			refs: map[string]string{
				"run.id": "run-123", "input.scope_files": "3",
				"input.ai_reviewer_claude_model":    "glm-5.3",
				"input.ai_reviewer_claude_backend":  "claude_code",
				"input.ai_reviewer_claude_tokens":   "900",
				"input.ai_reviewer_claude_fallback": "true",
			},
			note: "the claude reviewer ran on its fallback route (glm-5.3)",
		},
		{
			name: "the gate note names a merge step served by its fallback",
			refs: map[string]string{
				"run.id": "run-123", "input.scope_files": "3",
				"input.ai_converge_model":    "glm-5.3",
				"input.ai_converge_backend":  "claude_code",
				"input.ai_converge_tokens":   "400",
				"input.ai_converge_fallback": "true",
			},
			note: "the merge step ran on its fallback route (glm-5.3)",
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
				"input.ai_run_tokens": "700", "input.ai_reviewer_claude_model": "claude-opus-5-5",
				"input.ai_reviewer_claude_backend": "claude_code", "input.ai_reviewer_claude_tokens": "700",
				"input.ai_reviewer_gpt_model": "openai/gpt-6-astra", "input.ai_reviewer_gpt_tokens": "0",
			},
			want: []string{"| Revue Claude | claude-opus-5-5 | claude_code | high | 700 |", "| Revue GPT | openai/gpt-6-astra | indisponible | xhigh | 0 |"},
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
					BlockingCount int    `json:"blocking_count"`
					Note          string `json:"note"`
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
			if tc.note != "" && !strings.Contains(got.Gate.Note, tc.note) {
				t.Errorf("gate note %q does not carry %q", got.Gate.Note, tc.note)
			}
		})
	}
}
