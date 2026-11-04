package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usernotify/webpush"
)

// eraseFixture is the derivation fixture with every store the erasure
// handler acts on beside the identity, session and personal-access-token
// ones: the audit log, push subscriptions, subscription credentials and
// pool pledges, personal keys and secrets, config-share deliveries and a
// run store holding notes.
type eraseFixture struct {
	*derivationFixture
	audit   *audit.MemoryStore
	push    *webpush.MemSubscriptionStore
	oauth   *secrets.MemoryOAuthStore
	pledges *credpool.MemoryPledgeStore
	keys    *secrets.MemoryApiKeyStore
	gens    *secrets.MemoryGenericSecretStore
	shares  configshare.Store
	runs    *store.FilesystemRunStore
}

// The run store is the server's local one (Config.StoreDir): the notes route
// and the erasure both reach it through the run service.

func newEraseFixture(t *testing.T) *eraseFixture {
	t.Helper()
	return newEraseFixtureWithShares(t, configshare.NewMemoryStore())
}

func newEraseFixtureWithShares(t *testing.T, shares configshare.Store) *eraseFixture {
	t.Helper()
	return newEraseFixtureWithConfig(t, shares, nil)
}

// newEraseFixtureWithConfig is newEraseFixtureWithShares with a last say on
// the server's Config (extra may be nil).
func newEraseFixtureWithConfig(t *testing.T, shares configshare.Store, extra func(*Config)) *eraseFixture {
	t.Helper()
	dir := t.TempDir()
	runs, err := store.New(dir)
	if err != nil {
		t.Fatalf("run store: %v", err)
	}
	f := &eraseFixture{
		audit: audit.NewMemoryStore(), push: webpush.NewMemSubscriptionStore(),
		oauth: secrets.NewMemoryOAuthStore(), pledges: credpool.NewMemoryPledgeStore(),
		keys: secrets.NewMemoryApiKeyStore(), gens: secrets.NewMemoryGenericSecretStore(),
		shares: shares, runs: runs,
	}
	f.derivationFixture = newDerivationFixtureWith(t, func(c *Config) {
		c.Audit = f.audit
		c.PushSubscriptions = f.push
		c.OAuthForfait = f.oauth
		c.CredPoolPledges = f.pledges
		c.ApiKeys = f.keys
		c.GenericSecrets = f.gens
		c.ConfigShares = f.shares
		c.StoreDir = dir
		if extra != nil {
			extra(c)
		}
	})
	// The erasure's second audit scrub waits out the insert bound: short
	// here, where every insert is in memory.
	f.s.auditInsertTimeout = 50 * time.Millisecond
	return f
}

// erase posts the erasure of userID with the bearer and decodes the answer.
func (f *eraseFixture) erase(t *testing.T, bearer, userID string) (int, adminEraseUserResponse, string) {
	t.Helper()
	code, _, body := f.call(t, http.MethodPost, "/api/admin/users/"+userID+"/erase", bearer, "")
	var out adminEraseUserResponse
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode the erasure's answer %s: %v", body, err)
		}
	}
	return code, out, body
}

// platformEvents lists the platform audit rows of action, once the detached
// insert of the one the test waits for has landed.
func (f *eraseFixture) platformEvents(t *testing.T, action string, want int) []audit.Event {
	t.Helper()
	return awaitEvents(t, want, func() ([]audit.Event, error) {
		return f.audit.ListPlatform(context.Background(), audit.Page{Action: action})
	})
}

// tenantEvents is platformEvents for the tenant-scoped rows of one tenant.
func (f *eraseFixture) tenantEvents(t *testing.T, tenantID, action string, want int) []audit.Event {
	t.Helper()
	return awaitEvents(t, want, func() ([]audit.Event, error) {
		return f.audit.ListByTenant(context.Background(), tenantID, audit.Page{Action: action})
	})
}

