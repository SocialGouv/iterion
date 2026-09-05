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
