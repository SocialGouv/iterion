package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func seedGatewayKey(t *testing.T, keys secrets.ApiKeyStore, sealer secrets.Sealer, tenant, user string) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{ID: secrets.NewApiKeyID(), TenantID: tenant, ScopeTeamID: tenant, ScopeUserID: user, Provider: secrets.ProviderOpenAICompatible, BaseURL: "https://gw.example.test", Name: "gw", CreatedAt: time.Now().UTC()}
	sealed, err := secrets.SealAPIKeyFor(sealer, k, []byte("sk-gateway-0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	k.SealedSecret = sealed
	if err := keys.Create(store.WithTenant(t.Context(), tenant), k); err != nil {
		t.Fatal(err)
	}
	return k
}

// ADR-131 §6, the write half of the gateway pin: the walk spends a team's
// gateway key (#2005 PR-C'), so a webhook pin on one is admitted at write like
// a pin on any key of the team. Its exclusivity is the launch's to enforce —
// a pinned gateway key that is deleted, withheld by an audience or
// undecryptable when the run walks refuses the launch by name
// (pkg/server/cloudpublisher's TestGatewayPin_ABrokenPinRefusesTheLaunchByName).
// The spendability arm still applies to it: a user-scoped gateway key is
// invisible to the lane's synthetic actor, so the write is still where that
// pin is refused.
func TestValidateKeyOverrides_AdmitsAGatewayKeyPin(t *testing.T) {
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	keys := secrets.NewMemoryApiKeyStore()
	k := seedGatewayKey(t, keys, sealer, "t1", "")
	s := &Server{apiKeys: keys}
	ctx := store.WithTenant(t.Context(), "t1")
	if err := s.validateKeyOverrides(ctx, "t1", webhookActor("wh-gw"), map[string]string{"openai_compatible": k.ID}); err != nil {
		t.Fatalf("a pin on the team's gateway key refused at write: %v", err)
	}
	personal := seedGatewayKey(t, keys, sealer, "t1", "u1")
	err := s.validateKeyOverrides(ctx, "t1", webhookActor("wh-gw"), map[string]string{"openai_compatible": personal.ID})
	if err == nil || !strings.Contains(err.Error(), "not spendable by this lane") {
		t.Fatalf("a pin on a user-scoped gateway key = %v, want the spendability refusal", err)
	}
	// The vendor control: a vendor key of the tenant pins as it always did.
	vendor := secrets.ApiKey{ID: secrets.NewApiKeyID(), TenantID: "t1", ScopeTeamID: "t1", Provider: secrets.ProviderAnthropic, Name: "ant", CreatedAt: time.Now().UTC()}
	if err := keys.Create(ctx, vendor); err != nil {
		t.Fatal(err)
	}
	if err := s.validateKeyOverrides(ctx, "t1", webhookActor("wh-gw"), map[string]string{"anthropic": vendor.ID}); err != nil {
		t.Fatalf("a vendor pin refused: %v", err)
	}
}

// A gateway key authenticates against its tenant's own endpoint, which a
// stranger's run never dials; the pool's vocabulary never names the
// provider. A pledge of one is refused rather than parked as "Sharing".
func TestVerifyLendable_RefusesAGatewayKey(t *testing.T) {
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	keys := secrets.NewMemoryApiKeyStore()
	k := seedGatewayKey(t, keys, sealer, "t1", "donor")
	s := &Server{apiKeys: keys}
	r := httptest.NewRequest(http.MethodPut, "/api/me/pledges", nil)
	r = r.WithContext(store.WithTenant(r.Context(), "t1"))
	_, herr := s.verifyLendable(r, "donor", credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible), KeyID: k.ID})
	if herr == nil || herr.status != http.StatusBadRequest || !strings.Contains(herr.msg, "never pooled") {
		t.Fatalf("lending a gateway key = %+v, want 400 saying it is never pooled", herr)
	}
}
