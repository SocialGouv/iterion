package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

func TestReviewApproveReason(t *testing.T) {
	cases := []struct {
		cmd, args  string
		wantReason string
		wantOK     bool
	}{
		{"revi", "approve looks like a false positive", "looks like a false positive", true},
		{"revi", "approve", "", true}, // bare approve, no reason
		{"revi", "  approve   spaced  ", "spaced", true},
		{"revi", "review the diff", "", false}, // a normal re-review is NOT an approve
		{"revi", "", "", false},                // bare /revi re-review
		{"revi", "approver", "", false},        // must be the exact token "approve"
		{"billy", "approve x", "", false},      // another bot's command
		{"REVI", "APPROVE done", "done", true}, // case-insensitive
	}
	for _, c := range cases {
		reason, ok := reviewApproveReason(c.cmd, c.args)
		if ok != c.wantOK || reason != c.wantReason {
			t.Errorf("reviewApproveReason(%q,%q) = (%q,%v), want (%q,%v)", c.cmd, c.args, reason, ok, c.wantReason, c.wantOK)
		}
	}
}

// A `/revi approve` PR comment must be intercepted as an override — it must
// NOT launch a re-review bot. Without a forge token bound in the test server it
// filters gracefully (200), proving the approve branch ran instead of the
// command→bot routing.
func TestGitHubComment_ReviewApproveDoesNotLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)

	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve dispute"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("/revi approve must NOT launch a bot (launched=%d)", launched)
	}
}

// /revi approve authorizes through the SAME PR-comment command gate as every
// other /command (MinReplierRole / AuthorizedRepliers + WhoAmI loop-guard),
// keyed on the review-pr route — NOT the issue-author-trust gate. An
// unauthorized commenter is filtered and no status is posted.
func TestGitHubComment_ReviewApprove_UsesCommandGate(t *testing.T) {
	s := newWebhookTestServer(t)
	var gateCalls int
	var gotBot string
	s.webhookPRForgeCommandGate = func(_ context.Context, _ webhooks.Config, _ webhooks.Provider, _ prforge.ParsedNote, route webhooks.CommandRoute) (bool, string, error) {
		gateCalls++
		gotBot = route.BotID
		return false, "replier not authorized", nil // deny
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve dispute"},"sender":{"login":"mallory"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gateCalls != 1 {
		t.Fatalf("approve must consult the PR-comment command gate exactly once (calls=%d)", gateCalls)
	}
	if gotBot != "review-pr" {
		t.Fatalf("gate must be keyed on the review-pr route, got %q", gotBot)
	}
	if launched != 0 {
		t.Fatalf("denied approve must not launch anything (launched=%d)", launched)
	}
}

// TestReviewApprove_ThroughConnectionAdmin_NotForgeTokenBinding pins the fix
// for #662: /revi approve must post the commit status through the SAME client
// the publish path and the reconciler use — the connection's admin client —
// so a github_app integration mints its per-call installation token
// (which HAS `statuses` scope) instead of riding the bot's forge_token
// binding, a PAT that on the App path has no statuses scope. Without this
// the approve returned 502 "insufficient scope", the delivery was
// launch_error, and the documented override was inoperative on every App
// integration (dogfood 03/09/2026 on #646, ticket #662).
//
// The test wires:
//   - a memory connection store carrying an App-shaped connection (the
//     provider tag is what forgeConnectionForPR filters by, so any real
//     connection satisfies the test),
//   - a fake forgeGateClientFor (the same seam the publish tests use) that
//     answers GetPullRequest + SetCommitStatus,
//   - NO forge_token binding (the pre-fix path would 502 filtering here).
//
// The RED-first assertion: the fake gate client's SetCommitStatus was called
// with the resolved gate context and success state — proving the approve
// went through the connection's admin, not the token binding.
func TestReviewApprove_ThroughConnectionAdmin_NotForgeTokenBinding(t *testing.T) {
	s := newWebhookTestServer(t)
	// The webhook has NO forge_token binding wired: the pre-fix path
	// filters "no forge token to post the approval status" at this point
	// and never reaches SetCommitStatus. The fix uses the connection
	// instead — this test's gate-client seam is what proves it.
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "conn-app", TenantID: "t1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	// The fake gate client is the seam the publish tests use for the SAME
	// resolution (postGateStatus → gateClientFor). A run that lands here
	// exercises the App path in production, because forgeAdminFor mints
	// the installation token per call for a github_app connection.
	gc := &fakeGateClient{headSHA: "deadbeef1234"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	// The commenter-for-reply seam exists for the fail-with-reply path; a
	// happy approve doesn't touch it.
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return &stubCommenter{}, nil
	}
	// Authorize the commenter (WhoAmI + role gate live in the command
	// gate, which we bypass here — the fix is orthogonal to admission).
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	// The repo pinned the gate context on the manifest union (docs/merge-gate.md
	// §Overriding). The approve resolves this and writes success under it.
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}

	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve dispute — false positive","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("approve must answer 200, got %d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 1 {
		t.Fatalf("approve must post the status through the connection's gate client exactly once, got setCalls=%d — the pre-fix path filtered on missing forge_token and never called SetCommitStatus", gc.setCalls)
	}
	if gc.last.Context != "revi/review" {
		t.Fatalf("approve must post under the pinned gate context, got %q", gc.last.Context)
	}
	if gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("approve must post state=success, got %q", gc.last.State)
	}
	if gc.lastSHA != "deadbeef1234" {
		t.Fatalf("approve must post on the resolved PR head SHA, got %q", gc.lastSHA)
	}
}

