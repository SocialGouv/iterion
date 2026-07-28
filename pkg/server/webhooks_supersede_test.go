package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// With re-review on push, a burst of commits launches a run per push and the
// earlier ones analyse code that no longer exists — wasted budget, and a
// verdict posted on a dead commit. overlap=supersede keeps the run that
// matches what is actually on the branch.
//
// The scoping is what makes it safe: the key is (webhook, subject, bot), so
// two PRs never supersede each other and two bots on one PR never supersede
// each other either — they are doing different jobs.
func TestSupersede(t *testing.T) {
	// A second push on the SAME PR carries a new head SHA, hence a new
	// idempotency key and a genuine second launch.
	pushOn := func(sha string) string {
		return `{
		  "action": "opened", "number": 7,
		  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
		  "pull_request": {"number": 7, "title": "T", "body": "b",
		    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
		    "head": {"ref": "feature/x", "sha": "` + sha + `"}, "base": {"ref": "main"}},
		  "sender": {"login": "alice"}
		}`
	}
	otherPR := `{
	  "action": "opened", "number": 9,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 9, "title": "T", "body": "b",
	    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
	    "head": {"ref": "feature/y", "sha": "yyy111"}, "base": {"ref": "main"}},
	  "sender": {"login": "alice"}
	}`

	deliver := func(t *testing.T, s *Server, cfg webhooks.Config, pt, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	}

	t.Run("a newer delivery cancels the stale run", func(t *testing.T) {
		s := newWebhookTestServer(t)
		got := fanoutLauncher(s)
		var cancelled []string
		s.webhookCancelRun = func(runID string) error {
			cancelled = append(cancelled, runID)
			return nil
		}
		cfg, pt := ghConfig(t, s)
		cfg.Overlap = schedgate.OverlapSupersede

		deliver(t, s, cfg, pt, pushOn("sha-old"))
		if len(cancelled) != 0 {
			t.Fatalf("the first delivery has nothing to supersede, cancelled %v", cancelled)
		}
		deliver(t, s, cfg, pt, pushOn("sha-new"))

		if bots := botsOf(*got); len(bots) != 2 {
			t.Fatalf("both pushes must launch (the new one carries the current truth), got %v", bots)
		}
		if len(cancelled) != 1 || cancelled[0] != "run-review-pr" {
			t.Fatalf("the stale run must be cancelled, got %v", cancelled)
		}
	})

	// A cancel that fails must never stop the new run: the new run is the one
	// carrying the current truth.
	t.Run("a failing cancel does not block the launch", func(t *testing.T) {
		s := newWebhookTestServer(t)
		got := fanoutLauncher(s)
		s.webhookCancelRun = func(string) error { return context.DeadlineExceeded }
		cfg, pt := ghConfig(t, s)
		cfg.Overlap = schedgate.OverlapSupersede

		deliver(t, s, cfg, pt, pushOn("sha-old"))
		deliver(t, s, cfg, pt, pushOn("sha-new"))
		if bots := botsOf(*got); len(bots) != 2 {
			t.Fatalf("a failed cancel must not cost the new review, got %v", bots)
		}
	})

	// The historical contract: a webhook with no policy launches every
	// delivery. Promoting that silently would drop deliveries operators rely
	// on, so an empty Overlap must never cancel anything.
	t.Run("no policy never cancels", func(t *testing.T) {
		s := newWebhookTestServer(t)
		fanoutLauncher(s)
		var cancelled []string
		s.webhookCancelRun = func(runID string) error {
			cancelled = append(cancelled, runID)
			return nil
		}
		cfg, pt := ghConfig(t, s) // Overlap == ""
		deliver(t, s, cfg, pt, pushOn("sha-old"))
		deliver(t, s, cfg, pt, pushOn("sha-new"))
		if len(cancelled) != 0 {
			t.Fatalf("an unset policy must cancel nothing, got %v", cancelled)
		}
	})

	t.Run("scoping", func(t *testing.T) {
		// EvaluateOverlap is the shared decision; assert the vocabulary is the
		// one every other launch surface already speaks.
		if d, _ := schedgate.EvaluateOverlap([]string{"r1"}, schedgate.Policy{Overlap: schedgate.OverlapSupersede}); d != schedgate.DecisionSupersede {
			t.Fatalf("supersede must decide DecisionSupersede, got %v", d)
		}
		if d, _ := schedgate.EvaluateOverlap(nil, schedgate.Policy{Overlap: schedgate.OverlapSupersede}); d != schedgate.DecisionFire {
			t.Fatalf("nothing live → plain fire, got %v", d)
		}
		if err := schedgate.Validate(schedgate.Policy{Overlap: schedgate.OverlapSupersede, MaxConcurrent: 2}); err == nil {
			t.Fatal("max_concurrent with supersede is incoherent and must be rejected")
		}
	})

	t.Run("a different PR is never superseded", func(t *testing.T) {
		s := newWebhookTestServer(t)
		got := fanoutLauncher(s)
		cfg, pt := ghConfig(t, s)
		cfg.Overlap = schedgate.OverlapSupersede

		var cancelled []string
		s.webhookCancelRun = func(runID string) error {
			cancelled = append(cancelled, runID)
			return nil
		}
		deliver(t, s, cfg, pt, pushOn("sha-old"))
		deliver(t, s, cfg, pt, otherPR)
		if bots := botsOf(*got); len(bots) != 2 {
			t.Fatalf("two distinct PRs must both review, got %v", bots)
		}
		if len(cancelled) != 0 {
			t.Fatalf("PR #9 must not cancel PR #7's review, got %v", cancelled)
		}
	})

	t.Run("two bots on one PR do not supersede each other", func(t *testing.T) {
		s := newWebhookTestServer(t)
		got := fanoutLauncher(s)
		cfg, pt := fanoutConfig(t, s)
		cfg.Overlap = schedgate.OverlapSupersede
		cfg.BotRules[0].AuthorAllowlist = nil
		cfg.BotRules[1].AuthorDenylist = nil
		var cancelled []string
		s.webhookCancelRun = func(runID string) error {
			cancelled = append(cancelled, runID)
			return nil
		}

		deliver(t, s, cfg, pt, pushOn("sha-one"))
		if len(cancelled) != 0 {
			t.Fatalf("bots on the same PR must not cancel each other, got %v", cancelled)
		}
		bots := botsOf(*got)
		if len(bots) != 2 {
			t.Fatalf("both bots claim this PR and do different jobs, got %v", bots)
		}
		seen := map[string]bool{}
		for _, b := range bots {
			seen[b] = true
		}
		if !seen["dep-update-guard"] || !seen["review-pr"] {
			t.Fatalf("each bot must survive the other's supersede, got %v", bots)
		}
	})
}
