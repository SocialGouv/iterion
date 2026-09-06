package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// barrierDeliveryStore holds every idempotency-key read until n of them are
// in flight, then releases them together. It reproduces the window the
// launch_error retry lives in: two redeliveries of the same event both find
// the failed row before either has taken it over.
type barrierDeliveryStore struct {
	webhooks.DeliveryStore
	mu   sync.Mutex
	once sync.Once
	seen int
	n    int
	open chan struct{}
}

func newBarrierDeliveryStore(inner webhooks.DeliveryStore, n int) *barrierDeliveryStore {
	return &barrierDeliveryStore{DeliveryStore: inner, open: make(chan struct{}), n: n}
}

func (s *barrierDeliveryStore) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	d, err := s.DeliveryStore.GetByIdempotencyKey(ctx, key)
	s.mu.Lock()
	s.seen++
	held := s.seen <= s.n
	if s.seen >= s.n {
		s.once.Do(func() { close(s.open) })
	}
	s.mu.Unlock()
	// Only the first n reads wait: the claim LOSER re-reads the winning row
	// on its way out, and holding that read too would deadlock the test on
	// the very branch it exists to exercise.
	if held {
		select {
		case <-s.open:
		case <-time.After(5 * time.Second):
		}
	}
	return d, err
}

// A prior launch_error row is deliberately RETRYABLE: a redelivery of the same
// event must be able to relaunch it. Taking it over is therefore a CLAIM, not
// a read followed by a write — two redeliveries arriving together both find
// the failed row, and without the claim both go on to launch a run for one
// event, which is the storm shape a redelivery burst has.
func TestLaunchWebhookTarget_FailedRowRetryIsClaimedOnce(t *testing.T) {
	s := newWebhookTestServer(t)
	const idemKey = "idem-retry-claim"
	inner := webhooks.NewMemoryDeliveryStore()
	cfg := webhooks.Config{ID: "wh1", TenantID: "team1", Provider: webhooks.ProviderGitHub}
	meta := webhookEventMeta{ProjectPath: "o/r", SubjectID: "pr:7", Kind: "pull_request"}

	prior := newWebhookDelivery(cfg, meta, webhooks.StatusLaunchError, "hash", "1.2.3.4")
	prior.IdempotencyKey = idemKey
	prior.BotID = "review-pr"
	prior.Attempts = 1
	prior.Error = "boom"
	if err := inner.Insert(context.Background(), prior); err != nil {
		t.Fatalf("seed the failed row: %v", err)
	}
	s.webhookDeliveries = newBarrierDeliveryStore(inner, 2)

	var mu sync.Mutex
	launches := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		launches++
		return "run-launched", nil
	}

	// A super-admin identity: the launch gate is not what this pins.
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u", TeamID: "team1", IsSuperAdmin: true})
	target := forgeLaunchTarget{IdemKey: idemKey, BotID: "review-pr", Vars: map[string]string{}}
	results := make([]webhookLaunchResult, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.launchWebhookTarget(ctx, nil, cfg, meta, target, "hash", "1.2.3.4")
		}(i)
	}
	wg.Wait()

	mu.Lock()
	got := launches
	mu.Unlock()
	if got != 1 {
		t.Fatalf("two redeliveries of one failed event launched %d runs, want 1 — the retry must be claimed, not read-then-written (results: %+v)", got, results)
	}
	// The loser answers as a duplicate, never as a launch: an event that
	// launched nothing here has one run, and the caller must be told which.
	launched, duplicates := 0, 0
	for _, res := range results {
		switch res.Status {
		case webhooks.StatusLaunched:
			launched++
		case webhooks.StatusDuplicate:
			duplicates++
		}
	}
	if launched != 1 || duplicates != 1 {
		t.Fatalf("results = %+v, want exactly one launched and one duplicate", results)
	}
	// The claim counts the attempt exactly once.
	row, err := inner.GetByIdempotencyKey(context.Background(), idemKey)
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 — one retry happened, not two", row.Attempts)
	}
}
