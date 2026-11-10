// B7 of the #2005 PR-C' round-1 review: a key restricted to repository X or
// bot B reached a run on repository Y or bot C through a pool or the
// fallback door — the broker opens a lent row by id and checks ownership,
// provider and expiry only, and a credpool.Request names no repository.
// Every lent-key grant now passes the row's own audiences (acquireLent),
// and the preview judges every lent candidate the same way (previewLent).
//
// Mutants these witnesses red on, each run and restored: acquireLent
// sealing the grant without reading the row → every withheld row of the
// three lanes; previewLent passing every candidate through → the preview
// rows.

package cloudpublisher

import (
	"bytes"
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// lentKeyShapes are the audience rows every lane is judged on: the launch is
// bot docs-refresh on repository gwRepo.
var lentKeyShapes = []struct {
	name    string
	shape   func(*secrets.ApiKey)
	serves  bool
	preview credpool.Status
}{
	{"no audience (the control)", func(*secrets.ApiKey) {}, true, credpool.StatusActive},
	{"repos name another repository", func(k *secrets.ApiKey) { k.Repos = []string{"acme/other"} }, false, credpool.StatusRepoFiltered},
	{"bots name another bot", func(k *secrets.ApiKey) { k.Bots = []string{"review-pr"} }, false, credpool.StatusBotFiltered},
}

// lentKeyPool is a pool lending one donor's personal key of provider prov,
// the row shaped by shape; fallbackUse is the donor's door consent.
func lentKeyPool(t *testing.T, prov secrets.Provider, secret string, shape func(*secrets.ApiKey), fallbackUse bool) (*credpool.Broker, secrets.Sealer) {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	k := secrets.ApiKey{ID: secrets.NewApiKeyID(), TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: prov, Name: "lent", Fingerprint: secrets.FingerprintSHA256(secret)}
	shape(&k)
	if k.SealedSecret, err = secrets.SealAPIKey(sealer, k.ID, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	if err := keys.Create(store.WithTenant(ctx, "donor-team"), k); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, string(prov)), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(prov), KeyID: k.ID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: fallbackUse,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: keys, Sealer: sealer, Logger: testLogger(),
	})
	if broker == nil {
		t.Fatal("broker nil with every dependency wired")
	}
	return broker, sealer
}

func xaiWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "xai", Model: "xai/grok-4"},
	}}}
}

func lentLaunchCtx() context.Context {
	return secrets.WithLaunchRepo(store.WithTenant(context.Background(), poolTeam), gwRepo)
}

// The legacy pool (an empty bundle) and the attached pool lend the donor's
// xai key only to a launch its row's audiences name; the preview shows the
// refused candidate filtered and selects nothing in its place.
func TestLentKey_ThePoolsLendAKeyOnlyToALaunchItsAudienceNames(t *testing.T) {
	for _, lane := range []struct {
		name string
		att  *store.RunPoolAttachment
	}{
		{"legacy pool", nil},
		{"attached pool", &store.RunPoolAttachment{PoolID: "pool-1", ScopeID: credpool.TeamScopeID(poolTeam)}},
	} {
		for _, row := range lentKeyShapes {
			t.Run(lane.name+"/"+row.name, func(t *testing.T) {
				broker, sealer := lentKeyPool(t, secrets.ProviderXAI, "xai-donated-key", row.shape, false)
				var buf bytes.Buffer
				p := &Publisher{runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, logger: iterlog.New(iterlog.LevelInfo, &buf),
					identity: &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}}
				wf := xaiWorkflow()
				// The preview first: the launch's lease would read as serving.
				if lane.att == nil {
					preview, perr := previewReadOnly(p).PreviewCredentials(lentLaunchCtx(), previewSpec(poolTeam, "requester"), wf)
					if perr != nil {
						t.Fatalf("preview: %v", perr)
					}
					listed := false
					for _, c := range preview.Candidates {
						if c.Tier != "pool" || c.Provider != string(secrets.ProviderXAI) {
							continue
						}
						listed = true
						if c.Selected != row.serves || c.State != string(row.preview) {
							t.Fatalf("preview pool candidate selected=%v state=%q, want %v %q: %+v", c.Selected, c.State, row.serves, row.preview, c)
						}
					}
					if !listed {
						t.Fatalf("the preview lists no pool xai candidate: %+v", preview.Candidates)
					}
				}
				creds, err := p.resolveAndSealCredentials(lentLaunchCtx(), "run-lent", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, lane.att, boundaryDerived)
				if err != nil {
					t.Fatalf("resolveAndSealCredentials: %v", err)
				}
				sealed := false
				if creds.secretsRef != "" {
					rec, gerr := p.runSecrets.Get(lentLaunchCtx(), creds.secretsRef)
					if gerr != nil {
						t.Fatalf("RunSecrets.Get: %v", gerr)
					}
					b, oerr := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", "run-lent", rec.KeyID, rec.SealedBundle)
					if oerr != nil {
						t.Fatalf("OpenRunBundle: %v", oerr)
					}
					sealed = b.APIKeys[secrets.ProviderXAI] == "xai-donated-key"
				}
				if sealed != row.serves {
					t.Fatalf("lent xai key sealed = %v, want %v (log:\n%s)", sealed, row.serves, buf.String())
				}
				if !row.serves && len(creds.releasedGrants) != 1 {
					t.Fatalf("released grants = %d, want the refused grant handed back for the rollback", len(creds.releasedGrants))
				}
			})
		}
	}
}

// The fallback door lends a consenting donor's zai key only to a launch its
// row's audiences name.
func TestLentKey_TheFallbackDoorLendsAKeyOnlyToALaunchItsAudienceNames(t *testing.T) {
	for _, row := range lentKeyShapes {
		t.Run(row.name, func(t *testing.T) {
			broker, sealer := lentKeyPool(t, secrets.ProviderZAI, "zai-donated-key", row.shape, true)
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, poolTeam, secrets.ProviderAnthropic, "sk-ant-tenant")
			var buf bytes.Buffer
			p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, logger: iterlog.New(iterlog.LevelInfo, &buf)}
			wf := doorWf()
			creds, err := p.resolveAndSealCredentials(lentLaunchCtx(), "run-lent-door", poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, doorPolicy(), nil, boundaryDerived)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if got := creds.doorGrant != nil; got != row.serves {
				t.Fatalf("door grant = %v, want %v (log:\n%s)", creds.doorGrant, row.serves, buf.String())
			}
			if !row.serves && len(creds.releasedGrants) != 1 {
				t.Fatalf("released grants = %d, want the refused door grant handed back for the rollback", len(creds.releasedGrants))
			}
		})
	}
}
