package bots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
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
	u := unittest.Whole(unit.LoadDir("review-pr/main.bot"))
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

// runReviewPRPublishHealth renders publish_health with refs (unset refs read as
// the empty string, as an unmapped input does) and runs it the way production
// does: substituted into the command and executed by sh -c.
func runReviewPRPublishHealth(t *testing.T, refs map[string]string) (bool, string) {
	t.Helper()
	out, err := exec.Command("sh", "-c", renderReviewPRBodyHealthNode(t, "publish_health", refs)).Output()
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
	return res.Degraded, res.Banner
}

// The forge refuses inline anchors routinely (a line outside the diff hunk, a
// stale position), and the forge layer then folds the refused comments into the
// review body and says so: fallback "summary" (all of them) or "partial" (the
// rest). Such a review carries every finding, with its file:line, on the pull
// request. Reading it as "0 inline comments landed" and advising a credential
// fix sends the operator after an auth problem that does not exist
// (SocialGouv/egapro #5149 and #5157: published=true, both findings in the
// body). The three shapes are told apart: folded (healthy), published with no
// reported fold (degraded, no auth advice), not published (the FAILED banner
// and its auth advice, unchanged).
func TestReviewPRPublishHealthFoldedFindings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, tc := range []struct {
		name         string
		published    string
		bodyMissing  string
		fallback     string
		skipped      string
		wantDegraded bool
		wantParts    []string
		denyParts    []string
	}{
		{
			name: "published with every anchor folded into the body is healthy", published: "true", bodyMissing: "false", fallback: "summary",
			wantParts: []string{"forge publish OK", "2 finding(s) folded into the review body", "refused their inline anchors", "https://forge/review/1"},
			denyParts: []string{"FAILED", "MISSING", "auth"},
		},
		{
			name: "published with the rest folded is healthy", published: "true", bodyMissing: "false", fallback: "partial",
			wantParts: []string{"forge publish OK", "folded into the review body"},
			denyParts: []string{"FAILED", "MISSING", "auth"},
		},
		{
			name: "published with no reported fold is degraded without auth advice", published: "true", bodyMissing: "false", fallback: "",
			wantDegraded: true,
			wantParts:    []string{"INLINE COMMENTS MISSING", "the review landed", "2 inline comment(s)", "nothing reports them folded"},
			denyParts:    []string{"FORGE PUBLISH FAILED", "auth", "forge publish OK"},
		},
		{
			name: "an unknown fallback value is not a fold", published: "true", bodyMissing: "false", fallback: "dropped",
			wantDegraded: true,
			wantParts:    []string{"INLINE COMMENTS MISSING"},
			denyParts:    []string{"forge publish OK"},
		},
		{
			name: "not published keeps the failure banner and its auth advice", published: "false", bodyMissing: "false", fallback: "", skipped: "forge publish endpoint HTTP 502: bad gateway",
			wantDegraded: true,
			wantParts:    []string{"FORGE PUBLISH FAILED", "0 inline comments landed", "HTTP 502", "fix the forge CLI auth"},
			denyParts:    []string{"forge publish OK", "INLINE COMMENTS MISSING"},
		},
		{
			name: "a fold claim on a review that did not publish is still a failure", published: "false", bodyMissing: "false", fallback: "summary",
			wantDegraded: true,
			wantParts:    []string{"FORGE PUBLISH FAILED"},
			denyParts:    []string{"forge publish OK", "folded into"},
		},
		{
			name: "a fold into a body that did not land stays degraded", published: "true", bodyMissing: "true", fallback: "summary",
			wantDegraded: true,
			wantParts:    []string{"REVIEW BODY MISSING"},
			denyParts:    []string{"forge publish OK", "folded into"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			degraded, banner := runReviewPRPublishHealth(t, map[string]string{
				"input.total_findings": "2", "input.comments_posted": "0", "input.published": tc.published,
				"input.body_missing": tc.bodyMissing, "input.fallback": tc.fallback,
				"input.review_url": "https://forge/review/1", "input.skipped_reason": tc.skipped,
				"input.gate_requested": "true", "input.gate_posted": "true",
			})
			if degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, banner = %q; want degraded = %v", degraded, banner, tc.wantDegraded)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(banner, part) {
					t.Errorf("banner = %q; want it to contain %q", banner, part)
				}
			}
			for _, part := range tc.denyParts {
				if strings.Contains(banner, part) {
					t.Errorf("banner = %q; must not contain %q", banner, part)
				}
			}
		})
	}
}

