package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// storeApp seals a private key into a ForgeOAuthApp row and persists it.
func storeApp(t *testing.T, s *Server, id, tenant, owner, providerAppID, pem string, created time.Time) forge.ForgeOAuthApp {
	t.Helper()
	sealed, err := forge.SealForgeAppPrivateKey(s.sealer, id, pem)
	if err != nil {
		t.Fatalf("seal app key: %v", err)
	}
	app := forge.ForgeOAuthApp{
		ID: id, TenantID: tenant, Provider: forge.ProviderGitHub,
		ForgeBaseURL: "https://github.com", OwnerLogin: owner,
		ProviderAppID: providerAppID, AppSlug: "app-" + owner,
		SealedPrivateKey: sealed, CreatedAt: created,
	}
	if err := s.forgeOAuthApps.Create(context.Background(), app); err != nil {
		t.Fatalf("create app %s: %v", id, err)
	}
	return app
}

// A connection must resolve the App that owns ITS installation. Signing an
// installation token with another App's key does not degrade gracefully — the
// mint fails, or addresses a different installation entirely — and the symptom
// would surface in the background refresh worker, far from the cause.
func TestGitHubAppConfigForConnection(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	t0 := time.Unix(1700000000, 0).UTC()

	prod := storeApp(t, s, "app-prod", "t1", "SocialGouv", "111", "PEM-PROD", t0)
	sandbox := storeApp(t, s, "app-sandbox", "t1", "iterion-sandbox", "222", "PEM-SANDBOX", t0.Add(time.Hour))
	other := storeApp(t, s, "app-other", "t2", "SomeoneElse", "333", "PEM-OTHER", t0)

	ctx := context.Background()

	t.Run("resolves the app the connection names", func(t *testing.T) {
		conn := forge.Connection{ID: "c1", TenantID: "t1", Kind: forge.KindGitHubApp, OAuthAppID: sandbox.ID}
		cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
		if !ok {
			t.Fatal("want the sandbox app to resolve")
		}
		if cfg.AppID != 222 || cfg.PrivateKeyPEM != "PEM-SANDBOX" {
			t.Fatalf("resolved the wrong app: id=%d key=%q", cfg.AppID, cfg.PrivateKeyPEM)
		}
	})

	// The whole point: a second connection on the same host resolves a
	// DIFFERENT app. Under the old (tenant, provider, host) key both would have
	// collapsed onto one.
	t.Run("a sibling connection on the same host resolves its own app", func(t *testing.T) {
		conn := forge.Connection{ID: "c2", TenantID: "t1", Kind: forge.KindGitHubApp, OAuthAppID: prod.ID}
		cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
		if !ok {
			t.Fatal("want the prod app to resolve")
		}
		if cfg.AppID != 111 {
			t.Fatalf("want app 111, got %d", cfg.AppID)
		}
	})

	// Connections created before the link existed carry no app id. They are
	// unambiguous precisely because only one app per host could exist then, so
	// they keep resolving to the oldest.
	t.Run("legacy connection falls back to the oldest app on the host", func(t *testing.T) {
		conn := forge.Connection{ID: "c3", TenantID: "t1", Kind: forge.KindGitHubApp}
		cfg, _, ok := s.githubAppConfigForConnection(ctx, conn)
		if !ok {
			t.Fatal("want the legacy fallback to resolve")
		}
		if cfg.AppID != 111 {
			t.Fatalf("legacy fallback must pick the oldest app (111), got %d", cfg.AppID)
		}
	})

	// Fail CLOSED. Falling back to a tenant-level app here would sign with an
	// identity the operator did not choose.
	t.Run("cross-tenant app id resolves to nothing", func(t *testing.T) {
		conn := forge.Connection{ID: "c4", TenantID: "t1", Kind: forge.KindGitHubApp, OAuthAppID: other.ID}
		if _, _, ok := s.githubAppConfigForConnection(ctx, conn); ok {
			t.Fatal("a connection must never resolve another tenant's app")
		}
	})

	t.Run("dangling app id resolves to nothing", func(t *testing.T) {
		conn := forge.Connection{ID: "c5", TenantID: "t1", Kind: forge.KindGitHubApp, OAuthAppID: "deleted-app"}
		if _, _, ok := s.githubAppConfigForConnection(ctx, conn); ok {
			t.Fatal("a dangling app reference must not silently fall back")
		}
	})
}

// githubAppForInstall pins WHICH app an install flow is for, so the callback
// stamps the right one instead of re-deriving it.
func TestGitHubAppForInstall(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	t0 := time.Unix(1700000000, 0).UTC()
	prod := storeApp(t, s, "app-prod", "t1", "SocialGouv", "111", "PEM-PROD", t0)
	sandbox := storeApp(t, s, "app-sandbox", "t1", "iterion-sandbox", "222", "PEM-SANDBOX", t0.Add(time.Hour))
	ctx := context.Background()

	cfg, id, shared, ok := s.githubAppForInstall(ctx, "t1", sandbox.ID)
	if !ok || id != sandbox.ID || cfg.AppID != 222 || shared {
		t.Fatalf("explicit selection failed: ok=%v id=%q appID=%d shared=%v", ok, id, cfg.AppID, shared)
	}

	// No explicit choice keeps the legacy answer AND pins its record, so the
	// resulting connection stops depending on the host lookup from then on.
	cfg, id, _, ok = s.githubAppForInstall(ctx, "t1", "")
	if !ok || id != prod.ID || cfg.AppID != 111 {
		t.Fatalf("default selection failed: ok=%v id=%q appID=%d", ok, id, cfg.AppID)
	}

	if _, _, _, ok := s.githubAppForInstall(ctx, "t2", sandbox.ID); ok {
		t.Fatal("a team must not be able to install another team's app")
	}
}