// TestReviewApprove_WriteFailure_RepliesOnPRAnd200 pins the fail-with-reply
// path: a forge write error (e.g. "insufficient scope" the token cannot
// escape, or any transient forge outage) MUST NOT 502 the webhook — a
// repeated 5xx costs the whole hook (forges auto-disable). Instead: record
// launch_error on the delivery, best-effort post a reply on the PR so the
// maintainer sees why, and answer 200 to keep the hook alive.
func TestReviewApprove_WriteFailure_RepliesOnPRAnd200(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "conn-app", TenantID: "t1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "cafef00d", setErr: errInsufficientScope}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	commenter := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
		return commenter, nil
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}

	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("a write error MUST NOT 502 — forges disable hooks on repeated 5xx, got %d body=%s", w.Code, w.Body.String())
	}
	if len(commenter.bodies) != 1 {
		t.Fatalf("a failed approve must post ONE reply on the PR so the maintainer sees why, got %d", len(commenter.bodies))
	}
	if got := commenter.bodies[0]; !approveReplyContains(got, "@maintainer-jane", "I can't approve", "insufficient scope") {
		t.Fatalf("reply must name the maintainer AND state why:\n%s", got)
	}
}

// approveReplyContains is a tiny test helper — every needle must appear in body.
func approveReplyContains(body string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for i := 0; i+len(n) <= len(body); i++ {
			if body[i:i+len(n)] == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// errInsufficientScope simulates the forge write refusal that motivated the
// fix — a PAT lacking `statuses` scope, mirrored to any App-scope refusal
// on the SetCommitStatus write path.
type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }

var errInsufficientScope = &sentinelErr{msg: "forge: insufficient scope"}

// TestResolveGateContextFollowsTheRepoPin pins the override onto the check the
// repo actually requires.
//
// A repo where two bots gate different PRs cannot require either bot's own
// context — whichever bot did not run leaves it permanently absent — so it pins
// ONE shared name on the integration (docs/merge-gate.md). Approving under a
// literal `revi/review` there greens a status nothing requires, reports
// success, and leaves the real gate red: a fix that looks like it worked.
func TestResolveGateContextFollowsTheRepoPin(t *testing.T) {
	s := newWebhookTestServer(t)
	cases := []struct {
		name string
		cfg  webhooks.Config
		want string
	}{
		{
			name: "the repo's pin wins",
			cfg: webhooks.Config{
				LaunchVars:         map[string]string{gateContextVar: "from/manifest"},
				OperatorLaunchVars: map[string]string{gateContextVar: "iterion/review"},
			},
			want: "iterion/review",
		},
		{
			name: "the manifest union fills in when the repo pinned nothing",
			cfg:  webhooks.Config{LaunchVars: map[string]string{gateContextVar: "from/manifest"}},
			want: "from/manifest",
		},
		{
			name: "nothing pinned anywhere and no bot on disk: refuse rather than guess",
			cfg:  webhooks.Config{},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.resolveGateContext(c.cfg, "any-review-bot"); got != c.want {
				t.Errorf("resolveGateContext = %q, want %q", got, c.want)
			}
		})
	}
}
