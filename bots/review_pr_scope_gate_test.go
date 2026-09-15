package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// TestTheGateRefusesAReviewThatReadNothing closes the loop the scope sentinel
// opens.
//
// `diff_precheck` reports `changed_files: -1` when it could not compute the
// scope at all. The reviewers are told to say so, but `questions` is a
// NON-BLOCKING channel by design — so without this, a run that read no code
// produces zero findings and the deterministic gate posts SUCCESS. Zero
// findings out of zero files read is not an approval.
//
// Measured 2026-09-15 on a real cloud run: the workspace held no checkout (no
// `.git` anywhere), `reviewed_sha` came back empty, the reviewers correctly
// reported "no diff could be read and therefore no code was reviewed" — and
// the gate payload still carried `blocking_count: 0`.
//
// The precedent is already in this node: an unparseable finding set fails the
// gate CLOSED rather than reporting green by omission. A scope that never
// resolved is the same rule one step earlier.
func TestTheGateRefusesAReviewThatReadNothing(t *testing.T) {
	for _, bin := range []string{"python3", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}

	ws := t.TempDir()
	gittest.Run(t, ws, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(ws+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, ws, "add", "-A")
	gittest.Run(t, ws, "commit", "-m", "one")
	head := gittest.Run(t, ws, "rev-parse", "HEAD")

	type published struct {
		Gate map[string]any `json:"gate"`
	}

	run := func(t *testing.T, scopeFiles, findings string) published {
		t.Helper()
		var got published
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("publish payload is not JSON: %v (%q)", err, raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"published": true, "review_url": "https://forge/review/1",
				"comments_posted": 0, "suggestions_posted": 0,
				"gate_posted": got.Gate != nil, "gate_state": "failure",
			})
		}))
		defer srv.Close()

		body := toolCommand(t, "review-pr/main.bot", "publish_review")
		for ref, val := range map[string]string{
			"{{vars.workspace_dir}}":          ws,
			"{{input.reviewed_sha}}":          head,
			"{{vars.forge_publish_url}}":      srv.URL + "/api/v1/forge/publish-review",
			"{{vars.forge_publish_token}}":    "run-token",
			"{{vars.pr_review_mode}}":         "summary",
			"{{input.effective_review_mode}}": "mono",
			"{{input.pr_url}}":                "https://github.com/acme/widgets/pull/7",
			"{{input.findings}}":              findings,
			"{{input.questions}}":             "",
			"{{input.claude_findings}}":       findings,
			"{{input.gpt_findings}}":          "[]",
			"{{vars.gate_enabled}}":           "true",
			"{{vars.gate_severity}}":          "high",
			"{{vars.gate_context}}":           "revi/review",
			"{{input.scope_files}}":           scopeFiles,
		} {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by publish_review — the test wires nothing", ref)
			}
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		if _, err := exec.Command("sh", "-c", body).Output(); err != nil {
			t.Fatalf("publish_review failed: %v", err)
		}
		if got.Gate == nil {
			t.Fatal("published without a gate payload — the required status cannot be created")
		}
		return got
	}

	// Every shape that is not a non-negative count is the ABSENCE of an answer.
	// A guard written as a blacklist fails OPEN on the shapes it forgot — and
	// the unsubstituted render below is not hypothetical: it is exactly what
	// the first wiring of this mapping produced, and `ai_value` exists in the
	// same command to filter those renders out of the sibling AI_* mappings.
	for _, tc := range []struct{ name, scope string }{
		{"the -1 sentinel", "-1"},
		{"an unsubstituted template", "{{outputs.diff_precheck.changed_files}}"},
		{"a null render", "null"},
		{"a Go nil render", "<nil>"},
		{"a python None render", "None"},
		{"nothing at all", ""},
		{"prose instead of a count", "unknown"},
	} {
		t.Run("an unresolved scope blocks: "+tc.name, func(t *testing.T) {
			got := run(t, tc.scope, "[]")
			blocking, _ := got.Gate["blocking_count"].(float64)
			if blocking < 1 {
				t.Errorf("blocking_count = %v on a review that read NO code — the gate posts success and the pull request merges unreviewed", got.Gate["blocking_count"])
			}
			note, _ := got.Gate["note"].(string)
			if !strings.Contains(note, "no code was reviewed") {
				t.Errorf("the status must say why it is red, got %q — an operator hunting for a finding that does not exist is the failure this note exists to avoid", note)
			}
		})
	}

	// The other half, and the one that keeps the guard from being a blanket
	// refusal: a real scope with no findings is a real approval.
	t.Run("a resolved scope with no findings still passes", func(t *testing.T) {
		got := run(t, "3", "[]")
		blocking, _ := got.Gate["blocking_count"].(float64)
		if blocking != 0 {
			t.Errorf("blocking_count = %v on a clean review of three files — the gate would block every green PR", got.Gate["blocking_count"])
		}
	})

	// And a genuinely empty branch is not an unresolved one: 0 is a real
	// answer, -1 is the absence of one.
	t.Run("zero changed files is an answer, not an absence", func(t *testing.T) {
		got := run(t, "0", "[]")
		blocking, _ := got.Gate["blocking_count"].(float64)
		if blocking != 0 {
			t.Errorf("blocking_count = %v on a branch that changed nothing", got.Gate["blocking_count"])
		}
	})
}
