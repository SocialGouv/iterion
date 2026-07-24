package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
)

// The real GitHub clients must satisfy the server's merge-gate client — both
// the raw-token AdminClient and the production GitHub App client — so
// gateClientFor never silently degrades to "no capability" on the prod path.
var (
	_ forgeGateClient = (*forgegithub.AdminClient)(nil)
	_ forgeGateClient = (*forgegithub.AppClient)(nil)
)

// fakeGateClient records the merge-gate calls (head-SHA lookup + commit-status
// write) — the seam the gate tests use instead of a live forge.
type fakeGateClient struct {
	headSHA  string
	getErr   error
	setErr   error
	last     forge.CommitStatus
	lastSHA  string
	setCalls int
}

func (f *fakeGateClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{HeadSHA: f.headSHA}, f.getErr
}

func (f *fakeGateClient) SetCommitStatus(_ context.Context, _, sha string, st forge.CommitStatus) error {
	f.setCalls++
	f.lastSHA, f.last = sha, st
	return f.setErr
}

func publishBodyWithGate(gate string) string {
	return `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[],"gate":` + gate + `}`
}

func TestForgePublishReview_GateSuccessAndFailure(t *testing.T) {
	cases := []struct {
		name      string
		gate      string
		wantState string
	}{
		{"clean passes", `{"enabled":true,"blocking_count":0,"threshold":"high","total_findings":2}`, "success"},
		{"blocking fails", `{"enabled":true,"blocking_count":2,"threshold":"high","total_findings":5}`, "failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			gc := &fakeGateClient{headSHA: "deadbeefcafe"}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(tc.gate)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var resp publishReviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.GatePosted || resp.GateState != tc.wantState || resp.GateContext != "revi/review" || resp.GateSHA != "deadbeefcafe" {
				t.Fatalf("gate response wrong: %+v", resp)
			}
			if gc.setCalls != 1 || gc.lastSHA != "deadbeefcafe" || string(gc.last.State) != tc.wantState || gc.last.Context != "revi/review" {
				t.Fatalf("SetCommitStatus wrong: calls=%d sha=%q state=%q ctx=%q", gc.setCalls, gc.lastSHA, gc.last.State, gc.last.Context)
			}
			if gc.last.TargetURL == "" {
				t.Fatal("gate status must link to the review as evidence")
			}
		})
	}
}

func TestForgePublishReview_GateAbsentPostsNoStatus(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	// No gate field at all → advisory-only, no status.
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp publishReviewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.GatePosted || gc.setCalls != 0 {
		t.Fatalf("absent gate must post nothing: posted=%v calls=%d", resp.GatePosted, gc.setCalls)
	}

	// gate.enabled=false → still nothing.
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":false,"blocking_count":9}`)))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if gc.setCalls != 0 {
		t.Fatalf("disabled gate must post nothing: calls=%d", gc.setCalls)
	}
}

func TestForgePublishReview_GateFailureIsNonFatal(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	// SetCommitStatus fails → the review still published (200 + Published),
	// GatePosted false, GateError explains. Never fails the publish.
	gc := &fakeGateClient{headSHA: "abc", setErr: forge.StatusErr("github", "set commit status", 403)}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("gate failure must not fail the publish: status=%d", w.Code)
	}
	var resp publishReviewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Published || resp.GatePosted || resp.GateError == "" {
		t.Fatalf("expected published+gate-error, got %+v", resp)
	}

	// Provider without commit-status capability → reported, non-fatal.
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return nil, nil }
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Published || resp.GatePosted || resp.GateError == "" {
		t.Fatalf("no-capability gate must report non-fatally: %+v", resp)
	}
}

func TestForgePublishReview_GateMissingHeadSHA(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: ""} // forge returns no head sha
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
	var resp publishReviewResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.GatePosted || gc.setCalls != 0 || resp.GateError == "" {
		t.Fatalf("missing head sha must skip status with an error: %+v (calls=%d)", resp, gc.setCalls)
	}
}
