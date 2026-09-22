package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The launch tail never returned StatusFiltered before this branch — the two
// refusals it gained are the first — and writeSingleLaunchResult's default
// arm passes res.httpStatus straight to httpError. That is 0 on those paths,
// and WriteHeader(0) PANICS in net/http: the request goroutine dies, the
// forge sees a dropped connection instead of an answer, and a forge answers
// repeated failures by disabling the hook.
//
// Driven through a REAL http.Server rather than a ResponseRecorder, because a
// recorder tolerates WriteHeader(0) and would show nothing.
func TestWebhookTail_AFilteredRefusalAnswers200AndDoesNotPanic(t *testing.T) {
	cases := []struct {
		name   string
		cfg    webhooks.Config
		target forgeLaunchTarget
	}{
		{
			name: "lane-kind mismatch",
			cfg:  webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true},
			target: forgeLaunchTarget{
				IdemKey: "idem-f1", BotID: "review-pr",
				Vars: map[string]string{"pr_url": "https://github.com/o/r/pull/7"},
			},
		},
		{
			name: "untrusted launch with no admitted commit",
			cfg:  webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true},
			target: forgeLaunchTarget{
				IdemKey: "idem-f2", BotID: "review-pr", Trust: store.RunTrustFork,
				Vars: map[string]string{"pr_url": "https://github.com/o/r/pull/7"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			s.orgUsage = orgusage.NewMemoryCounter()
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				return "run-1", nil
			}
			meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:7", Kind: "pull_request"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u", TeamID: "t1"})
				res := s.launchWebhookTarget(ctx, r, c.cfg, meta, c.target, "hash", "1.2.3.4")
				s.writeSingleLaunchResult(w, r, res)
			}))
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("the request died (%v) — the handler panicked, so the forge gets a dropped connection and eventually disables the hook", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			// 200, like every other filtered outcome on these lanes: a 4xx/5xx
			// teaches the forge to disable the webhook after repeated failures.
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
			}
			var got map[string]string
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("body %s: %v", body, err)
			}
			if got["status"] != webhooks.StatusFiltered {
				t.Fatalf("status field = %q, want %q", got["status"], webhooks.StatusFiltered)
			}
			if got["error"] == "" {
				t.Fatal("the refusal carried no reason — an operator cannot act on a bare 'filtered'")
			}
		})
	}
}

// The same hazard on the OTHER writer of that switch. writeLaunchDenial hands
// d.status straight to httpx.WriteJSON, and no denial constructed today
// leaves it zero — which was equally true of the sibling arm until a new
// status reached it. A floor costs nothing and removes the whole class.
func TestWriteLaunchDenial_FloorsAMissingStatusInsteadOfPanicking(t *testing.T) {
	s := newWebhookTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A denial with no HTTP status — the shape a future reason token
		// would have if its constructor forgot one.
		s.writeLaunchDenial(w, r, &launchDenial{reason: "some_future_reason", detail: "constructed without a status"})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("the request died (%v) — WriteHeader(0) panicked the handler", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — an unclassified denial must be a server error, not a crash", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if got["error"] != "some_future_reason" {
		t.Fatalf("error = %q, want the denial's own reason to survive the floor", got["error"])
	}
}
