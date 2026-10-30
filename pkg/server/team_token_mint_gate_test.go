package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/forge"
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

// forgeMintFixture is the derivation fixture with the forge stack wired, so
// the Integrations routes register and their success path really reaches the
// orchestrator's mint (forge.Orchestrator.Provision → webhooks.MintToken).
func forgeMintFixture(t *testing.T) (*derivationFixture, *mockGitLab, string) {
	t.Helper()
	gl := newMockGitLab()
	glsrv := gl.server()
	t.Cleanup(glsrv.Close)
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	f := newDerivationFixtureWith(t, func(c *Config) {
		c.WebhookConfigs = webhooks.NewMemoryConfigStore()
		c.WebhookDeliveries = webhooks.NewMemoryDeliveryStore()
		c.GenericSecrets = secrets.NewMemoryGenericSecretStore()
		c.ConfigShares = configshare.NewMemoryStore()
		c.Sealer = sealer
		c.ForgeConnections = forge.NewMemoryConnectionStore()
		c.ForgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	})
	if f.s.forgeOrchestrator == nil {
		t.Fatal("no forge orchestrator was built: the Integrations routes are not registered")
	}
	f.s.forgeOrchestrator.Bots = testForgeBotLookup
	f.s.forgeOrchestrator.Invocations = nil
	return f, gl, glsrv.URL
}

// connectForge points team ta at the mock GitLab and answers the connection
// id the enable names.
func connectForge(t *testing.T, f *derivationFixture, bearer, glURL string) string {
	t.Helper()
	body := `{"provider":"gitlab","mode":"pat","forge_base_url":"` + glURL + `","pat":"glpat-token"}`
	code, _, resp := f.call(t, http.MethodPost, "/api/teams/ta/forge/connections", bearer, body)
	if code != http.StatusOK {
		t.Fatalf("connect the forge: %d %s", code, resp)
	}
	var out struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Connection.ID == "" {
		t.Fatalf("connect answered %q", resp)
	}
	return out.Connection.ID
}

// teamHookRows answers team ta's webhook rows, id and token hash: the hash
// is the mint's own fingerprint, so a request that re-provisions WITHOUT
// minting leaves it byte-identical. A count alone cannot tell a reused
// secret from a freshly minted one.
func teamHookRows(t *testing.T, f *derivationFixture) []webhooks.Config {
	t.Helper()
	cfgs, err := f.s.webhookConfigs.ListByTenant(context.Background(), "ta")
	if err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	return cfgs
}

// repoBotsEnableBody is the Integrations enable/reconcile request for a repo.
func repoBotsEnableBody(connID, repo string) string {
	return `{"connection_id":"` + connID + `","repo":"` + repo + `","bot_ids":["review-pr"]}`
}

// onlyHook answers the single webhook row team ta holds, failing when the
// count is not one: every assertion below compares ONE row's token hash.
func onlyHook(t *testing.T, f *derivationFixture) webhooks.Config {
	t.Helper()
	rows := teamHookRows(t, f)
	if len(rows) != 1 {
		t.Fatalf("team ta holds %d webhook rows, want 1", len(rows))
	}
	return rows[0]
}

