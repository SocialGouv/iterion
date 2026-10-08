package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// approvals_test pins the merge-gate gestures on the wire: the exact bodies
// GitLab must receive (merge_when_pipeline_succeeds + sha guard, reviewer
// union), the idempotency contract (duplicate approve = confirmed no-op),
// and the typed refusals (405 not-mergeable, 406 stale head, 403 eligibility).

func newApprovalsTestClient(t *testing.T, h http.HandlerFunc) *AdminClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL, "tok")
}

func TestApprovePullRequest_PinsTheAuditedSHA(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "https://gl/o/r/-/merge_requests/7"})
	})
	res, err := c.ApprovePullRequest(context.Background(), "o/r", 7, "abc123defabc123defabc123defabc123defabc1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/merge_requests/7/approve") {
		t.Errorf("approve path = %q, want .../merge_requests/7/approve", gotPath)
	}
	if gotBody["sha"] != "abc123defabc123defabc123defabc123defabc1" {
		t.Errorf("approve body sha = %v, want the audited sha pin", gotBody["sha"])
	}
	if res.URL != "https://gl/o/r/-/merge_requests/7" {
		t.Errorf("approve url = %q", res.URL)
	}
}

func TestApprovePullRequest_DuplicateIsAConfirmedNoOp(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"409 Cannot approve: already approved"}`))
	})
	if _, err := c.ApprovePullRequest(context.Background(), "o/r", 7, ""); err != nil {
		t.Fatalf("a duplicate approve must be the idempotent no-op the contract promises, got %v", err)
	}
}

func TestApprovePullRequest_ForbiddenIsTypedEligibility(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 forbidden"}`))
	})
	_, err := c.ApprovePullRequest(context.Background(), "o/r", 7, "")
	var perr *forge.PermissionError
	if !errors.As(err, &perr) {
		t.Fatalf("a 403 approve must surface as *forge.PermissionError (eligibility), got %T %v", err, err)
	}
}

func TestArmAutoMerge_SendsMWPSBodyAndReportsArmed(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	var gotMethod string
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted) // some versions answer 202 while recording the arming
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 7, "state": "opened", "web_url": "https://gl/o/r/-/merge_requests/7",
			"sha":                   "abc123defabc123defabc123defabc123defabc1",
			"detailed_merge_status": "merge_when_pipeline_succeeds",
		})
	})
	res, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{
		Method: forge.MergeSquash, SHA: "abc123defabc123defabc123defabc123defabc1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || !strings.HasSuffix(gotPath, "/merge_requests/7/merge") {
		t.Errorf("arm call = %s %q, want PUT .../merge", gotMethod, gotPath)
	}
	if gotBody["merge_when_pipeline_succeeds"] != true {
		t.Errorf("arm body merge_when_pipeline_succeeds = %v, want true", gotBody["merge_when_pipeline_succeeds"])
	}
	if gotBody["squash"] != true {
		t.Errorf("arm body squash = %v, want true for MergeSquash", gotBody["squash"])
	}
	if gotBody["sha"] != "abc123defabc123defabc123defabc123defabc1" {
		t.Errorf("arm body sha = %v, want the race guard pin", gotBody["sha"])
	}
	if res.State != forge.AutoMergeArmed {
		t.Errorf("arm state = %q, want %q", res.State, forge.AutoMergeArmed)
	}
}

func TestArmAutoMerge_PipelineAlreadyGreenReportsMerged(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "state": "merged", "web_url": "https://gl/o/r/-/merge_requests/7"})
	})
	res, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: "abc123def"})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != forge.AutoMergeMerged {
		t.Errorf("state = %q, want merged when the forge merged on the spot", res.State)
	}
}

func TestArmAutoMerge_ContractRefusesAnUnpinnedArming(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the forge must never be called for an unpinned arming")
	})
	if _, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: ""}); err == nil {
		t.Fatal("an unpinned arming is refused by contract, got nil error")
	}
}

func TestArmAutoMerge_MapsRefusalsToTypedSentinels(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		body    string
		wantErr error
	}{
		{"405 not mergeable", http.StatusMethodNotAllowed, `{"message":"405 Method Not Allowed"}`, forge.ErrNotMergeable},
		{"406 stale head", http.StatusNotAcceptable, `{"message":"SHA does not match HEAD of source branch"}`, forge.ErrStaleHead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: "abc123def"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("arm refusal %d = %v, want errors.Is %v", tc.code, err, tc.wantErr)
			}
		})
	}
}

