package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// gateLaunch's run-quota increment IS the metering, so every surface that
// abandons an admitted launch without creating a run has to release the unit.
// The webhook tail meters once per distinct delivery — the idempotency key
// carries the head SHA, so each push is a fresh key and a fresh increment —
// and a launch that FAILS creates no run at all. Leaving the unit spent turns
// a repeatable launch failure (a required secret that cannot resolve, a bot
// the registry cannot load, a publisher refusal) into monthly-quota
// exhaustion for the whole org, denying the surfaces that do work.
//
// The three sibling launch surfaces already release it: the trigger spine
// (trigger_launcher.go), the board dispatcher (boarddispatch.go) and the
// retry sweeper (retry_sweeper.go).
func TestLaunchWebhookTarget_ReleasesTheMeteredSlotWhenTheLaunchFails(t *testing.T) {
	newCase := func(t *testing.T, launchErr error) (*Server, *orgusage.MemoryCounter, context.Context) {
		t.Helper()
		s := newWebhookTestServer(t)
		counter := orgusage.NewMemoryCounter()
		s.orgUsage = counter
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			if launchErr != nil {
				return "", launchErr
			}
			return "run-1", nil
		}
		// A real tenant identity: a super-admin bypasses the gate entirely,
		// so metering only happens for the shape this test is about.
		ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "t1"})
		return s, counter, ctx
	}

	run := func(t *testing.T, s *Server, ctx context.Context, idem string) webhookLaunchResult {
		t.Helper()
		cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub}
		meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:7", Kind: "pull_request"}
		target := forgeLaunchTarget{IdemKey: idem, BotID: "review-pr", Vars: map[string]string{}}
		return s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
	}

	t.Run("an admitted launch keeps its slot", func(t *testing.T) {
		s, counter, ctx := newCase(t, nil)
		if res := run(t, s, ctx, "idem-ok"); res.Status != webhooks.StatusLaunched {
			t.Fatalf("status = %q, want %q (res: %+v)", res.Status, webhooks.StatusLaunched, res)
		}
		u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 1 {
			t.Fatalf("monthly runs = %d after one launched delivery, want 1 — a run that started spends a slot", u.Runs)
		}
	})

	t.Run("a failed launch releases its slot", func(t *testing.T) {
		s, counter, ctx := newCase(t, errors.New("resolve workflow secrets: required secret \"forge_token\" is not bound"))
		res := run(t, s, ctx, "idem-fail")
		if res.Status != webhooks.StatusLaunchError {
			t.Fatalf("status = %q, want %q (res: %+v)", res.Status, webhooks.StatusLaunchError, res)
		}
		u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 0 {
			t.Fatalf("monthly runs = %d after a launch that created no run, want 0 — an attacker who can make the launch fail repeatably would otherwise exhaust the org's monthly quota one delivery at a time", u.Runs)
		}
	})

	// The keep-direction (#1725): a publish that LANDED and then reported
	// failure leaves a run the runner may claim — the slot stays. A plain
	// failure above still refunds; the error's own fact decides.
	t.Run("a landed-but-failed publish keeps the slot", func(t *testing.T) {
		s, counter, ctx := newCase(t, &runview.QueueUnavailableError{Cause: errors.New("PROBE: ack timeout after the message landed")})
		res := run(t, s, ctx, "idem-queue")
		if res.Status != webhooks.StatusLaunchError {
			t.Fatalf("status = %q, want %q (res: %+v)", res.Status, webhooks.StatusLaunchError, res)
		}
		u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 1 {
			t.Fatalf("monthly runs = %d after a publish that landed, want 1 — the slot of a run the runner may be executing must not be handed back", u.Runs)
		}
	})

	// The leak is only interesting because it ACCUMULATES: the idempotency
	// key carries the head SHA, so N pushes are N distinct deliveries. Three
	// distinct failed deliveries must leave the counter where they found it.
	t.Run("repeated failures do not accumulate", func(t *testing.T) {
		s, counter, ctx := newCase(t, errors.New("launch refused"))
		for _, idem := range []string{"idem-a", "idem-b", "idem-c"} {
			if res := run(t, s, ctx, idem); res.Status != webhooks.StatusLaunchError {
				t.Fatalf("%s: status = %q, want %q", idem, res.Status, webhooks.StatusLaunchError)
			}
		}
		u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 0 {
			t.Fatalf("monthly runs = %d after three distinct failed deliveries, want 0", u.Runs)
		}
	})
}
