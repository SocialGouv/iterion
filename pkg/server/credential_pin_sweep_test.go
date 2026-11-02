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

// sweptSurfaces waits for the delete's audit line and returns the
// pins_cleared list it carries — best-effort and detached, so the poll is
// how a test reads it at all.
func sweptSurfaces(t *testing.T, auditStore *audit.MemoryStore, tenant, action string) []string {
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
			cleared, _ := e.Meta["pins_cleared"].([]string)
			return cleared
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit event with pins_cleared; got %+v", action, events)
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

// failingPinSweep breaks ONLY the targeted write: the list succeeds, the
// clear cannot. The delete must answer an explicit half-done receipt —
// the key row is gone (revocation is the irreversible half and happened
// first) while the un-swept pin stays and the operator is told so.
type failingPinSweep struct {
	webhooks.ConfigStore
}

func (f failingPinSweep) SetCredentialPins(ctx context.Context, id string, keyOverrides, secretOverrides map[string]string) error {
	return fmt.Errorf("sweep store down: %w", errors.New("write refused"))
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
