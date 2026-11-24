package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-162 decision 7, #3126: an untrusted run spends only the credentials
// sealed for it — the pod's environment (the deployment's money: a vendor
// key, the pool's gateway) answers a typed refusal. The publisher's stamp
// rides the sealed bundle, and an untrusted run can reach a runner with NO
// bundle: the publisher refuses one whose model routes its team's own keys
// cannot fund (the untrusted arm of unfundedRefusalFor), but a publisher that
// seals nothing at all, a run that calls no model, or a route the walk cannot
// attribute leaves the runner without a bundle to read the stamp from (an
// untrusted run is never handed a generic secret either). So the refusal
// travels with the run itself: the runner derives it from the wire's trust
// (store.RunTrust.AmbientLLMRefused, the predicate the publisher's stamp
// reads too), with or without a bundle.
//
// Driven on the real path from the wire message the publisher produces to
// the model registry the claw executor resolves through: injectCredentials
// (what executeRun calls before anything spends) and Registry.ResolveWithContext
// with the lookup the runner installs at boot. One scenario per branch of the
// derivation, each isolated: no bundle at all, and a bundle that carries no
// LLM key and no stamp of its own (a server sealing before the stamp existed,
// or any producer that drops it).
//
// Mutations, each red on its own row:
//   - the no-bundle branch returning ctx unchanged (today's behaviour) → "no
//     bundle at all" resolves the pod's env key;
//   - the bundle branch reading bundle.AmbientLLMRefused alone (today's
//     behaviour) → "a bundle without any LLM key" resolves the pod's env key;
//   - the predicate answering for every trust → the trusted controls are
//     refused.
func TestInjectCredentials_AnUntrustedRunRefusesThePodsEnvWithOrWithoutABundle(t *testing.T) {
	// The registry's credential path reads the INSTALLED lookup, which the
	// runner installs at boot (cmd/iterion/runner.go).
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	// The pod's money: what the env fallback spends when nothing refuses it.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-pod-env-the-deployments-money")
	const spec = "anthropic/claude-haiku-4-5"

	rs := secrets.NewMemoryRunSecretsStore()
	// A bundle without any LLM key and without the stamp: one generic
	// secret, nothing to spend on a model.
	sealed, dek := sealDekFixture(t, "team-a", "run-nokey", secrets.RunBundle{
		GenericSecrets: map[string]string{"scanner_token": "tok-not-an-llm-key"},
	})
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-nokey", TenantID: "team-a", RunID: "run-nokey", KeyID: secrets.DEKKeyID, SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}

	for _, row := range []struct {
		name    string
		msg     queue.RunMessage
		refused bool
	}{
		{"no bundle at all, untrusted", queue.RunMessage{RunID: "run-bare", TenantID: "team-a", Trust: store.RunTrustFork}, true},
		{"a bundle without any LLM key, untrusted", queue.RunMessage{RunID: "run-nokey", TenantID: "team-a", SecretsRef: "ref-nokey", BundleDEK: dek, Trust: store.RunTrustFork}, true},
		{"an unrecognised trust is untrusted", queue.RunMessage{RunID: "run-bare", TenantID: "team-a", Trust: store.RunTrust("mirror")}, true},
		{"no bundle at all, trusted (the env fallback, unchanged)", queue.RunMessage{RunID: "run-bare", TenantID: "team-a"}, false},
		{"a bundle without any LLM key, trusted (unchanged)", queue.RunMessage{RunID: "run-nokey", TenantID: "team-a", SecretsRef: "ref-nokey", BundleDEK: dek}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			msg := row.msg
			ctx, cleanup, err := r.injectCredentials(context.Background(), &msg)
			if err != nil {
				t.Fatalf("injectCredentials: %v", err)
			}
			if cleanup != nil {
				defer cleanup()
			}
			client, err := model.NewRegistry().ResolveWithContext(ctx, spec)
			if !row.refused {
				if err != nil || client == nil {
					t.Fatalf("a trusted run's env fallback must be unchanged: client=%v err=%v", client, err)
				}
				return
			}
			var refused *model.ErrAmbientCredentialRefused
			if !errors.As(err, &refused) || !errors.Is(err, credroute.ErrAmbientRefused) {
				t.Fatalf("ResolveWithContext = (%v, %v), want *model.ErrAmbientCredentialRefused — an untrusted run reached the pod's own key", client, err)
			}
			if client != nil {
				t.Fatal("a refused resolution built a client")
			}
		})
	}
}
