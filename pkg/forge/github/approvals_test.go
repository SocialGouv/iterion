package github

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

// approvals_test pins the GitHub twins of the merge-gate gestures: approval
// as an APPROVE review event, auto-merge arming through the GraphQL mutation
// with the expectedHeadOid guard, and the read-then-diff reviewer request.

func newApprovalsTestClient(t *testing.T, h http.Handler) *AdminClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
}

func TestApprovePullRequest_PostsAnApproveEvent(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	c := newApprovalsTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "html_url": "https://gh/o/r/pull/7#event"})
	}))
	res, err := c.ApprovePullRequest(context.Background(), "o/r", 7, "abc123def")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotPath, "/pulls/7/reviews") {
		t.Errorf("approve path = %q, want .../pulls/7/reviews", gotPath)
	}
	if gotBody["event"] != "APPROVE" {
		t.Errorf("review event = %v, want APPROVE", gotBody["event"])
	}
	if res.URL == "" {
		t.Error("approve result carries no url")
	}
}

func TestApprovePullRequest_ForbiddenIsTypedPermission(t *testing.T) {
	c := newApprovalsTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	_, err := c.ApprovePullRequest(context.Background(), "o/r", 7, "")
	var perr *forge.PermissionError
	if !errors.As(err, &perr) {
		t.Fatalf("a 403 approve must surface as *forge.PermissionError, got %T %v", err, err)
	}
}

// armServer is the two-endpoint fake the arming tests share: the REST node-id
// fetch plus the GraphQL mutation, both served from one httptest host.
func armServer(t *testing.T, record *[]string, graphql func(w http.ResponseWriter, r *http.Request)) *AdminClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		*record = append(*record, "REST "+r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"node_id": "PRNodeId9", "html_url": "https://gh/o/r/pull/7"})
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		*record = append(*record, "GraphQL "+r.URL.Path)
		graphql(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
}

func TestArmAutoMerge_SendsExpectedHeadOidAndReportsArmed(t *testing.T) {
	var calls []string
	var gotVars map[string]any
	var gotQuery string
	c := armServer(t, &calls, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery, gotVars = req.Query, req.Variables
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"enablePullRequestAutoMerge": map[string]any{
					"pullRequest": map[string]any{"state": "OPEN", "merged": false},
				},
			},
		})
	})
	res, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{
		Method: forge.MergeSquash, SHA: "abc123defabc123defabc123defabc123defabc1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "enablePullRequestAutoMerge") {
		t.Errorf("mutation = %q, want enablePullRequestAutoMerge", gotQuery)
	}
	if gotVars["oid"] != "abc123defabc123defabc123defabc123defabc1" {
		t.Errorf("expectedHeadOid = %v, want the audited sha", gotVars["oid"])
	}
	if gotVars["method"] != "SQUASH" {
		t.Errorf("mergeMethod = %v, want SQUASH", gotVars["method"])
	}
	if res.State != forge.AutoMergeArmed {
		t.Errorf("state = %q, want mwps", res.State)
	}
	if res.URL != "https://gh/o/r/pull/7" {
		t.Errorf("url = %q, want the PR html url the node-id read returned", res.URL)
	}
}

func TestArmAutoMerge_MapsStaleOidToTheTypedSentinel(t *testing.T) {
	var calls []string
	c := armServer(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":   map[string]any{},
			"errors": []map[string]any{{"type": "UNPROCESSABLE", "message": "Expected head oid to be abc123. However head oid is currently def456."}},
		})
	})
	_, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: "abc123def"})
	if !errors.Is(err, forge.ErrStaleHead) {
		t.Fatalf("a stale expectedHeadOid must map to forge.ErrStaleHead, got %v", err)
	}
}

func TestArmAutoMerge_ContractRefusesAnUnpinnedArming(t *testing.T) {
	c := newApprovalsTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the forge must never be called for an unpinned arming")
	}))
	if _, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: ""}); err == nil {
		t.Fatal("an unpinned arming is refused by contract, got nil error")
	}
}

func TestAddPullReviewers_ReadsThenRequestsOnlyTheMissing(t *testing.T) {
	var postBody map[string]any
	postCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/7/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []map[string]any{{"login": "alice"}}})
		case http.MethodPost:
			postCalled = true
			_ = json.NewDecoder(r.Body).Decode(&postBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []map[string]any{{"login": "bob"}}})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}

	added, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"Alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if !postCalled {
		t.Fatal("the request-reviewers write never happened")
	}
	reviewers, _ := postBody["reviewers"].([]any)
	if len(reviewers) != 1 || reviewers[0] != "bob" {
		t.Errorf("reviewers = %v, want [bob] only — alice is already requested and must not be re-sent", postBody["reviewers"])
	}
	if len(added) != 1 || added[0] != "bob" {
		t.Errorf("added = %v, want [bob]", added)
	}
}

func TestAddPullReviewers_AllPresentWritesNothing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/7/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a fully-present reviewer set must write nothing, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": []map[string]any{{"login": "bob"}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}

	added, err := c.AddPullReviewers(context.Background(), "o/r", 7, []string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want empty", added)
	}
}

func TestGetMergeability_TriStateAndDiagnostics(t *testing.T) {
	cases := []struct {
		name string
		pr   map[string]any
		want string
	}{
		{"mergeable", map[string]any{"mergeable": true, "mergeable_state": "clean"}, forge.MergeabilityMergeable},
		{"blocked", map[string]any{"mergeable": false, "mergeable_state": "blocked"}, forge.MergeabilityBlocked},
		{"computing", map[string]any{"mergeable": nil, "mergeable_state": "unknown"}, forge.MergeabilityPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newApprovalsTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tc.pr)
			}))
			m, err := c.GetMergeability(context.Background(), "o/r", 7)
			if err != nil {
				t.Fatal(err)
			}
			if m.MergeStatus != tc.want {
				t.Errorf("merge status = %q, want %q", m.MergeStatus, tc.want)
			}
			if m.ApprovalsLeft != -1 {
				t.Errorf("approvals = %d, want -1 (GitHub does not report a count here)", m.ApprovalsLeft)
			}
		})
	}
}

func TestArmAutoMerge_ClassifiesTheKnownRefusals(t *testing.T) {
	cases := []struct {
		name    string
		message string
		wantErr error
	}{
		{"clean status", "Pull request is in clean status", forge.ErrNotMergeable},
		{"head sha mismatch", "Head sha was not equal to the current head sha", forge.ErrStaleHead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			c := armServer(t, &calls, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data":   map[string]any{},
					"errors": []map[string]any{{"type": "UNPROCESSABLE", "message": tc.message}},
				})
			})
			_, err := c.ArmAutoMerge(context.Background(), "o/r", 7, forge.AutoMergeOptions{SHA: "abc123def"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("message %q → %v, want errors.Is %v", tc.message, err, tc.wantErr)
			}
		})
	}
}
