package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the half the write guards cannot reach: a pin validated at write
// outlives the record it names the moment its owner deletes that record.
// Nothing downstream reports it — OrderedAPIKeyCandidates drops an
// unresolvable pin id in silence and the walk seals the team's default —
// so the delete chokepoints sweep the pins instead, and these witnesses
// read the STORES after the delete: a stored pin naming a dead id is the
// defect; its absence is the fix. The webhook arm of the hole is
// pre-existing; the trigger subscription arm is the one the pin routes
// opened, and a cron subscription fires forever.

func newPinSweepServer(t *testing.T) (*Server, *tenantSpyKeyStore, *bytes.Buffer, *audit.MemoryStore) {
	t.Helper()
	srv, spy, logs, auditStore := byokServer(t)
	srv.webhookConfigs = webhooks.NewMemoryConfigStore()
	srv.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()
	srv.genericSecrets = secrets.NewMemoryGenericSecretStore()
	return srv, spy, logs, auditStore
}

func seedSweepKey(t *testing.T, keys secrets.ApiKeyStore, tenant, id string, prov secrets.Provider) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{
		ID: id, ScopeTeamID: tenant, Provider: prov, Name: id,
		Fingerprint: "fp-" + id, CreatedAt: time.Now().UTC(),
	}
	if err := keys.Create(store.WithTenant(t.Context(), tenant), k); err != nil {
		t.Fatal(err)
	}
	return k
}

func seedSweepWebhook(t *testing.T, cfgs webhooks.ConfigStore, cfg webhooks.Config) {
	t.Helper()
	if cfg.TokenHash == "" {
		cfg.TokenHash = "hash"
	}
	if err := cfgs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func seedSweepSubscription(t *testing.T, subs trigger.SubscriptionStore, sub trigger.Subscription) {
	t.Helper()
	if err := subs.Create(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
}

// deleteTeamKey runs DELETE /api/teams/{team}/api-keys/{keyID} as a
// super-admin, the role the chokepoint's sweep answers to.
func deleteTeamKey(srv *Server, keyID, team string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodDelete, "/api/teams/"+team+"/api-keys/"+keyID, nil)
	r.SetPathValue("id", team)
	r.SetPathValue("key_id", keyID)
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: team})
	r = r.WithContext(store.WithTenant(ctx, team))
	w := httptest.NewRecorder()
	srv.handleDeleteApiKey(w, r)
	return w
}

// deleteTeamSecret runs DELETE /api/teams/{team}/secrets/{secretID} the
// same way.
func deleteTeamSecret(srv *Server, secretID, team string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodDelete, "/api/teams/"+team+"/secrets/"+secretID, nil)
	r.SetPathValue("id", team)
	r.SetPathValue("secret_id", secretID)
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: team})
	r = r.WithContext(store.WithTenant(ctx, team))
	w := httptest.NewRecorder()
	srv.handleDeleteGenericSecret(w, r)
	return w
}

// sweptSurfaces waits for a delete's audit line that carries pins_cleared
// and returns the list.
func sweptSurfaces(t *testing.T, auditStore *audit.MemoryStore, tenant, action string) []string {
	t.Helper()
	return auditPinReceipt(t, auditStore, tenant, action, "pins_cleared")
}

