package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The unattended gate lanes read their launch-failure budget off the delivery
// row under the claim key — Attempts and FailedAt — so both store twins must
// carry the two fields through the Update the launch tail performs on a
// retried launch_error row, and give them back on the key lookup.
func TestDeliveryStore_AttemptsAndFailedAtRoundTrip(t *testing.T) {
	run := func(t *testing.T, st DeliveryStore) {
		t.Helper()
		ctx := context.Background()
		failedAt := time.Date(2026, 8, 26, 16, 9, 0, 0, time.UTC)
		d := Delivery{
			ID: "d1", TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
			IdempotencyKey: "claim-key", Status: StatusAccepted, Attempts: 1, ReceivedAt: failedAt.Add(-time.Minute),
		}
		if err := st.Insert(ctx, d); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		d.Status, d.Error, d.FailedAt = StatusLaunchError, "cloudpublisher: publish: nats: no responders", &failedAt
		if err := st.Update(ctx, d); err != nil {
			t.Fatalf("Update (first failure): %v", err)
		}
		got, err := st.GetByIdempotencyKey(ctx, "claim-key")
		if err != nil {
			t.Fatalf("GetByIdempotencyKey: %v", err)
		}
		if got.Attempts != 1 || got.FailedAt == nil || !got.FailedAt.Equal(failedAt) {
			t.Fatalf("after the first failure: attempts=%d failed_at=%v, want 1 / %v", got.Attempts, got.FailedAt, failedAt)
		}
		// The retry the tail performs: same row, one more attempt, a later
		// failure stamp.
		later := failedAt.Add(5 * time.Minute)
		d.Attempts, d.FailedAt = 2, &later
		if err := st.Update(ctx, d); err != nil {
			t.Fatalf("Update (second failure): %v", err)
		}
		got, err = st.GetByIdempotencyKey(ctx, "claim-key")
		if err != nil || got.Attempts != 2 || got.FailedAt == nil || !got.FailedAt.Equal(later) {
			t.Fatalf("after the second failure: %+v (%v), want attempts 2 / failed_at %v", got, err, later)
		}
	}

	onDeliveryStoreTwins(t, run)
}

// onDeliveryStoreTwins runs one delivery-store contract against both twins.
// The Mongo half skips without ITERION_TEST_MONGO_URI (the CI
// mongo-conformance job supplies it).
func onDeliveryStoreTwins(t *testing.T, run func(*testing.T, DeliveryStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryDeliveryStore()) })

	t.Run("mongo", func(t *testing.T) {
		uri := os.Getenv("ITERION_TEST_MONGO_URI")
		if uri == "" {
			t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo delivery suite")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatalf("mongo connect: %v", err)
		}
		nonce := make([]byte, 4)
		_, _ = rand.Read(nonce)
		db := client.Database("iterion_webhooks_" + hex.EncodeToString(nonce))
		t.Cleanup(func() {
			drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dropCancel()
			_ = db.Drop(drop)
			_ = client.Disconnect(drop)
		})
		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		run(t, NewMongoStores(db).Deliveries)
	})
}

// A prior launch_error row is retryable, so taking it over is a CLAIM: both
// twins must let exactly ONE caller past, or two redeliveries of the same
// event each launch a run for it.
func TestDeliveryStore_ClaimFailedRetry(t *testing.T) {
	onDeliveryStoreTwins(t, func(t *testing.T, st DeliveryStore) {
		ctx := context.Background()
		seed := func(t *testing.T, id string, status string, attempts int) Delivery {
			t.Helper()
			d := Delivery{
				ID: id, TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
				IdempotencyKey: "key-" + id, Status: status, Attempts: attempts,
				ReceivedAt: time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC),
			}
			if err := st.Insert(ctx, d); err != nil {
				t.Fatalf("Insert %s: %v", id, err)
			}
			return d
		}

		// Two callers read the same failed row; only the first claim lands.
		d := seed(t, "d-race", StatusLaunchError, 1)
		first, second := d, d
		first.Status, first.Attempts = StatusAccepted, 2
		second.Status, second.Attempts = StatusAccepted, 2
		claimed, err := st.ClaimFailedRetry(ctx, first, 1)
		if err != nil || !claimed {
			t.Fatalf("first claim = (%v, %v), want (true, nil)", claimed, err)
		}
		claimed, err = st.ClaimFailedRetry(ctx, second, 1)
		if err != nil {
			t.Fatalf("second claim errored: %v", err)
		}
		if claimed {
			t.Fatal("both callers claimed the same failed row — two runs would launch for one event")
		}
		got, err := st.GetByIdempotencyKey(ctx, "key-d-race")
		if err != nil || got.Attempts != 2 || got.Status != StatusAccepted {
			t.Fatalf("row after the claim = %+v (%v), want status accepted / attempts 2", got, err)
		}

		// A row that is not a launch failure is never claimable.
		live := seed(t, "d-live", StatusLaunched, 1)
		live.Attempts = 2
		if claimed, err := st.ClaimFailedRetry(ctx, live, 1); err != nil || claimed {
			t.Fatalf("claim of a launched row = (%v, %v), want (false, nil)", claimed, err)
		}

		// A failed row that never counted an attempt carries no attempts
		// field at all on Mongo (omitempty): claiming it at 0 must still work,
		// or a first retry could never be taken over.
		zero := seed(t, "d-zero", StatusLaunchError, 0)
		zero.Status, zero.Attempts = StatusAccepted, 1
		if claimed, err := st.ClaimFailedRetry(ctx, zero, 0); err != nil || !claimed {
			t.Fatalf("claim of an attempt-less failed row = (%v, %v), want (true, nil)", claimed, err)
		}
	})
}
