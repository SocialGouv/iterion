package cloudpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

type previewOnlyKeys struct {
	secrets.ApiKeyStore
	inner secrets.ApiKeyStore
}

func (s previewOnlyKeys) ListByTeam(c context.Context, t, u string) ([]secrets.ApiKey, error) {
	return s.inner.ListByTeam(c, t, u)
}

type previewOnlyOAuth struct {
	secrets.OAuthStore
	inner secrets.OAuthStore
}

func (s previewOnlyOAuth) ListByUser(c context.Context, u string) ([]secrets.OAuthRecord, error) {
	return s.inner.ListByUser(c, u)
}

type previewOnlyUsage struct {
	usagecap.Store
	inner usagecap.Store
}

func (s previewOnlyUsage) Latest(c context.Context, k string) ([]usagecap.Reading, error) {
	return s.inner.Latest(c, k)
}

type previewOnlyRuns struct {
	store.RunStore
	inner store.RunStore
}

func (s previewOnlyRuns) CountAliveRunsWithCredFingerprint(c context.Context, fp, run string) (int, error) {
	return s.inner.CountAliveRunsWithCredFingerprint(c, fp, run)
}

type unopenedPreviewSealer struct{ secrets.Sealer }
type unwrittenPreviewBundle struct{ secrets.RunSecretsStore }

func previewReadOnly(p *Publisher) *Publisher {
	q := &Publisher{runSecrets: unwrittenPreviewBundle{}, sealer: unopenedPreviewSealer{}, identity: p.identity, logger: p.logger, credPool: p.credPool, usageProbe: p.usageProbe, capPolicy: p.capPolicy, trust: p.trust, platformAudience: p.platformAudience}
	if p.apiKeys != nil {
		q.apiKeys = previewOnlyKeys{inner: p.apiKeys}
	}
	if p.oauthForfait != nil {
		q.oauthForfait = previewOnlyOAuth{inner: p.oauthForfait}
	}
	if p.usageCaps != nil {
		q.usageCaps = previewOnlyUsage{inner: p.usageCaps}
	}
	if p.store != nil {
		q.store = previewOnlyRuns{inner: p.store}
	}
	return q
}
func previewSpec(team, owner string) runview.CredentialPreviewSpec {
	return runview.CredentialPreviewSpec{Context: runview.CredentialPreviewContext{TeamID: team, BotID: "docs-refresh", Source: runview.CredentialPreviewSource{Kind: "personal"}}, OwnerID: owner}
}
func selectedDescriptions(v runview.CredentialPreview) []string {
	var out []string
	for _, c := range v.Candidates {
		if c.Selected {
			out = append(out, c.Tier+":"+c.Source+":"+c.Provider)
		}
	}
	sort.Strings(out)
	return out
}
func bundleDescriptions(b secrets.RunBundle) []string {
	var out []string
	tier := func(slot string) string {
		if b.PoolSourced[slot] {
			return "pool"
		}
		if b.PlatformSourced[slot] {
			return "platform"
		}
		if b.OrgSourced[slot] {
			return "org"
		}
		return "team"
	}
	for provider := range b.APIKeys {
		out = append(out, tier(string(provider))+":api_key:"+string(provider))
	}
	for kind := range b.OAuthCredentials {
		out = append(out, tier(kind)+":oauth:"+kind)
	}
	sort.Strings(out)
	return out
}

