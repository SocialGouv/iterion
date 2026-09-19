package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// ReviewRequestWithdrawer is the MIRROR of ReviewerAssigner's pin: github-only
// BY DESIGN, on BOTH client halves (an App connection is the ordinary shape,
// and a capability implemented only on *AdminClient is invisible to the
// `admin.(forge.ReviewRequestWithdrawer)` the publish tail performs — which is
// exactly how this lane would be dead on the connection kind that needs it
// most). GitLab must NOT implement it: there the reviewer role is what makes
// the native re-request button exist, so withdrawing it would dismantle the
// affordance selfAssignReviewer just created.
func TestReviewRequestWithdrawerCapabilityIsGitHubOnly(t *testing.T) {
	if _, ok := any(&forgegithub.AdminClient{}).(forge.ReviewRequestWithdrawer); !ok {
		t.Error("github AdminClient must implement forge.ReviewRequestWithdrawer")
	}
	if _, ok := any(&forgegithub.AppClient{}).(forge.ReviewRequestWithdrawer); !ok {
		t.Error("github AppClient must implement forge.ReviewRequestWithdrawer — the App connection is the shape that needs it (the review is posted by <app_slug>[bot], so GitHub never lifts the request)")
	}
	if _, ok := any(&forgegitlab.AdminClient{}).(forge.ReviewRequestWithdrawer); ok {
		t.Error("gitlab AdminClient must not implement forge.ReviewRequestWithdrawer (its reviewer role IS the re-request button — see forge.ReviewerAssigner)")
	}
	if _, ok := any(&forgeforgejo.AdminClient{}).(forge.ReviewRequestWithdrawer); ok {
		t.Error("forgejo AdminClient must not implement forge.ReviewRequestWithdrawer (accepted gap — its re-request lane is not wired either)")
	}
}

// fakeReviewRequestWithdrawer records the withdrawal calls; block/done mirror
// fakeReviewerAssigner so the same ordering guarantees can be asserted.
type fakeReviewRequestWithdrawer struct {
	mu      sync.Mutex
	calls   int
	repo    string
	num     int
	logins  []string
	removed []string
	err     error
	block   chan struct{}
	done    chan struct{}
}

func (f *fakeReviewRequestWithdrawer) WithdrawPullReviewRequests(_ context.Context, repo string, number int, logins []string) ([]string, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.repo, f.num, f.logins = repo, number, logins
	if f.done != nil {
		f.done <- struct{}{}
	}
	return f.removed, f.err
}

// armReviewRequestLogins puts the integration row + webhook config the tail
// reads its armed logins from behind the publish grant.
func armReviewRequestLogins(t *testing.T, s *Server, logins []string) {
	t.Helper()
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r", WebhookID: "w1",
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "w1", TenantID: "team1", Provider: webhooks.ProviderGitHub, ReviewRequestLogins: logins,
	}); err != nil {
		t.Fatal(err)
	}
}

func publishGitHubPR(token string) *http.Request {
	return publishReq(token, `{"pr_url":"https://github.com/o/r/pull/42","summary":"s","comments":[]}`)
}

// The gesture becomes repeatable: a successful publish withdraws the armed
// logins' pending review request — the half GitHub will never perform itself,
// since the review is posted by <app_slug>[bot] and not by the requested
// account.
func TestForgePublishReview_WithdrawsArmedReviewRequests(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	// A pasted "@handle" must reach the forge as a login — the same
	// normalization the anti-loop actor guard applies, from one definition.
	armReviewRequestLogins(t, s, []string{"@iterion-bot", "  ", "revu-bot"})
	frw := &fakeReviewRequestWithdrawer{removed: []string{"iterion-bot"}, done: make(chan struct{}, 4)}
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishGitHubPR("tok1"))
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	select {
	case <-frw.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the review request was never withdrawn after the publish")
	}
	frw.mu.Lock()
	defer frw.mu.Unlock()
	if frw.calls != 1 || frw.repo != "o/r" || frw.num != 42 {
		t.Fatalf("withdrawal not called with the PR: calls=%d repo=%q num=%d", frw.calls, frw.repo, frw.num)
	}
	if !slices.Equal(frw.logins, []string{"iterion-bot", "revu-bot"}) {
		t.Fatalf("logins = %v, want the normalized armed set [iterion-bot revu-bot]", frw.logins)
	}
}

