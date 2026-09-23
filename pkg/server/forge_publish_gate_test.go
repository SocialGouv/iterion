package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	// headRepo overrides HeadRepoFullName on the returned PullRef. Empty
	// defaults to the base repo the endpoint is called with (i.e. a
	// same-repo PR), so pre-#642 fixtures pass the fork-guard fail-CLOSED
	// check without editing every callsite; a fork test sets it explicitly.
	headRepo string
	// noHeadRepo returns an EMPTY HeadRepoFullName — the shape a provider
	// emits once a fork was deleted or blocked (head.repo: null).
	noHeadRepo bool
	// state is the PR state the stub reports; empty reads as open.
	state string
}

func (f *fakeGateClient) GetPullRequest(_ context.Context, repo string, _ int) (forge.PullRef, error) {
	head := f.headRepo
	if head == "" && !f.noHeadRepo {
		head = repo
	}
	return forge.PullRef{HeadSHA: f.headSHA, HeadRepoFullName: head, State: f.state}, f.getErr
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

// A verdict on a pull request that already merged names a revision nobody
// merges any more: the check is gone from the merge decision, the head the
// status lands on is pre-merge, and the branch it describes is scheduled for
// deletion. Observed in production (run 01a07840): a fixer kept working for
// half an hour past the squash and posted `revi/review=success` on the
// pre-merge head. The chokepoint is here — every campaign bot's gate status
// crosses postGateStatus, which already resolves the pull request.
func TestForgePublishReview_GateRefusedOnAClosedPullRequest(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			gc := &fakeGateClient{headSHA: "deadbeefcafe", state: state}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"context":"revi/review","blocking_count":0}`)))
			if w.Code != http.StatusOK {
				t.Fatalf("the review itself still lands: code=%d body=%s", w.Code, w.Body.String())
			}
			var resp publishReviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if gc.setCalls != 0 {
				t.Fatalf("no status may be written on a %s pull request's head, got %d write(s): %+v", state, gc.setCalls, gc.last)
			}
			if resp.GatePosted {
				t.Fatalf("gate_posted must be false on a %s pull request: %+v", state, resp)
			}
			if !strings.Contains(resp.GateError, state) {
				t.Fatalf("gate_error must name the state the bot's tail has to route on, got %q", resp.GateError)
			}
			if !resp.Published {
				t.Fatalf("the review comment is the one thing still worth posting: %+v", resp)
			}
		})
	}
}

// A verdict is a statement about the revision the bot READ. The endpoint
// resolves the head itself, so without a pin the two can differ: the bot audits
// A, a push lands B, and A's verdict certifies B. With a required check, zero
// required approvals and auto-merge armed, that is how an unaudited revision
// reaches the default branch.
func TestForgePublishReview_GateRefusedWhenTheHeadMovedSinceTheAudit(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	// The bot audited "aaaa11112222"; by the time it publishes, the head is "bbbb33334444".
	gc := &fakeGateClient{headSHA: "bbbb33334444"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(
		`{"enabled":true,"context":"revi/review","blocking_count":0,"audited_sha":"aaaa11112222"}`)))

	if w.Code != http.StatusOK {
		t.Fatalf("the review itself still lands: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if gc.setCalls != 0 {
		t.Fatalf("no status may certify a revision nobody audited, got %d write(s): %+v", gc.setCalls, gc.last)
	}
	if resp.GatePosted {
		t.Fatalf("gate_posted must be false when the head moved: %+v", resp)
	}
	// Both revisions belong in the reason: one names what was judged, the other
	// what the forge would have certified.
	if !strings.Contains(resp.GateError, "aaaa1111") || !strings.Contains(resp.GateError, "bbbb3333") {
		t.Fatalf("gate_error must name the audited revision AND the current head, got %q", resp.GateError)
	}
	if !resp.Published {
		t.Fatalf("the review comment is the one thing still worth posting: %+v", resp)
	}
}

func TestForgePublishReview_GatePostedWhenTheAuditedSHAIsTheHead(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "deadbeefcafe"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(
		`{"enabled":true,"context":"revi/review","blocking_count":0,"audited_sha":"deadbeefcafe"}`)))

	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.GatePosted || resp.GateState != "success" || resp.GateSHA != "deadbeefcafe" {
		t.Fatalf("a pin that MATCHES must post exactly as before: %+v (gate_error=%q)", resp, resp.GateError)
	}
	if resp.GateSHAUnpinned {
		t.Fatalf("a request carrying audited_sha is pinned, not unpinned: %+v", resp)
	}
	if gc.setCalls != 1 {
		t.Fatalf("SetCommitStatus calls = %d, want 1", gc.setCalls)
	}
}

// The pin is compared as a commit id, not as a string. The producing bundle's
// own validity predicate accepts an abbreviation (`looks_like_sha`: 7+ hex), so
// a full-string compare would refuse the RIGHT commit whenever a bot sent a
// short one — permanently, since nothing else fills the check.
func TestForgePublishReview_GateAcceptsAnAbbreviatedPinOfTheSameCommit(t *testing.T) {
	const head = "1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"
	for _, pin := range []string{"1e2a3b4", "1e2a3b4c5d6e", "1E2A3B4C5D6E", head, strings.ToUpper(head)} {
		t.Run(pin, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			gc := &fakeGateClient{headSHA: head}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(
				`{"enabled":true,"context":"revi/review","blocking_count":0,"audited_sha":"`+pin+`"}`)))
			var resp publishReviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.GatePosted {
				t.Fatalf("pin %q names the head and must post: gate_error=%q", pin, resp.GateError)
			}
			// The status lands on the FORGE's spelling, never on the caller's.
			if gc.lastSHA != head {
				t.Fatalf("status posted on %q, want the forge's head %q", gc.lastSHA, head)
			}
		})
	}
}

// A pin that is PRESENT and unreadable is a third state. Collapsing it into
// "absent" degrades silently to the unpinned certificate (the unsubstituted
// template this repo has paid for before); collapsing it into "the head moved"
// sends the reader hunting a push that never happened.
func TestForgePublishReview_GateRefusesAnUnreadablePinAsItsOwnFault(t *testing.T) {
	for _, pin := range []string{
		"{{outputs.prepare.head_sha}}", "null", "HEAD", "refs/heads/main", "none",
		" ", "\n", "abc", "zzzzzzzz", "1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4cff",
	} {
		t.Run(strconv.Quote(pin), func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			gc := &fakeGateClient{headSHA: "1e2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c"}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
			w := httptest.NewRecorder()
			body, _ := json.Marshal(map[string]any{"enabled": true, "context": "revi/review", "blocking_count": 0, "audited_sha": pin})
			s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(string(body))))
			var resp publishReviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if gc.setCalls != 0 || resp.GatePosted {
				t.Fatalf("an unreadable pin must certify nothing, got posted=%v calls=%d", resp.GatePosted, gc.setCalls)
			}
			if resp.GateSHAUnpinned {
				t.Fatal("a pin that was SENT is not an absent pin — reporting it unpinned hides a bot that meant to pin and rendered garbage")
			}
			if !strings.Contains(resp.GateError, "cannot be read") {
				t.Fatalf("the reason must blame the pin, not invent a push: %q", resp.GateError)
			}
		})
	}
}

// The refusal's whole job is to tell two revisions apart. Abbreviating both
// sides is how it names the same one twice.
func TestForgePublishReview_GateMismatchNamesBothRevisionsInFull(t *testing.T) {
	const audited = "abcdef012345" + "1111111111111111111111111111"
	const head = "abcdef012345" + "2222222222222222222222222222"
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: head}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(
		`{"enabled":true,"context":"revi/review","blocking_count":0,"audited_sha":"`+audited+`"}`)))
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.GateError, audited) || !strings.Contains(resp.GateError, head) {
		t.Fatalf("two revisions sharing their first 12 characters must both appear in full, got %q", resp.GateError)
	}
}

// Until every bundle in the fleet sends the pin, an absent audited_sha still
// posts — refusing outright would blank the required check on every repo whose
// bundle predates this change. It is NOT silent: the response says so and the
// server logs it, and that signal is what makes flipping the default to a
// refusal a measurable decision instead of a blind one.
func TestForgePublishReview_GateWithoutAnAuditedSHAIsReportedUnpinned(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "deadbeefcafe"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(
		`{"enabled":true,"context":"revi/review","blocking_count":0}`)))

	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.GatePosted || resp.GateSHA != "deadbeefcafe" {
		t.Fatalf("an unpinned gate still posts today: %+v", resp)
	}
	if !resp.GateSHAUnpinned {
		t.Fatalf("an unpinned gate must SAY it is unpinned, else the fleet's readiness is unmeasurable: %+v", resp)
	}
}

// An empty state is a provider that does not report one, never a closure:
// suppressing a required check on a guess is how a pull request deadlocks.
func TestForgePublishReview_GatePostedWhenTheStateIsUnknown(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	gc := &fakeGateClient{headSHA: "deadbeefcafe"} // state "" — unreported
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"blocking_count":0}`)))
	if gc.setCalls != 1 {
		t.Fatalf("an unreported state must not suppress the verdict, writes=%d", gc.setCalls)
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
