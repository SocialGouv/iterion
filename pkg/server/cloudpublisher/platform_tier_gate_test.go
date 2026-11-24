package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// platformTierTeams is the identity every gate test reads: team-in and the
// two blocked postures are named by the enforced audience below, team-org is
// admitted through its org, team-out is in no list.
func platformTierTeams() *fakeTeamResolver {
	return &fakeTeamResolver{
		orgs: map[string]string{
			"team-in": "org-1", "team-org": "org-in", "team-out": "org-1",
			"team-sovereign": "org-1", "team-pool": "org-1",
		},
		llmFallback: map[string]string{"team-sovereign": identity.LLMFallbackNone},
		runnerPool:  map[string]string{"team-pool": "pool-a"},
	}
}

func enforcedPlatformAudience() *platformcfg.PlatformCredentials {
	enforce := true
	return &platformcfg.PlatformCredentials{
		Enforce: &enforce,
		Teams:   []string{"team-in", "team-sovereign", "team-pool"},
		Orgs:    []string{"org-in"},
	}
}

func TestPlatformTierConsulted(t *testing.T) {
	unenforced := &platformcfg.PlatformCredentials{Teams: []string{"team-in"}}
	cases := []struct {
		name     string
		team     string
		ids      *fakeTeamResolver
		audience *platformcfg.Resolver[platformcfg.PlatformCredentials]
		want     bool
		wantErr  bool
	}{
		{name: "admitted team", team: "team-in", ids: platformTierTeams(), audience: audienceResolver(enforcedPlatformAudience(), nil), want: true},
		{name: "team admitted through its org", team: "team-org", ids: platformTierTeams(), audience: audienceResolver(enforcedPlatformAudience(), nil), want: true},
		{name: "no audience resolver", team: "team-out", ids: platformTierTeams(), audience: nil, want: true},
		{name: "outside an enforced audience", team: "team-out", ids: platformTierTeams(), audience: audienceResolver(enforcedPlatformAudience(), nil), want: false},
		{name: "sovereign team the audience names", team: "team-sovereign", ids: platformTierTeams(), audience: audienceResolver(enforcedPlatformAudience(), nil), want: false},
		{name: "runner-pool team the audience names", team: "team-pool", ids: platformTierTeams(), audience: audienceResolver(enforcedPlatformAudience(), nil), want: false},
		{name: "no team row under an unenforced record", team: "team-ghost", ids: platformTierTeams(), audience: audienceResolver(unenforced, nil), want: true},
		{name: "empty team id", team: "", ids: platformTierTeams(), audience: nil, want: false},
		{name: "team store error", team: "team-in", ids: &fakeTeamResolver{err: errors.New("mongo: connection reset")}, audience: nil, wantErr: true},
		{name: "invalid llm_fallback", team: "team-bad", ids: &fakeTeamResolver{orgs: map[string]string{"team-bad": "org-1"}, llmFallback: map[string]string{"team-bad": "sometimes"}}, audience: nil, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Publisher{identity: tc.ids, platformAudience: tc.audience, logger: testLogger()}
			got, err := p.PlatformTierConsulted(context.Background(), tc.team)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("PlatformTierConsulted(%q) = %v, nil — an unknown team posture must be an error, never an answer", tc.team, got)
				}
				if got {
					t.Errorf("PlatformTierConsulted(%q) answered consulted alongside its error", tc.team)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlatformTierConsulted(%q): %v", tc.team, err)
			}
			if got != tc.want {
				t.Errorf("PlatformTierConsulted(%q) = %v, want %v", tc.team, got, tc.want)
			}
		})
	}
}

// The catalog's answer and the walk's seal agree: for each posture, the
// bundle a trusted launch seals carries the platform key exactly when
// PlatformTierConsulted says the tier is consulted. Each team holds an
// openai key of its own so the sovereign launch is funded and the anthropic
// wire stays free for the platform key.
func TestPlatformTierConsulted_MatchesTheWalksSeal(t *testing.T) {
	for _, team := range []string{"team-in", "team-org", "team-out", "team-sovereign", "team-pool"} {
		t.Run(team, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
			seedKey(t, keys, sealer, team, secrets.ProviderOpenAI, "sk-openai-"+team)
			runSecrets := secrets.NewMemoryRunSecretsStore()
			p := &Publisher{
				apiKeys: keys, runSecrets: runSecrets, sealer: sealer, logger: testLogger(),
				identity:         platformTierTeams(),
				platformAudience: audienceResolver(enforcedPlatformAudience(), nil),
			}
			ctx := store.WithTenant(context.Background(), team)

			consulted, err := p.PlatformTierConsulted(ctx, team)
			if err != nil {
				t.Fatalf("PlatformTierConsulted: %v", err)
			}
			pool, err := p.runnerPoolForTeam(ctx, team)
			if err != nil {
				t.Fatalf("runnerPoolForTeam: %v", err)
			}
			creds, err := p.resolveAndSealCredentials(ctx, "run-"+team, p.orgIDForTeam(ctx, team), team, "owner", "", pool, nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if creds.secretsRef == "" {
				t.Fatalf("no bundle sealed — the team's own openai key should fund it")
			}
			rec, err := runSecrets.Get(ctx, creds.secretsRef)
			if err != nil {
				t.Fatalf("RunSecrets.Get: %v", err)
			}
			bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, pool, "run-"+team, rec.KeyID, rec.SealedBundle)
			if err != nil {
				t.Fatalf("OpenRunBundle: %v", err)
			}
			sealed := bundle.APIKeys[secrets.ProviderAnthropic] == "sk-ant-platform"
			if sealed != consulted {
				t.Fatalf("walk sealed the platform key = %v, PlatformTierConsulted = %v — the catalog's gate and the walk disagree", sealed, consulted)
			}
		})
	}
}

// flakyNthGetTeam fails the team read numbered failOn (0-based) only and
// answers every other read from the embedded resolver.
type flakyNthGetTeam struct {
	*fakeTeamResolver
	failOn int
	n      *int
}

func (f flakyNthGetTeam) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	k := *f.n
	*f.n++
	if k == f.failOn {
		return identity.Team{}, errors.New("mongo: connection reset")
	}
	return f.fakeTeamResolver.GetTeam(ctx, id)
}

// Either posture read failing alone is an error, whatever order the gate
// reads them in: the other read succeeding never turns an unknown runner
// pool or LLM fallback policy into "consulted".
func TestPlatformTierConsulted_EachReadErrorAlone(t *testing.T) {
	for _, failOn := range []int{0, 1} {
		n := 0
		p := &Publisher{identity: flakyNthGetTeam{platformTierTeams(), failOn, &n}, logger: testLogger()}
		got, err := p.PlatformTierConsulted(context.Background(), "team-pool")
		if err == nil || got {
			t.Errorf("failOn=%d: PlatformTierConsulted(team-pool) = %v, %v — want an error", failOn, got, err)
		}
	}
}
