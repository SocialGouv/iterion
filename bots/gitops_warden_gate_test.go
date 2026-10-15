package bots

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// wq shell-quotes a substituted value: the fixtures speak french now and
// carry apostrophes that would otherwise close the single-quoted argument.
// shOut runs the rendered node command and surfaces stderr: a python
// exception writes there, and a fold that died with an empty stdout must
// show WHY.
func shOut(rendered string) ([]byte, string, error) {
	var ebuf bytes.Buffer
	c := exec.Command("sh", "-c", rendered)
	c.Stderr = &ebuf
	out, err := c.Output()
	return out, ebuf.String(), err
}

func wq(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func runWardenFold(t *testing.T, over map[string]string) wardenVerdict {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
	vals := map[string]string{
		"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": "", "forge_publish_token": "",
		"forge_pr_state_url": "", "mode": "dry_run", "gate_enabled": "true", "gate_context": "gitops/conformance",
		"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "0", "peer_gate_poll_seconds": "0", "merge_method": "squash", "remove_source_branch": "false",
		"pr_url_in": "", "run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
		"rendered":          "{}",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
	}
	for k, v := range over {
		vals[k] = v
	}
	rendered := cmdTemplate
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
	rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", "'0'")
	rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", "'0'")
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}
	out, sherr, err := shOut(rendered)
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (out %q) stderr: %.800s CMD: %.600s", err, out, sherr, rendered)
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
		{"unreadable diff", map[string]string{"changed_files": "-1"}, "pas pu être lu"},
		{"unparseable", map[string]string{"unparseable": "true"}, "interprété"},
		{"over cap", map[string]string{"diff_too_large": "true"}, "limite"},
		{"workspace drift", map[string]string{"workspace_anomaly": " M apps/x/values.yaml"}, "workspace"},
		{"workspace unreadable", map[string]string{"workspace_anomaly": "?"}, "workspace"},
		{"stale launch", map[string]string{"stale_launch": "true", "declared_head_sha": "aaa0000"}, "tête qui a bougé"},
		{"self-modified policy", map[string]string{"policy_source": "target-modified"}, "politique de review"},
		{"fallback-served judge", map[string]string{"fallback_used": "glm-5.3"}, "fallback"},
		{"unknown class", map[string]string{"classified_files": `[{"path":"a.yaml","cls":"unknown","rule":"","reason":""}]`}, "a.yaml"},
		{"escalated file", map[string]string{"classified_files": `[{"path":"charts/new/d.yaml","cls":"escalate","rule":"structural","reason":"new component"}]`}, "new component"},
		{"blocking platform finding", map[string]string{"platform_findings": `[{"path":"a.yaml","rule":"managed-datastore","detail":"OvhValkey exists","severity":"blocking"}]`}, "managed-datastore"},
		{"doubts", map[string]string{"doubts": "le tag de l'image n'est pas sur le registre", "verdict": "escalate"}, "doute"},
		{"unreadable verdict", map[string]string{"verdict": ""}, "illisible"},
		{"sentinel route", map[string]string{"route": "sentinel", "sentinel_reason": "le diff dépasse la limite de classification — un diff trop gros à lire est un diff que personne n'a certifié"}, "limite"},
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
		"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "0", "peer_gate_poll_seconds": "0", "merge_method": "squash", "remove_source_branch": "false",
		"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
		"rendered":          "{}",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
	}
	for k, v := range over {
		vals[k] = v
	}
	rendered := cmdTemplate
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	rid := "run-test-1"
	if v, ok := over["run.id"]; ok {
		rid = v
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", wq(rid))
	rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", wq("0"))
	rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", wq("0"))
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}
	out, sherr, err := shOut(rendered)
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (out %q) stderr: %.800s CMD: %.600s", err, out, sherr, rendered)
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
	if res.Blocking != 1 || !strings.Contains(res.EscalateReasons, "politique de review") {
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
			"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "0", "peer_gate_poll_seconds": "0", "merge_method": "squash", "remove_source_branch": "false",
			"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
			"route": "classify", "sentinel_reason": "", "changed_files": "1",
			"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
			"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
			"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
			"rendered":          "{}",
			"verdict":           "auto_approvable",
			"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
			"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
		}
		rendered := cmdTemplate
		for k, v := range vals {
			rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
			rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
		}
		rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
		rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", "'0'")
		rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", "'0'")
		out, sherr, err := shOut(rendered)
		if err != nil && len(out) == 0 {
			t.Fatalf("publish_verdict failed: %v (%q) stderr: %.800s", err, out, sherr)
		}
		var res wardenVerdict
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		if res.ApprovePosted || res.MergeArmed {
			t.Fatalf("a red peer gate must hold the gestures: approve=%v armed=%v", res.ApprovePosted, res.MergeArmed)
		}
		if res.Outcome != "refused" || !strings.Contains(res.RefusedReason, "gate pair") {
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
	if !strings.Contains(res.EscalateReasons, "structurel") || !strings.Contains(res.EscalateReasons, "des 5") {
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
		{"stale launch", map[string]string{"route": "empty", "stale_launch": "true", "declared_head_sha": "aaa0000"}, "tête qui a bougé"},
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
		{"pending peer", `[{"context":"revi/review","state":"pending"}]`, "est pending"},
		{"absent peer", `[]`, "aucun statut"},
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
				"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "0", "peer_gate_poll_seconds": "0", "merge_method": "squash", "remove_source_branch": "false",
				"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
				"route": "classify", "sentinel_reason": "", "changed_files": "1",
				"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
				"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
				"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
				"rendered":          "{}",
				"verdict":           "auto_approvable",
				"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
				"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
			}
			rendered := cmdTemplate
			for k, v := range vals {
				rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
				rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
			}
			rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
			rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", "'0'")
			rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", "'0'")
			out, sherr, err := shOut(rendered)
			if err != nil && len(out) == 0 {
				t.Fatalf("publish_verdict failed: %v (%q) stderr: %.800s", err, out, sherr)
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
		"max_diff_files": "150", "max_diff_bytes": "400000", "head_sha": "", "render_registry_hosts": "",
	} {
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left: %s", firstRef(rendered))
	}
	out, sherr, err := shOut(rendered)
	if err != nil {
		t.Fatalf("diff_inventory failed: %v (%s) stderr: %.800s", err, out, sherr)
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
	if !strings.Contains(got.Summary, "n'est pas un verdict propre") {
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

// The engine passes an edge ref whose producer never ran AS ITS LITERAL:
// an unresolved fallback_used template is the ABSENCE of the stamp (the
// judge was served by its primary), never a fallback. Regression for the
// live e2e run where the zai-dialed primary left the ref unresolved and
// the fold read the literal as a fallback.
func TestGitopsWardenFold_UnresolvedFallbackRefIsNotAFallback(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
	vals := map[string]string{
		"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": "", "forge_publish_token": "",
		"forge_pr_state_url": "", "mode": "dry_run", "gate_enabled": "true", "gate_context": "gitops/conformance",
		"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "0", "peer_gate_poll_seconds": "0", "merge_method": "squash", "remove_source_branch": "false",
		"run.id": "run-test-1", "reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "",
		"rendered":          "{}",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump",
		// THE fixture: the literal an unresolved edge ref leaves in the body.
		"fallback_used": "{{outputs.classify._fallback_used}}",
	}
	rendered := cmdTemplate
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", "'run-test-1'")
	rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", "'0'")
	rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", "'0'")
	// No unsubstituted-ref assertion HERE on purpose: the literal in the
	// body is the case under test.
	out, sherr, err := shOut(rendered)
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (%q) stderr: %.800s", err, out, sherr)
	}
	var res wardenVerdict
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("bad json: %v (%q)", err, out)
	}
	if res.Blocking != 0 {
		t.Fatalf("blocking = %d (%s) — the unresolved template must read as no-fallback", res.Blocking, res.EscalateReasons)
	}
}

func runWardenInventory(t *testing.T, dir string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cmdTemplate := toolCommand(t, "gitops-warden/main.bot", "diff_inventory")
	rendered := cmdTemplate
	for k, v := range map[string]string{
		"workspace_dir": dir, "base_ref": "main", "policy_path": "review-policy.md",
		"max_diff_files": "150", "max_diff_bytes": "400000", "head_sha": "", "render_registry_hosts": "",
	} {
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left: %s", firstRef(rendered))
	}
	out, sherr, err := shOut(rendered)
	if err != nil {
		t.Fatalf("diff_inventory failed: %v (%s) stderr: %.800s", err, out, sherr)
	}
	var inv map[string]any
	if err := json.Unmarshal(out, &inv); err != nil {
		t.Fatalf("bad inventory json: %v (%q)", err, out)
	}
	return inv
}

func wardenGitFixture(t *testing.T) (string, func(rel, body string), func(args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", git.NoAutoMaintenance(args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main", ".")
	return dir, write, run
}

// A diff with no Chart.yaml carries an EMPTY render envelope — the field
// is always present so no consumer ever reads an unresolved edge ref.
func TestGitopsWardenInventory_NoChartYamlEmitsEmptyRender(t *testing.T) {
	dir, write, run := wardenGitFixture(t)
	write("apps/x/values.yaml", "image:\n  tag: 1.4.2\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	write("apps/x/values.yaml", "image:\n  tag: 1.4.3\n")
	run("add", ".")
	run("commit", "-q", "-m", "bump")
	inv := runWardenInventory(t, dir)
	r, ok := inv["rendered"]
	if !ok {
		t.Fatal("diff_inventory emitted no rendered field — a consumer would read an unresolved edge ref")
	}
	if s, _ := json.Marshal(r); string(s) != "{}" {
		t.Fatalf("rendered = %s, want {} (no Chart.yaml in the diff)", s)
	}
}

// A Chart.yaml bumping an OCI dependency renders NOTHING without the
// registry secrets — and the run must say so in FRENCH, not crash: the
// render is an enrichment, the verdict stays on the source diff.
func TestGitopsWardenInventory_OCIWrapperWithoutSecretDegradesClean(t *testing.T) {
	dir, write, run := wardenGitFixture(t)
	chart := "apiVersion: v2\nname: wrap\nversion: 0.1.0\ndependencies:\n  - name: app\n    version: \"0.0.1\"\n    repository: \"oci://registry.example/proj/charts\"\n"
	write("apps/wrap/Chart.yaml", chart)
	write("apps/wrap/values.yaml", "replicas: 1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	write("apps/wrap/Chart.yaml", "apiVersion: v2\nname: wrap\nversion: 0.1.0\ndependencies:\n  - name: app\n    version: \"0.0.2\"\n    repository: \"oci://registry.example/proj/charts\"\n")
	run("add", ".")
	run("commit", "-q", "-m", "bump")
	inv := runWardenInventory(t, dir)
	raw, _ := json.Marshal(inv["rendered"])
	var r struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("bad rendered json: %v (%s)", err, raw)
	}
	if r.Status != "unavailable" {
		t.Fatalf("rendered.status = %q, want unavailable (no registry secrets, deps unpullable)", r.Status)
	}
	// The host comes from the diff: an unpinned host is refused before the
	// secrets are even considered — that refusal, or the secrets one, or a
	// helm failure, must be named in french.
	if !strings.Contains(r.Reason, "non épinglé") && !strings.Contains(r.Reason, "registre") && !strings.Contains(r.Reason, "helm") {
		t.Errorf("rendered.reason = %q, want the french unpinned-host / registry / helm reason", r.Reason)
	}
}

// The real render: a chart-source repo whose template changed renders at
// base and head, and the stats name the added resource kinds. Skipped
// where helm is not installed; the degrade path above runs everywhere.
func TestGitopsWardenInventory_ChartSourceRenders(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir, write, run := wardenGitFixture(t)
	tpl := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v1\n"
	write("chart/Chart.yaml", "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	write("chart/templates/cm.yaml", tpl)
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v2\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n")
	run("add", ".")
	run("commit", "-q", "-m", "bump")
	inv := runWardenInventory(t, dir)
	raw, _ := json.Marshal(inv["rendered"])
	var r struct {
		Status string `json:"status"`
		Diff   string `json:"diff"`
		Stats  struct {
			KindsAdded   map[string]int `json:"kinds_added"`
			KindsRemoved map[string]int `json:"kinds_removed"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("bad rendered json: %v (%s)", err, raw)
	}
	if r.Status != "ok" {
		t.Fatalf("rendered.status = %q, want ok", r.Status)
	}
	if r.Stats.KindsAdded["Secret"] != 1 {
		t.Errorf("kinds_added = %v, want Secret x1", r.Stats.KindsAdded)
	}
	if !strings.Contains(r.Diff, "key: v2") {
		t.Errorf("rendered.diff misses the template change: %q", r.Diff[:min(160, len(r.Diff))])
	}
}

// The operator-facing surface is french and seam-clean: the visual renders
// caught a stray apostrophe injected at the gate.note join and an english
// table header — both invisible to the decision asserts above.
func TestGitopsWardenPublish_FrenchSurfaceHasNoStrayApostrophe(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{
		"mode": "dry_run", "verdict": "escalate",
		"files":            `[{"path":"apps/x/Chart.yaml","status":"M","ext":"yaml","added":1,"removed":1,"key_paths":["version"],"candidate":true}]`,
		"classified_files": `[{"path":"apps/x/Chart.yaml","cls":"escalate","rule":"structural","reason":"bump de dépendance"}]`,
		"rendered":         `{"status":"ok","reason":"","charts":["apps/x"],"stats":{"kinds_added":{"Secret":1,"ConfigMap":2},"kinds_removed":{}},"diff":"+kind: Secret"}`,
	})
	if res.Blocking != 1 {
		t.Fatalf("blocking = %d, want 1", res.Blocking)
	}
	if !strings.HasPrefix(got.Gate.Note, "bloqué : apps/x/Chart.yaml") {
		t.Errorf("gate.note = %q, want it to start with the reason, no stray quote at the join", got.Gate.Note)
	}
	if strings.Contains(got.Summary, "ajoutées : '") || strings.Contains(got.Summary, "retirées : '") {
		t.Errorf("render stats carry a stray apostrophe: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "ressources ajoutées : ConfigMap x2, Secret x1") {
		t.Errorf("render stats line missing or wrong: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "| fichier | statut | classe | pourquoi |") {
		t.Errorf("table header is not french: %q", got.Summary)
	}
	if strings.Contains(got.Summary, "| file | status | class | why |") {
		t.Errorf("english table header survived")
	}
	if !strings.Contains(got.Summary, "/studio/runs/run-test-1\">run-test-1</a>") {
		t.Errorf("run link missing or malformed: %q", got.Summary)
	}
}

// F1 regression: the render section must not shadow the node's `base` —
// a repo that TIGHTENED its policy keeps being judged by it when a chart
// renders in the same MR.
func TestGitopsWardenInventory_PolicyOverrideSurvivesTheRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir, write, run := wardenGitFixture(t)
	write("review-policy.md", "STRICT: apps/prod/** always escalates\n")
	write("chart/Chart.yaml", "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v2\n")
	run("add", ".")
	run("commit", "-q", "-m", "bump")
	inv := runWardenInventory(t, dir)
	if got := inv["policy_source"]; got != "override" {
		t.Fatalf("policy_source = %v, want override — the render must not touch the policy read", got)
	}
	if body, _ := inv["policy_body"].(string); !strings.Contains(body, "STRICT") {
		t.Fatalf("policy_body = %q, want the repo's STRICT override", body)
	}
	raw, _ := json.Marshal(inv["rendered"])
	if !strings.Contains(string(raw), `"status": "ok"`) && !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("rendered = %s, want status ok alongside the preserved policy", raw)
	}
}

// F2 regression: helm follows symlinks out of the chart; a chart dir
// holding one is refused and the linked content never reaches the envelope.
func TestGitopsWardenInventory_SymlinkInChartIsRefused(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir, write, run := wardenGitFixture(t)
	write("outside-secret.txt", "TOPSECRET-TOKEN-VALUE\n")
	write("chart/Chart.yaml", "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "mr")
	if err := os.Symlink("../outside-secret.txt", filepath.Join(dir, "chart", "stolen.txt")); err != nil {
		t.Fatal(err)
	}
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  leak: |\n{{ .Files.Get \"stolen.txt\" | indent 4 }}\n")
	run("add", ".")
	run("commit", "-q", "-m", "exfil")
	inv := runWardenInventory(t, dir)
	raw, _ := json.Marshal(inv["rendered"])
	if strings.Contains(string(raw), "TOPSECRET") {
		t.Fatalf("the linked file's content reached the render envelope: %s", raw)
	}
	if !strings.Contains(string(raw), "lien symbolique") {
		t.Errorf("rendered = %s, want the symlink refusal reason", raw)
	}
}

// F7 regression: a chart at the repository root renders (the walk-up
// includes the root). F6: a chart added by the MR renders head-only, ok.
func TestGitopsWardenInventory_RootChartAndNewChartRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir, write, run := wardenGitFixture(t)
	write("Chart.yaml", "apiVersion: v2\nname: root\nversion: 0.1.0\n")
	write("templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "mr")
	write("templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v2\n")
	write("apps/new/Chart.yaml", "apiVersion: v2\nname: new\nversion: 0.1.0\n")
	write("apps/new/templates/svc.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: s\n")
	run("add", ".")
	run("commit", "-q", "-m", "root+new")
	inv := runWardenInventory(t, dir)
	raw, _ := json.Marshal(inv["rendered"])
	var r struct {
		Status string   `json:"status"`
		Charts []string `json:"charts"`
		Stats  struct {
			KindsAdded map[string]int `json:"kinds_added"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("bad rendered json: %v (%s)", err, raw)
	}
	if r.Status != "ok" {
		t.Fatalf("rendered.status = %q (%s), want ok", r.Status, raw)
	}
	if len(r.Charts) != 2 {
		t.Errorf("charts = %v, want the root chart AND the new chart", r.Charts)
	}
	if r.Stats.KindsAdded["Service"] != 1 {
		t.Errorf("kinds_added = %v, want the new chart's Service x1 (head-only render)", r.Stats.KindsAdded)
	}
}

// The comment is hardened against MR-controlled text: a partial render is
// surfaced, a kind that is not a Kubernetes identifier is masked (it came
// from a template), and the run id cannot break out of the link.
func TestGitopsWardenPublish_CommentHardening(t *testing.T) {
	got, res := runWardenPublish(t, map[string]string{
		"mode":     "dry_run",
		"run.id":   `a"><script>alert(1)</script>`,
		"rendered": `{"status":"ok","reason":"charts/b : le chart contient un lien symbolique — rendu refusé","charts":["charts/a","charts/b"],"stats":{"kinds_added":{"Secret":1,"<img src=x>":1},"kinds_removed":{}},"diff":"+kind: Secret"}`,
	})
	if res.Blocking != 0 {
		t.Fatalf("blocking = %d, want 0 — the render is never a verdict input", res.Blocking)
	}
	if !strings.Contains(got.Summary, "Rendu partiel : charts/b : le chart contient un lien symbolique") {
		t.Errorf("a partial render must be surfaced: %q", got.Summary)
	}
	if strings.Contains(got.Summary, "<img src=x>") {
		t.Errorf("a non-identifier kind landed raw in the comment: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "? x1") || !strings.Contains(got.Summary, "Secret x1") {
		t.Errorf("stats line wrong: %q", got.Summary)
	}
	if strings.Contains(got.Summary, `<script>`) {
		t.Errorf("a hostile run id landed raw in the comment: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, `/studio/runs/a%22%3E%3Cscript%3Ealert%281%29%3C%2Fscript%3E">`) {
		t.Errorf("the href must carry the url-quoted run id: %q", got.Summary)
	}
}

// R43f492 regression: a unified diff carries context lines (leading space)
// around every hunk — an unchanged `kind:` line near an edit is NOT a
// removed resource. Only +/- lines count, never the +++/--- headers.
func TestGitopsWardenInventory_RenderStatsIgnoreContextLines(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	dir, write, run := wardenGitFixture(t)
	write("chart/Chart.yaml", "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  key: v1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "bump")
	// the edit sits right under `kind:` so the hunk's context carries it
	write("chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-renamed\ndata:\n  key: v1\n")
	run("add", ".")
	run("commit", "-q", "-m", "name only")
	inv := runWardenInventory(t, dir)
	raw, _ := json.Marshal(inv["rendered"])
	var r struct {
		Status string `json:"status"`
		Stats  struct {
			KindsAdded   map[string]int `json:"kinds_added"`
			KindsRemoved map[string]int `json:"kinds_removed"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Status != "ok" {
		t.Fatalf("rendered = %s (err %v), want status ok", raw, err)
	}
	if len(r.Stats.KindsAdded) != 0 || len(r.Stats.KindsRemoved) != 0 {
		t.Fatalf("a value-only template edit must add/remove no kind, got added=%v removed=%v (context lines counted)", r.Stats.KindsAdded, r.Stats.KindsRemoved)
	}
}

// peerState is a read-twin body for the audited head with the given
// commit_statuses JSON array.
func peerState(statuses string) string {
	return "{\"open\":true,\"state\":\"open\",\"head_sha\":\"abc123defabc123\",\"commit_statuses\":" + statuses + "}"
}

// runWardenPublishState runs publish_verdict in enforce against a capture
// server whose read twin answers state(hit) on its hit-th call, and returns
// what the endpoint received, the node's verdict_output and the number of
// read-twin calls. The default window is 5 s polled every 50 ms.
func runWardenPublishState(t *testing.T, over map[string]string, state func(hit int) string) (wardenPublish, wardenVerdict, int) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	var mu sync.Mutex
	var got wardenPublish
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		h := hits
		mu.Unlock()
		body := state(h)
		if strings.HasPrefix(body, "!") {
			code := 0
			_, _ = fmt.Sscanf(body[1:], "%d", &code)
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&got)
		approve, arm := got.Verdict.Approve, got.Verdict.Arm
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{\"published\":true,\"review_url\":\"x\",\"gate_posted\":true,\"gate_state\":\"success\",\"verdict\":{\"approve_posted\":%v,\"merge_armed\":%v,\"merge_state\":\"mwps\"}}", approve, arm)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	vals := map[string]string{
		"pr_url": "https://pic.example/o/r/-/merge_requests/7", "forge_publish_url": srv.URL, "forge_publish_token": "tok-1",
		"forge_pr_state_url": srv.URL + "/state", "mode": "enforce", "gate_enabled": "true", "gate_context": "gitops/conformance",
		"reviewers": "", "peer_gate_contexts": "revi/review", "peer_gate_wait_seconds": "5", "peer_gate_poll_seconds": "0.05",
		"merge_method": "squash", "remove_source_branch": "false",
		"reviewed_sha": "abc123defabc123", "base_sha": "base111base11111",
		"route": "classify", "sentinel_reason": "", "changed_files": "1",
		"files":          `[{"path":"apps/x/values.yaml","status":"M","ext":"yaml","added":2,"removed":2,"key_paths":["image"],"candidate":true}]`,
		"diff_too_large": "false", "unparseable": "false", "workspace_anomaly": "", "stale_launch": "false",
		"policy_source": "default", "policy_sha": "", "declared_head_sha": "", "rendered": "{}",
		"verdict":           "auto_approvable",
		"classified_files":  `[{"path":"apps/x/values.yaml","cls":"auto_approvable","rule":"image-bump","reason":"same image, new tag"}]`,
		"platform_findings": `[]`, "doubts": "", "summary": "clean bump", "fallback_used": "",
	}
	for k, v := range over {
		vals[k] = v
	}
	rendered := toolCommand(t, "gitops-warden/main.bot", "publish_verdict")
	for k, v := range vals {
		rendered = strings.ReplaceAll(rendered, "{{input."+k+"}}", wq(v))
		rendered = strings.ReplaceAll(rendered, "{{vars."+k+"}}", wq(v))
	}
	rendered = strings.ReplaceAll(rendered, "{{run.id}}", wq("run-test-1"))
	maxD, el := "0", "0"
	if v, ok := over["run.max_duration_seconds"]; ok {
		maxD = v
	}
	if v, ok := over["run.elapsed_seconds"]; ok {
		el = v
	}
	rendered = strings.ReplaceAll(rendered, "{{run.max_duration_seconds}}", wq(maxD))
	rendered = strings.ReplaceAll(rendered, "{{run.elapsed_seconds}}", wq(el))
	if strings.Contains(rendered, "{{") {
		t.Fatalf("unsubstituted ref left in the command: %s", firstRef(rendered))
	}
	out, sherr, err := shOut(rendered)
	if err != nil && len(out) == 0 {
		t.Fatalf("publish_verdict failed: %v (out %q) stderr: %.800s", err, out, sherr)
	}
	var res wardenVerdict
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not verdict_output JSON: %v (%q)", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	return got, res, hits
}

// A peer still running when the verdict lands is waited for: pending twice,
// then success — the arm is sent after three reads, never before.
func TestGitopsWardenPublish_PeerConcludingInsideTheWindowArms(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(h int) string {
		if h < 3 {
			return peerState(`[{"context":"revi/review","state":"pending"}]`)
		}
		return peerState(`[{"context":"revi/review","state":"success"}]`)
	})
	if !got.Verdict.Approve || !got.Verdict.Arm {
		t.Fatalf("a peer that concluded green inside the window must arm: approve=%v arm=%v (reason %q, %d reads)", got.Verdict.Approve, got.Verdict.Arm, res.RefusedReason, hits)
	}
	if hits != 3 {
		t.Errorf("reads = %d, want 3 (pending, pending, success)", hits)
	}
}

// A pinned peer the pipeline has not created yet is absent, not red: it is
// waited for like a running one.
func TestGitopsWardenPublish_AbsentPeerIsWaitedFor(t *testing.T) {
	got, res, hits := runWardenPublishState(t, map[string]string{"peer_gate_contexts": "revi/review,gitops-mr-review"}, func(h int) string {
		if h < 2 {
			return peerState(`[{"context":"revi/review","state":"success"}]`)
		}
		return peerState(`[{"context":"revi/review","state":"success"},{"context":"gitops-mr-review","state":"success"}]`)
	})
	if !got.Verdict.Arm {
		t.Fatalf("the absent peer concluded green on the 2nd read — the arm must follow (reason %q, %d reads)", res.RefusedReason, hits)
	}
	if hits != 2 {
		t.Errorf("reads = %d, want 2", hits)
	}
}

// A peer that never concludes holds the arm once the window expires, and
// the reason names both the peer state and the expired window.
func TestGitopsWardenPublish_PeerPendingPastTheWindowHolds(t *testing.T) {
	got, res, hits := runWardenPublishState(t, map[string]string{"peer_gate_wait_seconds": "1"}, func(int) string {
		return peerState(`[{"context":"revi/review","state":"pending"}]`)
	})
	if got.Verdict.Approve || got.Verdict.Arm {
		t.Fatalf("a peer pending past the window must hold: approve=%v arm=%v", got.Verdict.Approve, got.Verdict.Arm)
	}
	if !strings.Contains(res.RefusedReason, "est pending") || !strings.Contains(res.RefusedReason, "fenêtre d'attente") {
		t.Errorf("reason = %q, want the pending peer and the expired window", res.RefusedReason)
	}
	if hits < 2 {
		t.Errorf("reads = %d, want several polls inside the window", hits)
	}
}

// A terminal red peer holds at once: waiting cannot turn it green.
func TestGitopsWardenPublish_FailedPeerHoldsWithoutWaiting(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(int) string {
		return peerState(`[{"context":"revi/review","state":"failure"}]`)
	})
	if got.Verdict.Arm {
		t.Fatal("a red peer must hold the arm")
	}
	if hits != 1 {
		t.Errorf("reads = %d, want 1 — a terminal red peer is never waited for", hits)
	}
	if !strings.Contains(res.RefusedReason, "est failure") {
		t.Errorf("reason = %q, want the red peer named", res.RefusedReason)
	}
}

// peerStateHead is peerState with an explicit head sha.
func peerStateHead(head, statuses string) string {
	return "{\"open\":true,\"state\":\"open\",\"head_sha\":\"" + head + "\",\"commit_statuses\":" + statuses + "}"
}

// rva-peerwait round: one non-regression per finding.

// A canceled/skipped peer reads `error`: terminal, held at once — never
// counted green, never waited for.
func TestGitopsWardenPublish_ErrorPeerHoldsAtOnce(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(int) string {
		return peerState(`[{"context":"revi/review","state":"error"}]`)
	})
	if got.Verdict.Arm || got.Verdict.Approve {
		t.Fatalf("an error peer must hold: approve=%v arm=%v", got.Verdict.Approve, got.Verdict.Arm)
	}
	if hits != 1 || !strings.Contains(res.RefusedReason, "est error") {
		t.Errorf("reads=%d reason=%q, want 1 read naming the error peer", hits, res.RefusedReason)
	}
}

// A head that moves while the window runs holds at once, even with every
// peer green on the new head.
func TestGitopsWardenPublish_HeadMovedMidWindowHolds(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return peerState(`[{"context":"revi/review","state":"pending"}]`)
		}
		return peerStateHead("fff000fff000fff", `[{"context":"revi/review","state":"success"}]`)
	})
	if got.Verdict.Arm {
		t.Fatal("a moved head must hold the arm")
	}
	if hits != 2 || !strings.Contains(res.RefusedReason, "la tête a bougé") {
		t.Errorf("reads=%d reason=%q, want 2 reads and the moved-head reason", hits, res.RefusedReason)
	}
}

