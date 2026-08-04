package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The auto-fix lane launches a code-mutating bot with no human in the loop, on
// money the team pays. Every one of its refusals is therefore load-bearing, and
// a refusal that silently stops working is indistinguishable from the lane
// simply not firing — so they are pinned here rather than trusted to the
// walk-through.
//
// The happy path is exercised by the integration-shaped test at the bottom; the
// table above it is the set of ways it must NOT fire.
func TestAutofixRefusesWhatItMust(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "iterion/review"
	)

	type world struct {
		s        *Server
		launched *int
	}
	build := func(t *testing.T, tune func(*Server, *forge.RepoIntegration, *webhooks.Config)) world {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")

		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs

		conns := forge.NewMemoryConnectionStore()
		conn := forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}
		if err := conns.Create(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})

		ints := forge.NewMemoryRepoIntegrationStore()
		integ := forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}
		cfg := webhooks.Config{ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"}}
		if tune != nil {
			tune(s, &integ, &cfg)
		}
		if err := ints.Create(context.Background(), integ); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}

		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
			return stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: gateNm}, nil
		}

		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-fixer", nil
		}
		return world{s: s, launched: &launched}
	}

	// seedRun writes the reviewing run the event will point at.
	seedRun := func(t *testing.T, s *Server, botID string, inputs map[string]any) string {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, botID, inputs)
		if err != nil {
			t.Fatal(err)
		}
		// CreateRun's third argument is the WORKFLOW name; the bot id is a
		// separate field the launch path sets, and the resolver matches on it.
		run.BotID = botID
		run.Status = store.RunStatusFinished
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}

	gatingInputs := map[string]any{
		"pr_url": prURL, "gate_context": gateNm, "head_sha": head,
		forgePublishVarToken: "run-token",
	}

	cases := []struct {
		name    string
		inputs  map[string]any
		botID   string
		tune    func(*Server, *forge.RepoIntegration, *webhooks.Config)
		because string
	}{
		{
			name: "the repo did not opt in", inputs: gatingInputs, botID: "reviewer-bot",
			tune:    func(_ *Server, i *forge.RepoIntegration, _ *webhooks.Config) { i.AutoFixOnGateFailure = false },
			because: "off is the default, and a repo that never asked must never get an unattended code-mutating run",
		},
		{
			name:    "the run pinned no gate context",
			inputs:  map[string]any{"pr_url": prURL, "head_sha": head, forgePublishVarToken: "run-token"},
			botID:   "reviewer-bot",
			because: "with no pinned context there is no check to read a verdict from",
		},
		{
			name:    "the run names no revision",
			inputs:  map[string]any{"pr_url": prURL, "gate_context": gateNm, forgePublishVarToken: "run-token"},
			botID:   "reviewer-bot",
			because: "a run that cannot say which commit it judged cannot justify acting on any",
		},
		{
			name:    "the run holds no publish grant",
			inputs:  map[string]any{"pr_url": prURL, "gate_context": gateNm, "head_sha": head},
			botID:   "reviewer-bot",
			because: "the grant is what bounds this to a repo the team actually connected",
		},
		{
			name: "the pr_url points outside the grant's repo",
			inputs: map[string]any{
				"pr_url": "https://github.com/attacker/elsewhere/pull/1", "gate_context": gateNm,
				"head_sha": head, forgePublishVarToken: "run-token",
			},
			botID:   "reviewer-bot",
			because: "pr_url is a caller-chosen launch var; unchecked it would aim a mutating bot at any repo the connection reaches",
		},
		{
			name: "the fixer is not permitted on the webhook", inputs: gatingInputs, botID: "reviewer-bot",
			tune:    func(_ *Server, _ *forge.RepoIntegration, c *webhooks.Config) { c.BotIDs = []string{"someone-else"} },
			because: "the webhook's bot scope is the same admission every other lane applies",
		},
		{
			name: "the finished run IS the fixer", inputs: gatingInputs, botID: "fixer-bot",
			because: "a bot re-triggering itself on its own red verdict is a loop, refused on its face",
		},
		{
			// Found adversarially: gate_context comes from the RUN's inputs,
			// which whoever launched it chose. Unchecked, any red status on the
			// head whose name some run had used — a failing CI build, a
			// coverage bot — would launch a code-pushing agent.
			name: "the red status is not the check the repo pinned",
			inputs: map[string]any{
				"pr_url": prURL, "gate_context": "ci/build", "head_sha": head,
				forgePublishVarToken: "run-token",
			},
			botID:   "reviewer-bot",
			because: "only the repo's own required check may drive an unattended push",
		},
		{
			name: "the repo pinned no gate context at all", inputs: gatingInputs, botID: "reviewer-bot",
			tune:    func(_ *Server, i *forge.RepoIntegration, _ *webhooks.Config) { i.LaunchVars = nil },
			because: "a repo that pinned none has made no check required, so there is no gate to react to",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := build(t, c.tune)
			runID := seedRun(t, w.s, c.botID, c.inputs)
			_ = w.s.autofixForRun(context.Background(), trigger.Event{
				Source: trigger.SourceRun, Kind: trigger.KindRunFinished,
				Subject: trigger.Subject{ID: runID},
			})
			if *w.launched != 0 {
				t.Errorf("launched a fixer anyway — %s", c.because)
			}
		})
	}

	// A resumable failure is not a dead run: the runner republishes the outcome
	// before deciding to retry, and the gate it left is about to change.
	t.Run("a resumable failure is not a verdict", func(t *testing.T) {
		w := build(t, nil)
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := w.s.cfg.Store.CreateRun(context.Background(), id, "reviewer-bot", gatingInputs)
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = "reviewer-bot"
		run.Status = store.RunStatusFailedResumable
		if err := w.s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		_ = w.s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFailed,
			Subject: trigger.Subject{ID: run.ID},
		})
		if *w.launched != 0 {
			t.Error("acted on a run that is expected to resume and post its own verdict")
		}
	})

	// The per-head claim bounds a fixer that STOPS pushing. One that keeps
	// pushing without converging moves the head every pass, which frees a fresh
	// claim each time — so a ceiling per PR is the only thing that ends it, and
	// the org cost cap (the other backstop) defaults to unlimited.
	t.Run("a PR that never converges stops being auto-fixed", func(t *testing.T) {
		w := build(t, nil)
		for i := 0; i < maxAutofixAttemptsPerPR; i++ {
			if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
				ID: "spent-" + string(rune('a'+i)), TenantID: team, WebhookID: "w1",
				IdempotencyKey: "k" + string(rune('a'+i)), Status: webhooks.StatusLaunched,
				EventKind: autofixEventKind, ProjectPath: repo, SubjectID: "pr:7", RunID: "r",
			}); err != nil {
				t.Fatal(err)
			}
		}
		runID := seedRun(t, w.s, "reviewer-bot", gatingInputs)
		_ = w.s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFinished,
			Subject: trigger.Subject{ID: runID},
		})
		if *w.launched != 0 {
			t.Errorf("a %dth unattended pass fired on a PR that has not converged", maxAutofixAttemptsPerPR+1)
		}
	})

	// The per-head claim is what bounds the loop: the fixer pushes, the head
	// moves, and only then is another attempt available. Without it a red gate
	// that nobody fixes relaunches on every re-review, forever, on real money.
	t.Run("one attempt per head sha", func(t *testing.T) {
		w := build(t, nil)
		idem := knowledge.ChecksumHex([]byte("autofix|" + team + "|" + repo + "|7|" + head))
		if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
			ID: "d1", TenantID: team, WebhookID: "w1", IdempotencyKey: idem, Status: webhooks.StatusLaunched,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := w.s.webhookDeliveries.GetByIdempotencyKey(context.Background(), idem)
		if err != nil || got.ID != "d1" {
			t.Fatalf("the claim key must be readable back, got %+v (%v)", got, err)
		}
		runID := seedRun(t, w.s, "reviewer-bot", gatingInputs)
		_ = w.s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFinished,
			Subject: trigger.Subject{ID: runID},
		})
		if *w.launched != 0 {
			t.Error("a second attempt fired on a head that already had one — the loop is unbounded")
		}
	})
}