func TestCredentialPreviewMatchesSealedBundleAcrossTiers(t *testing.T) {
	for _, scenario := range []string{"pool", "pool_suppressed_by_other_wire", "platform_same_wire", "org", "org_denied", "all_closed_restore", "pinned_blocked_key", "ranked_accounts"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 12, MaxConcurrentRuns: 3})
			p := f.pub
			p.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg, CredentialAudience: identity.CredentialAudience{Teams: []string{poolTeam}}}}}
			p.apiKeys = secrets.NewMemoryApiKeyStore()
			p.oauthForfait = secrets.NewMemoryOAuthStore()
			p.usageCaps = usagecap.NewMemStore()
			spec := previewSpec(poolTeam, "webhook:private")
			closeFP := func(scope, fp string) {
				t.Helper()
				if err := p.usageCaps.Record(t.Context(), usagecap.Key("claude_code", scope, fp), usagecap.Reading{Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, ObservedAt: time.Now(), ResetsAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "pool":
				seedOAuth(t, p.oauthForfait, p.sealer, secrets.PlatformOwnerKey, "sk-hidden-platform")
			case "pool_suppressed_by_other_wire":
				seedKey(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID(poolOrg), secrets.ProviderOpenAI, "sk-org-hidden")
				seedKey(t, p.apiKeys, p.sealer, poolTeam, secrets.ProviderOpenAI, "sk-own-openai")
				seedOAuth(t, p.oauthForfait, p.sealer, secrets.PlatformOwnerKey, "sk-platform")
			case "platform_same_wire":
				p.credPool = nil
				seedKey(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai")
				seedKey(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-anthropic")
			case "org", "org_denied":
				seedKey(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID(poolOrg), secrets.ProviderAnthropic, "sk-org-private")
				if scenario == "org_denied" {
					p.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}
				}
			case "all_closed_restore", "pinned_blocked_key":
				p.credPool = nil
				seedKeyFP(t, p.apiKeys, p.sealer, poolTeam, secrets.ProviderAnthropic, "sk-team-key", "closed-key")
				seedOAuth(t, p.oauthForfait, p.sealer, secrets.OrgOwnerKey(poolTeam), "sk-team-account")
				closeFP(usagecap.TenantScope(poolTeam), "closed-key")
				closeFP(usagecap.TenantScope(poolTeam), seededFP(secrets.OrgOwnerKey(poolTeam)))
				if scenario == "pinned_blocked_key" {
					keys, _ := p.apiKeys.ListByTeam(t.Context(), poolTeam, "")
					spec.Launch.KeyOverrides = map[string]string{"anthropic": keys[0].ID}
				}
			case "ranked_accounts":
				owner := secrets.OrgOwnerKey(poolTeam)
				seedOAuth(t, p.oauthForfait, p.sealer, owner, "sk-first")
				first, _ := p.oauthForfait.Get(t.Context(), owner, secrets.OAuthKindClaudeCode)
				second := first
				second.ID = ""
				second.Rank = 1
				second.Fingerprint = "rank-one"
				second.SealedPayload, _ = secrets.SealOAuthPayload(p.sealer, owner, second.Kind, []byte(`{"claudeAiOauth":{"accessToken":"sk-second"}}`))
				if err := p.oauthForfait.Upsert(t.Context(), second); err != nil {
					t.Fatal(err)
				}
				closeFP(usagecap.TenantScope(poolTeam), first.Fingerprint)
			}
			preview, err := previewReadOnly(p).PreviewCredentials(t.Context(), spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Live resolution is the independent oracle, with real sealed records and
			// real pool admission. Every metadata-only dependency above rejects writes.
			ctx := store.WithTenant(t.Context(), poolTeam)
			res, err := p.resolveAndSealCredentials(ctx, "oracle-run", poolOrg, poolTeam, spec.OwnerID, spec.Context.BotID, nil, spec.Launch.KeyOverrides, nil, model.ModelOverrides{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.rs.Get(ctx, res.secretsRef)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := secrets.OpenRunBundle(f.sealer, "oracle-run", record.SealedBundle)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(selectedDescriptions(preview), bundleDescriptions(bundle)) {
				t.Fatalf("preview=%v live=%v", selectedDescriptions(preview), bundleDescriptions(bundle))
			}
			if scenario == "ranked_accounts" {
				for _, c := range preview.Candidates {
					if c.Selected && c.Source == "oauth" && c.Rank != 1 {
						t.Fatalf("selected wrong OAuth rank: %+v", c)
					}
				}
				if bundle.OAuthFingerprints["claude_code"] != "rank-one" {
					t.Fatal("live rank oracle disagrees")
				}
			}
			if scenario == "all_closed_restore" {
				for _, c := range preview.Candidates {
					if c.Selected && c.State != "restored" {
						t.Fatalf("blocked-only candidate must say restored: %+v", c)
					}
				}
			}
			if scenario == "pool" {
				found := false
				for _, c := range preview.Candidates {
					if c.Tier == "platform" && !c.Selected && c.Selection == "not_consulted" {
						found = true
					}
				}
				if !found {
					t.Fatal("platform rescue hidden behind current pool grant")
				}
			}
			if scenario == "pool_suppressed_by_other_wire" {
				for _, tier := range []string{"org", "pool"} {
					found := false
					for _, c := range preview.Candidates {
						if c.Tier == tier && !c.Selected && c.Selection == "not_consulted" {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s rescue hidden behind existing key", tier)
					}
				}
			}
			if scenario == "pool_suppressed_by_other_wire" && preview.Pool.Considered {
				t.Fatal("pool consulted despite existing OpenAI key")
			}
			if scenario == "pinned_blocked_key" {
				found := false
				for _, c := range preview.Candidates {
					found = found || c.Selected && c.Pinned && c.State == "blocked"
				}
				if !found {
					t.Fatal("explicit blocked pin was not honored")
				}
			}
			data, err := json.Marshal(preview)
			if err != nil {
				t.Fatal(err)
			}
			for _, hidden := range []string{"sk-", "fingerprint", "webhook:private", "donor", "sealed_payload"} {
				if strings.Contains(string(data), hidden) {
					t.Fatalf("JSON leaks private field/value %q", hidden)
				}
			}
		})
	}
}

func TestCredentialPreviewNewAccountIsUnknownAndNeverProbes(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f, "sk-account")
	owner := secrets.OrgOwnerKey(poolTeam)
	rec, _ := f.pub.oauthForfait.Get(t.Context(), owner, secrets.OAuthKindClaudeCode)
	rec.Fingerprint = (secrets.OAuthAccount{ID: "c7f30bce-7e8b-4bb7-8199-afc172f3d580", OrganizationID: "0d05ce64-6b67-4368-909c-0d26f5a5870a"}).Fingerprint()
	if err := f.pub.oauthForfait.Upsert(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	f.pub.usageCaps = usagecap.NewMemStore()
	f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) { panic("preview probed provider") }
	out, err := previewReadOnly(f.pub).PreviewCredentials(t.Context(), previewSpec(poolTeam, "requester"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range out.Candidates {
		if c.Selected && c.Source == "oauth" {
			found = true
			if c.State != "probe_required" || !c.Conditional || c.AccountGroup == "" || len(c.Windows) > 0 {
				t.Fatalf("unmeasured account pretends capacity: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("new account absent")
	}
}

// A missing concurrency reading cannot erase a known provider refusal. Both
// observations explain selection independently, and errors never leak details.
type previewCountStore struct {
	store.RunStore
	n   int
	err error
}

func (s previewCountStore) CountAliveRunsWithCredFingerprint(context.Context, string, string) (int, error) {
	return s.n, s.err
}
func TestCredentialPreviewCapacityDoesNotHideKnownWindow(t *testing.T) {
	for _, countError := range []bool{false, true} {
		t.Run(fmtBool(countError), func(t *testing.T) {
			f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
			withTenantForfait(t, f, "sk-healthy-account")
			f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
			seedKeyFP(t, f.pub.apiKeys, f.sealer, poolTeam, secrets.ProviderAnthropic, "sk-own-key", "fp-key")
			keys, _ := f.pub.apiKeys.ListByTeam(t.Context(), poolTeam, "")
			k := keys[0]
			k.MaxConcurrentRuns = 1
			if err := f.pub.apiKeys.Update(t.Context(), k); err != nil {
				t.Fatal(err)
			}
			count := previewCountStore{n: 1}
			if countError {
				count.err = errors.New("private DB diagnostics")
			}
			f.pub.store = count
			f.pub.usageCaps = usagecap.NewMemStore()
			if countError {
				if err := f.pub.usageCaps.Record(t.Context(), usagecap.Key("claude_code", usagecap.TenantScope(poolTeam), k.Fingerprint), usagecap.Reading{Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, ObservedAt: time.Now(), ResetsAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := previewReadOnly(f.pub).PreviewCredentials(t.Context(), previewSpec(poolTeam, "requester"), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range out.Candidates {
				if c.Tier == "team" && c.Source == "api_key" {
					want := "at_capacity"
					if countError {
						want = "blocked"
					}
					if c.State != want || c.Selected {
						t.Fatalf("known blocking evidence hidden: %+v", c)
					}
					if strings.Contains(c.Reason, "private") {
						t.Fatal("raw count error leaked")
					}
					if countError && (len(c.Windows) != 1 || c.Windows[0].Percent != nil) {
						t.Fatal("unmeasured rejected quota became zero percent")
					}
				}
			}
			actual, _ := f.resolve(t, "capacity-oracle", nil)
			if len(actual.APIKeys) != 0 || len(actual.OAuthCredentials["claude_code"]) == 0 {
				t.Fatal("live selection disagrees with preview")
			}
		})
	}
}
func fmtBool(v bool) string {
	if v {
		return "count_unknown_window_blocked"
	}
	return "at_capacity"
}