// The fold has to travel from the publish endpoint's answer to publish_health
// through the REAL edge: a fallback that publish_review reads but does not
// emit, or that the publish_review -> publish_health edge does not map, leaves
// publish_health reading an empty fallback and raising a false alarm. So this
// drives publish_review against an endpoint answering a summary fold, resolves
// every mapping of the compiled edge against what it emitted, and runs
// publish_health on the result.
func TestReviewPRPublishReviewPropagatesFallbackToHealth(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"published": true, "provider": "github", "review_url": "https://forge/review/5149",
			"comments_posted": 0, "suggestions_posted": 0, "verified": true, "fallback": "summary",
		})
	}))
	defer srv.Close()

	findings := `[{"file":"a.go","line":1,"title":"first finding","severity":"high"},{"file":"b.go","line":9,"title":"second finding","severity":"medium"}]`
	command := renderReviewPRBodyHealthNode(t, "publish_review", map[string]string{
		"vars.forge_publish_url":      srv.URL + "/api/v1/forge/publish-review",
		"vars.forge_publish_token":    "run-token",
		"input.pr_url":                "https://github.com/acme/widgets/pull/7",
		"input.findings":              findings,
		"input.effective_review_mode": "mono",
		"vars.gate_enabled":           "false",
	})
	out, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	var published map[string]any
	if err := dec.Decode(&published); err != nil {
		t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
	}
	if got, ok := published["fallback"].(string); !ok || got != "summary" {
		t.Fatalf("publish_output must carry the endpoint's fallback structured, got %#v in %s", published["fallback"], out)
	}

	u := unittest.Whole(unit.LoadDir("review-pr/main.bot"))
	if u.Merged == nil {
		t.Fatalf("review-pr: the bundle did not parse: %v", u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("review-pr: the bundle did not compile: %v", cr.Diagnostics)
	}
	upstream := map[string]string{"outputs.pr_gate.total_findings": "2"}
	for k, v := range published {
		upstream["outputs.publish_review."+k] = fmt.Sprint(v)
	}
	refs := map[string]string{}
	seen := false
	for _, edge := range cr.Workflow.Edges {
		if edge.From != "publish_review" || edge.To != "publish_health" {
			continue
		}
		seen = true
		for _, mapping := range edge.With {
			raw := strings.TrimSpace(mapping.Raw)
			if !strings.HasPrefix(raw, "{{") || !strings.HasSuffix(raw, "}}") {
				t.Fatalf("mapping %s = %q is not a single reference this test can resolve", mapping.Key, raw)
			}
			value, ok := upstream[strings.TrimSpace(raw[2:len(raw)-2])]
			if !ok {
				t.Fatalf("mapping %s = %q names an output neither publish_review nor the fixture provides", mapping.Key, raw)
			}
			refs["input."+mapping.Key] = value
		}
	}
	if !seen {
		t.Fatal("no publish_review -> publish_health edge")
	}

	degraded, banner := runReviewPRPublishHealth(t, refs)
	if degraded {
		t.Errorf("a review the forge published with both findings folded into its body must not read degraded: %q (inputs %v)", banner, refs)
	}
	if !strings.Contains(banner, "2 finding(s) folded into the review body") || !strings.Contains(banner, "https://forge/review/5149") {
		t.Errorf("the banner must say the findings were folded and link the review, got %q", banner)
	}
	if strings.Contains(banner, "auth") || strings.Contains(banner, "MISSING") {
		t.Errorf("a folded publish must not raise a missing-comment or auth alarm: %q", banner)
	}
}