// The table above only means something if the lane CAN fire: an assertion that
// nothing launched is satisfied just as well by a lane that never works. This is
// the positive control, and it runs the same builder as every refusal case.
func TestAutofixLaunchesTheFixerOnARedGate(t *testing.T) {
	const (
		team  = "t1"
		repo  = "acme/widgets"
		prURL = "https://github.com/acme/widgets/pull/7"
		head  = "cafe1234cafe1234cafe1234cafe1234cafe1234"
	)
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
		BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
		LaunchVars: map[string]string{gateContextVar: "iterion/review"},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
		return stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: "iterion/review"}, nil
	}

	var gotBot string
	var gotVars map[string]string
	var gotCtx context.Context
	s.webhookLaunchBot = func(ctx context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotBot, gotVars, gotCtx = botID, vars, ctx
		return "run-fixer", nil
	}

	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
		"pr_url": prURL, "gate_context": "iterion/review", "head_sha": head,
		forgePublishVarToken: "run-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	run.BotID = "reviewer-bot"
	run.Status = store.RunStatusFinished
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	if err := s.autofixForRun(context.Background(), trigger.Event{
		Source: trigger.SourceRun, Kind: trigger.KindRunFinished,
		Subject: trigger.Subject{ID: run.ID},
	}); err != nil {
		t.Fatalf("handler returned %v", err)
	}
	if gotBot != "fixer-bot" {
		t.Fatalf("launched %q, want the bot that declares it consumes a review", gotBot)
	}
	// A bus handler is not an HTTP request: it carries neither identity unless
	// this lane stamps both. The auth half gates admission; the STORE half is
	// what every tenant-scoped query asserts on, and a launch missing it does
	// not fail — it trips the tenancy guard deep inside SaveRun and takes the
	// process down. The stub below this seam is exactly where that was
	// invisible, so the contract is asserted on the ctx handed across it.
	if tenant, ok := store.TenantFromContext(gotCtx); !ok || tenant != "t1" {
		t.Errorf("launch ctx carries no store tenant (got %q, ok=%v) — the launch would reach Mongo untenanted", tenant, ok)
	}
	if id, ok := auth.FromContext(gotCtx); !ok || id.TeamID != "t1" {
		t.Errorf("launch ctx carries no auth identity (got %+v, ok=%v) — the admission gate has nothing to meter", id, ok)
	}
	// It must reach the fixer knowing WHICH revision it is answering for, or the
	// verdict it posts at the end cannot be pinned to one.
	if gotVars["head_sha"] != head || gotVars["pr_url"] != prURL {
		t.Errorf("the launch lacks its PR context: %v", gotVars)
	}
	// And the claim must exist afterwards, or the next re-review fires again.
	idem := knowledge.ChecksumHex([]byte("autofix|" + team + "|" + repo + "|7|" + head))
	if _, err := s.webhookDeliveries.GetByIdempotencyKey(context.Background(), idem); err != nil {
		t.Errorf("no per-head claim was recorded (%v) — the loop would be unbounded", err)
	}
}