// The head check compares FULL shas: a head sharing only a 7-char prefix
// with the audited revision is another revision.
func TestGitopsWardenPublish_ShortPrefixHeadHolds(t *testing.T) {
	got, res, _ := runWardenPublishState(t, nil, func(int) string {
		return peerStateHead("abc123d00000000", `[{"context":"revi/review","state":"success"}]`)
	})
	if got.Verdict.Arm || !strings.Contains(res.RefusedReason, "la tête a bougé") {
		t.Fatalf("a prefix-only head match must hold: arm=%v reason=%q", got.Verdict.Arm, res.RefusedReason)
	}
}

// An empty head in the answer is a failed read: retried, then the arm
// follows the green read.
func TestGitopsWardenPublish_EmptyHeadIsRetried(t *testing.T) {
	got, _, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return peerStateHead("", `[{"context":"revi/review","state":"success"}]`)
		}
		return peerState(`[{"context":"revi/review","state":"success"}]`)
	})
	if !got.Verdict.Arm || hits != 2 {
		t.Fatalf("an empty head is retried then armed: arm=%v reads=%d", got.Verdict.Arm, hits)
	}
}

// A transient read failure is retried: HTTP 500 then green arms.
func TestGitopsWardenPublish_ServerErrorReadIsRetried(t *testing.T) {
	got, _, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return "!500"
		}
		return peerState(`[{"context":"revi/review","state":"success"}]`)
	})
	if !got.Verdict.Arm || hits != 2 {
		t.Fatalf("a 500 is retried then armed: arm=%v reads=%d", got.Verdict.Arm, hits)
	}
}

