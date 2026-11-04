package server

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// #2644, the validators at their CALL SITES. Both machine lanes write pins
// on a create and on an edit, and each of those four routes calls the two
// validators itself — four routes, two validators, eight call sites. A
// validator-level test proves the arm and none of the sites: a route that
// dropped its call would store a pin its lane can never spend (a user-scoped
// key or secret, another team's record) or a name the sweep cannot address,
// and every validator test would stay green. So every site gets its own
// scenarios through its own route, each refused shape on a fresh server —
// one shape that slipped through cannot redden another — and the stored row
// is what is read after it.

// pinRouteRefusal is one shape every pin-writing route must refuse: one per
// arm a dropped call site would let through.
type pinRouteRefusal struct {
	name, field, pins string
}

var pinRouteRefusals = []pinRouteRefusal{
	{"a user-scoped key", "key_overrides", `{"anthropic":"key-alice"}`},
	{"a user-scoped secret", "secret_overrides", `{"forge_token":"sec-alice"}`},
	{"another team's secret", "secret_overrides", `{"forge_token":"sec-team-b"}`},
	{"a name the sweep cannot address", "secret_overrides", `{"forge.token":"sec-org"}`},
}

// The pins the seeded rows hold: the tenant's own org-scoped key and secret.
var (
	pinRouteKeyPins    = map[string]string{"anthropic": "key-org"}
	pinRouteSecretPins = map[string]string{"forge_token": "sec-org"}
)

// pinRouteServer is newPinSweepServer with every record the shapes name:
// team-a's org-scoped key and secret (what a seeded row pins), alice's
// personal key and secret, and team-b's secret.
func pinRouteServer(t *testing.T) *Server {
	t.Helper()
	srv, spy, _, _ := newPinSweepServer(t)
	seedSweepKey(t, spy, "team-a", "key-org", secrets.ProviderAnthropic)
	personal := secrets.ApiKey{
		ID: "key-alice", ScopeTeamID: "team-a", ScopeUserID: "alice", Provider: secrets.ProviderAnthropic,
		Name: "alice-personal", Fingerprint: "fp-alice", CreatedAt: time.Now().UTC(),
	}
	if err := spy.Create(store.WithTenant(t.Context(), "team-a"), personal); err != nil {
		t.Fatal(err)
	}
	seedSweepSecret(t, srv, "team-a", "sec-org")
	for _, sec := range []secrets.GenericSecret{
		{ID: "sec-alice", ScopeTeamID: "team-a", ScopeUserID: "alice", Name: "forge_token", CreatedAt: time.Now().UTC()},
		{ID: "sec-team-b", ScopeTeamID: "team-b", Name: "forge_token", CreatedAt: time.Now().UTC()},
	} {
		if err := srv.genericSecrets.Create(store.WithTenant(t.Context(), sec.ScopeTeamID), sec); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

// assertRefusedPin is the shared verdict: a 400 that names the map the pin
// came from, so the operator knows which field to fix.
func assertRefusedPin(t *testing.T, rec *httptest.ResponseRecorder, tc pinRouteRefusal) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("pinning %s = %d, want 400; body=%s", tc.name, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), tc.field) {
		t.Fatalf("the refusal %s does not name %s — the operator cannot tell which field to fix", rec.Body.String(), tc.field)
	}
}

// assertPinsKept reads the row's stored pins back: a refused edit moves none.
func assertPinsKept(t *testing.T, keys, secs map[string]string) {
	t.Helper()
	if !maps.Equal(keys, pinRouteKeyPins) || !maps.Equal(secs, pinRouteSecretPins) {
		t.Fatalf("the refused edit moved the stored pins to %v / %v, want %v / %v", keys, secs, pinRouteKeyPins, pinRouteSecretPins)
	}
}

// triggerReqIn builds a trigger-route request as a caller of team (the
// routes read the tenant off the identity); id is the {id} path value.
func triggerReqIn(t *testing.T, team, method, target, id, body string) *http.Request {
	t.Helper()
	ctx := auth.WithIdentity(t.Context(), auth.Identity{UserID: "op", TeamID: team})
	return orgReq(ctx, method, target, body, id)
}

