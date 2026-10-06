package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forge_verdict_test pins the merge-gate GESTURES: a verdict executes only on
// a posted gate, only within the grant's capabilities, only on the audited
// head — and an approval that failed never arms.

// fakeMergeOps records the three gestures instead of touching a forge.
type fakeMergeOps struct {
	approveErr, armErr, reviewerErr  error
	armResult                        forge.AutoMergeResult
	approvedSHA                      string
	armOpts                          forge.AutoMergeOptions
	approveCalls, armCalls, revCalls int
}

func (f *fakeMergeOps) ApprovePullRequest(_ context.Context, _ string, _ int, sha string) (forge.ApprovalResult, error) {
	f.approveCalls++
	f.approvedSHA = sha
	if f.approveErr != nil {
		return forge.ApprovalResult{}, f.approveErr
	}
	return forge.ApprovalResult{URL: "https://github.com/o/r/pull/42"}, nil
}

func (f *fakeMergeOps) ArmAutoMerge(_ context.Context, _ string, _ int, opts forge.AutoMergeOptions) (forge.AutoMergeResult, error) {
	f.armCalls++
	f.armOpts = opts
	if f.armErr != nil {
		return forge.AutoMergeResult{}, f.armErr
	}
	if f.armResult.State == "" {
		f.armResult.State = forge.AutoMergeArmed
	}
	return f.armResult, nil
}

func (f *fakeMergeOps) AddPullReviewers(_ context.Context, _ string, _ int, logins []string) ([]string, error) {
	f.revCalls++
	if f.reviewerErr != nil {
		return nil, f.reviewerErr
	}
	return logins, nil
}

func wireVerdict(t *testing.T, s *Server, gc forgeGateClient, ops *fakeMergeOps) {
	t.Helper()
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.forgeMergeOpsFor = func(context.Context, forge.Connection) (forgeMergeOps, error) {
		return forgeMergeOps{Approver: ops, Armer: ops, Setter: ops}, nil
	}
}

// verdictBody composes a publish body with the gate and verdict blocks.
func verdictBody(gate, verdict string) string {
	return `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[],"gate":` + gate + `,"verdict":` + verdict + `}`
}

const fullVerdict = `{"enabled":true,"approve":true,"arm_merge_when_pipeline_succeeds":true,
	"merge_method":"squash","request_reviewers":["devops-oncall"],"audited_sha":"abc123def","mode":"enforce"}`

const enabledGate = `{"enabled":true,"blocking_count":0,"audited_sha":"abc123def"}`

func TestForgePublishVerdict_CleanEnforceExecutesEveryVerb(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, fullVerdict)))
	if w.Code != 200 {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || !res.Verdict.Requested {
		t.Fatalf("verdict missing from response: %+v", res.Verdict)
	}
	if !res.Verdict.ApprovePosted {
		t.Errorf("approve_posted = false (%s)", res.Verdict.ApproveError)
	}
	if !res.Verdict.MergeArmed || res.Verdict.MergeState != forge.AutoMergeArmed {
		t.Errorf("merge armed=%v state=%q err=%q", res.Verdict.MergeArmed, res.Verdict.MergeState, res.Verdict.MergeError)
	}
	if len(res.Verdict.ReviewersAdded) != 1 || res.Verdict.ReviewersAdded[0] != "devops-oncall" {
		t.Errorf("reviewers_added = %v", res.Verdict.ReviewersAdded)
	}
	if ops.approvedSHA != "abc123def" {
		t.Errorf("approval pinned to %q, want the audited head", ops.approvedSHA)
	}
	if ops.armOpts.SHA != "abc123def" || ops.armOpts.Method != forge.MergeSquash {
		t.Errorf("arm opts = %+v, want sha pin + squash", ops.armOpts)
	}
}

func TestForgePublishVerdict_RefusedWithoutCapabilities(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}) // no caps
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, fullVerdict)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || res.Verdict.Refused == "" {
		t.Fatalf("a cap-less grant must refuse in band, got %+v", res.Verdict)
	}
	if !strings.Contains(res.Verdict.Refused, "capability") {
		t.Errorf("refusal = %q, want it to name the capability", res.Verdict.Refused)
	}
	if ops.approveCalls != 0 || ops.armCalls != 0 || ops.revCalls != 0 {
		t.Errorf("no gesture may run on a refused verdict: approve=%d arm=%d rev=%d", ops.approveCalls, ops.armCalls, ops.revCalls)
	}
	// The gate itself still posted — the check is the audit anchor, not the
	// gestures.
	if !res.GatePosted {
		t.Error("the gate status must still post when only the gestures are refused")
	}
}