// stubGateClient answers with one commit status on one head.
type stubGateClient struct {
	head    string
	state   forge.CommitState
	ctxName string
}

func (c stubGateClient) GetPullRequest(_ context.Context, _ string, number int) (forge.PullRef, error) {
	return forge.PullRef{Number: number, State: "open", HeadSHA: c.head, SourceBranch: "feat/x", TargetBranch: "main"}, nil
}
func (c stubGateClient) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	return nil
}
func (c stubGateClient) ListCommitStatuses(context.Context, string, string) ([]forge.CommitStatus, error) {
	return []forge.CommitStatus{{Context: c.ctxName, State: c.state}}, nil
}

// TestReviewFixerIsDerivedNotNamed: the lane must pick the fixer from what a bot
// DECLARES it consumes, never from a bot id in the engine. A repo that enables a
// different fixer gets that one.
func TestReviewFixerIsDerivedNotNamed(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "some-other-fixer", "prior_review")

	if got := s.reviewFixerFor(forge.RepoIntegration{BotIDs: []string{"some-other-fixer"}}); got != "some-other-fixer" {
		t.Errorf("reviewFixerFor = %q, want the bot that declares it consumes a review", got)
	}
	// A bot the repo has not enabled is not a candidate, however it is declared.
	if got := s.reviewFixerFor(forge.RepoIntegration{BotIDs: []string{"unrelated"}}); got != "" {
		t.Errorf("reviewFixerFor = %q, want none — that bot is not enabled on the repo", got)
	}
	if got := s.reviewFixerFor(forge.RepoIntegration{}); got != "" {
		t.Errorf("reviewFixerFor = %q, want none", got)
	}
}

// TestGateStatusOnSeparatesUnreadableFromAbsent pins the distinction the two
// callers assume opposite things from: a provider that cannot list statuses at
// all makes the reconciler abstain from overwriting, and makes this lane
// abstain from launching. Collapsing it into "absent" would flip both.
func TestGateStatusOnSeparatesUnreadableFromAbsent(t *testing.T) {
	ctx := context.Background()

	_, readable, err := gateStatusOn(ctx, noListGateClient{}, "acme/widgets", "abc", "iterion/review")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if readable {
		t.Error("a client that cannot list statuses must not report a readable gate")
	}
	// Unreadable must read as "already posted" for the reconciler, or it would
	// paint a synthetic failure over a verdict it never saw.
	if posted, err := gateAlreadyPosted(ctx, noListGateClient{}, "acme/widgets", "abc", "iterion/review"); err != nil || !posted {
		t.Errorf("gateAlreadyPosted = %v (%v), want true on an unreadable provider", posted, err)
	}

	// Readable but absent is a different fact: nothing has posted yet.
	empty := stubGateClient{head: "abc", ctxName: "other/check", state: forge.CommitStateSuccess}
	state, readable, err := gateStatusOn(ctx, empty, "acme/widgets", "abc", "iterion/review")
	if err != nil || !readable || state != "" {
		t.Errorf("gateStatusOn = (%q, %v, %v), want the zero state, readable, no error", state, readable, err)
	}
	if posted, err := gateAlreadyPosted(ctx, empty, "acme/widgets", "abc", "iterion/review"); err != nil || posted {
		t.Errorf("gateAlreadyPosted = %v (%v), want false when the context is absent", posted, err)
	}
}

type noListGateClient struct{}

func (noListGateClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{}, nil
}
func (noListGateClient) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	return nil
}
