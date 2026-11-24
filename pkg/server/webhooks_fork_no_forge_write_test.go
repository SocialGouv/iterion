package server

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// store.RunTrust promises an untrusted run posts "no status, no review, no
// comment". Withdrawing the run's publish grant does NOT deliver that for the
// status: markGateInFlight (and its fixer twin) write to the forge through
// the SERVER's own connection, after the launch, reading only vars.
//
// The consequence is worse than an unwanted write. The pending status it
// claims can never be answered — the run has no grant to publish a verdict
// with, and the reconciler abstains on a run whose grant is absent — so a
// required check would stay pending forever and the pull request would be
// permanently unmergeable. The lane would break exactly the PRs it exists to
// serve.
func TestLaunchWebhookTarget_AnUntrustedLaunchMakesNoForgeWrite(t *testing.T) {
	launch := func(t *testing.T, cfg webhooks.Config, trust store.RunTrust, idem string) int {
		t.Helper()
		gc := &listingGateClient{}
		s := inFlightFixture(t, gc)
		s.orgUsage = orgusage.NewMemoryCounter()
		s.webhookConfigs = webhooks.NewMemoryConfigStore()
		s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
		// An identity store holding the fixture's team: the fork target is
		// metered by the launch gate now (decision 5), and a server with no
		// identity store is local mode — which refuses an untrusted launch
		// outright rather than admitting it unmetered.
		idst := identity.NewMemoryStore()
		if _, err := idst.CreateTeam(context.Background(), identity.Team{ID: "team1", Name: "team1", Slug: "forkwrite-team1", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(make([]byte, 32)), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := auth.NewService(auth.Config{Store: idst, Sessions: auth.NewMemorySessionStore(), Signer: signer, SignupMode: auth.SignupOpen, RefreshTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		s.authSvc = svc
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			return "run-" + idem, nil
		}
		// A webhook member, not a super-admin: the fork lane's bound is the
		// perimeter against an outside party, and the gate's super-admin
		// bypass is one of the arms decision 5 closes for an untrusted
		// subject — a super-admin fork launch is refused, not excused.
		ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "team1", Role: identity.RoleMember, Kind: auth.KindWebhook})
		meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:42", Kind: "pull_request"}
		target := forgeLaunchTarget{
			IdemKey: idem, BotID: "review-pr", Vars: inFlightVars(), Trust: trust,
		}
		if !trust.Trusted() {
			target.ExpectedSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			target.ForkAuthor = &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"}
		}
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		if res.Status != webhooks.StatusLaunched {
			t.Fatalf("status = %q, want launched (err %s) — this test measures what a LAUNCHED run writes", res.Status, res.Error)
		}
		return gc.setCalls
	}

	ordinary := webhooks.Config{ID: "wh1", TenantID: "team1", Provider: webhooks.ProviderGitHub}
	forkLane := webhooks.Config{ID: "wh1", TenantID: "team1", Provider: webhooks.ProviderGitHub, ForkLane: true, ForkLanePool: "forks"}

	// The witness: the SAME vars, the same gate client, on a trusted launch —
	// a status IS posted. Without it, zero below could just mean the fixture
	// never had a gate context to claim.
	if got := launch(t, ordinary, store.RunTrustDefault, "idem-trusted"); got != 1 {
		t.Fatalf("a trusted launch posted %d statuses, want 1 — the fixture cannot witness a withheld write it could not have made", got)
	}
	if got := launch(t, forkLane, store.RunTrustFork, "idem-fork"); got != 0 {
		t.Fatalf("an untrusted launch posted %d forge status(es), want 0 — it claims a check it can never answer, leaving the PR unmergeable", got)
	}
	// A trust this binary does not recognise is not tested here because it
	// cannot reach a launch at all: the disjointness gate refuses it on BOTH
	// lane kinds (an ordinary lane requires proven trusted, the fork lane
	// requires the one class this binary knows), which
	// TestLaunchWebhookTarget_TrustPredicateAndPinCoupling pins. There is no
	// launched run for it to write from.
}