// auditPinReceipt waits for an audit line of the action that carries the
// named pin receipt (pins_cleared, pins_unreconciled) and returns the list
// — best-effort and detached, so the poll is how a test reads it at all.
// Events without the list (a delete whose sweep failed and reported
// nothing cleared) are skipped: the caller wants a receipt, not the first
// line with the action.
func auditPinReceipt(t *testing.T, auditStore *audit.MemoryStore, tenant, action, key string) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(context.Background(), tenant, audit.Page{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Action != action {
				continue
			}
			got, _ := e.Meta[key].([]string)
			if len(got) == 0 {
				continue
			}
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit event with %s; got %+v", action, key, events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDeleteApiKey_SweepsThePinsThatNameIt(t *testing.T) {
	srv, spy, _, auditStore := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	seedSweepKey(t, spy, "team-a", "key-alive", secrets.ProviderOpenAI)
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides:    map[string]string{"anthropic": dead.ID, "openai": "key-alive"},
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-other-tenant", TenantID: "team-b",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	seedSweepSubscription(t, srv.cfg.TriggerStore, trigger.Subscription{ID: "sub-1", TenantID: "team-a", BotID: "review-pr",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	seedSweepSubscription(t, srv.cfg.TriggerStore, trigger.Subscription{ID: "sub-2", TenantID: "team-a", BotID: "review-pr",
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})

	if w := deleteTeamKey(srv, dead.ID, "team-a"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d body=%s, want 204", w.Code, w.Body.String())
	}

	wh1, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.KeyOverrides) != 1 || wh1.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("wh-1 key_overrides = %v, want only the live pin — a stored pin naming the deleted key seals the team's default in silence", wh1.KeyOverrides)
	}
	if len(wh1.SecretOverrides) != 1 || wh1.SecretOverrides["forge_token"] != "sec-1" {
		t.Fatalf("wh-1 secret_overrides = %v — the sweep moved a pin that does not name the deleted key", wh1.SecretOverrides)
	}
	wh2, err := srv.webhookConfigs.Get(context.Background(), "wh-other-tenant")
	if err != nil {
		t.Fatal(err)
	}
	if wh2.KeyOverrides["anthropic"] != dead.ID {
		t.Fatalf("the sweep crossed tenants: wh-other-tenant lost its own row state: %v", wh2.KeyOverrides)
	}
	sub1, err := srv.cfg.TriggerStore.Get(context.Background(), "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub1.KeyOverrides) != 0 {
		t.Fatalf("sub-1 key_overrides = %v, want the clear — a cron subscription would otherwise fire under the default key forever", sub1.KeyOverrides)
	}
	sub2, err := srv.cfg.TriggerStore.Get(context.Background(), "sub-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub2.SecretOverrides) != 1 {
		t.Fatalf("sub-2 secret_overrides = %v — the sweep moved a pin that does not name the deleted key", sub2.SecretOverrides)
	}

	cleared := sweptSurfaces(t, auditStore, "team-a", "byok.deleted")
	want := []string{"webhook wh-1", "trigger sub-1"}
	slices.Sort(cleared)
	slices.Sort(want)
	if !slices.Equal(cleared, want) {
		t.Fatalf("audit pins_cleared = %v, want %v — the delete receipt must carry the surfaces that lost a pin", cleared, want)
	}
}

func TestDeleteGenericSecret_SweepsThePinsThatNameIt(t *testing.T) {
	srv, _, _, auditStore := newPinSweepServer(t)
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), "team-a"), secrets.GenericSecret{
		ID: "sec-1", ScopeTeamID: "team-a", Name: "FORGE_TOKEN", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), "team-a"), secrets.GenericSecret{
		ID: "sec-2", ScopeTeamID: "team-a", Name: "OTHER_TOKEN", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides:    map[string]string{"anthropic": "key-x"},
		SecretOverrides: map[string]string{"forge_token": "sec-1", "other": "sec-2"}})
	seedSweepSubscription(t, srv.cfg.TriggerStore, trigger.Subscription{ID: "sub-1", TenantID: "team-a", BotID: "review-pr",
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})

	if w := deleteTeamSecret(srv, "sec-1", "team-a"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d body=%s, want 204", w.Code, w.Body.String())
	}

	wh1, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.SecretOverrides) != 1 || wh1.SecretOverrides["other"] != "sec-2" {
		t.Fatalf("wh-1 secret_overrides = %v, want only the pin naming the surviving secret", wh1.SecretOverrides)
	}
	if len(wh1.KeyOverrides) != 1 || wh1.KeyOverrides["anthropic"] != "key-x" {
		t.Fatalf("wh-1 key_overrides = %v — the secret sweep moved a key pin", wh1.KeyOverrides)
	}
	sub1, err := srv.cfg.TriggerStore.Get(context.Background(), "sub-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub1.SecretOverrides) != 0 {
		t.Fatalf("sub-1 secret_overrides = %v, want the clear — the pinned workflow secret would silently go unfilled on every fire", sub1.SecretOverrides)
	}

	cleared := sweptSurfaces(t, auditStore, "team-a", "secret.deleted")
	want := []string{"webhook wh-1", "trigger sub-1"}
	slices.Sort(cleared)
	slices.Sort(want)
	if !slices.Equal(cleared, want) {
		t.Fatalf("audit pins_cleared = %v, want %v", cleared, want)
	}
}

