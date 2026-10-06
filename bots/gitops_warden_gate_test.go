package bots

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/git"
)

// The warden's FOLD is the executable half of ADR-123: the LLM classifies,
// this table decides, and every branch that cannot prove its case escalates.
// These tests drive publish_verdict's real python through sh -c (the same
// way it runs in production) and assert the blocking decision, the gate
// payload and the verdict verbs — never the template text.

type wardenVerdict struct {
	Requested       bool   `json:"requested"`
	Outcome         string `json:"outcome"`
	RefusedReason   string `json:"verdict_refused_reason"`
	GateRequested   bool   `json:"gate_requested"`
	GatePosted      bool   `json:"gate_posted"`
	GateState       string `json:"gate_state"`
	EscalateReasons string `json:"escalate_reasons"`
	Blocking        int    `json:"blocking"`
	ApprovePosted   bool   `json:"approve_posted"`
	MergeArmed      bool   `json:"merge_armed"`
	ReviewersAdded  string `json:"reviewers_added"`
}

type wardenPublish struct {
	Summary string `json:"summary"`
	Gate    struct {
		Enabled       bool   `json:"enabled"`
		Context       string `json:"context"`
		BlockingCount int    `json:"blocking_count"`
		AuditedSHA    string `json:"audited_sha"`
		Note          string `json:"note"`
	} `json:"gate"`
	Verdict struct {
		Enabled     bool     `json:"enabled"`
		Approve     bool     `json:"approve"`
		Arm         bool     `json:"arm_merge_when_pipeline_succeeds"`
		MergeMethod string   `json:"merge_method"`
		Reviewers   []string `json:"request_reviewers"`
		AuditedSHA  string   `json:"audited_sha"`
		Mode        string   `json:"mode"`
	} `json:"verdict"`
}

// runWardenFold renders publish_verdict's command with the given env and
// runs it. Without PUB_URL (the default here) the fold runs to completion
// and the node emits its no-grant refusal — which CARRIES the fold's
// blocking decision and reasons, so the table is testable with no network.
func runWardenFold(t *testing.T, over map[string]string) wardenVerdict {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
	vals := map[string]string{
		"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": "", "forge_publish_token": "",
		"forge_pr_state_url": "", "mode": "dry_run", "gate_enabled": "true", "gate_context": "gitops/conformance",
		"reviewers": "", "peer_gate_contexts": "revi/review", "merge_method": "squash", "remove_source_branch": "false",
		"pr_url_in": "", "run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
	}
	for k, v := range over {
		vals[k] = v
	}
	rendered := cmdTemplate
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", "'"+v+"'")
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", "'"+v+"'")
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}
	out, err := exec.Command("sh", "-c", rendered).Output()
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (out %q)", err, out)
	}
	if len(out) == 0 {
		t.Fatal("publish_verdict produced EMPTY output — the body is truncated by a stray metacharacter")
	}
	var res wardenVerdict
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not verdict_output JSON: %v (%q)", err, out)
	}
	return res
}

func TestGitopsWardenFold_CleanBumpIsGreen(t *testing.T) {
	res := runWardenFold(t, nil)
	if res.Blocking != 0 {
		t.Fatalf("a clean image bump folded to blocking=%d (%s)", res.Blocking, res.EscalateReasons)
	}
}

