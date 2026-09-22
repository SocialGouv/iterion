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
			target := forgeLaunchTarget{
				IdemKey: "idem-disjoint-" + string(rune('a'+i)),
				BotID:   "review-pr",
				Vars:    map[string]string{},
				Trust:   c.trust,
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
			u, err := counter.Usage(context.Background(), "t1", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if u.Runs != 0 {
				t.Fatalf("monthly runs = %d after a refused mismatch, want 0 — the gate has to sit ahead of gateLaunch", u.Runs)
			}
		})
	}
}