// failingPinSweep breaks ONLY the clear: the list succeeds, the removal
// cannot. The delete must answer an explicit half-done receipt — the key
// row is gone (revocation is the irreversible half and happened first)
// while the un-swept pin stays and the operator is told so.
type failingPinSweep struct {
	webhooks.ConfigStore
}

func (f failingPinSweep) ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error) {
	return false, fmt.Errorf("sweep store down: %w", errors.New("write refused"))
}

func TestDeleteApiKey_AFailedSweepIsAHalfDoneReceipt(t *testing.T) {
	srv, spy, _, _ := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	srv.webhookConfigs = failingPinSweep{ConfigStore: srv.webhookConfigs}

	// The client gets the server's opaque 5xx body (its generic shape is
	// httpx's, not this route's); the explicit half-done receipt — the
	// deletion that happened, the sweep that failed — is the detail
	// errtrack logs for that 5xx. What a test can hold the route to is the
	// state that IS: no success receipt, the key gone, the pin standing.
	w := deleteTeamKey(srv, dead.ID, "team-a")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a broken sweep = %d body=%s, want 500", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "deleted") {
		t.Fatalf("the 500 body claims the deletion — a half-done receipt must not read as success: %s", w.Body.String())
	}
	if _, err := spy.Get(store.WithTenant(context.Background(), "team-a"), dead.ID); !errors.Is(err, secrets.ErrApiKeyNotFound) {
		t.Fatalf("the key survived a 500 answer (%v) — a failed sweep must not un-revoke, the delete is the irreversible half", err)
	}
	wh1, err := srv.webhookConfigs.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if wh1.KeyOverrides["anthropic"] != dead.ID {
		t.Fatalf("the un-swept pin vanished from the store — the receipt must describe the state that IS: %v", wh1.KeyOverrides)
	}
}

// TestDeleteApiKey_RetryFinishesAnInterruptedSweep is the reconciliation
// witness: the row is already gone when the sweep fails, so a bare retry
// would answer 404 before any sweep ran and the dead pin would seal the
// team's default forever. The retry must finish the sweep — clearing the
// pin, receipting the audit line — and STILL answer 404: the deletion did
// happen. Muting the 404-path sweep (the mutant) leaves the pin standing
// behind the 404 and this red.
func TestDeleteApiKey_RetryFinishesAnInterruptedSweep(t *testing.T) {
	srv, spy, _, auditStore := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	srv.webhookConfigs = failingPinSweep{ConfigStore: mem}

	if w := deleteTeamKey(srv, dead.ID, "team-a"); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a broken sweep = %d body=%s, want 500", w.Code, w.Body.String())
	}
	if _, err := spy.Get(store.WithTenant(context.Background(), "team-a"), dead.ID); !errors.Is(err, secrets.ErrApiKeyNotFound) {
		t.Fatalf("the key survived the failed sweep (%v)", err)
	}

	// The store is healthy again — the very situation of a retry.
	srv.webhookConfigs = mem
	if w := deleteTeamKey(srv, dead.ID, "team-a"); w.Code != http.StatusNotFound {
		t.Fatalf("retry = %d body=%s, want 404 — the row is gone, a success receipt would claim a deletion that did not happen", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.KeyOverrides) != 0 {
		t.Fatalf("the retry answered 404 over a standing pin: %v — the retry must finish the interrupted sweep", wh1.KeyOverrides)
	}
	if cleared := sweptSurfaces(t, auditStore, "team-a", "byok.deleted"); len(cleared) != 1 || cleared[0] != "webhook wh-1" {
		t.Fatalf("the heal's audit receipt = %v, want [webhook wh-1]", cleared)
	}
}

