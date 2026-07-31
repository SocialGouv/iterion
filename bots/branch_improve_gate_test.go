package bots

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestBranchImproveGateNeverGreenOnAnUnresolvedFinding is the guard that keeps
// the fixer from grading its own homework.
//
// The fixer now fills the same shared merge-gate context a reviewer does. That
// is only legitimate while the verdict stays a COUNT: if a `refused` entry
// could green the check, a fixer would clear any review by contesting every
// finding — the exact self-certification the deterministic-gate doctrine
// exists to prevent. A refusal is an argument for a human to arbitrate; it
// never unblocks a merge.
//
// The bot's real python body is executed, so the assertion is on shipped
// behaviour rather than on a description of it.
func TestBranchImproveGateNeverGreenOnAnUnresolvedFinding(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	type published struct {
		Summary string         `json:"summary"`
		Gate    map[string]any `json:"gate"`
	}
	run := func(t *testing.T, ledger, clean, pushed, verifyOK string) (published, bool) {
		t.Helper()
		var got published
		var posted bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posted = true
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("payload is not JSON: %v (%q)", err, raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"published": true, "gate_posted": got.Gate != nil})
		}))
		defer srv.Close()

		body := toolCommand(t, "branch-improve-loop/main.bot", "publish_verdict")
		for ref, val := range map[string]string{
			"{{vars.forge_publish_url}}":   srv.URL + "/api/v1/forge/publish-review",
			"{{vars.forge_publish_token}}": "run-token",
			"{{input.pr_url}}":             "https://github.com/acme/widgets/pull/7",
			"{{input.finding_ledger}}":     ledger,
			"{{input.review_summary}}":     "did the work",
			"{{input.branch_clean}}":       clean,
			"{{input.commits_pushed}}":     pushed,
			"{{input.push_reason}}":        "pushed 2 commits",
			"{{input.verify_ok}}":          verifyOK,
			"{{vars.gate_enabled}}":        "true",
			"{{vars.gate_context}}":        "iterion/review",
		} {
			if !strings.Contains(body, ref) {
				t.Fatalf("%s is no longer referenced by publish_verdict — the test wires nothing", ref)
			}
			body = strings.ReplaceAll(body, ref, "'"+strings.ReplaceAll(val, "'", `'\''`)+"'")
		}
		out, err := exec.Command("sh", "-c", body).Output()
		if err != nil {
			t.Fatalf("publish_verdict failed: %v (out %q)", err, out)
		}
		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("output is not post_feedback_output JSON: %v (%q)", err, out)
		}
		return got, posted
	}

	blocking := func(t *testing.T, g map[string]any) float64 {
		t.Helper()
		if g == nil {
			t.Fatal("no gate payload — the required check never lands and the PR is stuck")
		}
		n, ok := g["blocking_count"].(float64)
		if !ok {
			t.Fatalf("gate carries no blocking_count: %v", g)
		}
		return n
	}

	allFixed := `[{"id":"R1111","status":"fixed","commit":"abc1234567890"}]`

	t.Run("a refused finding keeps the gate red", func(t *testing.T) {
		got, _ := run(t, `[{"id":"R1111","status":"fixed"},{"id":"R2222","status":"refused","note":"not reachable from any caller"}]`,
			"true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v with a refused finding — a fixer could clear any review by contesting it", n)
		}
		if !strings.Contains(got.Summary, "keeps the merge gate red") {
			t.Errorf("the PR must say a contested finding still blocks:\n%s", got.Summary)
		}
		// The argument itself must reach the human who arbitrates it.
		if !strings.Contains(got.Summary, "not reachable from any caller") {
			t.Errorf("the refusal's argument is missing from the table:\n%s", got.Summary)
		}
	})

	t.Run("a deferred finding keeps the gate red", func(t *testing.T) {
		got, _ := run(t, `[{"id":"R3333","status":"deferred","note":"needs a migration"}]`, "true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v with a deferred finding", n)
		}
	})

	t.Run("a red build keeps the gate red even with every finding fixed", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "true", "false")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v on a tree that does not build", n)
		}
	})

	t.Run("remaining issues in the diff keep the gate red", func(t *testing.T) {
		got, _ := run(t, allFixed, "false", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v while the fixer still reports issues in the diff", n)
		}
	})

	// Pushing code that nothing has reviewed must not be reported as vetted:
	// with no ledger there is no review of this revision to speak for.
	t.Run("code pushed with no review of it is not green", func(t *testing.T) {
		got, _ := run(t, `[]`, "true", "true", "true")
		if n := blocking(t, got.Gate); n < 1 {
			t.Errorf("blocking_count = %v on a head nothing reviewed", n)
		}
	})

	t.Run("everything fixed, clean and green passes", func(t *testing.T) {
		got, _ := run(t, allFixed, "true", "true", "true")
		if n := blocking(t, got.Gate); n != 0 {
			t.Errorf("blocking_count = %v when every finding is fixed, the re-review is clean and the build is green", n)
		}
		// It must not read as an independent review — the fixer wrote the code.
		note, _ := got.Gate["note"].(string)
		if !strings.Contains(note, "by the fixer") {
			t.Errorf("a green gate must say whose re-review it rests on, got %q", note)
		}
	})

	// Speaking for a revision this run did not change would overwrite whatever
	// verdict already sits on it — including a reviewer's.
	t.Run("nothing pushed and nothing answered posts nothing at all", func(t *testing.T) {
		_, posted := run(t, `[]`, "true", "false", "true")
		if posted {
			t.Error("posted a verdict for a head this run neither changed nor reviewed")
		}
	})
}
