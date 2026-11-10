// ADR-131 §5, the probe's half of "the slot can seal" (A4 and B3 of the
// PR-C' round-1 review): a gateway route is attributed to a key only where
// that key can actually seal for the run — the run's frozen lists admit the
// gateway slot and the key opens. A held key that cannot seal leaves the
// gateway routes to the runner's environment, said by name; the sovereign
// posture keeps attributing them (it owes them a key of its own), and every
// refusal sentence on that path names the held key and why it did not seal.
//
// Mutants these witnesses red on, each run and restored: the probe not asking
// the frozen lists → W15; asking them without noting the slot on the §5
// record → W15 and W6; the probe not opening the held key → W16's two
// undecryptable rows; the refusal without the held-key note → W16's sovereign
// and degraded rows; the env-funded line without it → W15 and W16's first
// row.

package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedUndecryptableGatewayKey stores a team gateway key whose sealed secret
// was bound to one base URL while the row names another — the store-level
// repoint the seal's AAD makes fail closed: OpenApiKey refuses it.
func seedUndecryptableGatewayKey(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string) secrets.ApiKey {
	t.Helper()
	k := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: teamID, ScopeTeamID: teamID,
		Provider: secrets.ProviderOpenAICompatible, Name: "team gateway (repointed)",
		BaseURL: gwURL, Fingerprint: gwFingerprint, CreatedAt: time.Now().UTC(),
	}
	sealed, err := secrets.SealAPIKeyFor(sealer, k, []byte(gwTeamKey))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	k.SealedSecret = sealed
	k.BaseURL = "https://repointed.example"
	if _, err := secrets.OpenApiKey(sealer, k); err == nil {
		t.Fatal("fixture: the repointed row still opens")
	}
	if err := st.Create(context.Background(), k); err != nil {
		t.Fatalf("create: %v", err)
	}
	return k
}

// failingScopeKeys fails the key list of one scope, every other read passing
// through: a degraded tier.
type failingScopeKeys struct {
	secrets.ApiKeyStore
	scope string
}

func (s failingScopeKeys) ListByTeam(ctx context.Context, teamID, userID string) ([]secrets.ApiKey, error) {
	if teamID == s.scope {
		return nil, errors.New("key list: connection reset")
	}
	return s.ApiKeyStore.ListByTeam(ctx, teamID, userID)
}

// W15 (A4): a team gateway key the run's FROZEN pair order holds out is not
// attributed — the run is env-funded exactly as ADR-131 §5's replay clause
// says, on the launch and on the resume of a pre-upgrade run alike, so the
// deployment knob (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL) does not refuse it
// claiming no tier holds a key. The env-funded line names the held key and
// the list that held it out, and the §5 line still appears once.
func TestGatewayKey_AKeyTheFrozenListsHoldOutIsNotAttributed(t *testing.T) {
	for _, row := range []struct {
		name     string
		boundary boundarySource
	}{
		{"launch under an order that names no gateway pair", boundaryDerived},
		{"resume of a pre-upgrade run", boundaryFrozen(nil)},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			p.requireLLMCredential = true
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-15", "", "team1", "owner1", gwBot, "", gatewayOnlyWorkflow(), nil, nil, model.RouteView{Workflow: gatewayOnlyWorkflow()}, nil, store.RunTrustDefault, nil, preUpgradePolicy(), nil, row.boundary)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials = %v, want nil — a key the frozen list holds out leaves the routes to the pod's gateway (ADR-131 §5)", err)
			}
			if got := openGatewayBundle(t, p, "run-gw-15", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != "" {
				t.Fatalf("gateway slot = %q under a frozen order that names no gateway pair", got)
			}
			log := buf.String()
			if !strings.Contains(log, envFundedLine) || !strings.Contains(log, "team gateway") || !strings.Contains(log, "frozen routing pair order names no pair for the openai_compatible_key slot") {
				t.Fatalf("the env-funded line does not name the held key and the list that held it out:\n%s", log)
			}
			if got := strings.Count(log, heldOutMarker); got != 1 {
				t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
			}
			if row.boundary.replay {
				return
			}
			// The preview reads the same probe: the run is env-funded, the
			// key named, and no refusal is mirrored.
			spec := previewSpec("team1", "owner1")
			spec.LLMRoutePolicy = preUpgradePolicy()
			preview, err := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), spec, gatewayOnlyWorkflow())
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			named := false
			for _, w := range preview.Warnings {
				if strings.Contains(w, "live launch refuses") {
					t.Fatalf("the preview mirrors a refusal the launch does not make: %q", w)
				}
				if strings.Contains(w, "rides the runner's openai_compatible gateway") && strings.Contains(w, "frozen routing pair order names no pair") {
					named = true
				}
			}
			if !named {
				t.Fatalf("the preview does not name the held-out key: %v", preview.Warnings)
			}
		})
	}
}

// W16 (B3): a held gateway key that cannot seal — its sealed secret does not
// open, or its tier's list could not be read — is named wherever the run's
// fate is said. An undecryptable key is not attributed (the routes ride the
// pod's gateway, named; before the walk spent any key, knob on or off), the
// sovereign posture still attributes and refuses naming the key and why, and
// a tier read as holding because its list failed refuses (knob on) naming the
// tier, never "no tier holds a credential".
func TestGatewayKey_AHeldKeyThatCannotSealIsNamed(t *testing.T) {
	t.Run("undecryptable, knob on and off: env-funded, named", func(t *testing.T) {
		for _, knob := range []bool{false, true} {
			p, buf := gatewayKeyPublisher(t)
			p.requireLLMCredential = knob
			k := seedUndecryptableGatewayKey(t, p.apiKeys, p.sealer, "team1")
			if _, err := resolveGatewayRun(t, p, "run-gw-16a", gatewayOnlyWorkflow(), nil); err != nil {
				t.Fatalf("knob=%v: resolveAndSealCredentials = %v, want nil — an undecryptable key leaves the routes to the pod's gateway", knob, err)
			}
			log := buf.String()
			if !strings.Contains(log, envFundedLine) || !strings.Contains(log, k.ID) || !strings.Contains(log, "does not open") {
				t.Fatalf("knob=%v: the env-funded line does not name the undecryptable key %s:\n%s", knob, k.ID, log)
			}
		}
	})
	t.Run("undecryptable, sovereign: refused, naming the key", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		k := seedUndecryptableGatewayKey(t, p.apiKeys, p.sealer, "team1")
		_, err := resolveGatewayRun(t, p, "run-gw-16b", gatewayOnlyWorkflow(), nil)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) || !refusal.Sovereign {
			t.Fatalf("resolveAndSealCredentials = %v, want the sovereign unfunded refusal", err)
		}
		if msg := err.Error(); !strings.Contains(msg, k.ID) || !strings.Contains(msg, "does not open") {
			t.Fatalf("refusal %q does not name the held key %s and why it did not seal", msg, k.ID)
		}
	})
	t.Run("degraded platform tier, knob on: refused, naming the tier", func(t *testing.T) {
		p, _ := gatewayKeyPublisher(t)
		p.requireLLMCredential = true
		p.apiKeys = failingScopeKeys{ApiKeyStore: p.apiKeys, scope: secrets.PlatformTenantID}
		_, err := resolveGatewayRun(t, p, "run-gw-16c", gatewayOnlyWorkflow(), nil)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) {
			t.Fatalf("resolveAndSealCredentials = %v, want the knob's unfunded refusal", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "platform tier") || !strings.Contains(msg, "could not be read") {
			t.Fatalf("refusal %q does not say the platform tier was read as holding a gateway key because its list failed", msg)
		}
	})
}