// TestDeleteGenericSecret_RetryFinishesAnInterruptedSweep is the secret
// twin of the reconciliation witness: same shape, its own 404 branch — a
// gate proven on one branch says nothing about its sibling.
func TestDeleteGenericSecret_RetryFinishesAnInterruptedSweep(t *testing.T) {
	srv, _, _, auditStore := newPinSweepServer(t)
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), "team-a"), secrets.GenericSecret{
		ID: "sec-1", ScopeTeamID: "team-a", Name: "FORGE_TOKEN", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})
	srv.webhookConfigs = failingPinSweep{ConfigStore: mem}

	if w := deleteTeamSecret(srv, "sec-1", "team-a"); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a broken sweep = %d body=%s, want 500", w.Code, w.Body.String())
	}

	srv.webhookConfigs = mem
	if w := deleteTeamSecret(srv, "sec-1", "team-a"); w.Code != http.StatusNotFound {
		t.Fatalf("retry = %d body=%s, want 404 — the row is gone, a success receipt would claim a deletion that did not happen", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.SecretOverrides) != 0 {
		t.Fatalf("the retry answered 404 over a standing pin: %v — the retry must finish the interrupted sweep", wh1.SecretOverrides)
	}
	if cleared := sweptSurfaces(t, auditStore, "team-a", "secret.deleted"); len(cleared) != 1 || cleared[0] != "webhook wh-1" {
		t.Fatalf("the heal's audit receipt = %v, want [webhook wh-1]", cleared)
	}
}

// TestDeleteApiKey_TheHealDemandsTheTenantRight: the 404 path has no row to
// read a scope from, so the heal gates on the tenant itself — and it must
// demand the same right the delete demands for a team-scoped record
// (canManageTeam), never sweep because a path spelled a tenant. A caller
// without the grant gets the bare 404 and the pin stands.
func TestDeleteApiKey_TheHealDemandsTheTenantRight(t *testing.T) {
	srv, spy, _, _ := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	srv.webhookConfigs = failingPinSweep{ConfigStore: mem}
	if w := deleteTeamKey(srv, dead.ID, "team-a"); w.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a broken sweep = %d body=%s, want 500", w.Code, w.Body.String())
	}

	// The retrying caller holds no grant over team-a: not a super-admin,
	// and the identity store answers every read with not-found.
	authSvc := &auth.Service{}
	authSvc.SetStoreForTest(noGrantIdentityStore{})
	srv.authSvc = authSvc
	srv.webhookConfigs = mem

	r := httptest.NewRequest(http.MethodDelete, "/api/teams/team-a/api-keys/"+dead.ID, nil)
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", dead.ID)
	ctx := auth.WithIdentity(r.Context(), auth.Identity{UserID: "u9", TeamID: "team-a"})
	r = r.WithContext(store.WithTenant(ctx, "team-a"))
	w := httptest.NewRecorder()
	srv.handleDeleteApiKey(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unauthorized retry = %d body=%s, want 404", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if wh1.KeyOverrides["anthropic"] != dead.ID {
		t.Fatalf("an unauthorized retry swept the team's pins: %v — the heal must demand the right the delete itself demands", wh1.KeyOverrides)
	}
}

// ctxCheckingPinSweep refuses to list under a dead context — the shape of a
// real store (the Mongo driver checks ctx), which a memory store cannot
// witness. The sweep the route runs must outlive the request: a client
// hanging up mid-sweep must not cancel the clear half-way and leave the
// dead pin standing with nobody watching. Passing the request context
// through (the mutant) fails the list under the canceled context and this
// red.
type ctxCheckingPinSweep struct {
	webhooks.ConfigStore
}

func (c ctxCheckingPinSweep) ListByTenant(ctx context.Context, tenantID string) ([]webhooks.Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list under a dead context: %w", err)
	}
	return c.ConfigStore.ListByTenant(ctx, tenantID)
}

func TestDeleteApiKey_TheSweepOutlivesADisconnectedClient(t *testing.T) {
	srv, spy, _, _ := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides: map[string]string{"anthropic": dead.ID}})
	srv.webhookConfigs = ctxCheckingPinSweep{ConfigStore: mem}

	r := httptest.NewRequest(http.MethodDelete, "/api/teams/team-a/api-keys/"+dead.ID, nil)
	r.SetPathValue("id", "team-a")
	r.SetPathValue("key_id", dead.ID)
	ctx, cancel := context.WithCancel(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}))
	defer cancel()
	r = r.WithContext(store.WithTenant(ctx, "team-a"))
	cancel() // the client hung up before the delete even ran

	w := httptest.NewRecorder()
	srv.handleDeleteApiKey(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete under a hung-up client = %d body=%s, want 204", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.KeyOverrides) != 0 {
		t.Fatalf("the hung-up client left the pin standing: %v — the sweep must not ride the request's context", wh1.KeyOverrides)
	}
}

