package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// TestResolveAndSeal_AnUnfundedUntrustedRunShipsNoBundle is #3126's probe on
// the REAL publisher: an untrusted run whose LLM route pins a provider no
// tier funds, on a deployment that does not set the opt-in
// RequireLLMCredential knob and a team that is not sovereign, is NOT refused
// by unfundedRefusalFor — and the walk seals no bundle at all (no LLM
// credential, and an untrusted run is never handed a generic secret), so the
// ambient stamp has nothing to ride. That is the message shape the runner
// receives (no SecretsRef, trust=fork), and why the runner derives the
// refusal from the message's trust (TestInjectCredentials_AnUntrustedRun
// RefusesThePodsEnvWithOrWithoutABundle consumes exactly this shape).
//
// The knob row is the control: the only publisher-side refusal of this shape
// is the deployment's opt-in, so a default deployment depends on the runner's
// derivation. Both orders of the publisher's own stamp are pinned by
// TestEveryUntrustedRunCarriesTheAmbientStamp (a sealed bundle carries it);
// moving the stamp after the no-bundle return changes nothing observable
// here, because that return seals nothing.
func TestResolveAndSeal_AnUnfundedUntrustedRunShipsNoBundle(t *testing.T) {
	for _, row := range []struct {
		name    string
		require bool
		refused bool
	}{
		{"a default deployment: no refusal, no bundle", false, false},
		{"the RequireLLMCredential knob (the control): refused", true, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			var buf bytes.Buffer
			p := &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), usageCaps: usagecap.NewMemStore(),
				runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
				logger: iterlog.New(iterlog.LevelInfo, &buf), requireLLMCredential: row.require}
			wf := compileTestSource(t, anthropicPinnedBot)
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-unfunded-fork", "", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, nil, nil)
			if row.refused {
				if !errors.Is(err, runview.ErrNoLLMCredential) {
					t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential under the knob", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAndSealCredentials = %v — the probe expected the default posture to refuse nothing for this shape; if it now refuses, the runner's derivation is a second floor, not the only one", err)
			}
			if res.secretsRef != "" || res.dek != nil {
				t.Fatalf("an unfunded untrusted run sealed a bundle (ref=%q) — the probe expected none; the stamp would then ride it", res.secretsRef)
			}
		})
	}
}