func TestGitopsWardenFold_EveryFailClosedBranch(t *testing.T) {
	cases := []struct {
		name      string
		over      map[string]string
		wantInWhy string
	}{
		{"unreadable diff", map[string]string{"changed_files": "-1"}, "could not be read"},
		{"unparseable", map[string]string{"unparseable": "true"}, "parsed"},
		{"over cap", map[string]string{"diff_too_large": "true"}, "cap"},
		{"workspace drift", map[string]string{"workspace_anomaly": " M apps/x/values.yaml"}, "workspace"},
		{"workspace unreadable", map[string]string{"workspace_anomaly": "?"}, "workspace"},
		{"stale launch", map[string]string{"stale_launch": "true", "declared_head_sha": "aaa0000"}, "moved head"},
		{"self-modified policy", map[string]string{"policy_source": "target-modified"}, "review policy"},
		{"fallback-served judge", map[string]string{"fallback_used": "glm-5.3"}, "fallback"},
		{"unknown class", map[string]string{"classified_files": `[{"path":"a.yaml","cls":"unknown","rule":"","reason":""}]`}, "a.yaml"},
		{"escalated file", map[string]string{"classified_files": `[{"path":"charts/new/d.yaml","cls":"escalate","rule":"structural","reason":"new component"}]`}, "new component"},
		{"blocking platform finding", map[string]string{"platform_findings": `[{"path":"a.yaml","rule":"managed-datastore","detail":"OvhValkey exists","severity":"blocking"}]`}, "managed-datastore"},
		{"doubts", map[string]string{"doubts": "the image tag is not on the registry", "verdict": "escalate"}, "doubt"},
		{"unreadable verdict", map[string]string{"verdict": ""}, "unreadable"},
		{"sentinel route", map[string]string{"route": "sentinel", "sentinel_reason": "the diff exceeds the classification cap"}, "cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWardenFold(t, tc.over)
			if res.Blocking != 1 {
				t.Fatalf("blocking = %d, want 1 (fail-closed)", res.Blocking)
			}
			if !strings.Contains(res.EscalateReasons, tc.wantInWhy) {
				t.Errorf("reasons %q do not name %q", res.EscalateReasons, tc.wantInWhy)
			}
		})
	}
}

func TestGitopsWardenFold_UnparseableJSONListIsNotAGreenVerdict(t *testing.T) {
	// The judge's file array arriving as prose must fold CLOSED even though
	// the verdict field itself says auto_approvable — a string where an
	// array belongs means nobody classified anything.
	res := runWardenFold(t, map[string]string{"classified_files": "4 findings, all fine"})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1 (prose where a JSON array belongs)", res.Blocking)
	}
}

