package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
)

// Every real forge admin client must satisfy the server's merge-gate client
// (GetPullRequest + SetCommitStatus) — including the production GitHub App
// client — so gateClientFor never silently degrades to "no capability" and
// deadlocks a required check. Asserting the two-method forgeGateClient here
// (not just forge.CommitStatusClient in each provider) guards a GetPullRequest
// signature drift on any provider.
var (
	_ forgeGateClient = (*forgegithub.AdminClient)(nil)
	_ forgeGateClient = (*forgegithub.AppClient)(nil)
	_ forgeGateClient = (*forgegitlab.AdminClient)(nil)
	_ forgeGateClient = (*forgeforgejo.AdminClient)(nil)
)

// ReviewerAssigner (the reviewer self-assign behind the re-request-review
// button) is gitlab-only BY DESIGN: GitHub lists a review's author as
// reviewer by itself, a GitHub App cannot be a PR reviewer at all, and
// Forgejo is an accepted gap. This pins the NEGATIVE — an accidental
// AddSelfAsPullReviewer on another client would silently start
// self-assigning (and on a GitHub App, erroring) at every publish.
func TestReviewerAssignerCapabilityIsGitLabOnly(t *testing.T) {
	if _, ok := any(&forgegithub.AdminClient{}).(forge.ReviewerAssigner); ok {
		t.Error("github AdminClient must not implement forge.ReviewerAssigner (GitHub adds the review author as reviewer by itself)")
	}
	if _, ok := any(&forgegithub.AppClient{}).(forge.ReviewerAssigner); ok {
		t.Error("github AppClient must not implement forge.ReviewerAssigner (a GitHub App cannot be a PR reviewer)")
	}
	if _, ok := any(&forgeforgejo.AdminClient{}).(forge.ReviewerAssigner); ok {
		t.Error("forgejo AdminClient must not implement forge.ReviewerAssigner (accepted gap — wire the trigger docs first)")
	}
	if _, ok := any(&forgegitlab.AdminClient{}).(forge.ReviewerAssigner); !ok {
		t.Error("gitlab AdminClient must implement forge.ReviewerAssigner")
	}
}

// fakeGateClient records the merge-gate calls (head-SHA lookup + commit-status
// write) — the seam the gate tests use instead of a live forge.
type fakeGateClient struct {
	headSHA string
	getErr  error
	setErr  error
	last    forge.CommitStatus
	lastSHA string
	// posted keeps every status in order: several now land on one head (the
	// launch's in-flight claim, then the verdict — or a synthetic failure then
	// the recovery's fresh claim), and only the SEQUENCE distinguishes a
	// correct hand-off from a status that overwrote something it should not.
	posted   []forge.CommitStatus
	setCalls int
}

func (f *fakeGateClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{HeadSHA: f.headSHA}, f.getErr
}

func (f *fakeGateClient) SetCommitStatus(_ context.Context, _, sha string, st forge.CommitStatus) error {
	f.setCalls++
	f.lastSHA, f.last = sha, st
	f.posted = append(f.posted, st)
	return f.setErr
}

func publishBodyWithGate(gate string) string {
	return `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[],"gate":` + gate + `}`
}

func TestForgePublishReview_GateDefaultContextIsNeutral(t *testing.T) {
	// A gate arriving with an EMPTY context must fall back to the bot-agnostic
	// default, never a specific bot's persona name.
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
	if gc.last.Context != defaultGateContext {
		t.Fatalf("empty gate context must default to %q, got %q", defaultGateContext, gc.last.Context)
	}
}

func TestForgePublishReview_GateSuccessAndFailure(t *testing.T) {
	cases := []struct {
		name      string
		gate      string
		wantState string
	}{
		{"clean passes", `{"enabled":true,"context":"revi/review","blocking_count":0,"threshold":"high","total_findings":2}`, "success"},
		{"blocking fails", `{"enabled":true,"context":"revi/review","blocking_count":2,"threshold":"high","total_findings":5}`, "failure"},
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

// fakeReviewerAssigner records self-assign calls — the seam behind the
// forge-native re-request-review button. Concurrency-safe: the production
// call site runs detached behind the response.
type fakeReviewerAssigner struct {
	mu    sync.Mutex
	calls int
	repo  string
	num   int
	err   error
	block chan struct{} // when non-nil, the call parks here first
	done  chan struct{} // signalled once per completed call
}

func (f *fakeReviewerAssigner) AddSelfAsPullReviewer(_ context.Context, repo string, number int) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.repo, f.num = repo, number
	if f.done != nil {
		f.done <- struct{}{}
	}
	return f.err
}

// A successful publish self-assigns the bot as reviewer (what makes the
// re-request-review button exist on the PR) — strictly BEHIND the response:
// the call is detached, so a slow/failing/absent assigner never delays the
// publish response nor the merge-gate status.
//
// This also closes the SECOND open item from SocialGouv/iterion#621 (the
// re-request affordance re-arming after a NOTE-triggered `/revi` review, not
// only after the reviewer-request button): handleForgePublishReview has no
// notion of which webhook lane produced the review it is publishing — the
// self-assign call fires unconditionally on every successful publish, so a
// dedicated "note-triggered" variant of this test would exercise the exact
// same code path under a different label. The genuinely open question — does
// GitLab's OWN sidebar visually re-arm the button once the bot holds the
// reviewer role again — is UI state on the forge's side, outside anything
// iterion's code decides; it is confirmed at the next real click.
func TestForgePublishReview_SelfAssignsReviewer(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	fra := &fakeReviewerAssigner{done: make(chan struct{}, 8)}
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return fra }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-fra.done:
	case <-time.After(5 * time.Second):
		t.Fatal("self-assign never ran after the publish")
	}
	fra.mu.Lock()
	if fra.calls != 1 || fra.repo != "o/r" || fra.num != 42 {
		t.Fatalf("self-assign not called with the PR: calls=%d repo=%q num=%d", fra.calls, fra.repo, fra.num)
	}
	fra.err = context.DeadlineExceeded
	fra.mu.Unlock()

	// A forge refusal is best-effort — the publish already landed.
	w2 := httptest.NewRecorder()
	s.handleForgePublishReview(w2, publishReq("tok1", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[]}`))
	if w2.Code != http.StatusOK {
		t.Fatalf("self-assign failure must not degrade the publish: code=%d", w2.Code)
	}
	<-fra.done

	// Capability absent (nil assigner) — publish untouched.
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return nil }
	w3 := httptest.NewRecorder()
	s.handleForgePublishReview(w3, publishReq("tok1", `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[]}`))
	if w3.Code != http.StatusOK {
		t.Fatalf("absent capability must not degrade the publish: code=%d", w3.Code)
	}
}

// The regression this ordering exists for: a HUNG self-assign (a stalled
// forge) must not sit in front of the required merge-gate status or the
// publish response. Before the fix the call ran synchronously between the
// review post and the gate post, so this test hung and the gate was hostage
// to a cosmetic call.
func TestForgePublishReview_SelfAssignNeverBlocksGateOrResponse(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	fra := &fakeReviewerAssigner{block: make(chan struct{}), done: make(chan struct{}, 1)}
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return fra }

	respDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
		respDone <- w
	}()
	var w *httptest.ResponseRecorder
	select {
	case w = <-respDone:
	case <-time.After(5 * time.Second):
		t.Fatal("publish response is hostage to a hung self-assign")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 1 || gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("gate must be posted before/despite the hung self-assign: setCalls=%d last=%+v", gc.setCalls, gc.last)
	}
	close(fra.block) // release the parked goroutine
	<-fra.done
}
