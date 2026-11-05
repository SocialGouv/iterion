package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A hand-off is a CROSSING, and it runs in both directions (#1639).
//
// Outbound is the dangerous one: `branch-improve-loop` consumes a `review` as
// `prior_review`, and it is the FIXER — it holds the publish grant and pushes
// commits. A run on a fork contributor's workspace produces artifacts that are
// a function of outsider-authored text, so seeding the fixer from one lets the
// author of a pull request write the instructions a credentialed pushing bot
// acts on.
func TestHandoff_AnUntrustedRunsReviewSeedsNobody(t *testing.T) {
	ctx := context.Background()
	s := newBotMetadataTeamServer(t)
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))

	seedReview := func(t *testing.T, prURL string, trust store.RunTrust) {
		t.Helper()
		tctx := store.WithTenant(ctx, "t1")
		run, err := rs.CreateRun(tctx, mustRunID(t), "reviewer-bot", map[string]any{"pr_url": prURL})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFinished
		run.BotID = "reviewer-bot"
		run.BotSourceTenant = "t1"
		run.BotSourceTier = store.BotSourceTierTeam
		run.Trust = trust
		if err := rs.SaveRun(tctx, run); err != nil {
			t.Fatal(err)
		}
		if err := rs.WriteArtifact(tctx, &store.Artifact{RunID: run.ID, NodeID: "publish_fork", Data: map[string]any{
			"total_findings": 1,
			"findings": []any{map[string]any{
				"severity": "high", "category": "correctness", "title": "off-by-one",
				"file": "a.go", "line": float64(3),
			}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := webhooks.Config{ID: "wh-1", TenantID: "t1"}

	for _, row := range []struct {
		name   string
		pr     string
		trust  store.RunTrust
		handed bool
	}{
		// The control: a TRUSTED producer really is handed over. Without it the
		// refusals below are indistinguishable from "the scan finds nothing".
		{"a trusted run's review seeds the consumer", "https://forge.example/acme/widgets/-/merge_requests/101", store.RunTrustDefault, true},
		{"a fork run's review seeds nobody", "https://forge.example/acme/widgets/-/merge_requests/102", store.RunTrustFork, false},
		// Trusted() admits one value: a trust this binary does not recognise
		// loses as a fork does.
		{"an unrecognised trust seeds nobody either", "https://forge.example/acme/widgets/-/merge_requests/103", store.RunTrust("vendored"), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			seedReview(t, row.pr, row.trust)
			got := s.realWebhookHandoff(ctx, cfg, bundle.HandoffKindReview, handoffQuery{PRURL: row.pr})
			if handed := strings.Contains(got, "off-by-one"); handed != row.handed {
				t.Fatalf("producer trust=%q: handed over = %v, want %v (seed %q) — the fixer consumes this as prior_review, and it pushes commits", row.trust, handed, row.handed, got)
			}
		})
	}
}

// The INBOUND direction, on a server whose catalog really declares a
// consumer: `review-pr` consumes `review_ledger` scoped to the PR, so an
// untrusted run would be handed the tenant's own internal review prose —
// readable by whoever authored the tree it is reading. Driven through
// stampHandoffs, the one door both seeding lanes take.
func TestStampHandoffs_SeedsAnUntrustedTargetWithNothing(t *testing.T) {
	ctx := context.Background()
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	looked := 0
	s.webhookHandoff = func(context.Context, webhooks.Config, bundle.HandoffKind, handoffQuery) string {
		looked++
		return "the tenant's own review prose"
	}
	cfg := webhooks.Config{ID: "wh-1", TenantID: "t1"}
	const pr = "https://github.com/acme/widgets/pull/7"

	// The control: a trusted target is stamped from the lookup. Without it,
	// "nothing was stamped" is indistinguishable from "no consumer".
	trustedVars := map[string]string{"pr_url": pr}
	s.stampHandoffs(ctx, cfg, "fixer-bot", trustedVars, handoffQuery{PRURL: pr}, store.RunTrustDefault)
	if trustedVars["prior_review"] != "the tenant's own review prose" || looked != 1 {
		t.Fatalf("a trusted target was not stamped (vars=%v, lookups=%d) — the fixture cannot witness a refusal it never grants", trustedVars, looked)
	}

	for _, trust := range []store.RunTrust{store.RunTrustFork, store.RunTrust("vendored")} {
		vars := map[string]string{"pr_url": pr}
		s.stampHandoffs(ctx, cfg, "fixer-bot", vars, handoffQuery{PRURL: pr}, trust)
		if _, ok := vars["prior_review"]; ok {
			t.Errorf("trust=%q was stamped with a hand-off (%v) — an untrusted run must be seeded with nothing", trust, vars)
		}
	}
	if looked != 1 {
		t.Errorf("the PR's runs were looked up %d times, want 1 (the trusted control only) — an untrusted target must not even read them", looked)
	}
}

// The same refusal through the launch tail every webhook lane funnels into
// (launchWebhookTarget), on the lane that carries an untrusted target — the
// fork-lane config — with a bot whose manifest declares it consumes a review.
// What the launched run receives is the assertion: the vars handed to the
// launcher, not a helper's return value.
func TestLaunchWebhookTarget_AnUntrustedTargetIsLaunchedWithoutAHandoff(t *testing.T) {
	launchedVars := func(t *testing.T, forkLane bool, trust store.RunTrust) (map[string]string, int) {
		t.Helper()
		s := newWebhookTestServer(t)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.cfg.WorkDir = writeConsumerBotFixture(t, "review-pr", "prior_review")
		looked := 0
		s.webhookHandoff = func(context.Context, webhooks.Config, bundle.HandoffKind, handoffQuery) string {
			looked++
			return "the tenant's own review prose"
		}
		var got map[string]string
		s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
			got = vars
			return "run-1", nil
		}
		ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1", Role: identity.RoleMember, Kind: auth.KindWebhook})
		cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: forkLane}
		meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:7", Kind: "pull_request"}
		target := forgeLaunchTarget{
			IdemKey: "idem-handoff-" + string(trust),
			BotID:   "review-pr",
			Vars:    map[string]string{"pr_url": "https://github.com/o/r/pull/7", "base_ref": "main"},
			Trust:   trust,
		}
		if !trust.Trusted() {
			target.ExpectedSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
		}
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		if res.Status != webhooks.StatusLaunched || got == nil {
			t.Fatalf("trust=%q: status = %q (res: %+v) — the row never reached the launcher, so it proves nothing", trust, res.Status, res)
		}
		return got, looked
	}

	// The control: the ordinary lane's trusted target is seeded.
	if vars, looked := launchedVars(t, false, store.RunTrustDefault); vars["prior_review"] != "the tenant's own review prose" || looked == 0 {
		t.Fatalf("a trusted target was launched without its declared seed (vars=%v, lookups=%d) — the fixture cannot witness a refusal it never grants", vars, looked)
	}
	vars, looked := launchedVars(t, true, store.RunTrustFork)
	if _, ok := vars["prior_review"]; ok {
		t.Errorf("the fork target was launched with prior_review=%q — the tenant's review prose handed to a run an outsider's code steers", vars["prior_review"])
	}
	if looked != 0 {
		t.Errorf("the PR's runs were looked up %d time(s) for an untrusted target, want 0", looked)
	}
}