// patPinnedToTeamTA mints a personal access token pinned to team ta — what
// `iterion remote login --team ta` stores and what every runbook runs from.
// mintPAT pins the caller's ACTIVE team, which in this fixture is the
// account's PERSONAL one: such a token is refused by the team pin itself
// ("not a member") and never reaches a mint, so it cannot witness this gate
// at all.
func patPinnedToTeamTA(t *testing.T, f *derivationFixture) string {
	t.Helper()
	code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, `{"name":"ci","team_id":"ta"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint a PAT pinned to ta: %d %s", code, body)
	}
	var v struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Token == "" {
		t.Fatalf("the mint answered %q", body)
	}
	return v.Token
}

// parkApproval writes a provision-approval record by hand, so the approve
// route can be driven against a record the store really holds — an approve
// of an id nothing holds answers 404 and would prove nothing about the mint.
func parkApproval(t *testing.T, f *derivationFixture, id, connID, repo, integrationID string, baseBots []string) {
	t.Helper()
	if f.s.provisionApprovals == nil {
		f.s.provisionApprovals = forge.NewMemoryProvisionApprovalStore()
	}
	rec := forge.ProvisionApproval{
		ID: id, OrgID: "o1", TenantID: "ta", ConnectionID: connID,
		RepoFullName: repo, BotIDs: []string{"review-pr"},
	}
	if integrationID != "" {
		rec.IntegrationID, rec.Replace, rec.BaseBotIDs = integrationID, true, baseBots
	}
	if err := f.s.provisionApprovals.Create(context.Background(), rec); err != nil {
		t.Fatalf("park the approval record: %v", err)
	}
}

// TestTeamTokenMints_TheRepoBotProvisioningIsGatedWhereItMints: the repo-bot
// provisioning mints the repo's webhook token through the orchestrator
// (forge.Orchestrator.Provision → reserveHookIdentity → webhooks.MintToken),
// and the derivation chokepoint is asked THERE — at the reservation, inside
// the provisioning lease — rather than at the route's door.
//
// So the verdict follows the request's own state, not its path: the FIRST
// enable of a repo reserves a hook identity and answers the refusals a
// team-token mint owes (401 for a revoked session's access token, 403 for a
// personal access token), while the gestures that reuse the integration's
// sealed secret — the reconcile docs/webhooks.md prescribes, a settings-only
// PATCH, the approval of an UPDATE record — mint nothing and stay open to a
// stored token. Each of those is checked on the token HASH, which a mint
// would change.
func TestTeamTokenMints_TheRepoBotProvisioningIsGatedWhereItMints(t *testing.T) {
	t.Run("the first enable of a repo: a revoked session's access token mints none", func(t *testing.T) {
		f, gl, glURL := forgeMintFixture(t)
		connID := connectForge(t, f, f.user.AccessToken, glURL)
		if code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.other.AccessToken, ""); code != http.StatusNoContent {
			t.Fatalf("sign out everywhere: %d %s", code, body)
		}
		f.s.authSvc.WaitRevocationPasses()
		code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/forge/repo-bots", f.user.AccessToken, repoBotsEnableBody(connID, "group/api"))
		if code != http.StatusUnauthorized {
			t.Errorf("the first enable with a revoked session's access token: %d %s, want 401", code, body)
		}
		if rows := teamHookRows(t, f); len(rows) != 0 {
			t.Errorf("a revoked session minted %d webhook row(s) through the provisioning", len(rows))
		}
		gl.mu.Lock()
		created := gl.createBody
		gl.mu.Unlock()
		if created != nil {
			t.Errorf("a revoked session had a hook pushed to the forge it named: %v", created)
		}
	})

	t.Run("the first enable of a repo: a personal access token mints none", func(t *testing.T) {
		f, gl, glURL := forgeMintFixture(t)
		connID := connectForge(t, f, f.user.AccessToken, glURL)
		patToken := patPinnedToTeamTA(t, f)
		code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/forge/repo-bots", patToken, repoBotsEnableBody(connID, "group/api"))
		if code != http.StatusForbidden {
			t.Errorf("the first enable with a personal access token: %d %s, want 403", code, body)
		}
		if want := "cannot mint another credential"; !strings.Contains(body, want) {
			t.Errorf("the refusal does not carry %q: %s", want, body)
		}
		if rows := teamHookRows(t, f); len(rows) != 0 {
			t.Errorf("a personal access token minted %d webhook row(s) through the provisioning", len(rows))
		}
		gl.mu.Lock()
		created := gl.createBody
		gl.mu.Unlock()
		if created != nil {
			t.Errorf("a personal access token had a hook pushed to the forge: %v", created)
		}
	})

	t.Run("a standing session still provisions", func(t *testing.T) {
		f, gl, glURL := forgeMintFixture(t)
		connID := connectForge(t, f, f.user.AccessToken, glURL)
		code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/forge/repo-bots", f.user.AccessToken, repoBotsEnableBody(connID, "group/api"))
		if code != http.StatusOK {
			t.Fatalf("enable with a live session: %d %s, want the provisioning to go through", code, body)
		}
		if rows := teamHookRows(t, f); len(rows) != 1 {
			t.Fatalf("a live session's enable wrote %d webhook rows, want 1", len(rows))
		}
		gl.mu.Lock()
		created := gl.createBody
		gl.mu.Unlock()
		if created == nil {
			t.Fatal("a live session's enable pushed no hook to the forge")
		}
	})

	// The other side of the SAME branch, on the same routes and the same
	// credential: what mints nothing is not refused. A gate at the route's
	// door refuses all three of these, which is what left eight runbooks
	// prescribing a command the instance answered 403 to.
	t.Run("what re-provisions without minting stays open to a personal access token", func(t *testing.T) {
		f, _, glURL := forgeMintFixture(t)
		connID := connectForge(t, f, f.user.AccessToken, glURL)
		code, _, body := f.call(t, http.MethodPost, "/api/teams/ta/forge/repo-bots", f.user.AccessToken, repoBotsEnableBody(connID, "group/api"))
		if code != http.StatusOK {
			t.Fatalf("enable with a live session: %d %s", code, body)
		}
		var enabled struct {
			IntegrationID string `json:"integration_id"`
		}
		if err := json.Unmarshal([]byte(body), &enabled); err != nil || enabled.IntegrationID == "" {
			t.Fatalf("the enable answered %q", body)
		}
		minted := onlyHook(t, f)
		patToken := patPinnedToTeamTA(t, f)
		parkApproval(t, f, "00000000-0000-4000-8000-0000000000bb", connID, "group/api", enabled.IntegrationID, []string{"review-pr"})

		for _, c := range []teamTokenMint{
			// The reconcile docs/webhooks.md prescribes: the same POST,
			// same hook id, same durable `iwh_` secret.
			{http.MethodPost, "/api/teams/ta/forge/repo-bots", repoBotsEnableBody(connID, "group/api")},
			// A settings-only patch: the gate switch of docs/merge-gate.md.
			{http.MethodPatch, "/api/teams/ta/forge/repo-bots/" + enabled.IntegrationID, `{"launch_vars":{"gate_context":"iterion/revi"}}`},
			// An org approval standing for an UPDATE of that integration.
			{http.MethodPost, "/api/orgs/o1/provision-approvals/00000000-0000-4000-8000-0000000000bb/approve", ""},
		} {
			code, _, body := f.call(t, c.method, c.path, patToken, c.body)
			if code != http.StatusOK {
				t.Errorf("%s %s with a personal access token: %d %s, want 200 — it mints nothing", c.method, c.path, code, body)
				continue
			}
			row := onlyHook(t, f)
			if row.ID != minted.ID || row.TokenHash != minted.TokenHash {
				t.Errorf("%s %s DID mint: webhook %s/%s became %s/%s", c.method, c.path,
					minted.ID, minted.TokenHash[:12], row.ID, row.TokenHash[:12])
			}
		}
	})

	t.Run("an approval standing for a repo's first enable still answers the chokepoint", func(t *testing.T) {
		f, _, glURL := forgeMintFixture(t)
		connID := connectForge(t, f, f.user.AccessToken, glURL)
		patToken := patPinnedToTeamTA(t, f)
		parkApproval(t, f, "00000000-0000-4000-8000-0000000000aa", connID, "group/api", "", nil)
		code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/provision-approvals/00000000-0000-4000-8000-0000000000aa/approve", patToken, "")
		if code != http.StatusForbidden {
			t.Errorf("approving a NEW-repo record with a personal access token: %d %s, want 403", code, body)
		}
		if want := "cannot mint another credential"; !strings.Contains(body, want) {
			t.Errorf("the refusal does not carry %q: %s", want, body)
		}
		if rows := teamHookRows(t, f); len(rows) != 0 {
			t.Errorf("the refused approval minted %d webhook row(s)", len(rows))
		}
		// The control, same record: a live session approves it.
		code, _, body = f.call(t, http.MethodPost, "/api/orgs/o1/provision-approvals/00000000-0000-4000-8000-0000000000aa/approve", f.user.AccessToken, "")
		if code != http.StatusOK {
			t.Fatalf("approving the same record with a live session: %d %s, want 200", code, body)
		}
		if rows := teamHookRows(t, f); len(rows) != 1 {
			t.Fatalf("the approved provisioning wrote %d webhook rows, want 1", len(rows))
		}
	})
}
