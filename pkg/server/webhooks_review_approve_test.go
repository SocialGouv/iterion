package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	fforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	fgithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The token-binding write path type-asserts the replier client into the
// gate and commenter surfaces. Both providers' clients must satisfy them,
// else the fallback is dead code behind a failed assertion.
var (
	_ forgeGateClient     = (*fgithub.AdminClient)(nil)
	_ forgeIssueCommenter = (*fgithub.AdminClient)(nil)
	_ forgeGateClient     = (*fforgejo.AdminClient)(nil)
	_ forgeIssueCommenter = (*fforgejo.AdminClient)(nil)
	// The command gates read commenter roles through the connection's admin
	// client when one covers the repo: every kind the resolver can return
	// must carry the replier surface, the App client included.
	_ prforgeReplierAPI = (*fgithub.AdminClient)(nil)
	_ prforgeReplierAPI = (*fgithub.AppClient)(nil)
	_ prforgeReplierAPI = (*fforgejo.AdminClient)(nil)
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
	s.webhookPRForgeCommandGate = func(_ context.Context, _ webhooks.Config, _ webhooks.Provider, _ prforge.ParsedNote, route webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		gateCalls++
		gotBot = route.BotID
		return gateRefused, "replier not authorized", nil // deny
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
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
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
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
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
	// The reply says what failed and what to do; the forge's own error text
	// is internal detail that stays on the audit row (a public PR is not the
	// place for it).
	if got := commenter.bodies[0]; !approveReplyContains(got, "@maintainer-jane", "I can't approve", "status") || approveReplyContains(got, "insufficient scope") {
		t.Fatalf("reply must name the maintainer and the failed write without pasting the forge error:\n%s", got)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError || !approveReplyContains(rows[0].Error, "insufficient scope") {
		t.Fatalf("the audit row keeps the forge error, got %+v", rows)
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

// #662 (self-approve): a PR author cannot approve their own PR — that
// is a merge-queue-bypass in another shape, and docs/merge-gate.md
// documents /revi approve as a "maintainer" affordance. Refused BEFORE
// the command gate so the maintainer sees a specific reason on the PR.
func TestReviewApprove_AuthorCannotApproveOwnPR(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	authored := &authoredGateClient{fakeGateClient: fakeGateClient{headSHA: "abc1234"}, author: "alice"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return authored, nil }
	commenter := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) { return commenter, nil }
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}

	// Author = alice; commenter also = alice → self-approve.
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve LGTM to my own PR","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("self-approve refusal must answer 200, got %d body=%s", w.Code, w.Body.String())
	}
	if authored.setCalls != 0 {
		t.Fatalf("self-approve MUST NOT write a status (setCalls=%d) — it is a merge-queue bypass in another shape", authored.setCalls)
	}
	if len(commenter.bodies) != 1 || !approveReplyContains(commenter.bodies[0], "@alice", "own pull request") {
		t.Fatalf("self-approve must post an actionable reply on the PR, got %v", commenter.bodies)
	}
}

// authoredGateClient overrides GetPullRequest to return a specific PR author.
type authoredGateClient struct {
	fakeGateClient
	author string
}

func (a *authoredGateClient) GetPullRequest(_ context.Context, _ string, number int) (forge.PullRef, error) {
	return forge.PullRef{Number: number, HeadSHA: a.headSHA, Author: a.author}, nil
}

// #662 (approve floor): when cfg.MinReplierRole is empty the approve
// lane defaults the gate floor to "maintainer" (docs/merge-gate.md
// documents this override as a maintainer affordance). Route's
// MinReplierRole is set BEFORE the gate stub runs, so a probing stub
// receives it directly.
func TestReviewApprove_DefaultsFloorToMaintainerWhenUnpinned(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "abc1234"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	var gotRoles []string
	s.webhookPRForgeCommandGate = func(_ context.Context, _ webhooks.Config, _ webhooks.Provider, _ prforge.ParsedNote, route webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		gotRoles = append(gotRoles, route.MinReplierRole)
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	// cfg.MinReplierRole intentionally UNPINNED — the approve default must apply.
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(gotRoles) != 1 || gotRoles[0] != "maintainer" {
		t.Fatalf("approve floor default must be \"maintainer\" when cfg pins nothing, gate saw route.MinReplierRole=%v", gotRoles)
	}
}

// The webhook's min_replier_role is the talk-back floor (who may question
// the bot); a force-green of a required check has its own, higher floor. An
// operator lowering the talk-back floor so reporters can ask the converse
// bot must not lower the merge-queue bypass with it — the pin may only
// RAISE the approve floor. The route floor the gate receives is therefore
// the higher of the pin and the maintainer default.
func TestReviewApprove_PinRaisesTheFloorNeverLowersIt(t *testing.T) {
	cases := []struct {
		pin, wantRoute string
	}{
		{"", "maintainer"},
		{"reporter", "maintainer"},
		{"developer", "maintainer"},
		{"maintainer", "maintainer"},
		{"owner", "owner"},
	}
	for _, c := range cases {
		t.Run("pin="+c.pin, func(t *testing.T) {
			s := newWebhookTestServer(t)
			conns := forge.NewMemoryConnectionStore()
			if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
				t.Fatal(err)
			}
			s.forgeConnections = conns
			gc := &fakeGateClient{headSHA: "abc"}
			s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
			var gotRoles []string
			s.webhookPRForgeCommandGate = func(_ context.Context, _ webhooks.Config, _ webhooks.Provider, _ prforge.ParsedNote, route webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
				gotRoles = append(gotRoles, route.MinReplierRole)
				return gateAuthorized, "authorized", nil
			}
			cfg, pt := ghConfig(t, s)
			cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
			cfg.MinReplierRole = c.pin
			w := httptest.NewRecorder()
			s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
			if len(gotRoles) != 1 || gotRoles[0] != c.wantRoute {
				t.Fatalf("pin %q must give the gate route floor %q, got %v", c.pin, c.wantRoute, gotRoles)
			}
		})
	}
}

// #662: a gate-client resolution ERROR must NOT 502 the delivery — a 5xx
// is the class forges answer by disabling the hook.
func TestReviewApprove_GateClientErrorRepliesInsteadOf502(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	// gate client seam that ERRORS on resolution (missing App config,
	// sealer key rotated, connection unreadable).
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return nil, errInsufficientScope // reused sentinel error type
	}
	commenter := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) { return commenter, nil }
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("gate client error must NOT 502 (hook auto-disable class), got %d body=%s", w.Code, w.Body.String())
	}
	if len(commenter.bodies) != 1 {
		t.Fatalf("gate client error must post a reply on the PR, got %d", len(commenter.bodies))
	}
	if got := commenter.bodies[0]; approveReplyContains(got, "insufficient scope") {
		t.Fatalf("the reply pasted the forge error text onto the PR:\n%s", got)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunchError {
		t.Fatalf("gate client error is a forge failure, want %q in the response, got %v", webhooks.StatusLaunchError, resp)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError {
		t.Fatalf("gate client error must audit one launch_error row, got %+v", rows)
	}
}

// #662: idempotency — a redelivered comment (forge "Redeliver", or a
// retry after a 5xx) must not run the approve flow twice.
func TestReviewApprove_IdempotentOnRedelivery(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "abc1234"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) { return &stubCommenter{}, nil }
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`
	// First delivery: writes the status.
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("first delivery must write once, got %d", gc.setCalls)
	}
	// Redelivery: MUST NOT write again, MUST NOT re-post a reply.
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("redelivery re-wrote status (setCalls=%d) — idempotency key not honoured", gc.setCalls)
	}
}

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

// approveWorld wires the connection-admin happy path every idempotency test
// starts from: one team connection, a fake gate client, a stub commenter, an
// allow-all command gate, and a pinned gate context.
func approveWorld(t *testing.T) (*Server, *fakeGateClient, *stubCommenter, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "abc1234"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	commenter := &stubCommenter{}
	s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) { return commenter, nil }
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	return s, gc, commenter, cfg, pt
}

const approveBodyByJane = `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"maintainer-jane"}}`

func approveDeliveries(t *testing.T, s *Server, cfg webhooks.Config) []webhooks.Delivery {
	t.Helper()
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A redelivery of an approve that already landed must answer `duplicate`,
// write nothing, and leave ONE audit row for the comment — the same shape
// the command lane keeps per event.
func TestReviewApprove_RedeliveryOfLandedApproveIsDuplicate(t *testing.T) {
	s, gc, _, cfg, pt := approveWorld(t)
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("first delivery must write once, got %d", gc.setCalls)
	}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("redelivery re-wrote the status (setCalls=%d)", gc.setCalls)
	}
	var resp map[string]string
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("redelivery must answer %q like every other lane, got %v", webhooks.StatusDuplicate, resp)
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].Status != webhooks.StatusLaunched {
		t.Fatalf("want exactly one launched audit row for the comment, got %+v", rows)
	}
}

// getBlindDeliveryStore answers every replay lookup with not-found while
// keeping the store's unique constraint on Insert: the shape two replicas
// see when they pass the replay check together — only the Insert decides.
type getBlindDeliveryStore struct{ webhooks.DeliveryStore }

func (getBlindDeliveryStore) GetByIdempotencyKey(context.Context, string) (webhooks.Delivery, error) {
	return webhooks.Delivery{}, webhooks.ErrNotFound
}

// Two replicas handling the same redelivery both miss the replay check; the
// one that loses the Insert under the stable key must NOT write the status.
// The unique constraint is the dedupe, not the read that precedes it.
func TestReviewApprove_ConcurrentTwinLosingTheClaimDoesNotWrite(t *testing.T) {
	s, gc, _, cfg, pt := approveWorld(t)
	inner := s.webhookDeliveries
	s.webhookDeliveries = getBlindDeliveryStore{inner}
	// The twin's claim already sits under the stable key, freshly received:
	// a claim older than approveClaimStaleAfter would be a dead writer's,
	// which the replay check reuses — not the in-flight twin modelled here.
	p, err := prforge.ParseIssueComment([]byte(approveBodyByJane))
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.Insert(context.Background(), webhooks.Delivery{
		ID: "twin", TenantID: cfg.TenantID, WebhookID: cfg.ID, Status: webhooks.StatusAccepted,
		IdempotencyKey: approveIdempotencyKey(cfg, p.ProjectPath, p.SubjectID()), ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 0 {
		t.Fatalf("the Insert loser wrote the status anyway (setCalls=%d) — the unique constraint must gate the forge write", gc.setCalls)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("Insert loser must answer %q, got %v", webhooks.StatusDuplicate, resp)
	}
}

// A forge write that failed (scope refusal, outage) is NOT a terminal
// duplicate: the forge's "Redeliver" is the operator's retry, and the
// redelivery must write — reusing the failed row, never a second one.
func TestReviewApprove_FailedWriteIsRetryableOnRedelivery(t *testing.T) {
	s, gc, _, cfg, pt := approveWorld(t)
	gc.setErr = errInsufficientScope
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError {
		t.Fatalf("failed write must leave one launch_error row, got %+v", rows)
	}
	gc.setErr = nil // the operator fixed the token scope
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 2 || gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("redelivery after a failed write must retry the status (setCalls=%d last=%+v)", gc.setCalls, gc.last)
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].Status != webhooks.StatusLaunched {
		t.Fatalf("the failed row must be reused and flipped to launched, got %+v", rows)
	}
}

// A configuration refusal (here: no gate context pinned) must not poison the
// comment: once the operator pins one, redelivering the same comment
// approves. Refusals are audited under their own keys, never the dedupe key.
func TestReviewApprove_ConfigRefusalDoesNotPoisonRedelivery(t *testing.T) {
	s, gc, commenter, cfg, pt := approveWorld(t)
	cfg.LaunchVars = nil // nothing pinned yet
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 0 || len(commenter.bodies) != 1 || !approveReplyContains(commenter.bodies[0], "no merge-gate context") {
		t.Fatalf("unpinned repo must refuse with a reply (setCalls=%d bodies=%v)", gc.setCalls, commenter.bodies)
	}
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("redelivery after the operator pinned gate_context must approve (setCalls=%d body=%s)", gc.setCalls, w2.Body.String())
	}
}

// fakeGitHubForge is an httptest GitHub REST API the token-client path talks
// to for real: WhoAmI, collaborator permission, PR head, status write, reply.
// It records every status and comment it receives.
type fakeGitHubForge struct {
	srv      *httptest.Server
	mu       sync.Mutex
	perms    map[string]string // login → role_name
	prAuthor string
	headSHA  string
	statuses []map[string]string
	comments []string
	// mintFail makes the installation-token mint answer GitHub's
	// permissions-not-granted 422 — the shape of an installation whose
	// grant lags the permission set iterion requests.
	mintFail bool
	// mintDenyStatuses refuses only a mint that asks for `statuses` and
	// serves the baseline with a token the status endpoint then 403s — an
	// installation created before the merge gate, or one that declined
	// statuses:write.
	mintDenyStatuses bool
	mints            int
	mintsBaseline    int // mints that did NOT ask for statuses
	// bearers records, per endpoint, the Authorization values the forge
	// saw — the proof of WHICH credential served a call: the minted
	// installation token (ghs_…) or the webhook's hand-owned binding.
	bearers map[string][]string
	// revokedBearer, when set, is an Authorization value every endpoint
	// answers 401 to — a binding whose token was revoked or rotated away
	// while the webhook still pins it.
	revokedBearer string
	// slug is what GET /app (the App-JWT identity probe) answers; appLookups
	// counts those probes.
	slug       string
	appLookups int
}

// appLookupCount returns how many times the App identity was probed.
func (f *fakeGitHubForge) appLookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appLookups
}

// revoked answers 401 when the request carries the revoked bearer.
func (f *fakeGitHubForge) revoked(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	rb := f.revokedBearer
	f.mu.Unlock()
	if rb == "" || r.Header.Get("Authorization") != rb {
		return false
	}
	w.WriteHeader(http.StatusUnauthorized)
	return true
}

// bearerNoStatuses is the installation token minted without statuses:write.
const bearerNoStatuses = "Bearer ghs_nostatus"

func newFakeGitHubForge(t *testing.T) *fakeGitHubForge {
	t.Helper()
	f := &fakeGitHubForge{perms: map[string]string{}, prAuthor: "alice", headSHA: "deadbeef1234", bearers: map[string][]string{}, slug: "iterion-forge-x"}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	seen := func(endpoint string, r *http.Request) {
		f.mu.Lock()
		f.bearers[endpoint] = append(f.bearers[endpoint], r.Header.Get("Authorization"))
		f.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, asksStatuses := req.Permissions["statuses"]
		f.mu.Lock()
		f.mints++
		if !asksStatuses {
			f.mintsBaseline++
		}
		fail, denyStatuses := f.mintFail, f.mintDenyStatuses
		f.mu.Unlock()
		if fail || (denyStatuses && asksStatuses) {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		token := "ghs_inst"
		if denyStatuses {
			token = "ghs_nostatus"
		}
		reply(w, http.StatusCreated, map[string]any{"token": token, "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("GET /api/v3/app", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.appLookups++
		slug := f.slug
		f.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{"id": 42, "slug": slug, "name": "iterion forge"})
	})
	mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		seen("user", r)
		if f.revoked(w, r) {
			return
		}
		reply(w, http.StatusOK, map[string]any{"login": "iterion-bot", "id": 1})
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		seen("permission", r)
		if f.revoked(w, r) {
			return
		}
		f.mu.Lock()
		role, ok := f.perms[r.PathValue("user")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(w, http.StatusOK, map[string]any{"permission": role, "role_name": role})
	})
	mux.HandleFunc("GET /api/v3/repos/acme/widgets/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		seen("pull", r)
		if f.revoked(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		reply(w, http.StatusOK, map[string]any{
			"number": 7, "state": "open", "html_url": "https://github.com/acme/widgets/pull/7",
			"head": map[string]any{"ref": "feat/x", "sha": f.headSHA, "repo": map[string]any{"full_name": "acme/widgets"}},
			"base": map[string]any{"ref": "main"},
			"user": map[string]any{"login": f.prAuthor},
		})
	})
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/statuses/{sha}", func(w http.ResponseWriter, r *http.Request) {
		seen("status", r)
		if f.revoked(w, r) {
			return
		}
		if r.Header.Get("Authorization") == bearerNoStatuses {
			reply(w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["sha"] = r.PathValue("sha")
		f.mu.Lock()
		f.statuses = append(f.statuses, body)
		f.mu.Unlock()
		reply(w, http.StatusCreated, map[string]any{"id": 1})
	})
	mux.HandleFunc("POST /api/v3/repos/acme/widgets/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		seen("comment", r)
		if f.revoked(w, r) {
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.comments = append(f.comments, body.Body)
		f.mu.Unlock()
		reply(w, http.StatusCreated, map[string]any{"id": 9, "html_url": "https://github.com/acme/widgets/pull/7#issuecomment-9"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubForge) snapshot() (statuses []map[string]string, comments []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.statuses...), append([]string(nil), f.comments...)
}

// bearersFor returns the Authorization values one endpoint received.
func (f *fakeGitHubForge) bearersFor(endpoint string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bearers[endpoint]...)
}

func (f *fakeGitHubForge) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints
}

// seedForgeToken stores a sealed team-scoped forge_token the webhook pins
// through SecretOverrides — the hand-owned setup docs/webhooks.md describes,
// resolved by the same resolveForgeToken production reads.
func seedForgeToken(t *testing.T, s *Server, cfg *webhooks.Config, plaintext string) {
	t.Helper()
	const id = "sec-forge-token"
	sealed, err := secrets.SealGenericSecret(s.sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()
	if err := s.genericSecrets.Create(context.Background(), secrets.GenericSecret{
		ID: id, TenantID: cfg.TenantID, ScopeTeamID: cfg.TenantID, Name: "forge_token",
		SealedSecret: sealed, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg.SecretOverrides = map[string]string{"forge_token": id}
}

// approveTokenWorld is the hand-owned webhook: a forge_token binding, NO
// forge.Connection row, and a real (fake-served) GitHub API behind the
// webhook's pinned ForgeBaseURL.
func approveTokenWorld(t *testing.T) (*Server, *fakeGitHubForge, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	s.forgeConnections = forge.NewMemoryConnectionStore()
	f := newFakeGitHubForge(t)
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	seedForgeToken(t, s, &cfg, "ghp_hand_owned")
	return s, f, cfg, pt
}

func approveBodyFrom(sender string) string {
	return `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/revi approve disputed finding","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-556"},"sender":{"login":"` + sender + `"}}`
}