// Falsifiable #4: on a connection without the grant the withdrawal fails
// WITHOUT failing the review already published — the response keeps its
// review_url and the merge gate keeps its status.
func TestForgePublishReview_WithdrawalRefusalNeverDegradesThePublish(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	armReviewRequestLogins(t, s, []string{"iterion-bot"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	frw := &fakeReviewRequestWithdrawer{
		err: &forge.PermissionError{
			Provider: forge.ProviderGitHub, Op: "DELETE requested reviewers",
			Missing: []string{"pull_requests:write"}, Cause: forge.ErrForbidden,
		},
		done: make(chan struct{}, 1),
	}
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", publishBodyWithGate(`{"enabled":true,"context":"revi/review","blocking_count":0}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("a withdrawal refusal must not degrade the publish: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Published || resp.ReviewURL == "" {
		t.Fatalf("the published review must be reported intact: %+v", resp)
	}
	if !resp.GatePosted || resp.GateState != string(forge.CommitStateSuccess) {
		t.Fatalf("the merge gate must survive a withdrawal refusal: %+v", resp)
	}
	<-frw.done
}

// A HUNG withdrawal must not sit in front of the response or the required
// merge-gate status — the regression the detached tail exists for, now with a
// second call inside it.
func TestForgePublishReview_WithdrawalNeverBlocksGateOrResponse(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	armReviewRequestLogins(t, s, []string{"iterion-bot"})
	gc := &fakeGateClient{headSHA: "abc"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	frw := &fakeReviewRequestWithdrawer{block: make(chan struct{}), done: make(chan struct{}, 1)}
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }

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
		t.Fatal("publish response is hostage to a hung review-request withdrawal")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 1 || gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("gate must be posted despite the hung withdrawal: setCalls=%d last=%+v", gc.setCalls, gc.last)
	}
	close(frw.block)
	<-frw.done
}

// slowAssigner is a reviewer self-assign that takes measurable time and hands
// back the deadline it was given — the fixture for proving the two halves of
// the tail hold SEPARATE budgets.
type slowAssigner struct {
	took     time.Duration
	deadline chan time.Time
}

func (a *slowAssigner) AddSelfAsPullReviewer(ctx context.Context, _ string, _ int) error {
	d, _ := ctx.Deadline()
	time.Sleep(a.took)
	a.deadline <- d
	return nil
}

// The two halves of the reviewer-settle tail take their own deadline off one
// detached parent, so a self-assign that consumed its budget cannot starve
// the withdrawal. Proved on the DEADLINES, not on a 30-second wait: one
// shared context hands both calls the identical instant, two contexts hand
// the second one an instant later by however long the first took.
func TestForgePublishReview_ReviewerSettleHalvesHoldSeparateBudgets(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	armReviewRequestLogins(t, s, []string{"iterion-bot"})
	const slow = 5 * time.Millisecond
	fra := &slowAssigner{took: slow, deadline: make(chan time.Time, 1)}
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return fra }
	wDeadline := make(chan time.Time, 1)
	wCtxErr := make(chan error, 1)
	s.forgeReviewRequestWithdrawerFor = func(ctx context.Context, _ forge.Connection) forge.ReviewRequestWithdrawer {
		d, _ := ctx.Deadline()
		wDeadline <- d
		wCtxErr <- ctx.Err()
		return &fakeReviewRequestWithdrawer{}
	}

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishGitHubPR("tok1"))
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}

	var saDeadline time.Time
	select {
	case saDeadline = <-fra.deadline:
	case <-time.After(5 * time.Second):
		t.Fatal("the self-assign never ran")
	}
	select {
	case wd := <-wDeadline:
		if !wd.After(saDeadline) {
			t.Fatalf("the withdrawal shares the self-assign's deadline (%s vs %s) — a hung self-assign would hand it a dead context",
				wd, saDeadline)
		}
		if gap := wd.Sub(saDeadline); gap < slow {
			t.Fatalf("the withdrawal's budget starts %s after the self-assign's, want at least the %s the self-assign took", gap, slow)
		}
		if err := <-wCtxErr; err != nil {
			t.Fatalf("the withdrawal got a context that is already done: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the withdrawal never ran after the self-assign")
	}
}

