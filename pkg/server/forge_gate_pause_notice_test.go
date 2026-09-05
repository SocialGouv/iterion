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
		// Wire the real bots/ catalog so pauseNoticeRoleForBot can read
		// each bot's manifest produces:/consumes: to derive its role
		// (reviewer vs fixer), never a bot id — the engine stays
		// bot-agnostic (CLAUDE.md).
		s.cfg.Bots.Paths = []string{botsDirAbs(t)}
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
		// BotID selects the pause-notice ROLE via the bot's manifest
		// (produces: review → reviewer; consumes: review → fixer). Without
		// it the role stays unknown and the notice is silent — the correct
		// behaviour for a bot that gates without either shape.
		run.BotID = "review-pr"
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

	t.Run("a bot that gates nothing (no gate_context) stays silent", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		// A run launched on the PR without a gate_context: it holds a
		// publish grant (the server mints one for any bot with a pr_url)
		// but claims no check, so a park there tells nobody nothing useful.
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "branch-improve-loop", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = "branch-improve-loop"
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 0 {
			t.Fatalf("a run gating nothing must post nothing, got %v", c.bodies)
		}
	})

	// A fixer (branch-improve-loop) that DOES carry gate_context (Billy
	// publishes his own verdict on the head he pushes — merge-gate.md
	// §Two bots, étape 3) must get the FIXER-specific notice: no
	// "verdict lands here" promise (his resume just re-clones the head,
	// there's no head-anchored review claim to answer) and, critically,
	// an explicit "don't push" — a push mid-park recreates the mid-run
	// collision revi-billy-loop.md forbids (the run 01a06728 dogfood
	// on #646 03/09/2026 wrongly posted the reviewer notice on Billy).
	t.Run("fixer with gate_context gets the don't-push notice, not the reviewer one", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(2 * time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "branch-improve-loop", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token", "gate_context": "revi/review",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = "branch-improve-loop"
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at, Reason: "usage_window", Attempts: 1}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 {
			t.Fatalf("a fixer with a gate context must post exactly one notice, got %d", len(c.bodies))
		}
		body := c.bodies[0]
		if !strings.Contains(body, "Fix run paused") {
			t.Fatalf("fixer notice must say \"Fix run paused\" (not \"Review paused\"):\n%s", body)
		}
		if !strings.Contains(body, "Don't push to this branch") {
			t.Fatalf("fixer notice MUST warn against pushing meanwhile:\n%s", body)
		}
		if strings.Contains(body, "The verdict lands here") {
			t.Fatal("a fixer does not promise a verdict — the reviewer's line must not appear")
		}
		if strings.Contains(body, "a new push restarts it sooner") {
			t.Fatal("a fixer has no \"restarts sooner\" mode — a push mid-park is the collision revi-billy-loop.md forbids")
		}
		// #650: the fixer wording must be behaviour-neutral on HOW the
		// resume re-anchors — the resume path owns that — so the notice
		// must not promise "re-clones the branch head".
		if strings.Contains(body, "re-clones the branch head") {
			t.Fatalf("fixer notice must use the behaviour-neutral \"re-reads the branch from the forge\" wording; got:\n%s", body)
		}
		if !strings.Contains(body, "re-reads the branch from the forge") {
			t.Fatalf("fixer notice must state the re-read behaviour so the operator knows why not to push; got:\n%s", body)
		}
	})

	// #650: an unknown role (bot not in catalog, or one that gates but
	// exposes no reviewer/fixer shape) must NOT stay silent — the run
	// already holds a required check via gate_context, and 32 of 35 catalog
	// bots classify as unknown; silence there would strand a developer on
	// a check with no signal (Vetty, for one). Post a
	// role-NEUTRAL notice (no push-side claim either way).
	t.Run("unknown role posts a neutral notice with no push-side claim", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "workflow-x", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token", "gate_context": "custom/gate",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = "no-such-bot-in-the-catalog"
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 {
			t.Fatalf("unknown role must post the neutral notice, got %d", len(c.bodies))
		}
		body := c.bodies[0]
		if !strings.Contains(body, "Run paused") {
			t.Fatalf("unknown role must open with \"Run paused\" (neither Review nor Fix):\n%s", body)
		}
		if strings.Contains(body, "The verdict lands here") {
			t.Fatal("unknown role must not promise a verdict")
		}
		if strings.Contains(body, "a new push restarts it sooner") || strings.Contains(body, "Don't push") {
			t.Fatal("unknown role must not claim anything about pushes either way")
		}
	})

	// #650: the launcher accepts both underscored and dashed spellings
	// (review-pr / review_pr — botregistry.NormalizeName folds them), but
	// effectiveFindByName was exact-match only, so a run whose BotID
	// persisted as "review_pr" classified as unknown. The pause notice's
	// role derivation must survive that.
	t.Run("normalised bot id spelling still resolves as reviewer", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
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
		// Store the launcher-normalised spelling — the catalog has "review-pr".
		run.BotID = "review_pr"
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], "Review paused") {
			t.Fatalf("normalised spelling must resolve to the reviewer notice, got %v", c.bodies)
		}
	})

	// Same for an empty BotID (inline .bot launched on a pr_url — no
	// catalog entry to derive a role from).
	t.Run("empty BotID posts the neutral notice", func(t *testing.T) {
		s, c := newWorld(t)
		at := time.Now().UTC().Add(time.Hour)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "inline-bot", map[string]any{
			"pr_url": prURL, forgePublishVarToken: "run-token", "gate_context": "custom/gate",
		})
		if err != nil {
			t.Fatal(err)
		}
		// run.BotID intentionally empty.
		run.Status = store.RunStatusFailedResumable
		run.RetryState = &store.RunRetryState{RetryAfter: &at}

		s.noticeGatePausedForRetry(context.Background(), run)

		if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], "Run paused") {
			t.Fatalf("empty BotID must post the neutral notice, got %v", c.bodies)
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
}
