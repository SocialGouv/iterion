package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2687: the webhook, share-link and invitation token mints are team state
// whose token outlives the credential it is minted from and is bound to
// nothing of it — a delivery channel that launches runs, a public read of
// the team's configuration, a membership whoever holds the token joins
// with. Each mint goes through the derivation chokepoint
// (auth.MintTeamToken): the access token of a revoked session mints none
// (401), a personal access token never does (403), and a standing session
// still mints.

// teamTokenMintFixture is the derivation fixture with the webhook and
// config-share stores enabled, so all six mint routes are registered.
func teamTokenMintFixture(t *testing.T) *derivationFixture {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return newDerivationFixtureWith(t, func(c *Config) {
		c.WebhookConfigs = webhooks.NewMemoryConfigStore()
		c.WebhookDeliveries = webhooks.NewMemoryDeliveryStore()
		c.ConfigShares = configshare.NewMemoryStore()
		c.GenericSecrets = secrets.NewMemoryGenericSecretStore()
		c.Sealer = sealer
	})
}

type teamTokenMint struct{ method, path, body string }

// teamTokenMints are the six mint routes, exercised against team ta (the
// fixture account manages it through its org-admin standing in o1). The
// rotates are listed once webhookID and shareID name a row to aim at.
func teamTokenMints(webhookID, shareID string) []teamTokenMint {
	share := `{"bot_id":"b","label":"l","repo_url":"https://github.com/o/r","repo_ref":"main","config_path":"b.json","allowed_paths":["categories.x.feeds"]}`
	invite := `{"email":"invitee@example.com","role":"member"}`
	out := []teamTokenMint{
		{http.MethodPost, "/api/teams/ta/webhooks", `{"name":"hook","wildcard_bots":true}`},
		{http.MethodPost, "/api/teams/ta/config-shares", share},
		{http.MethodPost, "/api/teams/ta/invitations", invite},
		{http.MethodPost, "/api/orgs/o1/invitations", invite},
	}
	if webhookID != "" {
		out = append(out, teamTokenMint{http.MethodPost, "/api/teams/ta/webhooks/" + webhookID + "/rotate", ""})
	}
	if shareID != "" {
		out = append(out, teamTokenMint{http.MethodPost, "/api/teams/ta/config-shares/" + shareID + "/rotate", ""})
	}
	return out
}

// mintedCounts answers how many webhook configs, config shares and
// invitations team ta holds.
func mintedCounts(t *testing.T, f *derivationFixture) (hooks, shares, invitations int) {
	t.Helper()
	ctx := context.Background()
	cfgs, err := f.s.webhookConfigs.ListByTenant(ctx, "ta")
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	rows, err := f.s.configShares.ListByTenant(ctx, "ta")
	if err != nil {
		t.Fatalf("list config shares: %v", err)
	}
	invs, err := f.s.authStore().ListInvitationsByTeam(ctx, "ta")
	if err != nil {
		t.Fatalf("list invitations: %v", err)
	}
	return len(cfgs), len(rows), len(invs)
}

// mintOneWebhookAndShare gives the rotations a row to aim at: one webhook
// and one config share minted by the fixture's live session.
func mintOneWebhookAndShare(t *testing.T, f *derivationFixture) (webhookID, shareID string) {
	t.Helper()
	code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/webhooks", f.user.AccessToken, teamTokenMints("", "")[0].body)
	if code/100 != 2 {
		t.Fatalf("baseline webhook mint: %d %s", code, body)
	}
	var hook struct {
		Config struct {
			ID string `json:"id"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &hook); err != nil || hook.Config.ID == "" {
		t.Fatalf("baseline webhook mint answered %q", body)
	}
	code, _, body = f.call(t, http.MethodPost, "/api/teams/ta/config-shares", f.user.AccessToken, teamTokenMints("", "")[1].body)
	if code/100 != 2 {
		t.Fatalf("baseline share mint: %d %s", code, body)
	}
	var share struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &share); err != nil || share.ID == "" {
		t.Fatalf("baseline share mint answered %q", body)
	}
	return hook.Config.ID, share.ID
}

// TestTeamTokenMints_AStandingSessionStillMints is the control: the gate
// refuses a credential that does not stand, not the mint itself — a live
// session creates and rotates each of the three tokens.
func TestTeamTokenMints_AStandingSessionStillMints(t *testing.T) {
	f := teamTokenMintFixture(t)
	webhookID, shareID := mintOneWebhookAndShare(t, f)
	for _, c := range teamTokenMints(webhookID, shareID)[2:] {
		code, _, body := f.call(t, c.method, c.path, f.user.AccessToken, c.body)
		if code/100 != 2 {
			t.Fatalf("%s %s with a live session: %d %s, want a mint", c.method, c.path, code, body)
		}
	}
}

// TestTeamTokenMints_ARevokedSessionsAccessTokenMintsNone: a sign-out
// everywhere ends the mints with the session, not at the token's own expiry:
// every one of the six answers 401 before its handler runs, and no row
// lands.
func TestTeamTokenMints_ARevokedSessionsAccessTokenMintsNone(t *testing.T) {
	f := teamTokenMintFixture(t)
	webhookID, shareID := mintOneWebhookAndShare(t, f)

	// Sign out everywhere from the other device's session: every session of
	// the account is revoked, the account stays ACTIVE.
	if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
		t.Fatalf("sign out everywhere: %d %s", code, body)
	}
	f.s.authSvc.WaitRevocationPasses()
	hooks, shares, invitations := mintedCounts(t, f)

	for _, c := range teamTokenMints(webhookID, shareID) {
		code, _, body := f.call(t, c.method, c.path, f.user.AccessToken, c.body)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a revoked session's access token: %d %s, want 401", c.method, c.path, code, body)
		}
	}
	if h, s, i := mintedCounts(t, f); h != hooks || s != shares || i != invitations {
		t.Fatalf("a revoked session's mints changed the state: webhooks %d→%d, shares %d→%d, invitations %d→%d",
			hooks, h, shares, s, invitations, i)
	}
}

// TestTeamTokenMints_APersonalAccessTokenMintsNone: a personal access token
// is pinned to its team and its own expiry, so it mints no team token that
// would outlive it — each of the six answers 403 with the chokepoint's own
// wording before its handler runs.
func TestTeamTokenMints_APersonalAccessTokenMintsNone(t *testing.T) {
	f := teamTokenMintFixture(t)
	code, patToken := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated || patToken == "" {
		t.Fatalf("mint the PAT: %d", code)
	}
	hooks, shares, invitations := mintedCounts(t, f)
	for _, c := range teamTokenMints("no-such-webhook", "no-such-share") {
		code, _, body := f.call(t, c.method, c.path, patToken, c.body)
		if code != http.StatusForbidden {
			t.Fatalf("%s %s with a personal access token: %d %s, want 403", c.method, c.path, code, body)
		}
		if want := "cannot mint another credential"; !strings.Contains(body, want) {
			t.Fatalf("%s %s: the 403 does not carry %q: %s", c.method, c.path, want, body)
		}
	}
	if h, s, i := mintedCounts(t, f); h != hooks || s != shares || i != invitations {
		t.Fatalf("a personal access token's mints changed the state: webhooks %d→%d, shares %d→%d, invitations %d→%d",
			hooks, h, shares, s, invitations, i)
	}
}
