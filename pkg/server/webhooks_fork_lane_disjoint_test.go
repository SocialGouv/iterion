package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The fork review lane and the ordinary lanes are DISJOINT subscriptions, and
// the check lives at launchWebhookTarget rather than at the top of each
// provider handler because three of that function's five callers never cross
// a handler: the debounce sweep, the gate relaunch and the gate auto-fix each
// rebuild a target from stored state and enter the tail directly — minutes to
// hours after the event, which is exactly when the config or the pull request
// has had time to change.
//
// Both directions are refused. The dangerous one is a fork target on an
// ordinary config: that config carries the repo's publish grant and the
// tenant's secret pins.
func TestLaunchWebhookTarget_RefusesALaneKindMismatch(t *testing.T) {
	type tc struct {
		name     string
		forkLane bool
		trust    store.RunTrust
		want     string
		wantWord string
	}
	cases := []tc{
		{
			name: "ordinary config + fork target", forkLane: false, trust: store.RunTrustFork,
			want: webhooks.StatusFiltered, wantWord: "ordinary lane",
		},
		{
			name: "fork-lane config + trusted target", forkLane: true, trust: store.RunTrustDefault,
			want: webhooks.StatusFiltered, wantWord: "opt-in fork review lane",
		},
		{
			name: "ordinary config + trusted target launches", forkLane: false, trust: store.RunTrustDefault,
			want: webhooks.StatusLaunched,
		},
		{
			name: "fork-lane config + fork target launches", forkLane: true, trust: store.RunTrustFork,
			want: webhooks.StatusLaunched,
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			counter := orgusage.NewMemoryCounter()
			s.orgUsage = counter
			launched := 0
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				launched++
				return "run-1", nil
			}
			ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1"})
			cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: c.forkLane}
			meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:7", Kind: "pull_request"}
			// The vars a REAL target carries. reviewPRVars always sets
			// pr_url, and an earlier revision of the grant withdrawal turned
			// that into a launch failure — invisible to this test precisely
			// because its target carried none. A stub missing the lane's
			// mandatory var certifies nothing.
			target := forgeLaunchTarget{
				IdemKey: "idem-disjoint-" + string(rune('a'+i)),
				BotID:   "review-pr",
				Vars:    map[string]string{"pr_url": "https://github.com/o/r/pull/7", "base_ref": "main"},
				Trust:   c.trust,
			}
			// An untrusted target must carry the commit its admission proved;
			// the tail refuses one that does not.
			if !c.trust.Trusted() {
				target.ExpectedSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			}

			res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")

			if res.Status != c.want {
				t.Fatalf("status = %q, want %q (res: %+v)", res.Status, c.want, res)
			}
			if c.want == webhooks.StatusLaunched {
				if launched != 1 {
					t.Fatalf("matching lane kinds launched %d run(s), want 1 — the gate must not refuse a legitimate pairing", launched)
				}
				return
			}
			if launched != 0 {
				t.Fatalf("a lane-kind mismatch launched %d run(s), want 0", launched)
			}
			if !strings.Contains(res.Error, c.wantWord) {
				t.Fatalf("refusal = %q, want it to name %q so the operator knows which of the two lanes to look at", res.Error, c.wantWord)
			}
			// Refused BEFORE metering: a mismatch that charged the org's
			// monthly quota would be a denial-of-wallet with no run to show
			// for it, and the sweeps retry.
			u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if u.Runs != 0 {
				t.Fatalf("monthly runs = %d after a refused mismatch, want 0 — the gate has to sit ahead of gateLaunch", u.Runs)
			}
		})
	}
}