// A webhook with a forge_token binding and no connection row (the hand-owned
// setup) must write the status through the token client — the status lands
// on the forge, under the pinned context, on the resolved head.
func TestReviewApprove_TokenBindingWritesTheStatusWithoutAConnection(t *testing.T) {
	s, f, cfg, pt := approveTokenWorld(t)
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	statuses, comments := f.snapshot()
	if len(statuses) != 1 {
		t.Fatalf("the token path must write the status exactly once, got %d (comments=%v body=%s)", len(statuses), comments, w.Body.String())
	}
	st := statuses[0]
	if st["sha"] != "deadbeef1234" || st["context"] != "revi/review" || st["state"] != "success" || st["description"] != "approved by @maintainer-jane: disputed finding" {
		t.Fatalf("status written with the wrong shape: %v", st)
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "revi-approved" {
		t.Fatalf("want revi-approved, got %v", resp)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusLaunched {
		t.Fatalf("want one launched audit row, got %+v", rows)
	}
}

// The real command gate, against the real (fake-served) permission API: the
// approve floor is maintainer, an operator's pin may raise it and never
// lower it, and neither the review bot's own comment nor the PR author can
// approve. A commenter the GATE refuses is answered in silence — the
// delivery audit names the reason, the PR gets no bot comment, because
// anyone able to comment can reach that branch. A refusal AFTER the gate
// (the author approving their own PR) is a maintainer's, and is told.
func TestReviewApprove_RealGateFloor(t *testing.T) {
	cases := []struct {
		name       string
		sender     string
		perm       string
		cfgFloor   string
		wantStatus bool
		wantSilent string // the gate refused: this reason on the audit row, no reply
		wantReply  string // refused past the gate: this reply on the PR
	}{
		{"write is refused when the webhook pins no floor", "dev-dan", "write", "", false, "replier not authorized", ""},
		{"a developer pin does not lower the floor: write is refused", "dev-dan", "write", "developer", false, "replier not authorized", ""},
		{"a reporter pin does not lower the floor: triage is refused", "triager-tom", "triage", "reporter", false, "replier not authorized", ""},
		{"maintain is accepted with no floor pinned", "maintainer-jane", "maintain", "", true, "", ""},
		{"an owner pin raises the floor: maintain is refused", "maintainer-jane", "maintain", "owner", false, "replier not authorized", ""},
		{"an owner pin raises the floor: admin is accepted", "admin-ann", "admin", "owner", true, "", ""},
		{"the review bot's own comment is refused", "iterion-bot", "admin", "", false, "loop-guard", ""},
		{"the PR author cannot approve their own PR", "alice", "admin", "", false, "", "own pull request"},
		{"a Forgejo repo owner is accepted with no floor pinned", "owner-olga", "owner", "", true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, f, cfg, pt := approveTokenWorld(t)
			s.webhookPRForgeCommandGate = nil // the real gate
			f.perms[c.sender] = c.perm
			cfg.MinReplierRole = c.cfgFloor
			w := httptest.NewRecorder()
			s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom(c.sender), prforge.EventHeaderIssueComment, pt))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			statuses, comments := f.snapshot()
			if c.wantStatus {
				if len(statuses) != 1 || len(comments) != 0 {
					t.Fatalf("want one status and no refusal reply, got statuses=%v comments=%v", statuses, comments)
				}
				return
			}
			if len(statuses) != 0 {
				t.Fatalf("must not write a status, got %v", statuses)
			}
			if c.wantSilent != "" {
				if len(comments) != 0 {
					t.Fatalf("a commenter the gate refuses gets NO reply (any PR commenter can drive one), got %v", comments)
				}
				if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered || !approveReplyContains(rows[0].Error, c.wantSilent) {
					t.Fatalf("the refusal must be audited as filtered with %q, got %+v", c.wantSilent, rows)
				}
				return
			}
			if len(comments) != 1 || !approveReplyContains(comments[0], "@"+c.sender, c.wantReply) {
				t.Fatalf("want one reply naming @%s and %q, got %v", c.sender, c.wantReply, comments)
			}
		})
	}
}

