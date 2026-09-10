package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #661 (a) — the run-doc stamp names only the credentials the run's
// RESOLVED routes can spend. Production shape: a bundle holding both the
// byok facade key (zai) and the org forfait (claude_code → anthropic); a
// rite whose nodes pin `provider: anthropic` executes entirely on the
// forfait, yet held one of the facade key's two slots for its whole life —
// and the next launch pinned to the facade hit `AT ITS CEILING (2/2)`,
// resolved without the key, and failed AUTH_FAILED.
func TestResolve_StampsOnlyTheSpendableFingerprints(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderZAI, "zai-facade", "fp-zai")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.OrgOwnerKey("team1"), "sk-ant-org")
	recs, err := oauth.ListByUser(context.Background(), secrets.OrgOwnerKey("team1"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("seeded forfait unreadable: %v (%d)", err, len(recs))
	}
	oauthFP := recs[0].Fingerprint

	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{apiKeys: keys, oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger()}
	ctx := store.WithTenant(context.Background(), "team1")
	resolve := func(runID string, wf *ir.Workflow) (secrets.RunBundle, credResolution) {
		t.Helper()
		creds, err := p.resolveAndSealCredentials(ctx, runID, "org1", "team1", "owner1", "rite", wf, nil, nil, model.ModelOverrides{}, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		rec, err := rs.Get(ctx, creds.secretsRef)
		if err != nil {
			t.Fatalf("run secrets: %v", err)
		}
		b, err := secrets.OpenRunBundle(sealer, runID, rec.SealedBundle)
		if err != nil {
			t.Fatalf("open bundle: %v", err)
		}
		return b, creds
	}
	pinned := func(provider string) *ir.Workflow {
		return &ir.Workflow{
			Name:  "rite",
			Entry: "oracle",
			Nodes: map[string]ir.Node{
				"oracle": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "oracle"}, LLMFields: ir.LLMFields{Backend: "claude_code", Provider: provider}},
				"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges: []*ir.Edge{{From: "oracle", To: "done"}},
		}
	}

	// Every node pinned to the forfait's provider: the bundle still seals
	// both credentials, the stamp names only the forfait.
	bundle, creds := resolve("rite-anthropic", pinned("anthropic"))
	if bundle.APIKeys[secrets.ProviderZAI] == "" || len(bundle.OAuthCredentials["claude_code"]) == 0 {
		t.Fatalf("the bundle must keep every sealed credential, got keys=%v oauth=%d", bundle.APIKeys, len(bundle.OAuthCredentials))
	}
	if len(creds.fingerprints) != 1 || creds.fingerprints[0] != oauthFP {
		t.Fatalf("stamp = %v, want only the forfait's %s — the facade key's slot must stay free for a run that spends it", creds.fingerprints, oauthFP)
	}

	// Pinned to the facade: only the facade is stamped.
	if _, creds := resolve("rite-zai", pinned("zai")); len(creds.fingerprints) != 1 || creds.fingerprints[0] != "fp-zai" {
		t.Fatalf("stamp = %v, want only the facade key's fp-zai", creds.fingerprints)
	}

	// Unpinned (an unresolvable route takes whatever the process holds):
	// everything stays stamped — fail open toward protection.
	if _, creds := resolve("rite-unpinned", pinned("")); len(creds.fingerprints) != 2 {
		t.Fatalf("stamp = %v, want both fingerprints for an unpinned run", creds.fingerprints)
	}
	// No workflow at all (unknown routes): same.
	if _, creds := resolve("rite-nil", nil); len(creds.fingerprints) != 2 {
		t.Fatalf("stamp = %v, want both fingerprints with no workflow", creds.fingerprints)
	}
}
