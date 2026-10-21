package bots

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

func renderReviewPRBodyHealthNode(t *testing.T, node string, refs map[string]string) string {
	t.Helper()
	return regexp.MustCompile(`\{\{([^}]+)\}\}`).ReplaceAllStringFunc(toolCommand(t, "review-pr/main.bot", node), func(ref string) string {
		value := refs[ref[2:len(ref)-2]]
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	})
}

func TestReviewPRPublishReviewPropagatesBodyMissingWithoutFoldedClaim(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"published": true, "provider": "gitlab", "review_url": "https://forge/review/1",
			"comments_posted": 1, "verified": true, "fallback": "partial", "body_missing": true,
		})
	}))
	defer srv.Close()

	command := renderReviewPRBodyHealthNode(t, "publish_review", map[string]string{
		"vars.forge_publish_url":      srv.URL + "/api/v1/forge/publish-review",
		"vars.forge_publish_token":    "run-token",
		"input.pr_url":                "https://gitlab.example/acme/widgets/-/merge_requests/7",
		"input.findings":              `[{"file":"a.go","line":1,"title":"finding","severity":"high"}]`,
		"input.effective_review_mode": "mono",
		"vars.gate_enabled":           "false",
	})
	out, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	var res struct {
		Published   bool   `json:"published"`
		BodyMissing bool   `json:"body_missing"`
		Summary     string `json:"summary"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
	}
	if !res.Published || !res.BodyMissing {
		t.Errorf("the HTTP signal must reach publish_output: %s", out)
	}
	if !strings.Contains(res.Summary, "review body did not land") {
		t.Errorf("the publish summary must name the missing body, got %q", res.Summary)
	}
	if strings.Contains(res.Summary, "folded") {
		t.Errorf("a missing body cannot carry a claim that anchors were folded into it: %q", res.Summary)
	}
}

func TestReviewPRPublishHealthBodyMissing(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, tc := range []struct {
		name         string
		bodyMissing  string
		gatePosted   string
		gateSkipped  string
		wantDegraded bool
		wantParts    []string
	}{
		{name: "missing body is degraded", bodyMissing: "true", gatePosted: "true", wantDegraded: true, wantParts: []string{"REVIEW BODY MISSING", "review body did not land"}},
		{name: "present body stays healthy", bodyMissing: "false", gatePosted: "true", wantParts: []string{"forge publish OK"}},
		{name: "body warning does not hide a gate warning", bodyMissing: "true", gatePosted: "false", gateSkipped: "stale audited_sha", wantDegraded: true, wantParts: []string{"REVIEW BODY MISSING", "MERGE GATE NOT POSTED", "stale audited_sha"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := renderReviewPRBodyHealthNode(t, "publish_health", map[string]string{
				"input.total_findings": "2", "input.comments_posted": "1", "input.published": "true",
				"input.review_url": "https://forge/review/1", "input.body_missing": tc.bodyMissing,
				"input.gate_requested": "true", "input.gate_posted": tc.gatePosted, "input.gate_skipped_reason": tc.gateSkipped,
			})
			out, err := exec.Command("sh", "-c", command).Output()
			if err != nil {
				t.Fatalf("publish_health failed: %v (out %q)", err, out)
			}
			var res struct {
				Degraded bool   `json:"degraded"`
				Banner   string `json:"banner"`
			}
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("output is not publish_health_output JSON: %v (%q)", err, out)
			}
			if res.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, banner = %q; want degraded = %v", res.Degraded, res.Banner, tc.wantDegraded)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(res.Banner, part) {
					t.Errorf("banner = %q; want it to contain %q", res.Banner, part)
				}
			}
			if tc.bodyMissing == "true" && strings.Contains(res.Banner, "forge publish OK") {
				t.Errorf("a missing body must never read as a healthy publish: %q", res.Banner)
			}
		})
	}
}

func TestReviewPRPublishHealthMapsBodyMissing(t *testing.T) {
	u := unit.LoadDir("review-pr/main.bot")
	if u.Merged == nil {
		t.Fatalf("review-pr: the bundle did not parse: %v", u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("review-pr: the bundle did not compile: %v", cr.Diagnostics)
	}
	seen := false
	for _, edge := range cr.Workflow.Edges {
		if edge.From != "publish_review" || edge.To != "publish_health" {
			continue
		}
		seen = true
		got := map[string]string{}
		for _, mapping := range edge.With {
			got[mapping.Key] = strings.TrimSpace(mapping.Raw)
		}
		if want := "{{outputs.publish_review.body_missing}}"; got["body_missing"] != want {
			t.Errorf("publish_review -> publish_health maps body_missing to %q, want %q", got["body_missing"], want)
		}
	}
	if !seen {
		t.Fatal("no publish_review -> publish_health edge")
	}
}