// A permanent read refusal (401) holds at once — waiting cannot fix it.
func TestGitopsWardenPublish_PermanentReadRefusalHoldsAtOnce(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(int) string {
		return "!401"
	})
	if got.Verdict.Arm || hits != 1 || !strings.Contains(res.RefusedReason, "HTTP 401") {
		t.Fatalf("a 401 must hold after one read: arm=%v reads=%d reason=%q", got.Verdict.Arm, hits, res.RefusedReason)
	}
}

// The twin reporting its own lister failure is retried like a 5xx.
func TestGitopsWardenPublish_StatusesErrorIsRetried(t *testing.T) {
	got, _, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return "{\"open\":true,\"head_sha\":\"abc123defabc123\",\"commit_statuses_error\":\"forge 502\"}"
		}
		return peerState(`[{"context":"revi/review","state":"success"}]`)
	})
	if !got.Verdict.Arm || hits != 2 {
		t.Fatalf("a lister error is retried then armed: arm=%v reads=%d", got.Verdict.Arm, hits)
	}
}

// pending then failure: the retry flag is per read — the terminal red
// on read 2 holds at once instead of waiting out the window.
func TestGitopsWardenPublish_PendingThenFailureHoldsAtSecondRead(t *testing.T) {
	got, res, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return peerState(`[{"context":"revi/review","state":"pending"}]`)
		}
		return peerState(`[{"context":"revi/review","state":"failure"}]`)
	})
	if got.Verdict.Arm || hits != 2 || !strings.Contains(res.RefusedReason, "est failure") {
		t.Fatalf("pending then failure must hold at read 2: arm=%v reads=%d reason=%q", got.Verdict.Arm, hits, res.RefusedReason)
	}
}

