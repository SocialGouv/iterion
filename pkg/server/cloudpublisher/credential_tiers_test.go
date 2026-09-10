package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #991 — a run records WHICH tier funded it, so "who paid for this?" stays
// answerable once the publisher's GRANTED log line has rotated away.
//
// The scenario is the one the ticket asks for, and it is the reason the
// field is re-stamped rather than written once: the same run, resolved
// twice, is funded by two different tiers. Nothing here hands the publisher
// a tier — if the stamp is not wired to the real resolution, the assertions
// read an empty list.
func TestCredentialTiers_platformThenPoolAfterTheKeyIsWithdrawn(t *testing.T) {
	ctx := context.Background()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	// The deployment's own fallback forfait, and a donor's pledge that is
	// NOT yet lending — the pool is consulted before the platform, so a
	// live pledge would win and the first half would prove nothing.
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform")
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")

	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	pledge := credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: false, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}
	if err := pledges.Upsert(ctx, pledge); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{
		runSecrets: rs,
		sealer:     sealer,
		// No apiKeys and no tenant forfait: this tenant brought nothing,
		// which is the only condition under which the shared tiers serve.
		oauthForfait: oauth,
		credPool: credpool.NewBroker(credpool.BrokerConfig{
			Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(),
			Ledger: credpool.NewMemoryLedger(), OAuth: oauth, Sealer: sealer, Logger: testLogger(),
		}),
		logger: iterlog.New(iterlog.LevelError, nil),
	}
	tctx := store.WithTenant(ctx, poolTeam)

	// 1. The launch. Every tenant tier abstained and no pledge is lending,
	//    so the deployment's own fallback pays.
	launch, err := p.resolveAndSealCredentials(tctx, "run-991", poolOrg, poolTeam, "requester", "docs-refresh", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolve (launch): %v", err)
	}
	if got := launch.tiers; len(got) != 1 || got[0] != store.CredentialTierPlatform {
		t.Fatalf("launch tiers = %v, want [platform]", got)
	}
	if s := launch.stamp(); len(s.Tiers) != 1 || s.Tiers[0] != store.CredentialTierPlatform {
		t.Fatalf("launch stamp tiers = %v, want the resolution's own answer", s.Tiers)
	}

	// 2. The platform forfait is withdrawn and the donor starts lending —
	//    the ordinary way a long-lived run's funding changes under it.
	rec, err := oauth.Get(ctx, secrets.PlatformOwnerKey, secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("read the platform record back: %v", err)
	}
	if err := oauth.Delete(ctx, rec.ID); err != nil {
		t.Fatalf("withdraw the platform forfait: %v", err)
	}
	pledge.Enabled = true
	if err := pledges.Upsert(ctx, pledge); err != nil {
		t.Fatalf("enable the pledge: %v", err)
	}

	// 3. The resume, same run. A field written only at launch would still
	//    say "platform" here and send an operator to the wrong door.
	resumed, err := p.resolveAndSealCredentials(tctx, "run-991", poolOrg, poolTeam, "requester", "docs-refresh", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolve (resume): %v", err)
	}
	if got := resumed.tiers; len(got) != 1 || got[0] != store.CredentialTierPool {
		t.Fatalf("resumed tiers = %v, want [pool] — the platform key is gone and a donor is paying", got)
	}

	// And the launch path writes it onto the in-memory document as one unit
	// with the fingerprints, the way the resume path writes it through
	// SetRunCredStamp.
	var r store.Run
	resumed.applyTo(&r)
	if len(r.CredentialTiers) != 1 || r.CredentialTiers[0] != store.CredentialTierPool {
		t.Fatalf("Run.CredentialTiers = %v after applyTo, want [pool]", r.CredentialTiers)
	}
	if len(r.CredFingerprints) != len(resumed.fingerprints) {
		t.Fatalf("applyTo left the fingerprints behind: %v vs %v", r.CredFingerprints, resumed.fingerprints)
	}
}

// credentialTierForSlot is the ONE place the tier is decided, read by both
// the GRANTED log line and the run stamp. Its own bench, because a helper
// two callers share is the one whose drift is invisible.
func TestCredentialTierForSlot_everyTier(t *testing.T) {
	const slot = "claude_code"
	poolGrant := &credpool.Grant{Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: slot}}
	cases := []struct {
		name   string
		bundle secrets.RunBundle
		grant  *credpool.Grant
		own    string
		want   string
	}{
		{"nothing claimed it — the tenant's own subscription", secrets.RunBundle{}, nil, store.CredentialTierOAuthForfait, store.CredentialTierOAuthForfait},
		{"nothing claimed it — the tenant's own key", secrets.RunBundle{}, nil, store.CredentialTierBYOK, store.CredentialTierBYOK},
		{"the org lent it", secrets.RunBundle{OrgSourced: map[string]bool{slot: true}}, nil, store.CredentialTierOAuthForfait, store.CredentialTierOrg},
		{"the deployment's fallback", secrets.RunBundle{PlatformSourced: map[string]bool{slot: true}}, nil, store.CredentialTierOAuthForfait, store.CredentialTierPlatform},
		{"a donor lent it", secrets.RunBundle{}, poolGrant, store.CredentialTierOAuthForfait, store.CredentialTierPool},
		// The pool wins over the provenance maps: a lent credential is the
		// donor's spend whatever slot it filled.
		{"a donor lent it, platform map set too", secrets.RunBundle{PlatformSourced: map[string]bool{slot: true}}, poolGrant, store.CredentialTierOAuthForfait, store.CredentialTierPool},
		// A grant for ANOTHER slot must not colour this one.
		{"a grant naming another slot", secrets.RunBundle{}, &credpool.Grant{Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "codex"}}, store.CredentialTierOAuthForfait, store.CredentialTierOAuthForfait},
		// Same ref, different source: an API-key grant does not claim the
		// OAuth slot of the same name.
		{"a grant of the other source", secrets.RunBundle{}, &credpool.Grant{Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: slot}}, store.CredentialTierOAuthForfait, store.CredentialTierOAuthForfait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialTierForSlot(tc.bundle, tc.grant, slot, credpool.SourceOAuth, tc.own); got != tc.want {
				t.Fatalf("tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// A resolution that sealed nothing names no tier: an empty list is "iterion
// cannot say", and a default would be a confident wrong answer about who
// paid — the failure the per-credential meter exists to remove.
func TestCredentialTiers_nothingSealedNamesNoTier(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	// Drop the pool so nothing at all can serve.
	f.pub.credPool = nil
	_, creds := f.mustResolveEmpty(t)
	if len(creds.tiers) != 0 {
		t.Fatalf("tiers = %v on a resolution that sealed nothing, want none", creds.tiers)
	}
	if s := creds.stamp(); len(s.Tiers) != 0 {
		t.Fatalf("stamp tiers = %v, want none", s.Tiers)
	}
	var r store.Run
	r.CredentialTiers = []string{store.CredentialTierPlatform}
	creds.applyTo(&r)
	if len(r.CredentialTiers) != 0 {
		t.Fatalf("applyTo left a stale tier in place: %v — a re-resolution replaces wholesale", r.CredentialTiers)
	}
}

// mustResolveEmpty runs the resolution and returns it whatever it sealed.
func (f *poolFixture) mustResolveEmpty(t *testing.T) (secrets.RunBundle, credResolution) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, "run-empty", poolOrg, poolTeam, "requester", "docs-refresh", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	return secrets.RunBundle{}, creds
}
