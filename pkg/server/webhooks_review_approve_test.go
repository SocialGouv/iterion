package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
