package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
