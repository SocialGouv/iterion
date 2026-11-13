package bots

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"sync"
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
// review body and says so with fallback "summary". Such a review carries every
// finding, with its file:line, on the pull request. Reading it as "0 inline
// comments landed" and advising a credential fix sends the operator after an
// auth problem that does not exist (SocialGouv/egapro #5149 and #5157:
// published=true, both findings in the body). fallback "partial" with zero
// inline comments is no fold: GitLab, which folds, reports "summary" when none
// landed, and on GitHub and Forgejo "partial" is the re-fetch listing fewer
// comments than were sent, the body as submitted. The three shapes are told
// apart: folded (healthy), published with no fold (degraded, no auth advice),
// not published (the FAILED banner and its auth advice, unchanged).
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
			wantParts: []string{"forge publish OK", "2 finding(s) folded into the review body", "https://forge/review/1"},
			denyParts: []string{"FAILED", "MISSING", "auth"},
		},
		{
			name: "partial with no inline comment is a shortfall, not a fold", published: "true", bodyMissing: "false", fallback: "partial",
			wantDegraded: true,
			wantParts:    []string{"INLINE COMMENTS MISSING", "the review landed", "fewer inline comments than were sent (fallback=partial)"},
			denyParts:    []string{"forge publish OK", "folded into the review body", "FORGE PUBLISH FAILED", "auth"},
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

// publishChainRequest is the request publish_review sent the endpoint.
type publishChainRequest struct {
	Summary  string            `json:"summary"`
	Mode     string            `json:"mode"`
	Comments []json.RawMessage `json:"comments"`
}

// publishChain is one run of publish_review -> publish_health: the endpoint's
// answer (status 0 is 200), converge's findings, the further publish_review
// refs (reviewer arrays, threshold, workspace anomaly, mode) and converge's
// own count as pr_gate passes it on ("" is the count of findings).
type publishChain struct {
	resp      map[string]any
	status    int
	findings  string
	refs      map[string]string
	gateTotal string
}

// runReviewPRPublishChain drives publish_review against an endpoint answering
// c.resp, resolves every mapping of the compiled publish_review ->
// publish_health edge against what it emitted and what pr_gate passes on, and
// runs publish_health on the result. It returns the request publish_review
// sent, its publish_output and the verdict.
func runReviewPRPublishChain(t *testing.T, c publishChain) (publishChainRequest, map[string]any, bool, string) {
	t.Helper()
	var mu sync.Mutex
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = body
		mu.Unlock()
		if c.status != 0 {
			w.WriteHeader(c.status)
		}
		_ = json.NewEncoder(w).Encode(c.resp)
	}))
	defer srv.Close()

	refs := map[string]string{
		"vars.forge_publish_url":      srv.URL + "/api/v1/forge/publish-review",
		"vars.forge_publish_token":    "run-token",
		"input.pr_url":                "https://github.com/acme/widgets/pull/7",
		"input.findings":              c.findings,
		"input.effective_review_mode": "mono",
		"vars.gate_enabled":           "false",
	}
	for k, v := range c.refs {
		refs[k] = v
	}
	out, err := exec.Command("sh", "-c", renderReviewPRBodyHealthNode(t, "publish_review", refs)).Output()
	if err != nil {
		t.Fatalf("publish_review failed: %v (out %q)", err, out)
	}
	var req publishChainRequest
	mu.Lock()
	sentBody := sent
	mu.Unlock()
	if err := json.Unmarshal(sentBody, &req); err != nil {
		t.Fatalf("publish_review sent no readable request: %v (%q)", err, sentBody)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	var published map[string]any
	if err := dec.Decode(&published); err != nil {
		t.Fatalf("output is not publish_output JSON: %v (%q)", err, out)
	}

	gateTotal := c.gateTotal
	if gateTotal == "" {
		var parsed []any
		if err := json.Unmarshal([]byte(c.findings), &parsed); err != nil {
			t.Fatalf("fixture findings are not a JSON array: %v", err)
		}
		gateTotal = fmt.Sprint(len(parsed))
	}
	u := unittest.Whole(unit.LoadDir("review-pr/main.bot"))
	if u.Merged == nil {
		t.Fatalf("review-pr: the bundle did not parse: %v", u.Diagnostics)
	}
	cr := ir.Compile(u.Merged)
	if cr.Workflow == nil {
		t.Fatalf("review-pr: the bundle did not compile: %v", cr.Diagnostics)
	}
	upstream := map[string]string{"outputs.pr_gate.total_findings": gateTotal}
	for k, v := range published {
		upstream["outputs.publish_review."+k] = fmt.Sprint(v)
	}
	healthRefs := map[string]string{}
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
			healthRefs["input."+mapping.Key] = value
		}
	}
	if !seen {
		t.Fatal("no publish_review -> publish_health edge")
	}
	degraded, banner := runReviewPRPublishHealth(t, healthRefs)
	return req, published, degraded, banner
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
	findings := `[{"file":"a.go","line":1,"title":"first finding","severity":"high"},{"file":"b.go","line":9,"title":"second finding","severity":"medium"}]`
	_, published, degraded, banner := runReviewPRPublishChain(t, publishChain{resp: map[string]any{
		"published": true, "provider": "github", "review_url": "https://forge/review/5149",
		"comments_posted": 0, "suggestions_posted": 0, "verified": true, "fallback": "summary",
	}, findings: findings})
	if got, ok := published["fallback"].(string); !ok || got != "summary" {
		t.Fatalf("publish_output must carry the endpoint's fallback structured, got %#v in %v", published["fallback"], published)
	}
	if degraded {
		t.Errorf("a review the forge published with both findings folded into its body must not read degraded: %q", banner)
	}
	if !strings.Contains(banner, "2 finding(s) folded into the review body") || !strings.Contains(banner, "https://forge/review/5149") {
		t.Errorf("the banner must say the findings were folded and link the review, got %q", banner)
	}
	if strings.Contains(banner, "auth") || strings.Contains(banner, "MISSING") {
		t.Errorf("a folded publish must not raise a missing-comment or auth alarm: %q", banner)
	}
}

