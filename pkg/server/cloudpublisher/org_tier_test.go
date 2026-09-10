package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The ORG tier: an organization's own shared credentials, lent to the teams
// its CredentialAudience admits. Four branches decide whether it fires, and
// each wrong answer spends someone else's money or strands a run:
//
//	team outside the audience  -> nothing (the org key is not theirs)
//	team inside the audience   -> the org key
//	team with its own BYOK     -> its own key wins, the org never shadows it
//	org document unreadable    -> nothing (fail closed)

// resolveBundleForOrg is resolveBundle with an org id, which the org tier
// needs to find its credentials and its audience.
func resolveBundleForOrg(t *testing.T, p *Publisher, runID, orgID, tenant, owner string) secrets.RunBundle {
	t.Helper()
	rs, ok := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	if !ok {
		t.Fatalf("resolveBundleForOrg needs a MemoryRunSecretsStore")
	}
	ctx := store.WithTenant(context.Background(), tenant)
	creds, err := p.resolveAndSealCredentials(ctx, runID, orgID, tenant, owner, "", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(p.sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle
}

// orgTierPublisher builds a publisher whose org `orgID` holds one shared
// anthropic key, with `audience` deciding who may spend it.
func orgTierPublisher(t *testing.T, orgID string, audience identity.CredentialAudience) *Publisher {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.OrgTierTenantID(orgID), secrets.ProviderAnthropic, "sk-ant-org-shared")
	return &Publisher{
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
		identity: &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID, "team-out": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: audience}},
		},
	}
}

// A team the audience does not name gets NOTHING from the org tier. This is
// the whole point of the audience: the zero value lends to nobody, so an
// org that never opted a team in cannot have its subscription spent by it.
func TestOrgTier_teamOutsideTheAudienceGetsNothing(t *testing.T) {
	const orgID = "org-1"
	p := orgTierPublisher(t, orgID, identity.CredentialAudience{Teams: []string{"team-in"}})

	b := resolveBundleForOrg(t, p, "run-out", orgID, "team-out", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("anthropic key = %q for a team outside the audience — the org tier lent a credential nobody granted", got)
	}
	if b.OrgSourced["anthropic"] {
		t.Errorf("OrgSourced = %v, want empty for a team outside the audience", b.OrgSourced)
	}
}

// The named team gets the org key, and the bundle records the provenance —
// without which the runner would meter one subscription once per borrowing
// team and no reading would ever accumulate.
func TestOrgTier_teamInsideTheAudienceSpendsTheOrgKey(t *testing.T) {
	const orgID = "org-1"
	p := orgTierPublisher(t, orgID, identity.CredentialAudience{Teams: []string{"team-in"}})

	b := resolveBundleForOrg(t, p, "run-in", orgID, "team-in", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-org-shared" {
		t.Fatalf("anthropic key = %q, want the org's shared key — the tier is not wired", got)
	}
	if !b.OrgSourced["anthropic"] {
		t.Errorf("OrgSourced = %v, missing \"anthropic\" — the usage cap would meter this per team", b.OrgSourced)
	}
	if b.PlatformSourced["anthropic"] || b.PoolSourced["anthropic"] {
		t.Errorf("an org credential is tagged as another tier: platform=%v pool=%v", b.PlatformSourced, b.PoolSourced)
	}
}

// all_teams admits a team the Teams list never names — including one
// created after the audience was set, which is the setting's reason to
// exist.
func TestOrgTier_allTeamsAdmitsAnUnnamedTeam(t *testing.T) {
	const orgID = "org-1"
	p := orgTierPublisher(t, orgID, identity.CredentialAudience{AllTeams: true})

	b := resolveBundleForOrg(t, p, "run-any", orgID, "team-out", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-org-shared" {
		t.Fatalf("anthropic key = %q under all_teams, want the org's shared key", got)
	}
}

// A team that brought its OWN key spends it. The org tier fills per wire
// family, so it must not land beside the team's credential — the delegates
// rank a ctx API key above a ctx OAuth dir on one wire, and a second key on
// the same wire would silently decide which one serves every call.
func TestOrgTier_neverShadowsTheTeamsOwnCredential(t *testing.T) {
	const orgID = "org-1"
	p := orgTierPublisher(t, orgID, identity.CredentialAudience{AllTeams: true})
	seedKey(t, p.apiKeys, p.sealer, "team-in", secrets.ProviderAnthropic, "sk-ant-team-own")

	b := resolveBundleForOrg(t, p, "run-own", orgID, "team-in", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-team-own" {
		t.Fatalf("anthropic key = %q, want the TEAM's own key — the org tier shadowed it", got)
	}
	if b.OrgSourced["anthropic"] {
		t.Errorf("OrgSourced = %v — the team's own key was tagged as org-sourced, which would meter its spend on the org", b.OrgSourced)
	}
}

// An unreadable org document must NOT admit the team. A tier that opened on
// a transient Mongo error would spend an org's subscription on a team its
// admins never named — and a tier below (pool, platform, the pod env) can
// still fund the run, so the cost of failing closed is a fallback while the
// cost of failing open is someone else's money.
func TestOrgTier_unreadableOrgFailsClosed(t *testing.T) {
	const orgID = "org-1"
	p := orgTierPublisher(t, orgID, identity.CredentialAudience{AllTeams: true})
	p.identity.(*fakeTeamResolver).orgErr = errors.New("mongo: connection reset")

	b := resolveBundleForOrg(t, p, "run-degraded", orgID, "team-in", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("anthropic key = %q while the org document was unreadable — the audience gate failed OPEN", got)
	}
}

// A run with no org id at all (local-mode team, pre-backfill row) never
// reaches the tier: there is no org whose audience could admit it.
func TestOrgTier_noOrgIDSkipsTheTier(t *testing.T) {
	p := orgTierPublisher(t, "org-1", identity.CredentialAudience{AllTeams: true})

	b := resolveBundleForOrg(t, p, "run-orgless", "", "team-in", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("anthropic key = %q for an org-less run — the tier resolved without an org", got)
	}
}
