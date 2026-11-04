package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #2644, the addressability guard. Both stores hold the pins as one Mongo
// subdocument and the sweep's $unset addresses each entry as
// "<field>.<name>", so a pin name holding a dot or a $ is a name the clear
// cannot reach: stored flat inside the subdocument, matched by the $unset
// as a nested path (or refused as an operator field), the sweep reports
// nothing removed and the dead pin stands behind a 204 — the silent
// sealed-default failure the sweep exists to close. The refusal happens at
// write, in the one validator both machine lanes' routes pass through.
func TestValidatePinName_GuardsTheKeysBothValidatorsShare(t *testing.T) {
	keys := secrets.NewMemoryApiKeyStore()
	org := seedVendorKey(t, keys, "t1", "", "org-default", true)
	gen := secrets.NewMemoryGenericSecretStore()
	ctx := store.WithTenant(t.Context(), "t1")
	sec := secrets.GenericSecret{
		ID: "sec-org", TenantID: "t1", ScopeTeamID: "t1",
		Name: "forge_token", CreatedAt: time.Now().UTC(),
	}
	if err := gen.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	s := &Server{apiKeys: keys, genericSecrets: gen}

	for _, tc := range []struct {
		name  string
		pins  map[string]string
		field string
	}{
		{"dotted key pin", map[string]string{"forge.token": org.ID}, "key_overrides"},
		{"$-prefixed key pin", map[string]string{"$anthropic": org.ID}, "key_overrides"},
		{"dotted secret pin", map[string]string{"forge.token": sec.ID}, "secret_overrides"},
		{"empty secret pin", map[string]string{"": sec.ID}, "secret_overrides"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.field == "key_overrides" {
				err = s.validateKeyOverrides(ctx, "t1", triggerSpineActor, tc.pins)
			} else {
				err = s.validateSecretOverrides(ctx, "t1", tc.pins)
			}
			if err == nil {
				t.Fatalf("%s accepted %v — a name the Mongo clear cannot address would stand in the store while the delete answers 204", tc.field, tc.pins)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal %q does not name the map it came from — the operator cannot tell which field to fix", err.Error())
			}
		})
	}

	// The guard judges the NAME, not the pin: clean names still reach the
	// value arms and pass them.
	if err := s.validateSecretOverrides(ctx, "t1", map[string]string{"forge_token": sec.ID}); err != nil {
		t.Fatalf("a clean pin name refused: %v", err)
	}
	if err := s.validateKeyOverrides(ctx, "t1", triggerSpineActor, map[string]string{"anthropic": org.ID}); err != nil {
		t.Fatalf("a clean provider pin refused: %v", err)
	}
}

// The route is where the request's JSON lands, so the refusal is witnessed
// where it answers: a dotted pin name on the create route is a 400 naming
// the constraint, and nothing is stored.
func TestTriggers_APinNameAMongoPathCannotAddressIsRefusedAtTheRoute(t *testing.T) {
	srv, _, sec := triggerPinServer(t)
	rec := httptest.NewRecorder()
	srv.handleCreateTrigger(rec, teamTriggerReq(t, http.MethodPost, "/api/v1/triggers", "",
		`{"bot_id":"feature-dev","invocation":"board","mode":"board","match":{"sources":["board"]},
		 "secret_overrides":{"forge.token":"`+sec.ID+`"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a dotted pin name = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Mongo path") {
		t.Errorf("the refusal %s does not say why the name is unusable — an operator cannot act on it", rec.Body.String())
	}
	if n, _ := srv.cfg.TriggerStore.ListByTenant(store.WithTenant(t.Context(), "t1"), "t1"); len(n) != 0 {
		t.Errorf("the refused create stored %d subscription(s)", len(n))
	}
}