func approveBodyFromID(sender string, id int) string {
	return `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","user":{"login":"alice"},"pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":` + fmt.Sprint(id) + `,"body":"/revi approve","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-` + fmt.Sprint(id) + `"},"sender":{"login":"` + sender + `"}}`
}

// /revi approve is intercepted before any scope or route admission, so a
// drive-by commenter with no repo role reaches the gate on every comment.
// Each comment is its own idempotency key: N comments must cost N silent
// filtered rows — no bot comment on the PR, and no PR read either, since the
// head is only resolved for a commenter who cleared the floor.
func TestReviewApprove_UnauthorizedCommenterIsRefusedInSilence(t *testing.T) {
	s, f, cfg, pt := approveTokenWorld(t)
	s.webhookPRForgeCommandGate = nil // the real gate; "outsider" has no role → 404 → none
	for _, id := range []int{556, 557, 558} {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFromID("outsider", id), prforge.EventHeaderIssueComment, pt))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	statuses, comments := f.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("an unauthorized commenter force-greened the gate: %v", statuses)
	}
	if len(comments) != 0 {
		t.Fatalf("three drive-by comments produced %d bot comments under the org's identity, want 0: %v", len(comments), comments)
	}
	if pulls := f.bearersFor("pull"); len(pulls) != 0 {
		t.Fatalf("the PR head was read %d time(s) for a commenter who never cleared the floor, want 0", len(pulls))
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 3 {
		t.Fatalf("want one filtered audit row per comment, got %+v", rows)
	}
	for _, r := range rows {
		if r.Status != webhooks.StatusFiltered || !approveReplyContains(r.Error, "replier not authorized") {
			t.Fatalf("each refusal must be audited as filtered with its reason, got %+v", r)
		}
	}
}