// TestDeleteGenericSecret_TheSweepOutlivesADisconnectedClient is the secret
// twin of the disconnect witness: same detach, its own route — a guard
// proven on one route says nothing about its sibling.
func TestDeleteGenericSecret_TheSweepOutlivesADisconnectedClient(t *testing.T) {
	srv, _, _, _ := newPinSweepServer(t)
	if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), "team-a"), secrets.GenericSecret{
		ID: "sec-1", ScopeTeamID: "team-a", Name: "FORGE_TOKEN", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})
	srv.webhookConfigs = ctxCheckingPinSweep{ConfigStore: mem}

	r := httptest.NewRequest(http.MethodDelete, "/api/teams/team-a/secrets/sec-1", nil)
	r.SetPathValue("id", "team-a")
	r.SetPathValue("secret_id", "sec-1")
	ctx, cancel := context.WithCancel(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}))
	defer cancel()
	r = r.WithContext(store.WithTenant(ctx, "team-a"))
	cancel()

	w := httptest.NewRecorder()
	srv.handleDeleteGenericSecret(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete under a hung-up client = %d body=%s, want 204", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.SecretOverrides) != 0 {
		t.Fatalf("the hung-up client left the pin standing: %v — the sweep must not ride the request's context", wh1.SecretOverrides)
	}
}

// noGrantIdentityStore answers every read with not-found: the shape of a
// caller whose only standing is the session itself, wired so the
// canManageTeam gate reads a refusal instead of a nil store.
type noGrantIdentityStore struct {
	identity.Store
}

func (noGrantIdentityStore) GetMembership(context.Context, string, string) (identity.Membership, error) {
	return identity.Membership{}, identity.ErrNotFound
}

func (noGrantIdentityStore) GetTeam(context.Context, string) (identity.Team, error) {
	return identity.Team{}, identity.ErrNotFound
}

// outageIdentityStore refuses every read with a store failure — a Mongo
// blip under the identity store, which the heal's gate must report as the
// outage it is, never fold into "no right".
type outageIdentityStore struct {
	identity.Store
}

func (outageIdentityStore) GetMembership(context.Context, string, string) (identity.Membership, error) {
	return identity.Membership{}, errors.New("identity store unavailable")
}

func (outageIdentityStore) GetTeam(context.Context, string) (identity.Team, error) {
	return identity.Team{}, errors.New("identity store unavailable")
}

func (outageIdentityStore) GetOrgMembership(context.Context, string, string) (identity.OrgMembership, error) {
	return identity.OrgMembership{}, errors.New("identity store unavailable")
}

