package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestAppBotLoginForForgeToken pins the github_app committer-attribution seam:
// when a run's resolved forge_token is backed by a github_app connection's
// managed secret, the publisher threads that App's bot login to the runner so
// the runner can seed the App-bot git committer (an installation token can't
// GET /user). A PAT/OAuth token, an unknown secret, or a missing store all
// resolve to "" (the runner then keeps its /user resolution or neutral fallback).
func TestAppBotLoginForForgeToken(t *testing.T) {
	ctx := context.Background()
	const tenant = "team-1"
	store := forge.NewMemoryConnectionStore()
	mustCreate := func(c forge.Connection) {
		t.Helper()
		if err := store.Create(ctx, c); err != nil {
			t.Fatalf("create conn: %v", err)
		}
	}
	mustCreate(forge.Connection{
		ID: "app-conn", TenantID: tenant, Provider: forge.ProviderGitHub,
		Kind: forge.KindGitHubApp, AccountLogin: "iterion-forge-abcd[bot]",
		ManagedSecretID: "sec-app",
	})
	mustCreate(forge.Connection{
		ID: "pat-conn", TenantID: tenant, Provider: forge.ProviderGitHub,
		Kind: forge.KindPAT, AccountLogin: "devthejo", ManagedSecretID: "sec-pat",
	})

	withStore := &Publisher{forgeConns: store}
	noStore := &Publisher{forgeConns: nil}

	res := func(name, secretID string) map[string]secrets.GenericResolution {
		return map[string]secrets.GenericResolution{name: {SecretID: secretID}}
	}

	cases := []struct {
		name     string
		pub      *Publisher
		tenant   string
		resolved map[string]secrets.GenericResolution
		want     string
	}{
		{"app forge_token → bot login", withStore, tenant, res("forge_token", "sec-app"), "iterion-forge-abcd[bot]"},
		{"app github_token → bot login", withStore, tenant, res("github_token", "sec-app"), "iterion-forge-abcd[bot]"},
		{"pat forge_token → empty", withStore, tenant, res("forge_token", "sec-pat"), ""},
		{"unknown secret → empty", withStore, tenant, res("forge_token", "sec-nope"), ""},
		{"no forge secret name → empty", withStore, tenant, res("kubeconfig", "sec-app"), ""},
		{"nil store → empty", noStore, tenant, res("forge_token", "sec-app"), ""},
		{"empty tenant → empty", withStore, "", res("forge_token", "sec-app"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.pub.appBotLoginForForgeToken(ctx, c.tenant, c.resolved); got != c.want {
				t.Errorf("appBotLoginForForgeToken = %q, want %q", got, c.want)
			}
		})
	}
}