// The lane not armed — no logins, no integration row, no webhook config: the
// tail must cost ZERO forge round-trips. This is the overwhelming majority of
// publishes (the discovery note of 2026-09-17 found the lane armed on none of
// the fleet's 15 GitHub bindings).
func TestForgePublishReview_UnarmedLaneMakesNoForgeCall(t *testing.T) {
	cases := []struct {
		name string
		arm  func(t *testing.T, s *Server)
	}{
		{"no stores at all", func(*testing.T, *Server) {}},
		{"empty review_request_logins", func(t *testing.T, s *Server) { armReviewRequestLogins(t, s, nil) }},
		{"only blank logins", func(t *testing.T, s *Server) { armReviewRequestLogins(t, s, []string{" ", "@"}) }},
		{"webhook config of another team", func(t *testing.T, s *Server) {
			armReviewRequestLogins(t, s, []string{"iterion-bot"})
			s.webhookConfigs = webhooks.NewMemoryConfigStore()
			if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
				ID: "w1", TenantID: "other-team", ReviewRequestLogins: []string{"iterion-bot"},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		// An integration carrying NO webhook must short-circuit on its own,
		// not lean on the config store answering ErrNotFound for "". The
		// store keys on Config.ID, so a row with an empty id is storable and
		// Get("") returns it — without the WebhookID guard this arms the
		// withdrawal from a config that belongs to no webhook at all.
		{"integration row without a webhook", func(t *testing.T, s *Server) {
			ints := forge.NewMemoryRepoIntegrationStore()
			if err := ints.Create(context.Background(), forge.RepoIntegration{
				ID: "i1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r",
			}); err != nil {
				t.Fatal(err)
			}
			s.forgeIntegrations = ints
			s.webhookConfigs = webhooks.NewMemoryConfigStore()
			if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
				ID: "", TenantID: "team1", Provider: webhooks.ProviderGitHub,
				ReviewRequestLogins: []string{"iterion-bot"},
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			tc.arm(t, s)
			frw := &fakeReviewRequestWithdrawer{done: make(chan struct{}, 1)}
			s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }
			// The self-assign runs first in the same goroutine; its
			// completion is the fence proving the tail reached the
			// withdrawal's decision point.
			fra := &fakeReviewerAssigner{done: make(chan struct{}, 1)}
			s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return fra }

			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishGitHubPR("tok1"))
			if w.Code != http.StatusOK {
				t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
			}
			select {
			case <-fra.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the reviewer-settle tail never ran")
			}
			select {
			case <-frw.done:
				t.Fatal("an unarmed lane must issue no withdrawal at all")
			case <-time.After(200 * time.Millisecond):
			}
			frw.mu.Lock()
			defer frw.mu.Unlock()
			if frw.calls != 0 {
				t.Fatalf("calls=%d, want 0 — an unarmed lane must cost no forge round-trip", frw.calls)
			}
		})
	}
}