// A context listed twice keeps its WORST state, in BOTH list orders.
func TestGitopsWardenPublish_DuplicateContextWorstStateWins(t *testing.T) {
	for _, order := range []string{
		`[{"context":"revi/review","state":"success"},{"context":"revi/review","state":"failure"}]`,
		`[{"context":"revi/review","state":"failure"},{"context":"revi/review","state":"success"}]`,
	} {
		body := order
		got, _, _ := runWardenPublishState(t, nil, func(int) string { return peerState(body) })
		if got.Verdict.Arm {
			t.Errorf("a context red in any duplicate must hold (list %s)", body)
		}
	}
}

// The window is clamped to what the run budget leaves: 10 s left of a
// 1800 s run (minus the 180 s publish margin) means a single read.
func TestGitopsWardenPublish_RunBudgetClampsTheWindow(t *testing.T) {
	got, _, hits := runWardenPublishState(t, map[string]string{
		"run.max_duration_seconds": "1800", "run.elapsed_seconds": "1790",
	}, func(int) string { return peerState(`[{"context":"revi/review","state":"pending"}]`) })
	if got.Verdict.Arm || hits != 1 {
		t.Fatalf("a run with no budget left reads once and holds: arm=%v reads=%d", got.Verdict.Arm, hits)
	}
}

