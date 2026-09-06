package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A fixer that DECLINED did not die: it read the diff, concluded its mission
// presumed a defect that is not there, and pushed nothing on purpose (issue
// #706). Two things follow, and both are keyed on the typed code alone —
// never on which bot it was, which is what keeps the engine bot-agnostic:
//
//  1. The unattended lanes must treat it as a no-op. A refusal is not a dead
//     run to relaunch, and re-dispatching it re-derives the same answer.
//  2. Somebody has to be told. The head did not move, so the gate stays
//     exactly as it was and the pull request would otherwise carry no trace
//     of the decision at all — the reviewer's red check with a fixer that
//     appears to have done nothing.
func TestFixerDeclineIsANoOpWithANotice(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "iterion/review"
		reason = "the merge queue ejected this PR on an unrelated flaky test; the diff introduces no defect [verified: HEAD unmoved, tree clean]"
	)

	newWorld := func(t *testing.T) (*Server, *stubCommenter, *int) {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
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

		ints := forge.NewMemoryRepoIntegrationStore()
		if err := ints.Create(context.Background(), forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
			ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
		}); err != nil {
			t.Fatal(err)
		}
		// A red gate on the head: without the decline branch this lane WOULD
		// launch, which is exactly the storm the test pins shut.
		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
			return stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: gateNm}, nil
		}
		c := &stubCommenter{}
		s.forgeIssueCommenterFor = func(context.Context, forge.Connection) (forgeIssueCommenter, error) {
			return c, nil
		}
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-fixer", nil
		}
		return s, c, &launched
	}

	seedRun := func(t *testing.T, s *Server, code store.FailureCode, runErr string) *store.Run {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
			"pr_url": prURL, "gate_context": gateNm, "head_sha": head,
			forgePublishVarToken: "run-token",
		})
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately NOT the bot the integration resolves as its fixer: the
		// lane already refuses to relaunch a fixer off its own verdict, so a
		// fixer-bot run would make every assertion below vacuous. Any bot may
		// decline — a reviewer that finds its own dispatch premise wrong is
		// exactly as entitled to, and a fixer must not be launched off it
		// (there are no findings behind a refusal).
		run.BotID = "reviewer-bot"
		run.Status = store.RunStatusFailed
		run.FailureCode = code
		run.Error = runErr
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return run
	}

	t.Run("a declined run launches nothing and says why on the PR", func(t *testing.T) {
		s, c, launched := newWorld(t)
		run := seedRun(t, s, declinedFailureCode, reason)

		if err := s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFailed,
			Subject: trigger.Subject{ID: run.ID},
		}); err != nil {
			t.Fatalf("autofixForRun: %v", err)
		}

		if *launched != 0 {
			t.Errorf("launched %d run(s) off a DECLINED outcome — a refusal is not a dead run to retry, "+
				"and the relaunch re-derives the same answer", *launched)
		}
		if len(c.bodies) != 1 {
			t.Fatalf("exactly one decline notice must reach the pull request, got %d", len(c.bodies))
		}
		if c.repo != repo || c.number != 7 {
			t.Fatalf("notice landed on %s#%d", c.repo, c.number)
		}
		body := c.bodies[0]
		if !strings.Contains(body, reason) {
			t.Errorf("the notice does not carry the bot's own reason — the author is told a fixer ran and nothing else:\n%s", body)
		}
		if !strings.Contains(body, gateDeclineNoticeMarker) {
			t.Errorf("the notice carries no marker, so nothing can find or dedup iterion's own decline notices:\n%s", body)
		}
	})

	t.Run("a plain failure is untouched by the decline branch", func(t *testing.T) {
		// The negative that keeps the branch honest: a fixer that actually
		// DIED still gets whatever the lane would normally do, and posts no
		// decline notice.
		s, c, launched := newWorld(t)
		run := seedRun(t, s, store.FailureExecutionFailed, "node \"campaign\": boom")

		if err := s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFailed,
			Subject: trigger.Subject{ID: run.ID},
		}); err != nil {
			t.Fatalf("autofixForRun: %v", err)
		}
		// The differential that makes the case above mean something: on the
		// SAME world, a run that is not DECLINED does reach the launch. If
		// this ever stops launching, the decline branch is being credited
		// for a refusal some other guard was already making.
		if *launched == 0 {
			t.Fatal("the lane refused a plain failure too — the decline branch proves nothing on this fixture")
		}
		for _, b := range c.bodies {
			if strings.Contains(b, gateDeclineNoticeMarker) {
				t.Fatalf("a crashed run got a decline notice:\n%s", b)
			}
		}
	})

	t.Run("the notice is keyed on the code, not on the bot", func(t *testing.T) {
		// Any bot may decline; the lane knows the code and nothing else.
		s, c, launched := newWorld(t)
		run := seedRun(t, s, declinedFailureCode, reason)
		run.BotID = "some-other-bot"
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if err := s.autofixForRun(context.Background(), trigger.Event{
			Source: trigger.SourceRun, Kind: trigger.KindRunFailed,
			Subject: trigger.Subject{ID: run.ID},
		}); err != nil {
			t.Fatalf("autofixForRun: %v", err)
		}
		if *launched != 0 {
			t.Errorf("launched %d run(s) off a DECLINED outcome from another bot", *launched)
		}
		if len(c.bodies) != 1 {
			t.Fatalf("the decline notice must not depend on which bot declined, got %d bodies", len(c.bodies))
		}
	})
}

// The mission the merge-queue auto-heal hands the fixer used to presume the
// defect exists ("Rebase … and fix whatever breaks …"), so "there is nothing
// to fix" was not an outcome the bot could return — only a preamble to doing
// it anyway. It must now grant the refusal explicitly, in bot-agnostic terms.
func TestAutoHealMissionPermitsARefusal(t *testing.T) {
	mission := autoHealMission("CI failure", "main", "Fix the thing", "the description")
	for _, want := range []string{"nothing", "push"} {
		if !strings.Contains(strings.ToLower(mission), want) {
			t.Errorf("auto-heal mission does not mention %q:\n%s", want, mission)
		}
	}
	if !strings.Contains(strings.ToLower(mission), "decline") {
		t.Errorf("the auto-heal mission never grants the fixer permission to decline, so a bot that "+
			"correctly concludes the eject was not this branch's fault has no outcome but to push anyway:\n%s", mission)
	}
	// Bot-agnostic: the mission names a role and an outcome, never a bot.
	for _, forbidden := range []string{"branch-improve-loop", "billy"} {
		if strings.Contains(strings.ToLower(mission), forbidden) {
			t.Errorf("the auto-heal mission names %q — the engine must not know which bot serves the role", forbidden)
		}
	}
}
