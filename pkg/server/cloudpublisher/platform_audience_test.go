package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The PLATFORM credential audience. Its default direction is the opposite
// of the org tier's, deliberately: the org tier lends a key someone chose
// to lend, so silence must not lend it; the platform tier is what the
// deployment already runs on, so silence must not take it away.

func audienceResolver(rec *platformcfg.PlatformCredentials, err error) *platformcfg.Resolver[platformcfg.PlatformCredentials] {
	return platformcfg.NewResolverFunc(func(context.Context) (*platformcfg.PlatformCredentials, error) {
		return rec, err
	}, nil)
}

// platformAudiencePublisher builds a publisher holding one platform key.
func platformAudiencePublisher(t *testing.T, audience *platformcfg.Resolver[platformcfg.PlatformCredentials]) *Publisher {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	return &Publisher{
		apiKeys:          keys,
		runSecrets:       secrets.NewMemoryRunSecretsStore(),
		sealer:           sealer,
		logger:           testLogger(),
		platformAudience: audience,
	}
}

// No record at all: every tenant keeps drawing on the platform tier. This
// is the migration property — a deployment that never writes the record
// behaves byte-identically to before the family existed.
func TestPlatformAudience_absentRecordAdmitsEveryone(t *testing.T) {
	p := platformAudiencePublisher(t, audienceResolver(nil, nil))

	b := resolveBundleForOrg(t, p, "run-1", "org-1", "team-x", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-platform" {
		t.Fatalf("anthropic key = %q with no audience record — the tier stopped serving a deployment that opted into nothing", got)
	}
}

// A record that names teams but does NOT enforce still admits everyone.
// Naming a team must not, by itself, cut every other tenant off from the
// deployment's only credential — that would be discovered as a fleet of
// 401s, one write after an innocent-looking edit.
func TestPlatformAudience_namingTeamsWithoutEnforcingChangesNothing(t *testing.T) {
	p := platformAudiencePublisher(t, audienceResolver(&platformcfg.PlatformCredentials{
		Teams: []string{"team-allowed"},
	}, nil))

	b := resolveBundleForOrg(t, p, "run-2", "org-1", "team-other", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-platform" {
		t.Fatalf("anthropic key = %q — listing a team silently locked the others out", got)
	}
}

// Enforcement on: only the named tenants draw.
func TestPlatformAudience_enforcedAdmitsOnlyTheNamed(t *testing.T) {
	enforce := true
	rec := &platformcfg.PlatformCredentials{Enforce: &enforce, Teams: []string{"team-allowed"}}

	p := platformAudiencePublisher(t, audienceResolver(rec, nil))
	if got := resolveBundleForOrg(t, p, "run-in", "org-1", "team-allowed", "webhook:cfg").APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-platform" {
		t.Fatalf("named team got %q, want the platform key", got)
	}

	p2 := platformAudiencePublisher(t, audienceResolver(rec, nil))
	if got := resolveBundleForOrg(t, p2, "run-out", "org-1", "team-other", "webhook:cfg").APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("unnamed team got %q — the enforced audience did not gate", got)
	}
}

// Orgs are the grain an operator actually governs at: a team is created
// inside an org without asking the platform, so admitting the org must
// admit teams the list never names.
func TestPlatformAudience_orgAdmitsItsUnnamedTeams(t *testing.T) {
	enforce := true
	p := platformAudiencePublisher(t, audienceResolver(&platformcfg.PlatformCredentials{
		Enforce: &enforce, Orgs: []string{"org-1"},
	}, nil))

	if got := resolveBundleForOrg(t, p, "run-org", "org-1", "team-brand-new", "webhook:cfg").APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-platform" {
		t.Fatalf("a team of an admitted org got %q, want the platform key", got)
	}
}

// An unreadable settings record must NOT take the credential away. Failing
// closed here would turn a settings blip into a fleet-wide outage — the
// exact failure mode the probes doc warns about for critical checks on a
// shared backend. This is the inverse of the ORG tier's rule, and the
// asymmetry is the design.
func TestPlatformAudience_unreadableRecordKeepsServing(t *testing.T) {
	p := platformAudiencePublisher(t, audienceResolver(nil, errors.New("mongo: connection reset")))

	b := resolveBundleForOrg(t, p, "run-degraded", "org-1", "team-x", "webhook:cfg")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-platform" {
		t.Fatalf("anthropic key = %q on a degraded settings read — a config blip became a fleet-wide credential outage", got)
	}
}

// Validate is what keeps an operator from enforcing an audience that names
// nobody: reachable by accident, and its symptom is every credential-less
// run failing at its first LLM call.
func TestPlatformAudience_refusesEnforcingAnEmptyAudience(t *testing.T) {
	enforce := true
	if err := (platformcfg.PlatformCredentials{Enforce: &enforce}).Validate(); err == nil {
		t.Fatal("enforcing an audience with no team and no org was accepted — it refuses every credential-less run")
	}
	if err := (platformcfg.PlatformCredentials{Enforce: &enforce, Orgs: []string{"org-1"}}).Validate(); err != nil {
		t.Fatalf("a named org was refused: %v", err)
	}
}