// TestDeleteRoutes_TheHealReportsAnIdentityStoreOutage: the 404 path's
// gate reads the caller's standing from the identity store, and a read the
// store cannot complete is an outage, not a "no". Folding it into a bool
// (the mutant) skips the heal in silence and answers 404 over pins that
// may still stand — the untreated retry the heal exists to close, with no
// trace. Both delete chokepoints answer their explicit 5xx instead, so the
// next retry heals; the org-tier form of the gate surfaces the same
// failure through the shared pass. A caller the gate REFUSES on a healthy
// store keeps the bare 404 (TestDeleteApiKey_TheHealDemandsTheTenantRight).
func TestDeleteRoutes_TheHealReportsAnIdentityStoreOutage(t *testing.T) {
	srv, _, _, _ := newPinSweepServer(t)
	authSvc := &auth.Service{}
	authSvc.SetStoreForTest(outageIdentityStore{})
	srv.authSvc = authSvc
	caller := auth.Identity{UserID: "u9", TeamID: "team-a"}

	t.Run("api key", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodDelete, "/api/teams/team-a/api-keys/key-gone", nil)
		r.SetPathValue("id", "team-a")
		r.SetPathValue("key_id", "key-gone")
		r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), caller), "team-a"))
		w := httptest.NewRecorder()
		srv.handleDeleteApiKey(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("retry under an identity outage = %d body=%s, want 500 — a 404 here skips the heal in silence", w.Code, w.Body.String())
		}
	})
	t.Run("secret", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodDelete, "/api/teams/team-a/secrets/sec-gone", nil)
		r.SetPathValue("id", "team-a")
		r.SetPathValue("secret_id", "sec-gone")
		r = r.WithContext(store.WithTenant(auth.WithIdentity(r.Context(), caller), "team-a"))
		w := httptest.NewRecorder()
		srv.handleDeleteGenericSecret(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("retry under an identity outage = %d body=%s, want 500 — a 404 here skips the heal in silence", w.Code, w.Body.String())
		}
	})
	t.Run("org tier", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodDelete, "/api/orgs/org-1/credentials/api-keys/key-gone", nil)
		r = r.WithContext(auth.WithIdentity(r.Context(), caller))
		_, err := srv.retryCredentialPinSweep(r, secrets.OrgTierTenantID("org-1"), "key-gone")
		if err == nil || !strings.Contains(err.Error(), "identity store unavailable") {
			t.Fatalf("org-tier heal under an identity outage err = %v, want the gate's read failure surfaced", err)
		}
	})
}

// landingEditPinSweep performs an operator's pin edit INSIDE the sweep's
// listing call — the list-to-write window, the exact interleaving the
// finding names: a listing copy taken before the edit must never become a
// wholesale rewrite that loses the edit. Without the write-time judgment
// (the mutant rewrites the row wholesale from the listing copy) the edit
// vanishes with a 204 and no trace.
type landingEditPinSweep struct {
	webhooks.ConfigStore
	tenant  string
	landing func(ctx context.Context, s webhooks.ConfigStore)
}

func (l *landingEditPinSweep) ListByTenant(ctx context.Context, tenantID string) ([]webhooks.Config, error) {
	rows, err := l.ConfigStore.ListByTenant(ctx, tenantID)
	if err == nil && tenantID == l.tenant && l.landing != nil {
		landing := l.landing
		l.landing = nil
		landing(ctx, l.ConfigStore)
	}
	return rows, err
}

func TestDeleteApiKey_APinEditLandingMidSweepSurvives(t *testing.T) {
	srv, spy, _, _ := newPinSweepServer(t)
	dead := seedSweepKey(t, spy, "team-a", "key-dead", secrets.ProviderAnthropic)
	mem := webhooks.NewMemoryConfigStore()
	seedSweepWebhook(t, mem, webhooks.Config{ID: "wh-1", TenantID: "team-a",
		KeyOverrides:    map[string]string{"anthropic": dead.ID, "openai": "key-alive"},
		SecretOverrides: map[string]string{"forge_token": "sec-1"}})
	srv.webhookConfigs = &landingEditPinSweep{ConfigStore: mem, tenant: "team-a",
		landing: func(ctx context.Context, s webhooks.ConfigStore) {
			cfg, err := s.Get(ctx, "wh-1")
			if err != nil {
				t.Errorf("landing read: %v", err)
				return
			}
			cfg.SecretOverrides["added"] = "sec-added"
			if err := s.Update(ctx, cfg); err != nil {
				t.Errorf("landing edit: %v", err)
			}
		}}

	if w := deleteTeamKey(srv, dead.ID, "team-a"); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d body=%s, want 204", w.Code, w.Body.String())
	}
	wh1, err := mem.Get(context.Background(), "wh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(wh1.KeyOverrides) != 1 || wh1.KeyOverrides["openai"] != "key-alive" {
		t.Fatalf("key_overrides = %v, want the dead pin gone and the live one standing", wh1.KeyOverrides)
	}
	if wh1.SecretOverrides["added"] != "sec-added" || wh1.SecretOverrides["forge_token"] != "sec-1" {
		t.Fatalf("secret_overrides = %v — the sweep rewrote the map from its listing copy and lost the pin edit that landed mid-sweep, an explicit choice gone with a 204 and no trace", wh1.SecretOverrides)
	}
}