// webhookAdminReq builds a team-webhook route request as an administrator of
// team-a, stamped the way the mux and the derivation gate stamp it.
func webhookAdminReq(t *testing.T, method, target, webhookID, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetPathValue("id", "team-a")
	if webhookID != "" {
		r.SetPathValue("webhook_id", webhookID)
	}
	ctx := testMintVerdictContext(store.WithTenant(auth.WithIdentity(r.Context(),
		auth.Identity{UserID: "u1", IsSuperAdmin: true, TeamID: "team-a"}), "team-a"))
	return r.WithContext(ctx)
}

const pinRouteTriggerBase = `"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]}`

func TestTriggerCreateRefusesEveryPinItsLaneCannotSpend(t *testing.T) {
	for _, tc := range pinRouteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			srv := pinRouteServer(t)
			rec := httptest.NewRecorder()
			srv.handleCreateTrigger(rec, triggerReqIn(t, "team-a", http.MethodPost, "/api/v1/triggers", "",
				"{"+pinRouteTriggerBase+`,"`+tc.field+`":`+tc.pins+"}"))
			assertRefusedPin(t, rec, tc)
			if subs, _ := srv.cfg.TriggerStore.ListByTenant(context.Background(), "team-a"); len(subs) != 0 {
				t.Fatalf("the refused create stored %d subscription(s): %+v", len(subs), subs)
			}
		})
	}
}

func TestTriggerUpdateRefusesEveryPinItsLaneCannotSpend(t *testing.T) {
	for _, tc := range pinRouteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			srv := pinRouteServer(t)
			seedSweepSubscription(t, srv.cfg.TriggerStore, trigger.Subscription{
				ID: "sub-pinned", TenantID: "team-a", BotID: "feature-dev",
				Invocation: bundle.InvocationKindBoard, Mode: bundle.ExecutionBoard,
				Match:        trigger.Matcher{Sources: []trigger.Source{trigger.SourceBoard}},
				KeyOverrides: maps.Clone(pinRouteKeyPins), SecretOverrides: maps.Clone(pinRouteSecretPins), Enabled: true,
			})
			rec := httptest.NewRecorder()
			srv.handleUpdateTrigger(rec, triggerReqIn(t, "team-a", http.MethodPut, "/api/v1/triggers/sub-pinned", "sub-pinned",
				"{"+pinRouteTriggerBase+`,"`+tc.field+`":`+tc.pins+"}"))
			assertRefusedPin(t, rec, tc)
			got, err := srv.cfg.TriggerStore.Get(context.Background(), "sub-pinned")
			if err != nil {
				t.Fatal(err)
			}
			assertPinsKept(t, got.KeyOverrides, got.SecretOverrides)
		})
	}
}

func TestWebhookCreateRefusesEveryPinItsLaneCannotSpend(t *testing.T) {
	for _, tc := range pinRouteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			srv := pinRouteServer(t)
			rec := httptest.NewRecorder()
			srv.handleCreateWebhook(rec, webhookAdminReq(t, http.MethodPost, "/api/teams/team-a/webhooks", "",
				`{"name":"wh","provider":"gitlab","wildcard_bots":true,"`+tc.field+`":`+tc.pins+"}"))
			assertRefusedPin(t, rec, tc)
			if cfgs, _ := srv.webhookConfigs.ListByTenant(context.Background(), "team-a"); len(cfgs) != 0 {
				t.Fatalf("the refused create stored %d webhook(s)", len(cfgs))
			}
		})
	}
}

func TestWebhookUpdateRefusesEveryPinItsLaneCannotSpend(t *testing.T) {
	for _, tc := range pinRouteRefusals {
		t.Run(tc.name, func(t *testing.T) {
			srv := pinRouteServer(t)
			seedSweepWebhook(t, srv.webhookConfigs, webhooks.Config{
				ID: "wh-pinned", TenantID: "team-a",
				KeyOverrides: maps.Clone(pinRouteKeyPins), SecretOverrides: maps.Clone(pinRouteSecretPins),
			})
			rec := httptest.NewRecorder()
			srv.handleUpdateWebhook(rec, webhookAdminReq(t, http.MethodPut, "/api/teams/team-a/webhooks/wh-pinned", "wh-pinned",
				`{"`+tc.field+`":`+tc.pins+"}"))
			assertRefusedPin(t, rec, tc)
			got, err := srv.webhookConfigs.Get(context.Background(), "wh-pinned")
			if err != nil {
				t.Fatal(err)
			}
			assertPinsKept(t, got.KeyOverrides, got.SecretOverrides)
		})
	}
}
