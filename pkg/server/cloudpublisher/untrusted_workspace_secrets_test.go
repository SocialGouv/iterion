package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// untrustedFixture builds a publisher whose store HOLDS a resolvable
// workflow secret for (team, alice). The secret being present is the whole
// point: a test that withheld the store too would pass on an empty bundle
// without proving anything about trust.
func untrustedFixture(t *testing.T, name, value string) (*Publisher, *secrets.MemoryRunSecretsStore, secrets.Sealer) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	genericStore := secrets.NewMemoryGenericSecretStore()
	secretID := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(sealer, secretID, []byte(value))
	if err != nil {
		t.Fatalf("SealGenericSecret: %v", err)
	}
	if err := genericStore.Create(context.Background(), secrets.GenericSecret{
		ID: secretID, ScopeTeamID: "team", ScopeUserID: "alice",
		Name: name, SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	runSecrets := secrets.NewMemoryRunSecretsStore()
	return &Publisher{genericSecrets: genericStore, runSecrets: runSecrets, sealer: sealer}, runSecrets, sealer
}

func bundleOf(t *testing.T, p *Publisher, runSecrets *secrets.MemoryRunSecretsStore, sealer secrets.Sealer, ctx context.Context, runID, ref string) secrets.RunBundle {
	t.Helper()
	if ref == "" {
		return secrets.RunBundle{}
	}
	rec, err := runSecrets.Get(ctx, ref)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return b
}

// The runner writes the resolved forge_token INTO the run's clone as a git
// credential store (pkg/runner/loop_gitws.go). On the fork review lane that
// clone holds a pull-request author's code, so the tenant's workflow secrets
// must never enter the bundle at all.
//
// The trusted arm is what makes the untrusted arm mean something: the SAME
// fixture, the SAME store, resolving the secret — so a failure to resolve
// cannot be mistaken for the control working. This is the forbidden
// alternative, not an empty value.
func TestResolveAndSealCredentials_UntrustedWorkspaceGetsNoWorkflowSecret(t *testing.T) {
	wf := func(optional bool) *ir.Workflow {
		return &ir.Workflow{Secrets: map[string]*ir.Secret{
			"forge_token": {As: "env", Optional: optional},
		}}
	}

	t.Run("a trusted run resolves it", func(t *testing.T) {
		p, runSecrets, sealer := untrustedFixture(t, "forge_token", "ghp_realtoken")
		ctx := store.WithTenant(context.Background(), "team")
		creds, err := p.resolveAndSealCredentials(ctx, "run-trusted", "", "team", "alice", "", wf(true), nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil)
		if err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil", err)
		}
		b := bundleOf(t, p, runSecrets, sealer, ctx, "run-trusted", creds.secretsRef)
		if b.GenericSecrets["forge_token"] != "ghp_realtoken" {
			t.Fatalf("a trusted run did NOT get the secret (%+v) — the fixture proves nothing about trust if the secret cannot resolve at all", b.GenericSecrets)
		}
	})

	t.Run("a fork-trust run does not", func(t *testing.T) {
		p, runSecrets, sealer := untrustedFixture(t, "forge_token", "ghp_realtoken")
		ctx := store.WithTenant(context.Background(), "team")
		creds, err := p.resolveAndSealCredentials(ctx, "run-fork", "", "team", "alice", "", wf(true), nil, nil, model.ModelOverrides{}, nil, store.RunTrustFork, nil)
		if err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil (an OPTIONAL secret is withheld, not an error)", err)
		}
		b := bundleOf(t, p, runSecrets, sealer, ctx, "run-fork", creds.secretsRef)
		if got, ok := b.GenericSecrets["forge_token"]; ok {
			t.Fatalf("forge_token = %q reached an untrusted workspace's bundle — the runner would write it into a fork author's clone as a git credential", got)
		}
	})

	// The predicate is Trusted(), not "== fork": a trust value this binary
	// does not recognise (a newer replica's document mid-rollout, a
	// hand-edited row) must lose the secrets too. Mutating the check to
	// `trust.IsFork()` leaves this subtest, and only this subtest, red.
	t.Run("an unrecognised trust value does not either", func(t *testing.T) {
		p, runSecrets, sealer := untrustedFixture(t, "forge_token", "ghp_realtoken")
		ctx := store.WithTenant(context.Background(), "team")
		creds, err := p.resolveAndSealCredentials(ctx, "run-unknown", "", "team", "alice", "", wf(true), nil, nil, model.ModelOverrides{}, nil, store.RunTrust("vendored"), nil)
		if err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil", err)
		}
		b := bundleOf(t, p, runSecrets, sealer, ctx, "run-unknown", creds.secretsRef)
		if got, ok := b.GenericSecrets["forge_token"]; ok {
			t.Fatalf("forge_token = %q reached a workspace whose trust this binary does not recognise — an unknown trust must fail CLOSED", got)
		}
	})

	// Withholding must not be silent when the workflow says the credential
	// is not optional: a bot that cannot be given what it declares it needs
	// is a misconfigured lane, and running it anyway is the "dead capability,
	// green test" shape.
	t.Run("a REQUIRED secret makes the launch fail loudly", func(t *testing.T) {
		p, _, _ := untrustedFixture(t, "forge_token", "ghp_realtoken")
		ctx := store.WithTenant(context.Background(), "team")
		_, err := p.resolveAndSealCredentials(ctx, "run-req", "", "team", "alice", "", wf(false), nil, nil, model.ModelOverrides{}, nil, store.RunTrustFork, nil)
		if !errors.Is(err, errUntrustedRequiresSecrets) {
			t.Fatalf("resolveAndSealCredentials = %v, want errUntrustedRequiresSecrets — a required secret that cannot be supplied must stop the launch, not start a bot with it unset", err)
		}
	})
}