// A gate that could not be EVALUATED — no credential to read the commenter's
// standing — is a configuration miss the maintainer has to fix, and is told
// so on the PR. The reason the gate produced names internal state (a
// connection id, a forge's error text): that stays on the audit row and in
// the log, never in the comment.
func TestReviewApprove_UnevaluableGateRepliesWithoutInternals(t *testing.T) {
	s, gc, commenter, cfg, pt := approveWorld(t)
	const internal = "connection conn-app covers github.com/acme/widgets but its client cannot serve (forge: 422 permissions not granted to this installation), and this webhook has no forge_token binding to fall back on"
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateUnevaluable, internal, nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK || gc.setCalls != 0 {
		t.Fatalf("status=%d setCalls=%d body=%s", w.Code, gc.setCalls, w.Body.String())
	}
	if len(commenter.bodies) != 1 {
		t.Fatalf("an unevaluable gate is a configuration miss: the maintainer must be told on the PR, got %v", commenter.bodies)
	}
	if got := commenter.bodies[0]; !approveReplyContains(got, "@maintainer-jane", "I cannot approve here") ||
		approveReplyContains(got, "conn-app") || approveReplyContains(got, "422") || approveReplyContains(got, "not granted") {
		t.Fatalf("the reply must say what to fix and carry no connection id or forge error text, got:\n%s", got)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered || !approveReplyContains(rows[0].Error, "conn-app", "not granted") {
		t.Fatalf("the audit row keeps the internal detail, got %+v", rows)
	}
}

// The disabled-bot refusal is a configuration miss like every other, and is
// told on the PR — to a MAINTAINER. For a commenter the gate refuses it is
// not reached at all: the gate runs first, so nobody learns which bots a
// webhook enables by typing a command.
func TestReviewApprove_DisabledReviewBotIsSilentForAnUnauthorizedCommenter(t *testing.T) {
	s, gc, commenter, cfg, pt := approveWorld(t)
	cfg.BotIDs = []string{"feature-dev"}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateRefused, "replier not authorized: mallory", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("mallory"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK || gc.setCalls != 0 {
		t.Fatalf("status=%d setCalls=%d body=%s", w.Code, gc.setCalls, w.Body.String())
	}
	if len(commenter.bodies) != 0 {
		t.Fatalf("an unauthorized commenter was told which bots this webhook enables: %v", commenter.bodies)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered || !approveReplyContains(rows[0].Error, "replier not authorized") {
		t.Fatalf("want one filtered row carrying the gate's refusal, got %+v", rows)
	}
}

// Neither a connection covering the repo nor a forge_token binding: the
// refusal must name exactly that (a configuration miss), not a capability
// the provider does have.
func TestReviewApprove_NoConnectionAndNoTokenIsNamedRefusal(t *testing.T) {
	s := newWebhookTestServer(t)
	s.forgeConnections = forge.NewMemoryConnectionStore()
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered {
		t.Fatalf("want one filtered row, got %+v", rows)
	}
	if !approveReplyContains(rows[0].Error, "no team connection covers", "no forge_token binding") {
		t.Fatalf("the refusal must name both missing credentials, got %q", rows[0].Error)
	}
}

// A force-green is ROLE-only: AuthorizedRepliers is "who may talk back to
// the bot", not "who may bypass the merge queue". An allowlisted login with
// no repo permission is refused by the gate — in silence, audited.
func TestReviewApprove_AllowlistDoesNotBypassTheRoleFloor(t *testing.T) {
	s, f, cfg, pt := approveTokenWorld(t)
	s.webhookPRForgeCommandGate = nil // the real gate
	cfg.AuthorizedRepliers = []string{"outsider"}
	// no f.perms entry for outsider → the forge answers 404 → "none"
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("outsider"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	statuses, comments := f.snapshot()
	if len(statuses) != 0 {
		t.Fatalf("an allowlisted login with no repo role force-greened the gate: %v", statuses)
	}
	if len(comments) != 0 {
		t.Fatalf("a commenter the gate refuses gets no reply, got %v", comments)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered || !approveReplyContains(rows[0].Error, "replier not authorized") {
		t.Fatalf("want one filtered row naming the role refusal, got %+v", rows)
	}
}

// The review bot not being enabled on the webhook is a configuration
// refusal like every other: it replies with the reason instead of staying
// silent.
func TestReviewApprove_BotNotPermittedRepliesWithReason(t *testing.T) {
	s, gc, commenter, cfg, pt := approveWorld(t)
	cfg.BotIDs = []string{"feature-dev"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK || gc.setCalls != 0 {
		t.Fatalf("status=%d setCalls=%d body=%s", w.Code, gc.setCalls, w.Body.String())
	}
	if len(commenter.bodies) != 1 || !approveReplyContains(commenter.bodies[0], "@maintainer-jane", "not enabled on this webhook") {
		t.Fatalf("want one reply naming the disabled review bot, got %v", commenter.bodies)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered {
		t.Fatalf("want one filtered row, got %+v", rows)
	}
}

func seedAcceptedApproveClaim(t *testing.T, s *Server, cfg webhooks.Config, id string, age time.Duration) {
	t.Helper()
	p, err := prforge.ParseIssueComment([]byte(approveBodyByJane))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
		ID: id, TenantID: cfg.TenantID, WebhookID: cfg.ID, Status: webhooks.StatusAccepted,
		IdempotencyKey: approveIdempotencyKey(cfg, p.ProjectPath, p.SubjectID()), ReceivedAt: time.Now().UTC().Add(-age),
	}); err != nil {
		t.Fatal(err)
	}
}

// A claim left `accepted` by a writer that died before recording its
// outcome must not become a zero-signal duplicate forever: past
// approveClaimStaleAfter it is reused — the status write is idempotent on
// the forge — and flipped on the same row.
func TestReviewApprove_StaleAcceptedClaimIsReused(t *testing.T) {
	s, gc, _, cfg, pt := approveWorld(t)
	seedAcceptedApproveClaim(t, s, cfg, "dead-writer", 48*time.Hour)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	if gc.setCalls != 1 {
		t.Fatalf("a 48h-old accepted claim must be reused and the status written, got setCalls=%d body=%s", gc.setCalls, w.Body.String())
	}
	rows := approveDeliveries(t, s, cfg)
	if len(rows) != 1 || rows[0].ID != "dead-writer" || rows[0].Status != webhooks.StatusLaunched {
		t.Fatalf("the stale claim must be flipped in place to launched, got %+v", rows)
	}
}

// A claim younger than approveClaimStaleAfter is a writer still in flight:
// the redelivery short-circuits as a duplicate without writing.
func TestReviewApprove_YoungAcceptedClaimIsDuplicate(t *testing.T) {
	s, gc, _, cfg, pt := approveWorld(t)
	seedAcceptedApproveClaim(t, s, cfg, "in-flight", time.Minute)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyByJane, prforge.EventHeaderIssueComment, pt))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if gc.setCalls != 0 || resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("a young accepted claim must short-circuit as duplicate (setCalls=%d resp=%v)", gc.setCalls, resp)
	}
}

// A connection-only integration (a team connection covers the repo, no
// forge_token binding on the webhook): the real gate must read the
// commenter's role through the connection's client — the same resolution
// the status write already uses — and the approve must land.
func TestReviewApprove_ConnectionOnlyIntegrationAuthorizesWithoutToken(t *testing.T) {
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	f.perms["maintainer-jane"] = "maintain"
	conn := forge.Connection{
		ID: "c-pat", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime, CreatedAt: time.Now(),
	}
	sealed, err := forge.SealPAT(s.sealer, conn.ID, "ghp_connection_token")
	if err != nil {
		t.Fatal(err)
	}
	conn.SealedPayload = sealed
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	// No generic secret store: resolveForgeToken yields "". No gate stub and
	// no gate-client stub: the real gate and the real admin client both talk
	// to the fake forge through the connection.
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	statuses, comments := f.snapshot()
	if len(statuses) != 1 || statuses[0]["state"] != "success" || statuses[0]["context"] != "revi/review" {
		t.Fatalf("connection-only integration must approve through the connection, got statuses=%v comments=%v body=%s", statuses, comments, w.Body.String())
	}
}

// approveStaleBindingWorld is the half-configured shape #662 came from: a
// healthy team connection covers the repo AND the webhook still pins a
// forge_token binding whose token the forge no longer accepts. The real
// gate and the real clients talk to the fake forge; the binding's bearer is
// refused on every endpoint.
func approveStaleBindingWorld(t *testing.T) (*Server, *fakeGitHubForge, webhooks.Config, string) {
	t.Helper()
	s := newWebhookTestServer(t)
	f := newFakeGitHubForge(t)
	f.perms["maintainer-jane"] = "maintain"
	f.perms["alice"] = "admin"
	conn := forge.Connection{
		ID: "c-pat", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
		Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime, CreatedAt: time.Now(),
	}
	sealed, err := forge.SealPAT(s.sealer, conn.ID, "ghp_connection_token")
	if err != nil {
		t.Fatal(err)
	}
	conn.SealedPayload = sealed
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = f.srv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	seedForgeToken(t, s, &cfg, "ghp_revoked")
	f.revokedBearer = "Bearer ghp_revoked"
	return s, f, cfg, pt
}

// The forge_token binding is the FALLBACK identity; the covering connection
// is primary for the status write and for the head read that precedes it. A
// binding that no longer works — revoked, rotated, wrong scope — must not
// fail an approve the connection serves end to end, and the self-approve
// guard must still fire off the head the connection read.
func TestReviewApprove_StaleBindingDoesNotBlockTheConnection(t *testing.T) {
	t.Run("a maintainer's approve lands through the connection", func(t *testing.T) {
		s, f, cfg, pt := approveStaleBindingWorld(t)
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		statuses, comments := f.snapshot()
		if len(statuses) != 1 || statuses[0]["state"] != "success" {
			t.Fatalf("the connection serves the read and the write, so the approve must land whatever the binding's health — got statuses=%v comments=%v body=%s", statuses, comments, w.Body.String())
		}
		for _, b := range f.bearersFor("pull") {
			if b == "Bearer ghp_revoked" {
				t.Errorf("the PR head was read through the fallback binding while the connection could serve it")
			}
		}
	})
	t.Run("the PR author is still refused, off the head the connection read", func(t *testing.T) {
		s, f, cfg, pt := approveStaleBindingWorld(t)
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("alice"), prforge.EventHeaderIssueComment, pt))
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		statuses, comments := f.snapshot()
		if len(statuses) != 0 {
			t.Fatalf("a self-approve wrote a status: %v", statuses)
		}
		if len(comments) != 1 || !approveReplyContains(comments[0], "@alice", "own pull request") {
			t.Fatalf("want the self-approve reply off the head the connection read, got comments=%v body=%s", comments, w.Body.String())
		}
	})
}

