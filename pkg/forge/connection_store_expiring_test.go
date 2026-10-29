package forge

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The #2698 contract, held against both twins: the refresh sweep's scan never
// hands a connection whose credential is already dead (needs_reauth, revoked)
// to the refresher — before the status filter, every such row kept its past
// expiry and was swept into a guaranteed invalid_grant on every tick.
func TestExpiringBefore_SkipsDeadCredentials(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		expiringBeforeContract(t, NewMemoryConnectionStore())
	})
	t.Run("mongo", func(t *testing.T) {
		uri := os.Getenv("ITERION_TEST_MONGO_URI")
		if uri == "" {
			t.Skip("ITERION_TEST_MONGO_URI not set")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatal(err)
		}
		db := client.Database("iterion_conn_expiring_" + uuid.NewString())
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := db.Drop(cleanup); err != nil {
				t.Error(err)
			}
			if err := client.Disconnect(cleanup); err != nil {
				t.Error(err)
			}
		})
		st := NewMongoConnectionStore(db)
		if err := st.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
		expiringBeforeContract(t, st)
	})
}

func expiringBeforeContract(t *testing.T, st ConnectionStore) {
	t.Helper()
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	seed := func(c Connection) {
		t.Helper()
		if err := st.Create(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	seed(Connection{ID: "due-active", TenantID: "t1", Kind: KindOAuthApp, Status: StatusActive, AccessTokenExpiresAt: &past})
	// The ticket's case: a needs_reauth row keeps its past expiry.
	seed(Connection{ID: "due-needs-reauth", TenantID: "t1", Kind: KindOAuthApp, Status: StatusNeedsReauth, AccessTokenExpiresAt: &past})
	seed(Connection{ID: "due-revoked", TenantID: "t1", Kind: KindOAuthApp, Status: StatusRevoked, AccessTokenExpiresAt: &past})
	// Degraded stays listed: the security-read lane still owes it a pass.
	seed(Connection{ID: "due-degraded", TenantID: "t1", Kind: KindGitHubApp, Status: StatusDegraded, AccessTokenExpiresAt: &past})
	// A row from before the status field keeps its historical behaviour.
	seed(Connection{ID: "due-no-status", TenantID: "t1", Kind: KindOAuthApp, AccessTokenExpiresAt: &past})
	seed(Connection{ID: "pat", TenantID: "t1", Kind: KindPAT, Status: StatusRevoked, AccessTokenExpiresAt: &past})
	seed(Connection{ID: "not-due", TenantID: "t1", Kind: KindOAuthApp, Status: StatusActive, AccessTokenExpiresAt: &future})

	due, err := st.ExpiringBefore(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range due {
		got[c.ID] = true
	}
	for _, want := range []string{"due-active", "due-degraded", "due-no-status"} {
		if !got[want] {
			t.Errorf("%s missing from the sweep", want)
		}
	}
	for _, refuse := range []string{"due-needs-reauth", "due-revoked", "pat", "not-due"} {
		if got[refuse] {
			t.Errorf("%s handed to the sweep — a dead credential is a guaranteed invalid_grant per tick", refuse)
		}
	}
}

// The worker reading the filtered scan: a needs_reauth connection is never
// handed to the refresher, and its row is not even touched (the production
// symptom was updated_at bumping on every 10-minute tick).
func TestRefreshWorker_NeverRetriesANeedsReauthConnection(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	conn, _ := seedOAuthConn(t, sealer, connStore, secStore, now.Add(-time.Hour))
	// The refresh that failed flipped it needs_reauth; the past expiry stays.
	conn.Status = StatusNeedsReauth
	conn.StatusReason = "refresh failed: invalid_grant"
	if err := connStore.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	calls := 0
	w := &RefreshWorker{
		Connections: connStore,
		Secrets:     secStore,
		Sealer:      sealer,
		Now:         func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			calls++
			return fakeRefresher{newAccess: "new-access", expiresAt: now.Add(time.Hour)}
		},
	}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 || calls != 0 {
		t.Fatalf("refreshed=%d refresher calls=%d — a needs_reauth connection must not be retried", n, calls)
	}
	got, err := connStore.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusNeedsReauth || !got.UpdatedAt.Equal(conn.UpdatedAt) {
		t.Fatalf("the sweep touched a row it cannot help: status=%q updated_at=%v", got.Status, got.UpdatedAt)
	}
}
