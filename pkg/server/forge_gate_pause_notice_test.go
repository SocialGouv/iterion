package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

type stubCommenter struct {
	bodies []string
	repo   string
	number int
}

func (c *stubCommenter) CommentIssue(_ context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	c.repo, c.number = repo, number
	c.bodies = append(c.bodies, body)
	return forge.CommentRef{}, nil
}

// A review parked on a provider quota leaves its required check on the
// in-flight claim — which reads exactly like a review that died. The park
// must therefore say so on the pull request: what happened, when it
// resumes, and (for a spend ceiling) that waiting alone may not be enough.
func TestGatePausedNoticePostsOnThePR(t *testing.T) {
	const (
		team  = "t1"
		repo  = "acme/widgets"
		prURL = "https://github.com/acme/widgets/pull/7"
	)
	newWorld := func(t *testing.T) (*Server, *stubCommenter) {
		t.Helper()
		s := newWebhookTestServer(t)
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs
		conns := forge.NewMemoryConnectionStore()
		if err := conns.Create(context.Background(), forge.Connection{
			ID: "c1", TenantID: team, Provider: forge.ProviderGitHub,
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
		c := &stubCommenter{}
		s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
			return c, nil
		}
		return s, c
	}
	parkedRun := func(t *testing.T, s *Server, errText string, at time.Time) *store.Run {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "review-pr", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token", "gate_context": "revi/review",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailedResumable
		run.Error = errText
		run.RetryState = &store.RunRetryState{RetryAfter: &at, Reason: "usage_window", Attempts: 2}
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return run
	}

	t.Run("window park says when it resumes", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(95 * time.Minute)
		run := parkedRun(t, s, "node \"campaign\": rate_limited: You've hit your weekly limit · resets 9pm", at)

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 {
			t.Fatalf("exactly one notice must be posted, got %d", len(c.bodies))
		}
		body := c.bodies[0]
		if c.repo != repo || c.number != 7 {
			t.Fatalf("notice landed on %s#%d", c.repo, c.number)
		}
		for _, want := range []string{"Review paused", at.Format("15:04 UTC"), "attempt 2", "weekly limit"} {
			if !strings.Contains(body, want) {
				t.Fatalf("notice missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "Waiting may not be enough") {
			t.Fatal("a provider WINDOW does reopen on schedule — the spend caveat must not appear")
		}
	})

	t.Run("spend ceiling warns that waiting may not suffice", func(t *testing.T) {
		s, c := newWorld(t)
		run := parkedRun(t, s, "node \"campaign\": rate_limited: You've hit your org's monthly spend limit · ask your admin to raise it", time.Now().UTC().Add(time.Hour))

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], "Waiting may not be enough") {
			t.Fatalf("a spend ceiling must say an admin has to act:\n%v", c.bodies)
		}
	})

	t.Run("a grant for another repo cannot aim the comment", func(t *testing.T) {
		s, c := newWorld(t)
		// The grant covers acme/widgets; pr_url — a LAUNCH VAR — points
		// elsewhere on the same host.
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: "acme/other"})
		run := parkedRun(t, s, "rate_limited: You've hit your weekly limit", time.Now().UTC().Add(time.Hour))

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 0 {
			t.Fatalf("the grant's repo scope must bound where the bot identity speaks, got %v", c.bodies)
		}
	})

	t.Run("a bot that owes no verdict stays silent", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		// A fixer launched on the PR: it holds a publish grant (the server
		// mints one for any bot with a pr_url) but claims no check.
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "branch-improve-loop", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 0 {
			t.Fatalf("only a run owing a gate verdict may say \"the verdict lands here\", got %v", c.bodies)
		}
	})

	t.Run("a run that gates nothing stays silent", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "feed-watch", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 0 {
			t.Fatalf("a non-gating run must post nothing, got %v", c.bodies)
		}
	})

	// A DLQ-parked run reaches this path with the same RetryState the
	// pre-park usage-window carried (an operator resume never clears it —
	// see #669 part 3), so a bare RetryAfter-driven notice would falsely
	// promise an automatic resume for a run that only replay-by-hand can
	// wake. The FailureCode is the truth: DLQ_PARKED means an operator has
	// to act; the notice must name the DLQ, name the admin path, and NOT
	// print the "resume automatically at HH:MM" line.
	t.Run("a DLQ park names the operator path, not a schedule", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		run := parkedRun(t, s, "max deliveries exhausted: some cause (parked on DLQ — replay via /api/admin/dlq)", at)
		run.FailureCode = store.FailureDLQParked
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 {
			t.Fatalf("a DLQ park must post one operator-shaped notice, got %d", len(c.bodies))
		}
		body := c.bodies[0]
		if strings.Contains(body, "provider's quota is exhausted") {
			t.Fatalf("DLQ park was announced as a quota pause — the FailureCode-blind read #669 measured live:\n%s", body)
		}
		if strings.Contains(body, "resume it **automatically") || strings.Contains(body, at.Format("15:04 UTC")) {
			t.Fatalf("DLQ park promised an automatic resume that never comes:\n%s", body)
		}
		for _, want := range []string{"parked on the DLQ", "iterion remote admin dlq", "operator"} {
			if !strings.Contains(body, want) {
				t.Fatalf("DLQ notice missing %q:\n%s", want, body)
			}
		}
	})
}