// With no connection the binding IS the write path, and a read failure
// through it is the failure of the only credential the lane holds: a
// visible launch_error with no status written — never a silent drop.
func TestReviewApprove_BindingOnlyReadFailureIsALaunchError(t *testing.T) {
	s, f, cfg, pt := approveTokenWorld(t)
	f.revokedBearer = "Bearer ghp_hand_owned"
	// The gate would refuse on the revoked token too; stub it so the read
	// through the write path is the only thing under test.
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), approveBodyFrom("maintainer-jane"), prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a forge read failure must not 5xx (hook auto-disable class), got %d body=%s", w.Code, w.Body.String())
	}
	if statuses, _ := f.snapshot(); len(statuses) != 0 {
		t.Fatalf("wrote a status without a readable head: %v", statuses)
	}
	if rows := approveDeliveries(t, s, cfg); len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError {
		t.Fatalf("the only credential failing its read must audit one launch_error row, got %+v", rows)
	}
}

// A forge error while READING the commenter's standing is not a refusal and
// not a 5xx: GitHub disables a hook that keeps answering 5xx, taking every
// future launch, re-review and override down with it (#662). The commenter
// is unverified at that point, so the outcome is a 200 launch_error on the
// delivery row and nothing on the PR.
func TestReviewApprove_AuthzForgeErrorIs200LaunchError(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateRefused, "", errors.New("collaborator permission: github: http 503")
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/7"}},"comment":{"id":1,"body":"/revi approve flaky finding","html_url":"https://github.com/acme/widgets/pull/7#issuecomment-1","user":{"login":"maintainer"}},"sender":{"login":"maintainer"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))

	if w.Code != http.StatusOK {
		t.Fatalf("a forge error on the authz read must never be a 5xx (the forge disables the hook), got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunchError {
		t.Fatalf("status must be launch_error, got %v", resp)
	}
	if launched != 0 {
		t.Fatalf("nothing may launch on an unverified commenter (launched=%d)", launched)
	}
	ds, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 10)
	if err != nil || len(ds) == 0 {
		t.Fatalf("no delivery recorded: %v %d", err, len(ds))
	}
	if ds[0].Status != webhooks.StatusLaunchError || !strings.Contains(ds[0].Error, "authz check") {
		t.Fatalf("the delivery row must carry the authz failure, got %s %q", ds[0].Status, ds[0].Error)
	}
}