// Only a fold that put the findings into the review body reads healthy, and
// every producer of one has to say so. The endpoint answers here are the
// shapes pkg/server's TestForgePublishReview_FallbackFromRealClients reads off
// the real GitHub and Forgejo clients: "partial" at zero inline comments is a
// re-fetch shortfall with the body as submitted (degraded); the server's own
// fold in summary mode says "summary" (healthy). publish_review's own fold --
// every finding without a diff anchor, written into the summary, no inline
// comment sent -- reaches an endpoint that saw nothing to fold, so the node
// has to report it itself; and it must not when an anchored comment was sent
// and lost, which the fold does not cover.
func TestReviewPRPublishChainReadsOnlyARealFold(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	const anchored = `[{"file":"a.go","line":1,"title":"FINDING-ONE","severity":"high"},{"file":"b.go","line":9,"title":"FINDING-TWO","severity":"medium"}]`
	const unanchored = `[{"file":"","line":0,"title":"FINDING-ONE","severity":"high"},{"file":"b.go","line":0,"title":"FINDING-TWO","severity":"medium"}]`
	const mixed = `[{"file":"a.go","line":1,"title":"FINDING-ONE","severity":"high"},{"file":"","line":0,"title":"FINDING-TWO","severity":"medium"}]`
	answer := func(fallback string) map[string]any {
		m := map[string]any{
			"published": true, "provider": "github", "review_url": "https://forge/review/7",
			"comments_posted": 0, "suggestions_posted": 0, "verified": true, "gate_posted": false,
		}
		if fallback != "" {
			m["fallback"] = fallback
		}
		return m
	}
	for _, tc := range []struct {
		name         string
		findings     string
		mode         string
		resp         map[string]any
		wantMode     string
		wantComments int
		wantFallback string
		wantDegraded bool
		wantBanner   []string
		denyBanner   []string
	}{
		{
			name: "partial at zero inline comments is a shortfall, not a fold", findings: anchored, resp: answer("partial"),
			wantMode: "inline", wantComments: 2, wantFallback: "partial", wantDegraded: true,
			wantBanner: []string{"INLINE COMMENTS MISSING", "fewer inline comments than were sent (fallback=partial)"},
			denyBanner: []string{"forge publish OK", "folded into the review body", "FORGE PUBLISH FAILED", "auth"},
		},
		{
			name: "every finding without an anchor is folded by publish_review itself", findings: unanchored, resp: answer(""),
			wantMode: "inline", wantComments: 0, wantFallback: "summary",
			wantBanner: []string{"forge publish OK", "2 finding(s) folded into the review body", "https://forge/review/7"},
			denyBanner: []string{"MISSING", "FAILED", "auth"},
		},
		{
			name: "an anchored comment sent and lost is not covered by the unanchored fold", findings: mixed, resp: answer(""),
			wantMode: "inline", wantComments: 1, wantFallback: "", wantDegraded: true,
			wantBanner: []string{"INLINE COMMENTS MISSING", "reason: none given"},
			denyBanner: []string{"forge publish OK", "auth"},
		},
		{
			name: "summary mode reads the server's own fold", findings: anchored, mode: "summary", resp: answer("summary"),
			wantMode: "summary", wantComments: 2, wantFallback: "summary",
			wantBanner: []string{"forge publish OK", "2 finding(s) folded into the review body"},
			denyBanner: []string{"MISSING", "FAILED", "auth"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := publishChain{resp: tc.resp, findings: tc.findings}
			if tc.mode != "" {
				chain.refs = map[string]string{"vars.pr_review_mode": tc.mode}
			}
			req, published, degraded, banner := runReviewPRPublishChain(t, chain)
			if req.Mode != tc.wantMode || len(req.Comments) != tc.wantComments {
				t.Fatalf("publish_review sent mode=%q with %d inline comment(s), want mode=%q with %d", req.Mode, len(req.Comments), tc.wantMode, tc.wantComments)
			}
			if tc.wantComments == 0 {
				for _, title := range []string{"FINDING-ONE", "FINDING-TWO"} {
					if !strings.Contains(req.Summary, title) {
						t.Fatalf("a finding without an anchor must be written into the summary it claims to fold into: %q missing from %q", title, req.Summary)
					}
				}
			}
			if got, _ := published["fallback"].(string); got != tc.wantFallback {
				t.Errorf("publish_output fallback = %q, want %q (output %v)", got, tc.wantFallback, published)
			}
			if tc.wantFallback == "partial" && strings.Contains(fmt.Sprint(published["summary"]), "folded into") {
				t.Errorf("a shortfall must not be described as a fold: %q", published["summary"])
			}
			if degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, banner = %q; want degraded = %v", degraded, banner, tc.wantDegraded)
			}
			for _, part := range tc.wantBanner {
				if !strings.Contains(banner, part) {
					t.Errorf("banner = %q; want it to contain %q", banner, part)
				}
			}
			for _, part := range tc.denyBanner {
				if strings.Contains(banner, part) {
					t.Errorf("banner = %q; must not contain %q", banner, part)
				}
			}
		})
	}
}

