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
	// Every effort dial reads `max`, a level no case's run used: the table
	// prints the effort the engine recorded on the node that ran, so `max`
	// in a row would be the dial read back (#3283 F11).
	for _, dial := range []string{"ITERION_VIBE_EFFORT_CLAUDE", "ITERION_VIBE_EFFORT_GPT", "ITERION_VIBE_EFFORT_CLAUDE_GLANCE", "ITERION_VIBE_EFFORT_GPT_GLANCE", "ITERION_VIBE_EFFORT_EMIT"} {
		t.Setenv(dial, "max")
	}
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
				"input.ai_reviewer_model":   "openai/gpt-6-astra",
				"input.ai_reviewer_backend": "claw",
				"input.ai_reviewer_tokens":  "205000",
				"input.ai_reviewer_effort":  "xhigh", "input.ai_reviewer_effort_source": "node",
				"input.ai_converge_model":   "openai/summary-model",
				"input.ai_converge_backend": "another-harness",
				"input.ai_converge_tokens":  "20000",
				"input.ai_converge_effort":  "medium", "input.ai_converge_effort_source": "node",
			},
			want:   []string{"<code>run-123</code>", "**225 800**", "| Revue GPT | openai/gpt-6-astra | claw | xhigh (nœud) | 205 000 |", "| Synthèse · GPT | openai/summary-model | another-harness | medium (nœud) | 20 000 |"},
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
				"input.ai_reviewer_model":   "claude-sonnet-4-6",
				"input.ai_reviewer_backend": "claude_code",
				"input.ai_reviewer_tokens":  "760",
				"input.ai_reviewer_effort":  "xhigh", "input.ai_reviewer_effort_source": "node",
			},
			want:   []string{"| Revue Claude | claude-sonnet-4-6 | claude_code | xhigh (nœud) | 760 |"},
			absent: []string{"Revue GPT"},
		},
		{
			name: "a glm model labels the row Revue GLM",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "900",
				"input.ai_reviewer_alternate_model":   "glm-5.3",
				"input.ai_reviewer_alternate_backend": "claude_code",
				"input.ai_reviewer_alternate_tokens":  "860",
				"input.ai_reviewer_alternate_effort":  "high", "input.ai_reviewer_alternate_effort_source": "node",
			},
			want:   []string{"| Revue GLM | glm-5.3 | claude_code | high (nœud) | 860 |"},
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
				"input.ai_reviewer_model":   "claude-sonnet-4-6",
				"input.ai_reviewer_backend": "claude_code",
				"input.ai_reviewer_tokens":  "72128",
				"input.ai_reviewer_wire":    "facade:zai:https://api.z.ai/api/anthropic",
				"input.ai_reviewer_effort":  "xhigh", "input.ai_reviewer_effort_source": "node",
				"input.ai_converge_model":   "claude-opus-5-5",
				"input.ai_converge_backend": "claude_code",
				"input.ai_converge_tokens":  "4316",
				"input.ai_converge_wire":    "facade:zai:https://api.z.ai/api/anthropic",
				"input.ai_converge_effort":  "medium", "input.ai_converge_effort_source": "node",
			},
			want:   []string{"| Revue GLM | claude-sonnet-4-6 | claude_code | xhigh (nœud) | 72 128 |", "| Synthèse · GLM | claude-opus-5-5 | claude_code | medium (nœud) | 4 316 |"},
			absent: []string{"Revue Claude", "Revue GPT", "Synthèse · Claude", "api.z.ai", "facade:"},
		},
		{
			// A facade the label names no vendor for claims no family: the
			// model id it was sent is exactly what cannot be trusted there.
			name: "an unrecognised facade claims no family",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "500",
				"input.ai_reviewer_alternate_model":   "claude-opus-5-5",
				"input.ai_reviewer_alternate_backend": "claude_code",
				"input.ai_reviewer_alternate_tokens":  "500",
				"input.ai_reviewer_alternate_wire":    "facade:https://gateway.example",
				"input.ai_reviewer_alternate_effort":  "high", "input.ai_reviewer_alternate_effort_source": "node",
			},
			want:   []string{"| Revue | claude-opus-5-5 | claude_code | high (nœud) | 500 |"},
			absent: []string{"Revue Claude", "gateway.example"},
		},
		{
			name: "the gate note names a claude reviewer served by its fallback",
			refs: map[string]string{
				"run.id": "run-123", "input.scope_files": "3",
				"input.ai_reviewer_alternate_model":    "glm-5.3",
				"input.ai_reviewer_alternate_backend":  "claude_code",
				"input.ai_reviewer_alternate_tokens":   "900",
				"input.ai_reviewer_alternate_fallback": "true",
			},
			note: "the claude reviewer ran on its fallback route (glm-5.3)",
		},
		{
			// The script's arm, not the bot's route: `converge` declares no
			// fallbacks since ADR-169 made it the one merge step, so this
			// flag is set here rather than produced by a run. The publish
			// script still owes a note for a deployment that re-points the
			// merge at a CLI backend carrying a rescue.
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
			name: "a paired review says it ran two reviewers",
			refs: map[string]string{
				"run.id": "run-123", "input.effective_review_mode": "paired",
				"input.ai_reviewer_alternate_model": "claude-opus-5-5",
				"input.ai_reviewer_model":           "openai/gpt-6-astra",
			},
			want: []string{"Revue à deux relecteurs ; 0 finding(s) cross-confirmed by both reviewers."},
		},
		{
			name: "both reviewers and a measured zero",
			refs: map[string]string{
				"input.ai_run_tokens": "700", "input.ai_reviewer_alternate_model": "claude-opus-5-5",
				"input.ai_reviewer_alternate_backend": "claude_code", "input.ai_reviewer_alternate_tokens": "700",
				"input.ai_reviewer_alternate_effort": "high", "input.ai_reviewer_alternate_effort_source": "node",
				"input.ai_reviewer_model": "openai/gpt-6-astra", "input.ai_reviewer_tokens": "0",
				"input.ai_reviewer_effort": "xhigh", "input.ai_reviewer_effort_source": "node",
			},
			want: []string{"| Revue Claude | claude-opus-5-5 | claude_code | high (nœud) | 700 |", "| Revue GPT | openai/gpt-6-astra | indisponible | xhigh (nœud) | 0 |"},
		},
		{
			// What the run used, from the engine's record on the node that
			// ran — never the dial, which reads `max` here. A routing cell's
			// `low` is the reviewer's level; a level the gateway's feed
			// replaced names the feed and the level asked for; an edge's
			// level names the edge; a source the table does not know is
			// printed as given (HTML-escaped like every engine value).
			name: "the effort the run used, never the env dial",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "3000",
				"input.ai_reviewer_alternate_model": "openai_compatible/scaleway/deepseek-v4.1-flash", "input.ai_reviewer_alternate_backend": "claw",
				"input.ai_reviewer_alternate_tokens": "1000",
				"input.ai_reviewer_alternate_effort": "low", "input.ai_reviewer_alternate_effort_source": "cell",
				"input.ai_reviewer_model": "openai_compatible/scaleway/deepseek-v4.1-flash", "input.ai_reviewer_backend": "claw",
				"input.ai_reviewer_tokens": "1000",
				"input.ai_reviewer_effort": "xhigh", "input.ai_reviewer_effort_source": "edge",
				"input.ai_reviewer_glance_model": "openai_compatible/scaleway/deepseek-v4.1-flash", "input.ai_reviewer_glance_backend": "claw",
				"input.ai_reviewer_glance_tokens": "400",
				"input.ai_reviewer_glance_effort": "low", "input.ai_reviewer_glance_effort_source": "<b>who</b>",
				"input.ai_converge_model": "openai_compatible/scaleway/deepseek-v4.1-flash", "input.ai_converge_backend": "claw",
				"input.ai_converge_tokens": "200",
				"input.ai_converge_effort": "high", "input.ai_converge_effort_source": "feed:pliage", "input.ai_converge_effort_requested": "medium",
			},
			want: []string{
				"| Étape | Modèle | Harness | Effort (source) | Tokens |",
				"| Revue | openai_compatible/scaleway/deepseek-v4.1-flash | claw | low (cellule) | 1 000 |",
				"| xhigh (arête) | 1 000 |",
				"| low (&lt;b&gt;who&lt;/b&gt;) | 400 |",
				"| high (feed pliage ; demandé medium) | 200 |",
			},
			absent: []string{"max", "<b>who</b>"},
		},
		{
			// The harness's own answers and the other request sources: a
			// level the client did not send, a recovery at a lower level, an
			// operator dial, the launch's row, the overrides when the run
			// cannot tell which layer, a level the harness clamped.
			name: "what the harness sent, and every source of a request",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "3000",
				"input.ai_reviewer_alternate_model": "anthropic/glm-5.3", "input.ai_reviewer_alternate_backend": "claw", "input.ai_reviewer_alternate_tokens": "1000",
				"input.ai_reviewer_alternate_effort_source": "backend", "input.ai_reviewer_alternate_effort_requested": "high",
				"input.ai_reviewer_model": "openai/gpt-6-astra", "input.ai_reviewer_backend": "claw", "input.ai_reviewer_tokens": "1000",
				"input.ai_reviewer_effort": "high", "input.ai_reviewer_effort_source": "node", "input.ai_reviewer_effort_lowered": "low",
				"input.ai_reviewer_glance_model": "openai/gpt-6-luna", "input.ai_reviewer_glance_backend": "claw", "input.ai_reviewer_glance_tokens": "400",
				"input.ai_reviewer_glance_effort": "medium", "input.ai_reviewer_glance_effort_source": "launch",
				"input.ai_converge_model": "claude-opus-5-5", "input.ai_converge_backend": "claude_code", "input.ai_converge_tokens": "200",
				"input.ai_converge_effort": "low", "input.ai_converge_effort_source": "backend", "input.ai_converge_effort_requested": "none",
			},
			want: []string{
				"| non envoyé (harness ; demandé high) | 1 000 |",
				"| high (nœud) · abaissé à low (récupération) | 1 000 |",
				"| medium (lancement) | 400 |",
				"| low (harness ; demandé none) | 200 |",
			},
		},
		{
			name: "the overrides when the run cannot tell which layer",
			refs: map[string]string{
				"run.id": "run-123", "input.ai_run_tokens": "100",
				"input.ai_converge_model": "claude-opus-5-5", "input.ai_converge_backend": "claude_code", "input.ai_converge_tokens": "100",
				"input.ai_converge_effort": "xhigh", "input.ai_converge_effort_source": "override",
			},
			want: []string{"| xhigh (surcharge) | 100 |"},
		},
		{
			name: "missing telemetry stays unavailable and HTML stays text",
			refs: map[string]string{
				"run.id": "<details>|unsafe", "input.ai_run_tokens": "{{run.tokens}}",
				"input.ai_reviewer_model":  "<b>model</b>|extra\nrow",
				"input.ai_reviewer_tokens": "-1",
			},
			want:   []string{"**indisponible**", "&lt;details&gt;&#124;unsafe", "&lt;b&gt;model&lt;/b&gt;&#124;extra row", "| indisponible | indisponible | indisponible |"},
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
				"input.findings":     `[{"file":"a.go","line":1,"title":"A real finding","severity":"high"}]`,
				"input.review_scope": "code:a.go",
				"vars.gate_enabled":  "true", "vars.gate_severity": "high",
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