// Two properties the lane's chokepoint must hold that the table above does
// not reach, each the subject of a HIGH finding.
func TestLaunchWebhookTarget_TrustPredicateAndPinCoupling(t *testing.T) {
	newCase := func(t *testing.T) (*Server, context.Context, *orgusage.MemoryCounter, *int) {
		t.Helper()
		s := newWebhookTestServer(t)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-1", nil
		}
		return s, auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1"}), counter, &launched
	}
	meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:9", Kind: "pull_request"}
	vars := func() map[string]string {
		return map[string]string{"pr_url": "https://github.com/o/r/pull/9", "base_ref": "main"}
	}

	// `ForkLane != Trust.IsFork()` admits an unrecognised trust onto an
	// ORDINARY lane, because IsFork() is false for it. store.RunTrust
	// forbids that reading in as many words; this is the site where it
	// would cost the most.
	t.Run("an unrecognised trust is refused on an ordinary lane", func(t *testing.T) {
		s, ctx, counter, launched := newCase(t)
		cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: false}
		target := forgeLaunchTarget{
			IdemKey: "idem-unknown", BotID: "review-pr", Vars: vars(),
			Trust: store.RunTrust("quarantine-v2"), ExpectedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		}
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		if res.Status != webhooks.StatusFiltered || *launched != 0 {
			t.Fatalf("status=%q launched=%d — an ordinary lane must require PROVEN trusted, not merely 'not fork'", res.Status, *launched)
		}
		if u, _ := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC()); u.Runs != 0 {
			t.Fatalf("monthly runs = %d, want 0 — the refusal must precede metering", u.Runs)
		}
	})

	// verifyFetchedCommit is a no-op on an empty pin, so an untrusted launch
	// without one fetches whatever its author points the ref at now. The
	// branch wrote that hazard down as a comment; it has to be a check.
	t.Run("an untrusted launch with no admitted commit is refused", func(t *testing.T) {
		s, ctx, _, launched := newCase(t)
		cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true}
		target := forgeLaunchTarget{
			IdemKey: "idem-nopin", BotID: "review-pr", Vars: vars(),
			Trust: store.RunTrustFork, ExpectedSHA: "   ", // blank reads as absent everywhere
		}
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		if res.Status != webhooks.StatusFiltered || *launched != 0 {
			t.Fatalf("status=%q launched=%d — an untrusted launch with no pin disables the runner's comparison entirely", res.Status, *launched)
		}
		if !strings.Contains(res.Error, "no admitted commit") {
			t.Fatalf("refusal = %q, want it to name the missing pin", res.Error)
		}
	})

	// And the pairing that MUST work, or the lane cannot exist.
	t.Run("a fork lane with a pinned commit launches", func(t *testing.T) {
		s, ctx, _, launched := newCase(t)
		cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, ForkLane: true}
		target := forgeLaunchTarget{
			IdemKey: "idem-ok", BotID: "review-pr", Vars: vars(),
			Trust: store.RunTrustFork, ExpectedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		}
		res := s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		if res.Status != webhooks.StatusLaunched || *launched != 1 {
			t.Fatalf("status=%q launched=%d, want a launch — every guard added here must leave the lane's own shape working (err: %s)", res.Status, *launched, res.Error)
		}
	})
}

// forgePREventTargets is the target builder for BOTH PR-event lanes — the
// auto-review path, which IS the fork review lane's primary route. The
// provenance was threaded through every other launch route (the command lane,
// the debounce row, the board dispatch) and stopped one call short of this
// one, so a fork-lane config could admit nothing: its own builder produced
// targets with no trust, which the disjointness gate then refused.
//
// The shipped disjointness table cannot see that — it hand-builds its
// targets, never calling this. A stub that supplies the field under test
// certifies nothing about the code that must supply it.
func TestForgePREventTargets_CarriesTheLaunchProvenance(t *testing.T) {
	cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub}
	rules := []webhooks.BotRule{{BotID: "review-pr"}, {BotID: "second-bot"}}
	build := func(prov launchProvenance) []forgeLaunchTarget {
		return forgePREventTargets(cfg, rules, "idem", "https://github.com/o/r/pull/7", "main", "notes",
			"https://github.com/o/r.git", "refs/pull/7/head", nil, prov)
	}

	prov := launchProvenance{Trust: store.RunTrustFork, ExpectedSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
	got := build(prov)
	if len(got) != 2 {
		t.Fatalf("built %d targets, want one per rule", len(got))
	}
	for _, target := range got {
		if target.Trust != prov.Trust || target.ExpectedSHA != prov.ExpectedSHA {
			t.Fatalf("%s: trust=%q pin=%q — every target of the fan-out must carry it, or one bot launches neutered and its sibling does not",
				target.BotID, target.Trust, target.ExpectedSHA)
		}
	}

	// And the zero value keeps both of today's callers byte-identical.
	for _, target := range build(launchProvenance{}) {
		if !target.Trust.Trusted() || target.ExpectedSHA != "" {
			t.Fatalf("%s: a trusted caller produced trust=%q pin=%q, want the untouched defaults", target.BotID, target.Trust, target.ExpectedSHA)
		}
	}
}