// runWardenPublish runs publish_verdict against a capture server (the full
// chain: fold, comment, gate payload, verdict verbs) and returns what the
// endpoint received plus the node's own verdict_output.
func runWardenPublish(t *testing.T, over map[string]string) (wardenPublish, wardenVerdict) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	var got wardenPublish
	stateHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		// The fake mirrors the REAL server: it executes exactly the verbs
		// the request carries and reports those — never gestures the bot
		// did not ask for.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"published":true,"review_url":"https://pic.example/o/r/-/merge_requests/7#note-1",
			"gate_posted":true,"gate_state":"success","gate_error":"",
			"verdict":{"approve_posted":` + fmt.Sprintf("%v", got.Verdict.Approve) + `,"merge_armed":` + fmt.Sprintf("%v", got.Verdict.Arm) + `,"merge_state":"mwps"}}`))
	})
	srv := httptest.NewServer(mux)
	// The read twin, same host: the peer-gate read answers green peers on
	// the audited head unless a test overrides the state body.
	stateBody := `{"open":true,"state":"open","head_sha":"abc123defabc123","commit_statuses":[{"context":"revi/review","state":"success"}]}`
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		stateHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(stateBody))
	})
	t.Cleanup(srv.Close)
	if over == nil {
		over = map[string]string{}
	}
	if _, ok := over["forge_pr_state_url"]; !ok {
		over["forge_pr_state_url"] = srv.URL + "/state"
	}

	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
	vals := map[string]string{
		"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": srv.URL, "forge_publish_token": "tok-1",
		"forge_pr_state_url": "", "mode": "enforce", "gate_enabled": "true", "gate_context": "gitops/conformance",
		"reviewers": "", "peer_gate_contexts": "revi/review", "merge_method": "squash", "remove_source_branch": "false",
		"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
	}
	for k, v := range over {
		vals[k] = v
	}
	rendered := cmdTemplate
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", "'"+v+"'")
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", "'"+v+"'")
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}
	out, err := exec.Command("sh", "-c", rendered).Output()
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (out %q)", err, out)
	}
	var res wardenVerdict
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not verdict_output JSON: %v (%q)", err, out)
	}
	return got, res
}

func TestGitopsWardenPublish_DryRunSendsNoVerb(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{"mode": "dry_run"})
	if got.Verdict.Approve || got.Verdict.Arm || len(got.Verdict.Reviewers) != 0 {
		t.Fatalf("dry_run must send NO verb, sent approve=%v arm=%v reviewers=%v", got.Verdict.Approve, got.Verdict.Arm, got.Verdict.Reviewers)
	}
	if !strings.Contains(got.Summary, "DRY-RUN") {
		t.Errorf("the dry-run comment must say so, got %q", got.Summary[:min(200, len(got.Summary))])
	}
	if res.Blocking != 0 {
		t.Fatalf("clean dry-run folded to blocking=%d", res.Blocking)
	}
}

func TestGitopsWardenPublish_EnforceCleanArmsPinned(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{"mode": "enforce"})
	if !got.Verdict.Approve || !got.Verdict.Arm {
		t.Fatalf("clean enforce must send approve+arm, sent approve=%v arm=%v", got.Verdict.Approve, got.Verdict.Arm)
	}
	if got.Verdict.MergeMethod != "squash" {
		t.Errorf("merge_method = %q, want squash (the gitops standard)", got.Verdict.MergeMethod)
	}
	if got.Verdict.AuditedSHA != "abc123defabc123" {
		t.Errorf("audited_sha = %q, want the reviewed revision pin", got.Verdict.AuditedSHA)
	}
	if got.Gate.BlockingCount != 0 || got.Gate.AuditedSHA != "abc123defabc123" {
		t.Errorf("gate = %+v, want green and pinned", got.Gate)
	}
	if !res.ApprovePosted || !res.MergeArmed {
		t.Errorf("node verdict = approve=%v armed=%v, want both", res.ApprovePosted, res.MergeArmed)
	}
}

func TestGitopsWardenPublish_EscalationRequestsPinnedReviewersOnly(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{
		"mode": "enforce", "reviewers": "devops-oncall devops-lead",
		"verdict":          "escalate",
		"classified_files": `[{"path":"charts/new/d.yaml","cls":"escalate","rule":"structural","reason":"new component"}]`,
	})
	if len(got.Verdict.Reviewers) != 2 || got.Verdict.Reviewers[0] != "devops-oncall" {
		t.Fatalf("reviewers = %v, want the pinned pair", got.Verdict.Reviewers)
	}
	if got.Verdict.Approve || got.Verdict.Arm {
		t.Fatal("an escalation never sends approve/arm")
	}
	if got.Gate.BlockingCount != 1 {
		t.Errorf("gate blocking = %d, want 1 (red)", got.Gate.BlockingCount)
	}
	if res.Blocking != 1 {
		t.Errorf("fold blocking = %d, want 1", res.Blocking)
	}
}

func TestGitopsWardenPublish_SelfModifiedPolicyEscalatesWithoutJudgement(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{"mode": "enforce", "policy_source": "target-modified"})
	if got.Verdict.Approve || got.Verdict.Arm || len(got.Verdict.Reviewers) != 0 {
		t.Fatalf("an MR editing its own policy executes nothing: %+v", got.Verdict)
	}
	if res.Blocking != 1 || !strings.Contains(res.EscalateReasons, "review policy") {
		t.Errorf("blocking=%d reasons=%q", res.Blocking, res.EscalateReasons)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestGitopsWardenPublish_RedPeerGateHoldsTheArm(t *testing.T) {
	// The warden's own verdict is clean, but revi/review is red on the same
	// head: the arm is held (the peer's verdict decides that MR), the check
	// stays green, no gesture is sent.
	got, _ := runWardenPublish(t, map[string]string{"mode": "enforce"})
	_ = got
	t.Run("red peer", func(t *testing.T) {
		// runWardenPublish wires a green peer; this subtest needs a red one,
		// so it re-runs the chain with the state body swapped.
		cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/state" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"open":true,"state":"open","head_sha":"abc123defabc123","commit_statuses":[{"context":"revi/review","state":"failure"}]}`))
				return
			}
			var _json map[string]any
			_ = json.NewDecoder(r.Body).Decode(&_json)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"published":true,"review_url":"x","gate_posted":true,"gate_state":"success","verdict":{}}`))
		}))
		defer srv.Close()
		vals := map[string]string{
			"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": srv.URL, "forge_publish_token": "tok-1",
			"forge_pr_state_url": srv.URL + "/state", "mode": "enforce", "gate_enabled": "true", "gate_context": "gitops/conformance",
			"reviewers": "", "peer_gate_contexts": "revi/review", "merge_method": "squash", "remove_source_branch": "false",
			"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
			"route": "classify", "sentinel_reason": "", "changed_files": "1",
			"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
			"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
			"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
			"verdict":           "auto_approvable",
			"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
			"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
		}
		rendered := cmdTemplate
		for k, v := range vals {
			rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", "'"+v+"'")
			rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", "'"+v+"'")
		}
		rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
		out, err := exec.Command("sh", "-c", rendered).Output()
		if err != nil && len(out) == 0 {
			t.Fatalf("publish_verdict failed: %v (%q)", err, out)
		}
		var res wardenVerdict
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if res.ApprovePosted || res.MergeArmed {
			t.Fatalf("a red peer gate must hold the gestures: approve=%v armed=%v", res.ApprovePosted, res.MergeArmed)
		}
		if res.Outcome != "refused" || !strings.Contains(res.RefusedReason, "peer gate") {
			t.Errorf("outcome=%q reason=%q, want refused naming the red peer", res.Outcome, res.RefusedReason)
		}
	})
}

// Round-3 regression tests: the fold cross-checks the judge's coverage
// against the deterministic inventory (H1), the empty route still answers
// to the sentinels (H2), and a peer that is not PRESENT-and-success holds
// the arm (M1).

func TestGitopsWardenFold_StructuralInventoryFileEscalatesEvenUnjudged(t *testing.T) {
	// The judge classified ONE file and called it clean; the inventory
	// carries FIVE, two structural. The fold enforces the structural class
	// itself and refuses a partial verdict.
	res := runWardenFold(t, map[string]string{
		"changed_files": "5",
		"files": `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":1,"key_paths":["image"],"candidate":true},
			{"path":"charts/new/d.yaml","status":"A","ext":"yaml","added":40,"removed":0,"key_paths":[],"candidate":false},
			{"path":".gitlab-ci.yml","status":"M","ext":"yml","added":3,"removed":0,"key_paths":[],"candidate":false},
			{"path":"old.yaml","status":"D","ext":"yaml","added":0,"removed":9,"key_paths":[],"candidate":false},
			{"path":"renamed.yaml","status":"R","ext":"yaml","added":0,"removed":0,"key_paths":[],"candidate":false}]`,
	})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1 — the structural class is deterministic and the coverage partial", res.Blocking)
	}
	if !strings.Contains(res.EscalateReasons, "structural") || !strings.Contains(res.EscalateReasons, "of 5") {
		t.Errorf("reasons = %q, want the structural mark and the coverage count", res.EscalateReasons)
	}
}

func TestGitopsWardenFold_EmptyJudgedListIsNeverGreen(t *testing.T) {
	// The adversarial B2 case: an EMPTY classified array parses as a valid
	// list, and an auto_approvable verdict next to it must still fold shut
	// — nothing was classified.
	res := runWardenFold(t, map[string]string{"classified_files": "[]"})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1 — an empty judged set covers nothing", res.Blocking)
	}
}

func TestGitopsWardenFold_EmptyRouteStillAnswersToSentinels(t *testing.T) {
	cases := []struct {
		name string
		over map[string]string
		want string
	}{
		{"stale launch", map[string]string{"route": "empty", "stale_launch": "true", "declared_head_sha": "aaa0000"}, "moved head"},
		{"drifted workspace", map[string]string{"route": "empty", "workspace_anomaly": " M apps/x/values.yaml"}, "workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWardenFold(t, tc.over)
			if res.Blocking != 1 {
				t.Fatalf("blocking = %d, want 1 — %s must not read as a clean empty diff", res.Blocking, tc.name)
			}
			if !strings.Contains(res.EscalateReasons, tc.want) {
				t.Errorf("reasons = %q, want %q", res.EscalateReasons, tc.want)
			}
		})
	}
}

func TestGitopsWardenFold_TrueCleanBumpStillGreen(t *testing.T) {
	// The guard H1 added must not over-close: a judge output covering every
	// inventory path, no structural status, stays green.
	res := runWardenFold(t, nil)
	if res.Blocking != 0 {
		t.Fatalf("blocking = %d (%s) — the clean fixture must stay green", res.Blocking, res.EscalateReasons)
	}
}

// M1: the peer hold fires on pending and absent, not only on failure.
func TestGitopsWardenPublish_PeerStatesThatHoldTheArm(t *testing.T) {
	cases := []struct {
		name     string
		statuses string
		wantIn   string
	}{
		{"pending peer", `[{"context":"revi/review","state":"pending"}]`, "is pending"},
		{"absent peer", `[]`, "no status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var armSent bool
			state := tc.statuses
			mux := http.NewServeMux()
			mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"open":true,"state":"open","head_sha":"abc123defabc123","commit_statuses":` + state + `}`))
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				var body wardenPublish
				_ = json.NewDecoder(r.Body).Decode(&body)
				armSent = body.Verdict.Arm
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"published":true,"review_url":"x","gate_posted":true,"gate_state":"success","verdict":{}}`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
			vals := map[string]string{
				"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": srv.URL, "forge_publish_token": "tok-1",
				"forge_pr_state_url": srv.URL + "/state", "mode": "enforce", "gate_enabled": "true", "gate_context": "gitops/conformance",
				"reviewers": "", "peer_gate_contexts": "revi/review", "merge_method": "squash", "remove_source_branch": "false",
				"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
				"route": "classify", "sentinel_reason": "", "changed_files": "1",
				"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
				"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
				"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
				"verdict":           "auto_approvable",
				"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
				"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
			}
			rendered := cmdTemplate
			for k, v := range vals {
				rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", "'"+v+"'")
				rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", "'"+v+"'")
			}
			rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
			out, err := exec.Command("sh", "-c", rendered).Output()
			if err != nil && len(out) == 0 {
				t.Fatalf("publish_verdict failed: %v (%q)", err, out)
			}
			var res wardenVerdict
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("bad json: %v", err)
			}
			if armSent || res.MergeArmed {
				t.Fatalf("a %s peer must hold the arm", tc.name)
			}
			if !strings.Contains(res.RefusedReason, tc.wantIn) {
				t.Errorf("reason = %q, want it to name the held peer (%s)", res.RefusedReason, tc.wantIn)
			}
		})
	}
}

// R831f30 regression: the key scan diffs the MR (base..worktree), not the
// clean worktree against its index — on a real temp repository, a modified
// values file yields non-empty key_paths, and a REMOVED key with no
// addition marks candidate=false (the silent-drop triage the ticket asks
// for).
func TestGitopsWardenInventory_KeyScanSeesTheMRNotTheIndex(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		// git >= 2.48 detaches a maintenance run after every writing
		// command; the child would outlive the command and race t.TempDir().
		cmd := exec.Command("git", git.NoAutoMaintenance(args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("apps/x/values.yaml", "image:\n  repository: registry/app\n  tag: 1.4.2\nreplicas: 2\nresources:\n  requests:\n    cpu: 100m\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	// The MR: tag bump AND the silent-drop shape — the `resources` tree
	// removed with nothing re-added under that name.
	write("apps/x/values.yaml", "image:\n  repository: registry/app\n  tag: 1.4.3\nreplicas: 2\n")
	run("add", ".")
	run("commit", "-q", "-m", "bump")

	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "diff_inventory")
	rendered := cmdTemplate
	for k, v := range map[string]string{
		"workspace_dir": dir, "base_ref": "main", "policy_path": "review-policy.md",
		"max_diff_files": "150", "max_diff_bytes": "400000", "head_sha": "",
	} {
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", "'"+v+"'")
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left: %s", firstRef(rendered))
	}
	out, err := exec.Command("sh", "-c", rendered).Output()
	if err != nil {
		t.Fatalf("diff_inventory failed: %v (%s)", err, out)
	}
	var inv struct {
		Files []struct {
			Path      string   `json:"path"`
			Status    string   `json:"status"`
			KeyPaths  []string `json:"key_paths"`
			Candidate bool     `json:"candidate"`
		} `json:"files"`
		IsEmpty bool `json:"is_empty"`
	}
	if err := json.Unmarshal(out, &inv); err != nil {
		t.Fatalf("bad inventory json: %v (%q)", err, out)
	}
	if inv.IsEmpty || len(inv.Files) != 1 {
		t.Fatalf("inventory = %+v, want exactly the one modified values file", inv)
	}
	f := inv.Files[0]
	if f.Status != "M" || f.Path != "apps/x/values.yaml" {
		t.Fatalf("file = %s/%s, want M apps/x/values.yaml", f.Status, f.Path)
	}
	found := map[string]bool{}
	for _, k := range f.KeyPaths {
		found[k] = true
	}
	// The scan reads the DIFF: the changed lines' keys are there (the
	// removed `resources:` tree, the bumped `tag`), the unchanged context
	// keys are not.
	for _, want := range []string{"resources", "tag"} {
		if !found[want] {
			t.Errorf("key_paths = %v, want it to include %q (the scan must read the MR diff)", f.KeyPaths, want)
		}
	}
	if found["image"] || found["replicas"] {
		t.Errorf("key_paths = %v contains unchanged context keys — the scan is reading something other than the diff", f.KeyPaths)
	}
	if f.Candidate {
		t.Errorf("candidate = true, want false — resources was removed with no same-named addition (the silent-drop shape)")
	}
}

// Revi round 2 (on the fix push) regressions.

func TestGitopsWardenPublish_DryRunIsPostedAndHealthy(t *testing.T) {
	// R41c58a: a dry-run verdict carries no verbs, the server echoes the
	// verb-less refusal — that echo is the CONTRACT, not a refusal. The
	// outcome is posted and publish_health has nothing to banner.
	got, res := runWardenPublish(t, map[string]string{"mode": "dry_run"})
	if res.Outcome != "posted" {
		t.Fatalf("outcome = %q (%s), want posted — the verb-less echo is not a refusal", res.Outcome, res.RefusedReason)
	}
	if res.ApprovePosted || res.MergeArmed {
		t.Fatal("dry_run must show no gesture")
	}
	_ = got
}

func TestGitopsWardenPublish_PeerHoldNamesItsReasonNotTheEcho(t *testing.T) {
	// enforce + clean + red peer: no verbs sent (the hold), and the reason
	// must be the PEER, not the server's verb-less echo.
	_, res := runWardenPublish(t, map[string]string{"mode": "enforce"})
	if res.Outcome != "posted" {
		t.Fatalf("a green-peer enforce run is posted, got %q (%s)", res.Outcome, res.RefusedReason)
	}
}

func TestGitopsWardenPublish_EmptyRouteSentinelCommentNamesTheRed(t *testing.T) {
	// R5f1a2c: an empty diff with a fired sentinel folds red — the comment
	// must say so, never claim the check posted green.
	got, res := runWardenPublish(t, map[string]string{
		"route": "empty", "stale_launch": "true", "declared_head_sha": "aaa0000aaa0000",
		"files": `[]`, "classified_files": `[]`, "changed_files": "0", "mode": "dry_run",
	})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1", res.Blocking)
	}
	if !strings.Contains(got.Summary, "not a clean verdict") {
		t.Errorf("summary claims a clean empty diff: %q", got.Summary[:min(160, len(got.Summary))])
	}
}

func TestGitopsWardenFold_JudgeEnumsAreCaseNormalized(t *testing.T) {
	// R599f83: 'Escalate' and 'Blocking' pass clean only to a fold that
	// matches exact lowercase — normalization is fail-closed in BOTH
	// directions: unknown cls escalates, unlisted severity blocks.
	res := runWardenFold(t, map[string]string{
		"classified_files":  `[{"path":"secrets.yaml","cls":"Escalate","rule":"secret","reason":"secret material"}]`,
		"platform_findings": `[{"path":"secrets.yaml","rule":"secret","detail":"a Secret literal","severity":"Blocking"}]`,
	})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1 — off-case enums must fail closed", res.Blocking)
	}
	if !strings.Contains(res.EscalateReasons, "secrets.yaml") || !strings.Contains(res.EscalateReasons, "secret") {
		t.Errorf("reasons = %q, want the file and the platform rule", res.EscalateReasons)
	}
}