func TestForgePublishVerdict_RefusedWithoutAGateAnchor(t *testing.T) {
	cases := []struct {
		name string
		gate string
		gc   *fakeGateClient
	}{
		{"no gate block at all", ``, &fakeGateClient{headSHA: "abc123def"}},
		{"gate disabled", `{"enabled":false}`, &fakeGateClient{headSHA: "abc123def"}},
		{"gate not posted (stale pin)", `{"enabled":true,"audited_sha":"old123old"}`, &fakeGateClient{headSHA: "abc123def"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
				Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
			ops := &fakeMergeOps{}
			wireVerdict(t, s, tc.gc, ops)

			body := verdictBody(tc.gate, fullVerdict)
			if tc.gate == "" {
				// No gate block at all: the body must not carry the key, not
				// carry it empty (which would be invalid JSON, a different
				// failure than the one this case exercises).
				body = `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[],"verdict":` + fullVerdict + `}`
			}
			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", body))
			var res publishReviewResponse
			_ = json.NewDecoder(w.Body).Decode(&res)
			if res.Verdict == nil || res.Verdict.Refused == "" {
				t.Fatalf("verdict must refuse without a gate anchor, got %+v", res.Verdict)
			}
			if ops.approveCalls != 0 || ops.armCalls != 0 {
				t.Errorf("no gesture may run: approve=%d arm=%d", ops.approveCalls, ops.armCalls)
			}
		})
	}
}

func TestForgePublishVerdict_RefusedOnAMovedHead(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge}})
	// The gate pins the OLD sha and posts on the head anyway (legacy unpinned
	// shape is gone — this gate would be refused), so instead: gate pin equals
	// head, verdict audited a DIFFERENT revision.
	gc := &fakeGateClient{headSHA: "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	stale := `{"enabled":true,"approve":true,"arm_merge_when_pipeline_succeeds":true,"audited_sha":"aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(`{"enabled":true,"blocking_count":0,"audited_sha":"bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"}`, stale)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || !strings.Contains(res.Verdict.Refused, "head moved") {
		t.Fatalf("a stale verdict pin must be refused in band, got %+v", res.Verdict)
	}
	if ops.approveCalls != 0 || ops.armCalls != 0 {
		t.Errorf("no gesture may run on a stale pin: approve=%d arm=%d", ops.approveCalls, ops.armCalls)
	}
}