// The tail's guards must hold on their OWN, not lean on goSafe's recover.
// Called DIRECTLY, outside the goroutine: inside it a nil-store dereference
// is recovered and logged, which every other test here reads as a correct
// "nothing to withdraw" skip — a panicking guard and a working guard are
// indistinguishable through the handler. Measured: removing the nil-store
// check leaves the whole suite green when this test is absent.
func TestWithdrawReviewRequests_GuardsHoldWithoutGoSafe(t *testing.T) {
	grant := ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"}
	conn := forge.Connection{ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub}

	cases := []struct {
		name string
		arm  func(t *testing.T, s *Server)
		want int
	}{
		{"both stores nil", func(*testing.T, *Server) {}, 0},
		{"integration store only", func(t *testing.T, s *Server) {
			s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
		}, 0},
		{"config store only", func(t *testing.T, s *Server) {
			s.webhookConfigs = webhooks.NewMemoryConfigStore()
		}, 0},
		// The positive control: without it the three above could be passing
		// because the harness never reaches the forge at all.
		{"fully armed", func(t *testing.T, s *Server) { armReviewRequestLogins(t, s, []string{"iterion-bot"}) }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newForgePublishTestServer(t)
			s.forgeIntegrations, s.webhookConfigs = nil, nil
			tc.arm(t, s)
			frw := &fakeReviewRequestWithdrawer{}
			s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }

			// A panic here fails the test outright — which is the point.
			s.withdrawReviewRequests(context.Background(), conn, grant, 42)

			frw.mu.Lock()
			defer frw.mu.Unlock()
			if frw.calls != tc.want {
				t.Fatalf("calls=%d, want %d", frw.calls, tc.want)
			}
		})
	}
}

// The capability absent (a provider that deliberately does not implement it)
// must leave the publish untouched, exactly like the self-assign's miss.
func TestForgePublishReview_WithdrawalCapabilityAbsentIsClean(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	armReviewRequestLogins(t, s, []string{"iterion-bot"})
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return nil }
	fra := &fakeReviewerAssigner{done: make(chan struct{}, 1)}
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return fra }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishGitHubPR("tok1"))
	if w.Code != http.StatusOK {
		t.Fatalf("absent capability must not degrade the publish: code=%d", w.Code)
	}
	select {
	case <-fra.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reviewer-settle tail never ran")
	}
}

// A FAILED publish withdraws nothing: the request must stay pending so the
// standing ask still names a review that never landed.
func TestForgePublishReview_FailedPublishWithdrawsNothing(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	armReviewRequestLogins(t, s, []string{"iterion-bot"})
	fake.err = errors.New("forge down")
	frw := &fakeReviewRequestWithdrawer{done: make(chan struct{}, 1)}
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return frw }

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishGitHubPR("tok1"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d, want 502 on a failed review post", w.Code)
	}
	select {
	case <-frw.done:
		t.Fatal("a failed publish must leave the review request pending")
	case <-time.After(200 * time.Millisecond):
	}
}

// NormalizedReviewRequestLogins must be ONE definition with two readers: the
// anti-loop actor guard and this tail. A drift between them would have the
// tail withdraw "@bot" (which GitHub does not know) while the guard trusts
// "bot", or the reverse.
func TestNormalizedReviewRequestLoginsFeedsBothGuardAndWithdrawal(t *testing.T) {
	cfg := webhooks.Config{
		Provider:            webhooks.ProviderGitHub,
		ReviewRequestLogins: []string{" @iterion-bot ", "", "@", "revu-bot"},
	}
	want := []string{"iterion-bot", "revu-bot"}
	if got := cfg.NormalizedReviewRequestLogins(); !slices.Equal(got, want) {
		t.Fatalf("NormalizedReviewRequestLogins() = %v, want %v", got, want)
	}
	// The actor guard reads the same set, plus the App's own login.
	guard := iterionBotLogins(cfg, forge.Connection{Provider: forge.ProviderGitHub, AppSlug: "iterion"})
	for _, l := range want {
		if !slices.Contains(guard, l) {
			t.Fatalf("actor guard %v dropped the armed login %q — the two halves disagree", guard, l)
		}
	}
	if !slices.Contains(guard, "iterion[bot]") {
		t.Fatalf("actor guard %v must still recognise the App's own login", guard)
	}
	// And appending to the returned slice must not write into the config's
	// own backing array: the guard appends to it.
	if len(cfg.ReviewRequestLogins) != 4 || cfg.ReviewRequestLogins[0] != " @iterion-bot " {
		t.Fatalf("the config's own logins were mutated: %v", cfg.ReviewRequestLogins)
	}
}
