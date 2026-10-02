package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestReviewPRPublishPinsTheGateToTheReviewedRevision pins the one line that
// ties Revi's verdict to the revision it judged:
// payload['gate']['audited_sha'] = reviewed in publish_review. revi/review is
// iterion's own required check, and the publish endpoint resolves the pull
// request head when it WRITES the status — so a push landing between the
// review and the publish would otherwise take this verdict as its own. The
// server refuses a stale pin instead of retargeting; a payload that drops
// the pin goes quietly back to certifying whatever revision arrived last.
//
// Measured before this test existed (#1636): mutating the pin away, or to a
// hardcoded constant, kept the whole bots/ suite green. A source-level grep
// is not the witness either (the sibling bundle carries expectedHeadOid in a
// COMMENT), so the assertion here is on what goes over the wire: the command
// runs for real against a stub of the server publish endpoint — the shape
// TestDepUpdateGuardGateVerdict already uses for post_feedback.
//
// The sibling property — a gate which did not land must raise MERGE GATE NOT
// POSTED through publish_health rather than read as a healthy publish — is
// pinned by TestReviewPRPublishHealthSeesALostGateOnEveryExit.
func TestReviewPRPublishPinsTheGateToTheReviewedRevision(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "review-pr/main.bot", "publish_review")

	const reviewed = "f00dcafe1234"

	type gate struct {
		Enabled       bool   `json:"enabled"`
		Context       string `json:"context"`
		BlockingCount int    `json:"blocking_count"`
		AuditedSHA    string `json:"audited_sha"`
	}
	type published struct {
		PRURL string `json:"pr_url"`
		Gate  *gate  `json:"gate"`
	}

	var got published
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server registers exactly ONE path, and it is the only one
		// exempt from the auth middleware. A stub answering any path lets a
		// bot post to a URL production rejects with 401.
		if r.URL.Path != "/api/v1/forge/publish-review" {
			t.Errorf("published to %q, want the endpoint path — anything else hits the auth middleware", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		seen = true
		if tok := r.Header.Get("X-Iterion-Run"); tok != "run-token" {
			t.Errorf("publish must authenticate with the run grant, got %q", tok)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode publish payload: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"published": true, "review_url": "https://forge/review/1",
			"comments_posted": 0, "gate_posted": true,
			"gate_state": "success", "gate_sha": reviewed,
		})
	}))
	defer srv.Close()

	rendered := cmdTemplate
	// The node reads its inputs through env assignments on the command head,
	// so values are substituted single-quoted and the whole command runs
	// through sh -c — the same shape production produces. Asserting on the
	// template text would prove nothing about what the shell and python
	// actually see.
	for ref, val := range map[string]string{
		"{{run.id}}":              "run01test",
		"{{input.ai_run_tokens}}": "''",

		"{{input.ai_reviewer_claude_model}}":           "''",
		"{{input.ai_reviewer_claude_backend}}":         "''",
		"{{input.ai_reviewer_claude_tokens}}":          "''",
		"{{input.ai_reviewer_claude_fallback}}":        "''",
		"{{input.ai_reviewer_claude_wire}}":            "''",
		"{{input.ai_reviewer_gpt_model}}":              "''",
		"{{input.ai_reviewer_gpt_backend}}":            "''",
		"{{input.ai_reviewer_gpt_tokens}}":             "''",
		"{{input.ai_reviewer_gpt_wire}}":               "''",
		"{{input.ai_reviewer_claude_glance_model}}":    "''",
		"{{input.ai_reviewer_claude_glance_backend}}":  "''",
		"{{input.ai_reviewer_claude_glance_tokens}}":   "''",
		"{{input.ai_reviewer_claude_glance_fallback}}": "''",
		"{{input.ai_reviewer_claude_glance_wire}}":     "''",
		"{{input.ai_reviewer_gpt_glance_model}}":       "''",
		"{{input.ai_reviewer_gpt_glance_backend}}":     "''",
		"{{input.ai_reviewer_gpt_glance_tokens}}":      "''",
		"{{input.ai_reviewer_gpt_glance_wire}}":        "''",
		"{{input.ai_converge_model}}":                  "''",
		"{{input.ai_converge_backend}}":                "''",
		"{{input.ai_converge_tokens}}":                 "''",
		"{{input.ai_converge_fallback}}":               "''",
		"{{input.ai_converge_wire}}":                   "''",

		"{{input.review_scope}}":          "''",
		"{{input.reviewed_sha}}":          "'" + reviewed + "'",
		"{{input.pr_url}}":                "'https://github.com/acme/widgets/pull/7'",
		"{{input.findings}}":              "'[]'",
		"{{input.questions}}":             "''",
		"{{input.ticket_conformance}}":    "''",
		"{{input.claude_findings}}":       "'[]'",
		"{{input.gpt_findings}}":          "'[]'",
		"{{input.effective_review_mode}}": "'mono'",
		// A NON-NEGATIVE count is the only shape that means the scope was
		// answered; anything else fails the gate closed before the pin.
		"{{input.scope_files}}": "'3'",
		// A clean tree: no workspace-anomaly routing under test here.
		"{{input.workspace_anomaly}}": "''",

		// An empty WS keeps the stale-anchor guard inert: it reads git only
		// when both a sha-looking REVIEWED_SHA and a workspace are set.
		"{{vars.workspace_dir}}":       "''",
		"{{vars.forge_publish_url}}":   "'" + srv.URL + "/api/v1/forge/publish-review'",
		"{{vars.forge_publish_token}}": "'run-token'",
		"{{vars.pr_review_mode}}":      "'inline'",
		"{{vars.gate_enabled}}":        "'true'",
		"{{vars.gate_severity}}":       "'high'",
		"{{vars.gate_context}}":        "'revi/review'",
		"{{vars.fixer_hint}}":          "''",
	} {
		rendered = strings.ReplaceAll(rendered, ref, val)
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}

	out, err := exec.Command("sh", "-c", rendered).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	var res struct {
		Published     bool `json:"published"`
		GateRequested bool `json:"gate_requested"`
		GatePosted    bool `json:"gate_posted"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
	}
	if !seen {
		t.Fatal("the publish never reached the endpoint")
	}
	if !res.Published || !res.GateRequested || !res.GatePosted {
		t.Fatalf("a clean review with the gate on must publish and post its gate: %q", out)
	}
	if got.Gate == nil {
		t.Fatal("no gate block in the payload — the required check would never land")
	}
	// The whole point: the verdict travels pinned to the revision the review
	// JUDGED, not to whatever the head says when the server writes the status.
	if got.Gate.AuditedSHA != reviewed {
		t.Errorf("gate audited_sha = %q, want the reviewed revision %q — an unpinned verdict certifies whichever commit arrived last", got.Gate.AuditedSHA, reviewed)
	}
	if got.Gate.Context != "revi/review" {
		t.Errorf("gate context = %q, want revi/review", got.Gate.Context)
	}
}