func TestForgePublishVerdict_MalformedVerdictIsA400(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing audited sha", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"approve":true}}`},
		{"unpinnable audited sha", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"approve":true,"audited_sha":"main"}}`},
		{"bad merge method", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"arm_merge_when_pipeline_succeeds":true,"merge_method":"rebase","audited_sha":"abc123def"}}`},
		{"empty reviewer login", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"request_reviewers":["  "],"audited_sha":"abc123def"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
				Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
			wireVerdict(t, s, &fakeGateClient{headSHA: "abc123def"}, &fakeMergeOps{})

			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", tc.body))
			if w.Code != 400 {
				t.Fatalf("code = %d, want 400 (the malformed verdict is learned BEFORE anything posts), body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestForgePublishVerdict_FailedApprovalSuppressesTheArm(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{approveErr: errors.New("already approved by a human — eligibility rule")}
	wireVerdict(t, s, gc, ops)

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, fullVerdict)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil {
		t.Fatal("verdict missing")
	}
	if res.Verdict.ApprovePosted {
		t.Error("approve_posted must be false")
	}
	if ops.armCalls != 0 {
		t.Errorf("the arm must be SKIPPED on a failed approval, got %d arm calls", ops.armCalls)
	}
	if res.Verdict.MergeError == "" {
		t.Error("the skipped arm must say why (never an unconfirmed approval)")
	}
}

func TestForgePublishCapabilitiesFor_FailsClosedOnAnUnresolvableBot(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	if caps := s.forgePublishCapabilitiesFor(context.Background(), "team1", "no-such-bot"); len(caps) != 0 {
		t.Errorf("an unresolvable bot must mint NO capabilities (fail closed), got %v", caps)
	}
}

func TestVerdictMissingCaps_IsExact(t *testing.T) {
	v := &publishReviewVerdict{Approve: true, ArmMergeWhenPipelineSucceeds: true, RequestReviewers: []string{"a"}}
	if got := verdictMissingCaps(nil, v); len(got) != 3 {
		t.Errorf("missing = %v, want all three", got)
	}
	if got := verdictMissingCaps([]string{publishCapApprove, publishCapMerge, publishCapReviewers}, v); len(got) != 0 {
		t.Errorf("missing = %v, want none", got)
	}
	if got := verdictMissingCaps([]string{publishCapApprove}, &publishReviewVerdict{Approve: true}); len(got) != 0 {
		t.Errorf("missing = %v, want none (approve only)", got)
	}
}

// Regression tests for the adversarial round 1 findings (F1 F2 F3 F5 F7).

func TestForgePublishVerdict_GesturesNeverFollowARedGate(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	// A RED gate (blocking findings) with the full verb set: approval and
	// arming are refused, the ESCALATION verb still runs.
	red := `{"enabled":true,"blocking_count":5,"threshold":"high","audited_sha":"abc123def"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(red, fullVerdict)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil {
		t.Fatal("verdict missing")
	}
	if res.Verdict.ApprovePosted || ops.approveCalls != 0 {
		t.Errorf("an approval must never follow a red check (posted=%v calls=%d)", res.Verdict.ApprovePosted, ops.approveCalls)
	}
	if res.Verdict.MergeArmed || ops.armCalls != 0 {
		t.Errorf("an arming must never follow a red check (armed=%v calls=%d)", res.Verdict.MergeArmed, ops.armCalls)
	}
	if len(res.Verdict.ReviewersAdded) != 1 {
		t.Errorf("the escalation verb stays allowed on red, reviewers = %v", res.Verdict.ReviewersAdded)
	}
	if res.GateState != "failure" {
		t.Errorf("the fixture must actually post a red check, got %q", res.GateState)
	}
}

type flippingGateClient struct {
	fakeGateClient
	reads  int
	newSHA string
}

func (f *flippingGateClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	f.reads++
	if f.reads >= 2 {
		f.headSHA = f.newSHA // a push lands between the check and the gestures
	}
	return f.fakeGateClient.GetPullRequest(ctx, repo, number)
}

func TestForgePublishVerdict_HeadReReadBeforeTheGestures(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	// read 1: the gate resolves head abc; read 2 (the verdict's own re-read):
	// the head is now bcd. The gestures must refuse, not mutate.
	gc := &flippingGateClient{fakeGateClient: fakeGateClient{headSHA: "abc123def"}, newSHA: "bcd123efbcd123efbcd123efbcd123efbcd123ef"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, fullVerdict)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || res.Verdict.Refused == "" {
		t.Fatalf("a head that moved between the check and the gestures must refuse, got %+v", res.Verdict)
	}
	if ops.approveCalls != 0 || ops.armCalls != 0 {
		t.Errorf("no gesture may run on the moved head: approve=%d arm=%d", ops.approveCalls, ops.armCalls)
	}
}

func TestForgePublishVerdict_DryRunVerdictCarryingVerbsIsRefused(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	dry := `{"enabled":true,"approve":true,"arm_merge_when_pipeline_succeeds":true,"audited_sha":"abc123def","mode":"dry_run"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, dry)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || res.Verdict.Refused == "" {
		t.Fatalf("a dry_run verdict carrying verbs is a bundle bug and must refuse in band, got %+v", res.Verdict)
	}
	if ops.approveCalls != 0 || ops.armCalls != 0 {
		t.Errorf("dry_run must execute nothing: approve=%d arm=%d", ops.approveCalls, ops.armCalls)
	}
}

func TestForgePublishVerdict_EnabledWithoutVerbsIsEchoedNotSilent(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove}})
	gc := &fakeGateClient{headSHA: "abc123def"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, `{"enabled":true,"mode":"enforce"}`)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || !res.Verdict.Requested || res.Verdict.Refused == "" {
		t.Fatalf("an enabled verb-less verdict must be echoed as refused, got %+v", res.Verdict)
	}
}