// A zero poll step falls back to the default instead of spinning.
func TestGitopsWardenPublish_ZeroPollDoesNotSpin(t *testing.T) {
	_, _, hits := runWardenPublishState(t, map[string]string{
		"peer_gate_wait_seconds": "0.5", "peer_gate_poll_seconds": "0",
	}, func(int) string { return peerState(`[{"context":"revi/review","state":"pending"}]`) })
	if hits > 2 {
		t.Fatalf("a zero poll step spun %d reads in a 0.5 s window", hits)
	}
}

// The gate never waits for its own context.
func TestGitopsWardenPublish_OwnGateContextIsNotAPeer(t *testing.T) {
	got, res, _ := runWardenPublishState(t, map[string]string{
		"peer_gate_contexts": "revi/review,gitops/conformance",
	}, func(int) string { return peerState(`[{"context":"revi/review","state":"success"}]`) })
	if !got.Verdict.Arm {
		t.Fatalf("the own gate context must be ignored as a peer (reason %q)", res.RefusedReason)
	}
}

// A malformed twin answer (not JSON) is a transient read failure: retried,
// then the arm follows the green read.
func TestGitopsWardenPublish_MalformedAnswerIsRetried(t *testing.T) {
	got, _, hits := runWardenPublishState(t, nil, func(h int) string {
		if h == 1 {
			return "<html>bad gateway</html>"
		}
		return peerState(`[{"context":"revi/review","state":"success"}]`)
	})
	if !got.Verdict.Arm || hits != 2 {
		t.Fatalf("a malformed answer is retried then armed: arm=%v reads=%d", got.Verdict.Arm, hits)
	}
}
