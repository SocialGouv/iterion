package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forgeAdminFor is called once per lane per delivery, and a GitHub-App
// client keeps its minted tokens on the instance — so a fresh client per call
// meant a fresh mint per lane. One client per connection, validated against
// the connection state it was built from, is what makes the second call free
// and what lets a denial noted by one lane (PreflightFor's denied set) reach
// the next.
func TestForgeAdminForReusesOneAppClientPerConnection(t *testing.T) {
	s, conn := appConnectionFixture(t)
	ctx := context.Background()
	build := func(c forge.Connection) forge.Admin {
		t.Helper()
		admin, err := s.forgeAdminFor(ctx, c)
		if err != nil {
			t.Fatalf("forgeAdminFor: %v", err)
		}
		return admin
	}
	first := build(conn)
	if build(conn) != first {
		t.Fatal("two calls on the same connection built two clients: every lane mints its own tokens")
	}
	other := conn
	other.ID = "c-other"
	if build(other) == first {
		t.Error("two connections must never share a client")
	}
	if build(conn) != first {
		t.Error("another connection's build must not evict this connection's entry")
	}

	// The invalidation rule: ONE entry per connection, held only while the
	// state the client is built from is unchanged — a moved grant (the
	// health probe synced it) or a status change builds a fresh client, which
	// is then the one served.
	synced := conn
	synced.GrantedPermissions = map[string]string{"contents": "write", "metadata": "read"}
	second := build(synced)
	if second == first {
		t.Error("a connection whose granted permissions moved must get a fresh client")
	}
	if build(synced) != second {
		t.Error("the fresh client must be the one served from then on")
	}
	revoked := synced
	revoked.Status = forge.StatusNeedsReauth
	third := build(revoked)
	if third == second {
		t.Error("a connection whose status changed must get a fresh client")
	}

	// Explicit eviction — the delete route and the refresh route (which
	// re-mints on purpose) use it.
	s.forgetForgeAppClient(conn.ID)
	if build(revoked) == third {
		t.Error("forgetForgeAppClient must drop the entry so the next call builds a fresh client")
	}
}

// The cache is for App clients only: a bearer-token client holds no state
// worth keeping, and caching it would pin a token the connection may rotate.
func TestForgeAdminForBuildsBearerClientsPerCall(t *testing.T) {
	s := newWebhookTestServer(t)
	sealed, err := forge.SealPAT(s.sealer, "c-pat", "ghp_fixture")
	if err != nil {
		t.Fatal(err)
	}
	conn := forge.Connection{ID: "c-pat", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT, Status: forge.StatusActive, SealedPayload: sealed}
	a, err := s.forgeAdminFor(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.forgeAdminFor(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("a PAT connection's client must be built per call")
	}
}