func TestForgePublishVerdict_ReviewerListIsCapped(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapReviewers}})
	wireVerdict(t, s, &fakeGateClient{headSHA: "abc123def"}, &fakeMergeOps{})

	var arr []string
	for i := 0; i < 51; i++ {
		arr = append(arr, fmt.Sprintf("user%02d", i))
	}
	b, _ := json.Marshal(arr)
	body := `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"request_reviewers":` + string(b) + `,"audited_sha":"abc123def"}}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", body))
	if w.Code != 400 {
		t.Fatalf("code = %d, want 400 over the reviewer cap", w.Code)
	}
}

// F-c (round 2): the head-vs-check guard is load-bearing even when the
// verdict's own pin follows the moved head — the gestures must stay coupled
// to the revision the POSTED CHECK certifies, not to whichever revision the
// verdict claims to have read.
func TestForgePublishVerdict_VerdictRepinnedToANewHeadStillRefuses(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
		Capabilities: []string{publishCapApprove, publishCapMerge, publishCapReviewers}})
	// read 1 (the gate): head abc → the check posts on abc, green.
	// read 2 (the verdict's re-read): a push landed, head is bcd.
	// The verdict was re-pinned to bcd (audited_sha=bcd): both pins are
	// self-consistent, and the gestures must STILL refuse — the check on the
	// forge certifies abc, and green-check-on-abc must never arm bcd.
	gc := &flippingGateClient{fakeGateClient: fakeGateClient{headSHA: "abc123def"}, newSHA: "bcd123efbcd123efbcd123efbcd123efbcd123ef"}
	ops := &fakeMergeOps{}
	wireVerdict(t, s, gc, ops)

	repinned := `{"enabled":true,"approve":true,"arm_merge_when_pipeline_succeeds":true,"audited_sha":"bcd123efbcd123efbcd123efbcd123efbcd123ef","mode":"enforce"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", verdictBody(`{"enabled":true,"blocking_count":0,"audited_sha":"abc123def"}`, repinned)))
	var res publishReviewResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	if res.Verdict == nil || res.Verdict.Refused == "" {
		t.Fatalf("gestures must never decouple from the posted check, got %+v", res.Verdict)
	}
	if ops.approveCalls != 0 || ops.armCalls != 0 {
		t.Errorf("approve/arm ran on a revision the posted check does not certify: approve=%d arm=%d", ops.approveCalls, ops.armCalls)
	}
}

// F-c: both edges of the reviewer cap — 50 passes, 51 is a 400.
func TestForgePublishVerdict_ReviewerCapEdges(t *testing.T) {
	for _, n := range []int{50, 51} {
		s, _ := newForgePublishTestServer(t)
		registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
			Capabilities: []string{publishCapReviewers}})
		wireVerdict(t, s, &fakeGateClient{headSHA: "abc123def"}, &fakeMergeOps{})
		var arr []string
		for i := 0; i < n; i++ {
			arr = append(arr, fmt.Sprintf("user%02d", i))
		}
		b, _ := json.Marshal(arr)
		body := `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","verdict":{"enabled":true,"request_reviewers":` + string(b) + `,"audited_sha":"abc123def"}}`
		w := httptest.NewRecorder()
		s.handleForgePublishReview(w, publishReq("tok1", body))
		if n <= 50 && w.Code != 200 {
			t.Errorf("%d reviewers: code = %d, want 200 (the cap is inclusive)", n, w.Code)
		}
		if n > 50 && w.Code != 400 {
			t.Errorf("%d reviewers: code = %d, want 400", n, w.Code)
		}
	}
}

// F-a: case and separator variants of dry_run never execute.
func TestForgePublishVerdict_DryRunVariantsNeverExecute(t *testing.T) {
	for _, mode := range []string{"DRY_RUN", "Dry_Run", "dry-run", "DRY_run", " dry_run "} {
		s, _ := newForgePublishTestServer(t)
		registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r",
			Capabilities: []string{publishCapApprove, publishCapMerge}})
		ops := &fakeMergeOps{}
		wireVerdict(t, s, &fakeGateClient{headSHA: "abc123def"}, ops)
		dry := `{"enabled":true,"approve":true,"arm_merge_when_pipeline_succeeds":true,"audited_sha":"abc123def","mode":"` + mode + `"}`
		w := httptest.NewRecorder()
		s.handleForgePublishReview(w, publishReq("tok1", verdictBody(enabledGate, dry)))
		var res publishReviewResponse
		_ = json.NewDecoder(w.Body).Decode(&res)
		if res.Verdict == nil || res.Verdict.Refused == "" {
			t.Fatalf("mode %q must refuse, got %+v", mode, res.Verdict)
		}
		if ops.approveCalls != 0 || ops.armCalls != 0 {
			t.Fatalf("mode %q executed gestures: approve=%d arm=%d", mode, ops.approveCalls, ops.armCalls)
		}
	}
}
