package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The sync-debounce sweep is the caller the launch tail's disjointness gate
// exists FOR — it fires minutes to hours after the event, which is when the
// config has had time to change. So it is also the caller most likely to meet
// the tail's refusals, and it classified anything not launched/duplicate as a
// transient failure.
//
// A refusal is a VERDICT: it is a pure function of the parked row and the
// config the sweep just re-read, so retrying cannot change the answer. Read
// as a failure it burned the whole retry budget, wrote a terminal delivery
// row per attempt, and finally told the operator the review had been
// ABANDONED with a launch_error — a diagnosis that sends them looking in the
// wrong place for something that is working as designed.
func TestSyncDebounce_ARefusalIsAVerdictNotAFailure(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	attempts := 0
	s.webhookLaunchBot = func(_ context.Context, bot string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		attempts++
		return "run-" + bot, nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	// A push parks a review on the ordinary lane…
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	// …and inside the quiet window the operator flips the subscription's lane
	// kind. The parked target is trusted; the config is now the fork lane.
	stored, err := s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.ForkLane = true
	if err := s.webhookConfigs.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC()
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(4*time.Minute))
	if attempts != 0 {
		t.Fatalf("a refused launch started %d run(s), want 0", attempts)
	}

	// The row is ACKNOWLEDGED, exactly as a duplicate is: no re-arm, no
	// second attempt, no growing pile of terminal rows.
	for _, at := range []time.Duration{20 * time.Minute, 2 * time.Hour, 24 * time.Hour} {
		s.sweepDeferredWebhookLaunches(context.Background(), base.Add(at))
	}

	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	filtered, launchErrors := 0, 0
	for _, d := range rows {
		switch d.Status {
		case webhooks.StatusFiltered:
			filtered++
		case webhooks.StatusLaunchError:
			launchErrors++
		}
	}
	if launchErrors != 0 {
		t.Fatalf("the sweep recorded %d launch_error row(s) for a REFUSAL — the operator is told the review was abandoned by a failure, for something working as designed (rows: %+v)", launchErrors, rows)
	}
	if filtered != 1 {
		t.Fatalf("recorded %d filtered rows, want exactly 1 — a verdict is written once, not once per retry", filtered)
	}
}
