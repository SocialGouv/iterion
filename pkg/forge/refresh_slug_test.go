package forge

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// slugRefresher is a refresher that, like the GitHub App one, also reports
// the App's slug when the connection record lacks it.
type slugRefresher struct {
	fakeRefresher
	slug string
}

func (r slugRefresher) Refresh(ctx context.Context, conn Connection, tok string) (RefreshedToken, error) {
	out, err := r.fakeRefresher.Refresh(ctx, conn, tok)
	if err == nil && conn.AppSlug == "" {
		out.AppSlug = r.slug
	}
	return out, nil
}

// The slug a refresher resolves is persisted onto the connection record —
// the field iterionBotLogins builds the App's "<slug>[bot]" identity from —
// so a connection created before the slug was recorded (or under a platform
// App with no slug configured) stops presenting an inert loop guard after
// its first refresh. The account login the connect flow derives from the
// slug is repaired along with it when it was left as the bare "[bot]".
func TestRefreshWorker_PersistsTheAppSlugTheRefresherResolved(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	connStore := NewMemoryConnectionStore()
	secStore := secrets.NewMemoryGenericSecretStore()
	now := time.Unix(1700000000, 0).UTC()
	c, _ := seedOAuthConn(t, sealer, connStore, secStore, now.Add(2*time.Minute))
	ctx := context.Background()
	c.Kind, c.Provider, c.AccountLogin, c.AppSlug = KindGitHubApp, ProviderGitHub, "[bot]", ""
	if err := connStore.Update(ctx, c); err != nil {
		t.Fatal(err)
	}

	w := &RefreshWorker{
		Connections: connStore, Secrets: secStore, Sealer: sealer,
		Now: func() time.Time { return now },
		RefresherFor: func(Connection) TokenRefresher {
			return slugRefresher{fakeRefresher{newAccess: "new-access", expiresAt: now.Add(time.Hour)}, "iterion-forge-x"}
		},
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce: n=%d err=%v", n, err)
	}
	got, err := connStore.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppSlug != "iterion-forge-x" {
		t.Fatalf("AppSlug = %q, want the slug the refresher resolved persisted on the record", got.AppSlug)
	}
	if got.AccountLogin != "iterion-forge-x[bot]" {
		t.Errorf("AccountLogin = %q, want the bare [bot] repaired from the slug", got.AccountLogin)
	}
	if got.SealedPayload == nil || got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("the token refresh itself must still land: %+v", got)
	}
}
