package forge

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The worker reading the filtered scan: a needs_reauth connection is never
// handed to the refresher, and its row is not even touched (the production
// symptom was updated_at bumping on every 10-minute tick). The scan's half
// of this contract is pinned against both store twins in pkg/connection
// (TestForgeExpiringBefore_SkipsDeadCredentials); this half is the worker's.
func TestRefreshWorker_NeverRetriesANeedsReauthConnection(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := connection.NewMemoryForgeStore()
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
	got, err := connStore.Get(context.Background(), conn.TenantID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusNeedsReauth || !got.UpdatedAt.Equal(conn.UpdatedAt) {
		t.Fatalf("the sweep touched a row it cannot help: status=%q updated_at=%v", got.Status, got.UpdatedAt)
	}
}