// awaitEvents polls list until it answers want rows or five seconds pass:
// the handlers' audit inserts are detached from the request they serve.
func awaitEvents(t *testing.T, want int, list func() ([]audit.Event, error)) []audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := list()
		if err != nil {
			t.Fatal(err)
		}
		if len(events) >= want || time.Now().After(deadline) {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAdminEraseUser walks the route end to end: the refusals (the caller's
// own account, the last owner of a shared org), then an erasure checked
// store by store — row, sessions, personal access tokens, push
// subscriptions, audit rows scrubbed, the audit line of the act — and that
// a second erasure answers 200 with already_erased.
func TestAdminEraseUser(t *testing.T) {
	f := newEraseFixture(t)
	ctx := context.Background()
	admin := f.superAdmin(t)
	userID := f.user.User.ID

	// What the account holds: a personal access token, a push subscription,
	// two audit rows naming it, a shared org it owns alone.
	if code, _ := f.mintPAT(t, f.user.AccessToken, "ci"); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("mint a personal access token: %d", code)
	}
	// The mint's own audit line, carrying the user's IP and agent: landed
	// before the erasure, so the scrub's count is exact below.
	if lines := f.tenantEvents(t, f.user.ActiveTeamID, "pat.created", 1); len(lines) != 1 {
		t.Fatalf("the mint wrote %d audit line(s), want 1", len(lines))
	}
	if err := f.push.Upsert(ctx, &webpush.Subscription{TenantID: "ta", UserID: userID, Endpoint: "https://push.example/one", P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	digest, ok := f.svc.SubjectDigest(fixtureEmail)
	if !ok {
		t.Fatal("SubjectDigest: the active key is not in the key set")
	}
	for _, e := range []audit.Event{
		{ID: "a1", Scope: audit.ScopeTenant, TenantID: "ta", ActorID: userID, Action: "pat.created", IP: "10.0.0.1", UserAgent: "ua", Meta: map[string]any{"name": "ci"}, CreatedAt: time.Now().UTC()},
		{ID: "a2", Scope: audit.ScopeTenant, TenantID: "o1", ActorID: admin.User.ID, Action: "org_member.added", Target: "member", TargetID: userID, IP: "10.0.0.2", UserAgent: "ua", Meta: map[string]any{"role": "admin", "email": fixtureEmail}, CreatedAt: time.Now().UTC()},
		// An invitation row names the person by address only.
		{ID: "a3", Scope: audit.ScopeTenant, TenantID: "o1", ActorID: admin.User.ID, Action: "org_invitation.created", Target: "invitation", TargetID: "inv-x", IP: "10.0.0.3", UserAgent: "ua-admin", Meta: map[string]any{"role": "member", "email": strings.ToUpper(fixtureEmail)}, CreatedAt: time.Now().UTC()},
		// Refused sign-ins (ADR-157): an anonymous attempt on the account
		// under a digest of a retired key (found by its target alone), and
		// one by address naming no account (found by the digest alone).
		{ID: "a4", Scope: audit.ScopePlatform, Action: "auth.login.failed", Target: "user", TargetID: userID, IP: "10.0.0.4", UserAgent: "ua-attempt", Meta: map[string]any{"reason": "bad_password", "subject_hash": "00000000retired0"}, CreatedAt: time.Now().UTC()},
		{ID: "a5", Scope: audit.ScopePlatform, Action: "auth.login.failed", IP: "10.0.0.5", UserAgent: "ua-attempt", Meta: map[string]any{"reason": "unknown_account", "subject_hash": digest}, CreatedAt: time.Now().UTC()},
	} {
		if err := f.audit.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// What the account holds beside its identity: its own subscription
	// credential and a pool pledge of it, a personal API key and secret, a
	// config-editor edit, a run note it wrote — and, beside them, what is
	// not the person's and stays: an org's credential, a team-wide key.
	teamCtx := store.WithTenant(ctx, "ta")
	for _, rec := range []secrets.OAuthRecord{
		{UserID: userID, Kind: secrets.OAuthKindClaudeCode, AccountEmail: fixtureEmail, AccountLabel: fixtureEmail},
		{UserID: secrets.OrgOwnerKey("o1"), Kind: secrets.OAuthKindClaudeCode, AccountEmail: "org@example.org"},
	} {
		if err := f.oauth.Upsert(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.pledges.Upsert(ctx, credpool.Pledge{ID: credpool.PledgeID(userID, credpool.SourceOAuth, "claude_code"), PoolID: "pool-1", UserID: userID, Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []secrets.ApiKey{
		{ID: "key-own", ScopeTeamID: "ta", ScopeUserID: userID, Provider: secrets.ProviderAnthropic, Name: "mine"},
		{ID: "key-team", ScopeTeamID: "ta", Provider: secrets.ProviderAnthropic, Name: "team"},
	} {
		if err := f.keys.Create(teamCtx, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.gens.Create(teamCtx, secrets.GenericSecret{ID: "sec-own", ScopeTeamID: "ta", ScopeUserID: userID, Name: "MINE"}); err != nil {
		t.Fatal(err)
	}
	if err := f.shares.RecordDelivery(ctx, &configshare.Delivery{ID: "d1", ShareID: "sh1", TenantID: "ta", At: time.Now().UTC(), SourceIP: "10.0.0.9", UserAgent: "ua-editor", Method: "PATCH", Actor: configshare.ActorUser(userID), Status: 200}); err != nil {
		t.Fatal(err)
	}
	const runID = "erase-run"
	run, err := f.runs.CreateRun(ctx, runID, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.TenantID = f.user.ActiveTeamID
	if err := f.runs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/runs/"+runID+"/notes", f.user.AccessToken, `{"body":"flaky, re-ran"}`); code != http.StatusCreated || !strings.Contains(body, fixtureEmail) {
		t.Fatalf("a note posted by the account: %d %s, want 201 authored by its address", code, body)
	}
	seedOrg(t, f.s, "own", "own")
	if err := f.s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: userID, OrgID: "own", Role: identity.OrgRoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	t.Run("the caller's own account is refused", func(t *testing.T) {
		code, _, body := f.erase(t, admin.AccessToken, admin.User.ID)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "self_erasure") {
			t.Fatalf("erasing oneself: %d %s, want 422 self_erasure", code, body)
		}
		if u, err := f.s.authStore().GetUser(ctx, admin.User.ID); err != nil || u.Status != identity.UserStatusActive {
			t.Fatalf("the refusal touched the admin's row: %+v (%v)", u, err)
		}
	})

	t.Run("the last owner of a shared org is refused, naming it", func(t *testing.T) {
		code, _, body := f.erase(t, admin.AccessToken, userID)
		if code != http.StatusConflict || !strings.Contains(body, "last_org_owner") || !strings.Contains(body, "own") {
			t.Fatalf("erasing the last owner: %d %s, want 409 last_org_owner naming own", code, body)
		}
		if u, err := f.s.authStore().GetUser(ctx, userID); err != nil || u.Status != identity.UserStatusActive {
			t.Fatalf("the refusal touched the row: %+v (%v)", u, err)
		}
		if _, usable := f.tokensOf(t); usable != 1 {
			t.Fatalf("the refusal revoked the personal access token (%d usable)", usable)
		}
	})

	if err := f.s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: admin.User.ID, OrgID: "own", Role: identity.OrgRoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	t.Run("an unknown account is 404", func(t *testing.T) {
		if code, _, body := f.erase(t, admin.AccessToken, "nobody"); code != http.StatusNotFound {
			t.Fatalf("erasing nobody: %d %s", code, body)
		}
	})

	var first adminEraseUserResponse
	t.Run("the erasure, store by store", func(t *testing.T) {
		code, out, body := f.erase(t, admin.AccessToken, userID)
		if code != http.StatusOK {
			t.Fatalf("erase: %d %s", code, body)
		}
		first = out
		f.svc.WaitRevocationPasses()
		if out.AlreadyErased {
			t.Fatal("the first erasure reports the account was erased already")
		}
		if out.User.Status != string(identity.UserStatusErased) || out.User.Email != identity.ErasedEmail(userID) || out.User.Name != "" {
			t.Fatalf("the answered row: %+v", out.User)
		}
		u, err := f.s.authStore().GetUser(ctx, userID)
		if err != nil || u.Status != identity.UserStatusErased || u.PasswordHash != "" || u.Name != "" {
			t.Fatalf("the stored row: %+v (%v)", u, err)
		}
		// Sessions: deleted, not marked — the row carries the sign-in's IP.
		if out.SessionsDeleted != 2 {
			t.Fatalf("sessions_deleted = %d, want 2 (the registration's and the other device's)", out.SessionsDeleted)
		}
		for _, raw := range []string{f.user.RefreshToken, f.other.RefreshToken} {
			if _, err := f.sessions.MemorySessionStore.GetSessionByTokenHash(ctx, auth.HashRefreshToken(raw)); !errors.Is(err, auth.ErrSessionNotFound) {
				t.Fatalf("a session row of the erased account is still stored (%v)", err)
			}
		}
		if out.PersonalAccessTokensRevoked != 1 {
			t.Fatalf("personal_access_tokens_revoked = %d, want 1", out.PersonalAccessTokensRevoked)
		}
		if _, usable := f.tokensOf(t); usable != 0 {
			t.Fatalf("%d personal access token(s) still usable", usable)
		}
		if out.PushSubscriptionsRemoved != 1 {
			t.Fatalf("push_subscriptions_removed = %d, want 1", out.PushSubscriptionsRemoved)
		}
		if subs, err := f.push.ListForUsers(ctx, []string{userID}); err != nil || len(subs) != 0 {
			t.Fatalf("push subscriptions left: %d (%v)", len(subs), err)
		}
		if out.MembershipsRemoved != 2 || out.OrgMembershipsRemoved != 3 {
			t.Fatalf("memberships_removed = %d (want 2: personal team, ta), org_memberships_removed = %d (want 3: personal, o1, own)", out.MembershipsRemoved, out.OrgMembershipsRemoved)
		}
		// The person's credentials: gone, and nothing a resumed run of
		// theirs could be sealed with; the org's and the team's stay.
		if out.OAuthCredentialsRemoved != 1 || out.PoolPledgesRemoved != 1 || out.APIKeysRemoved != 1 || out.SecretsRemoved != 1 {
			t.Fatalf("credentials removed: oauth %d, pledges %d, api keys %d, secrets %d — want 1 each", out.OAuthCredentialsRemoved, out.PoolPledgesRemoved, out.APIKeysRemoved, out.SecretsRemoved)
		}
		if recs, err := f.oauth.ListByUser(ctx, userID); err != nil || len(recs) != 0 {
			t.Fatalf("subscription credentials of the erased account left: %+v (%v)", recs, err)
		}
		if recs, err := f.oauth.ListByUser(ctx, secrets.OrgOwnerKey("o1")); err != nil || len(recs) != 1 {
			t.Fatalf("the org's subscription credential went with the account: %+v (%v)", recs, err)
		}
		if pledges, err := f.pledges.ListByUser(ctx, userID); err != nil || len(pledges) != 0 {
			t.Fatalf("pool pledges of the erased account left: %+v (%v)", pledges, err)
		}
		if _, err := f.keys.Get(teamCtx, "key-own"); !errors.Is(err, secrets.ErrApiKeyNotFound) {
			t.Fatalf("the personal API key survived: %v", err)
		}
		if _, err := f.keys.Get(teamCtx, "key-team"); err != nil {
			t.Fatalf("the team-wide API key went with the account: %v", err)
		}
		if _, err := f.gens.Get(teamCtx, "sec-own"); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
			t.Fatalf("the personal secret survived: %v", err)
		}
		// The run note keeps the pseudonym, not the address.
		if out.RunNotesReattributed != 1 {
			t.Fatalf("run_notes_reattributed = %d, want 1", out.RunNotesReattributed)
		}
		if code, _, body := f.call(t, http.MethodGet, "/api/runs/"+runID+"/notes", admin.AccessToken, ""); code != http.StatusOK || strings.Contains(strings.ToLower(body), fixtureEmail) || !strings.Contains(body, userID) {
			t.Fatalf("the notes after the erasure: %d %s, want the id as author and no address", code, body)
		}
		if out.ConfigShareDeliveriesScrubbed != 1 {
			t.Fatalf("config_share_deliveries_scrubbed = %d, want 1", out.ConfigShareDeliveriesScrubbed)
		}
		if ds, err := f.shares.ListDeliveries(ctx, "sh1", 0); err != nil || len(ds) != 1 || ds[0].SourceIP != "" || ds[0].UserAgent != "" {
			t.Fatalf("the config-editor edit still carries the person's IP or agent: %+v (%v)", ds, err)
		}
		if len(out.PersonalOrgsMarkedForPurge) != 1 {
			t.Fatalf("personal_orgs_marked_for_purge = %v, want the registration's org", out.PersonalOrgsMarkedForPurge)
		}
		// Audit: the rows naming the account lose what identifies the person
		// and keep proving what was done.
		if out.AuditEventsScrubbed != 6 {
			t.Fatalf("audit_events_scrubbed = %d, want 6 (the five seeded rows and the mint's own line)", out.AuditEventsScrubbed)
		}
		refusals, err := f.audit.ListPlatform(ctx, audit.Page{Action: "auth.login.failed"})
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, e := range refusals {
			if e.ID != "a4" && e.ID != "a5" {
				continue
			}
			seen++
			if e.IP != "" || e.UserAgent != "" || e.Meta["subject_hash"] != nil || e.Meta["reason"] == nil {
				t.Fatalf("a refused sign-in of the erased address still identifies the person, or lost its reason: %+v", e)
			}
		}
		if seen != 2 {
			t.Fatalf("found %d of the 2 seeded refused sign-ins", seen)
		}
		invitations, err := f.audit.ListByTenant(ctx, "o1", audit.Page{Action: "org_invitation.created"})
		if err != nil || len(invitations) != 1 || invitations[0].Meta["email"] != nil || invitations[0].IP != "10.0.0.3" {
			t.Fatalf("the invitation row addressed to the account: %+v (%v), want its address gone and the inviter's IP kept", invitations, err)
		}
		for _, tenant := range []string{"ta", "o1", f.user.ActiveTeamID} {
			events, err := f.audit.ListByTenant(ctx, tenant, audit.Page{})
			if err != nil || len(events) == 0 {
				t.Fatalf("the audit rows of %s: %d (%v)", tenant, len(events), err)
			}
			for _, e := range events {
				if e.ActorID != userID && e.TargetID != userID {
					continue
				}
				if e.Meta["email"] != nil || (e.ActorID == userID && (e.IP != "" || e.UserAgent != "")) {
					t.Fatalf("an audit row naming the erased account still identifies the person: %+v", e)
				}
				if e.ActorID != userID && e.ID == "a2" && e.IP != "10.0.0.2" {
					t.Fatalf("the admin's IP on a row the account is only the target of went: %+v", e)
				}
				if e.ActorID == "" && e.TargetID == "" {
					t.Fatalf("the scrub took the pseudonym too: %+v", e)
				}
			}
		}
		lines := f.platformEvents(t, "user.erased", 1)
		if len(lines) != 1 || lines[0].ActorID != admin.User.ID || lines[0].TargetID != userID || lines[0].ActorKind != "super_admin" {
			t.Fatalf("the audit line of the act: %+v", lines)
		}
		if lines[0].Meta["sessions_deleted"] != float64(2) && lines[0].Meta["sessions_deleted"] != int64(2) {
			t.Fatalf("the audit line does not carry what was removed: %+v", lines[0].Meta)
		}
	})

	t.Run("nothing of the account signs in or mints again", func(t *testing.T) {
		// An access token is stateless until its own expiry, as after a
		// disable; what reads the account refuses it — a mint, a switch,
		// a refresh — and nothing signs the account in again.
		if code, _, body := f.call(t, http.MethodPost, "/api/me/tokens", f.user.AccessToken, `{"name":"after"}`); code != http.StatusUnauthorized {
			t.Fatalf("a mint with an access token of the erased account: %d %s, want 401", code, body)
		}
		if _, err := f.svc.Refresh(ctx, f.other.RefreshToken, "ua", "ip"); err == nil {
			t.Fatal("a refresh token of the erased account still refreshes")
		}
		if _, err := f.svc.Login(ctx, fixtureEmail, fixturePassword, "ua", "ip"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login with the erased account's credentials: %v", err)
		}
	})

	t.Run("erasing twice is once", func(t *testing.T) {
		code, out, body := f.erase(t, admin.AccessToken, userID)
		if code != http.StatusOK || !out.AlreadyErased {
			t.Fatalf("second erasure: %d %s, want 200 already_erased", code, body)
		}
		if out.SessionsDeleted != 0 || out.PersonalAccessTokensRevoked != 0 || out.PushSubscriptionsRemoved != 0 || out.MembershipsRemoved != 0 || out.OrgMembershipsRemoved != 0 ||
			out.OAuthCredentialsRemoved != 0 || out.PoolPledgesRemoved != 0 || out.APIKeysRemoved != 0 || out.SecretsRemoved != 0 || out.RunNotesReattributed != 0 || out.ConfigShareDeliveriesScrubbed != 0 ||
			out.AuditEventsScrubbed != 0 {
			t.Fatalf("the second erasure removed something (the first one's audit line keeps the admin's IP: nothing of it is scrubbed): %+v", out)
		}
		if out.User != first.User {
			t.Fatalf("the second erasure changed the row: %+v, was %+v", out.User, first.User)
		}
		if lines := f.platformEvents(t, "user.erased", 2); len(lines) != 2 {
			t.Fatalf("the second erasure wrote %d audit line(s), want its own", len(lines))
		}
	})

	t.Run("an erased account admits no console write", func(t *testing.T) {
		if code, _, body := f.call(t, http.MethodPatch, "/api/admin/users/"+userID, admin.AccessToken, `{"status":"active"}`); code != http.StatusConflict || !strings.Contains(body, "user_erased") {
			t.Fatalf("PATCH on an erased account: %d %s, want 409 user_erased", code, body)
		}
		if code, _, body := f.call(t, http.MethodPost, "/api/admin/users/"+userID+"/reset-password", admin.AccessToken, ""); code != http.StatusUnprocessableEntity || strings.Contains(body, "temp_password") {
			t.Fatalf("reset-password on an erased account: %d %s, want 422 and no password", code, body)
		}
		if code, _, body := f.call(t, http.MethodPut, "/api/orgs/o1/members/"+userID, admin.AccessToken, `{"role":"member"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "erased") {
			t.Fatalf("placing an erased account in an org: %d %s, want 422 naming erased", code, body)
		}
		if code, _, body := f.call(t, http.MethodPost, "/api/admin/orgs", admin.AccessToken, `{"name":"Ghost","owner_email":"`+identity.ErasedEmail(userID)+`"}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, "erased") {
			t.Fatalf("creating an org owned by the erased account: %d %s, want 422 naming erased", code, body)
		}
		if oms, err := f.s.authStore().ListOrgMembershipsByUser(ctx, userID); err != nil || len(oms) != 0 {
			t.Fatalf("the erased account holds org memberships: %+v (%v)", oms, err)
		}
		if u, err := f.s.authStore().GetUser(ctx, userID); err != nil || u.Status != identity.UserStatusErased {
			t.Fatalf("a refused write changed the row: %+v (%v)", u, err)
		}
	})

	t.Run("an ordinary account is refused the route", func(t *testing.T) {
		other, err := f.svc.Register(ctx, "other@example.com", fixturePassword, "", "", "ua", "ip")
		if err != nil {
			t.Fatal(err)
		}
		if code, _, body := f.erase(t, other.AccessToken, admin.User.ID); code != http.StatusForbidden {
			t.Fatalf("an ordinary account erasing: %d %s, want 403", code, body)
		}
	})
}

// failingShares is the memory config-share store whose ScrubActor fails
// while fail is set: a step of the server's half failing once the account
// carries the erased status.
type failingShares struct {
	*configshare.MemoryStore
	fail bool
}

func (f *failingShares) ScrubActor(ctx context.Context, actor string) (int64, error) {
	if f.fail {
		return 0, errors.New("config-share store down")
	}
	return f.MemoryStore.ScrubActor(ctx, actor)
}

// TestAdminEraseUser_AnIncompleteErasureIsRecorded: a step failing once the
// account carries the erased status answers 500 and still writes the
// `user.erased` line, marked incomplete — the line a replay after a
// restoration reads, which an act begun must leave; the repeated request
// completes the erasure and writes its own line.
//
// The mutation this reddens: write the line only on success (the order
// where the audit follows the error switch) — no line after the 500.
func TestAdminEraseUser_AnIncompleteErasureIsRecorded(t *testing.T) {
	shares := &failingShares{MemoryStore: configshare.NewMemoryStore(), fail: true}
	f := newEraseFixtureWithShares(t, shares)
	ctx := context.Background()
	admin := f.superAdmin(t)
	userID := f.user.User.ID

	if code, _, body := f.erase(t, admin.AccessToken, userID); code != http.StatusInternalServerError {
		t.Fatalf("an erasure whose step fails: %d %s, want 500", code, body)
	}
	if u, err := f.s.authStore().GetUser(ctx, userID); err != nil || u.Status != identity.UserStatusErased || u.Email != fixtureEmail {
		t.Fatalf("the row after the failed step: %+v (%v), want erased and its address kept for the repetition", u, err)
	}
	lines := f.platformEvents(t, "user.erased", 1)
	if len(lines) != 1 || lines[0].TargetID != userID || lines[0].Meta["incomplete"] != true || lines[0].Meta["anonymized"] != false {
		t.Fatalf("the line of the incomplete act: %+v, want one, incomplete, before the anonymization", lines)
	}

	shares.fail = false
	code, out, body := f.erase(t, admin.AccessToken, userID)
	if code != http.StatusOK || out.AlreadyErased || out.User.Email != identity.ErasedEmail(userID) {
		t.Fatalf("the repeated erasure: %d %s", code, body)
	}
	lines = f.platformEvents(t, "user.erased", 2)
	if len(lines) != 2 {
		t.Fatalf("the repeated erasure wrote %d line(s) in all, want 2", len(lines))
	}
	for _, l := range lines {
		if l.Meta["incomplete"] == nil && l.Meta["invitations_removed"] == nil {
			t.Fatalf("a line carries no counts: %+v", l)
		}
	}
}