func TestArmAutoMerge_ForbiddenIsTypedPermission(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"403 forbidden"}`))
	})
	_, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: "abc123def"})
	var perr *forge.PermissionError
	if !errors.As(err, &perr) {
		t.Fatalf("a 403 arm must surface as *forge.PermissionError, got %T %v", err, err)
	}
}

func TestAddPullReviewers_UnionPreservesHumansAndReportsAdded(t *testing.T) {
	var putBody map[string]any
	putCalled := false
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 42, "username": "bob"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "reviewers": []map[string]any{{"id": 11}}})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			putCalled = true
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7})
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		}
	})
	added, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"@bob"})
	if err != nil {
		t.Fatal(err)
	}
	if !putCalled {
		t.Fatal("the reviewer union write never happened")
	}
	ids, _ := putBody["reviewer_ids"].([]any)
	if len(ids) != 2 || ids[0] != float64(11) || ids[1] != float64(42) {
		t.Errorf("reviewer_ids = %v, want [11 42] — the human stays, bob joins", putBody["reviewer_ids"])
	}
	if len(added) != 1 || added[0] != "bob" {
		t.Errorf("added = %v, want [bob]", added)
	}
}

func TestAddPullReviewers_AlreadyPresentWritesNothing(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 11, "username": "bob"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "reviewers": []map[string]any{{"id": 11}}})
		default:
			t.Errorf("a fully-present reviewer set must write nothing, got %s %s", r.Method, r.URL.Path)
		}
	})
	added, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want empty (already on the set)", added)
	}
}

func TestAddPullReviewers_RefusesAReplaceWriteItCannotProve(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7}) // no reviewers field at all
	})
	if _, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"bob"}); err == nil {
		t.Fatal("a response with no reviewers field must refuse the replace-write, got nil error")
	}
}

func TestAddPullReviewers_UnresolvableLoginFailsClosed(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			t.Errorf("no write may follow an unresolved login, got %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"nobody"}); err == nil {
		t.Fatal("an unresolvable login is an error (fail closed), got nil")
	}
}

func TestGetMergeability_DegradesWithoutDetailedStatusOrApprovals(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "merge_status": "can_be_merged"})
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			w.WriteHeader(http.StatusNotFound) // an instance without the approvals read
		}
	})
	m, err := c.GetMergeability(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if m.MergeStatus != forge.MergeabilityMergeable {
		t.Errorf("merge status = %q, want mergeable (legacy field)", m.MergeStatus)
	}
	if m.ApprovalsLeft != -1 || m.ApprovalsRequired != -1 {
		t.Errorf("approvals = %d/%d, want -1/-1 (the forge does not say)", m.ApprovalsRequired, m.ApprovalsLeft)
	}
}

func TestGetMergeability_DetailedStatusWins(t *testing.T) {
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"iid": 7, "merge_status": "can_be_merged", "detailed_merge_status": "discussions_not_resolved",
				"blocking_discussions_resolved": false,
			})
		case strings.HasSuffix(r.URL.Path, "/approvals"):
			_ = json.NewEncoder(w).Encode(map[string]any{"approvals_required": 2, "approvals_left": 1})
		}
	})
	m, err := c.GetMergeability(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if m.MergeStatus != forge.MergeabilityBlocked {
		t.Errorf("merge status = %q, want blocked (detailed wins over legacy)", m.MergeStatus)
	}
	if m.UnresolvedDiscussions != 1 {
		t.Errorf("unresolved = %d, want 1 (the forge reports a boolean)", m.UnresolvedDiscussions)
	}
	if m.ApprovalsRequired != 2 || m.ApprovalsLeft != 1 {
		t.Errorf("approvals = %d/%d, want 2/1", m.ApprovalsRequired, m.ApprovalsLeft)
	}
}

func TestAddPullReviewers_LoginQueryIsEscaped(t *testing.T) {
	var gotRawQuery string
	c := newApprovalsTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/users") {
			gotRawQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 42, "username": "alice&friends"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "reviewers": []map[string]any{}})
	})
	if _, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"alice&friends"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotRawQuery, "alice%26friends") {
		t.Errorf("query = %q, want the login escaped (alice%%26friends) — a raw & splits the query", gotRawQuery)
	}
}