// publish_health judges a loss against what the review carries, not only
// against converge's own count (pr_gate.total_findings). The merge floor
// restores findings converge dropped -- converge answering [] with a count of
// 0 is the shape it exists for -- so a converge count of 0 over two restored
// findings that never landed read as "forge publish OK: 0 inline comment(s)".
// The banner counts the inline comments actually sent, and findings the
// anomaly strip moved out of the PR (the body lists them by title only) read
// under their own name, not as comments to re-post.
func TestReviewPRPublishHealthJudgesWhatTheReviewCarries(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	const reviewer = `[{"file":"a.go","line":1,"title":"FINDING-ONE","severity":"high","detail":"DETAIL-ONE"},{"file":"b.go","line":9,"title":"FINDING-TWO","severity":"medium","detail":"DETAIL-TWO"}]`
	const mixed = `[{"file":"a.go","line":1,"title":"FINDING-ONE","severity":"high"},{"file":"","line":0,"title":"FINDING-TWO","severity":"medium"}]`
	floor := map[string]string{"input.claude_findings": reviewer, "input.severity_threshold": "medium", "input.max_findings": "15"}
	answer := func(fallback string) map[string]any {
		m := map[string]any{
			"published": true, "provider": "github", "review_url": "https://forge/review/7",
			"comments_posted": 0, "suggestions_posted": 0, "verified": true, "gate_posted": false,
		}
		if fallback != "" {
			m["fallback"] = fallback
		}
		return m
	}
	for _, tc := range []struct {
		name         string
		chain        publishChain
		wantComments int
		wantInReview string
		wantDegraded bool
		wantBanner   []string
		denyBanner   []string
	}{
		{
			name:         "findings the merge floor restored and that never landed are a loss",
			chain:        publishChain{resp: answer("partial"), findings: `[]`, refs: floor, gateTotal: "0"},
			wantComments: 2, wantInReview: "2", wantDegraded: true,
			wantBanner: []string{"INLINE COMMENTS MISSING", "none of its 2 inline comment(s) did", "fallback=partial"},
			denyBanner: []string{"forge publish OK", "auth"},
		},
		{
			name: "findings the merge floor restored and whose publish failed are a failure",
			chain: publishChain{resp: map[string]any{"published": false, "skipped_reason": "create pull review: HTTP 502"},
				status: http.StatusBadGateway, findings: `[]`, refs: floor, gateTotal: "0"},
			wantComments: 2, wantInReview: "2", wantDegraded: true,
			wantBanner: []string{"FORGE PUBLISH FAILED", "2 finding(s) to post", "HTTP 502"},
			denyBanner: []string{"forge publish OK"},
		},
		{
			name:         "the banner counts the inline comments sent, and names the ones in the body",
			chain:        publishChain{resp: answer("partial"), findings: mixed},
			wantComments: 1, wantInReview: "2", wantDegraded: true,
			wantBanner: []string{"INLINE COMMENTS MISSING", "none of its 1 inline comment(s) did", "1 more finding(s) without a diff anchor are in the review body"},
			denyBanner: []string{"none of its 2", "forge publish OK", "auth"},
		},
		{
			name: "findings reclassified as workspace anomaly read as reclassified, not as comments to re-post",
			chain: publishChain{resp: answer(""), findings: reviewer,
				refs: map[string]string{"input.workspace_anomaly": " M a.go\n M b.go"}},
			wantComments: 0, wantInReview: "0", wantDegraded: true,
			wantBanner: []string{"FINDINGS RECLASSIFIED", "all 2 finding(s)", "by title only"},
			denyBanner: []string{"INLINE COMMENTS MISSING", "re-post", "forge publish OK", "auth"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, published, degraded, banner := runReviewPRPublishChain(t, tc.chain)
			if len(req.Comments) != tc.wantComments {
				t.Fatalf("publish_review sent %d inline comment(s), want %d (summary %q)", len(req.Comments), tc.wantComments, req.Summary)
			}
			if got := fmt.Sprint(published["findings_in_review"]); got != tc.wantInReview {
				t.Errorf("publish_output findings_in_review = %s, want %s (output %v)", got, tc.wantInReview, published)
			}
			if got := fmt.Sprint(published["comments_sent"]); got != fmt.Sprint(tc.wantComments) {
				t.Errorf("publish_output comments_sent = %s, want %d", got, tc.wantComments)
			}
			if tc.chain.refs["input.workspace_anomaly"] != "" {
				// The verdict rests on what the body carries for them: the
				// titles, never the detail a fold would have published.
				if !strings.Contains(req.Summary, "FINDING-ONE") || strings.Contains(req.Summary, "DETAIL-ONE") {
					t.Fatalf("the anomaly section must list the reclassified finding by title only: %q", req.Summary)
				}
				if got := fmt.Sprint(published["findings_reclassified"]); got != "2" {
					t.Errorf("publish_output findings_reclassified = %s, want 2", got)
				}
			}
			if degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, banner = %q; want degraded = %v", degraded, banner, tc.wantDegraded)
			}
			for _, part := range tc.wantBanner {
				if !strings.Contains(banner, part) {
					t.Errorf("banner = %q; want it to contain %q", banner, part)
				}
			}
			for _, part := range tc.denyBanner {
				if strings.Contains(banner, part) {
					t.Errorf("banner = %q; must not contain %q", banner, part)
				}
			}
		})
	}
}
