package cloudpublisher

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// A shared tier holding a metered key AND a forfait on ONE wire family hands
// the family to the forfait. The first credential to fill a family is what
// every unpinned node of the run spends: filling keys first would let a
// platform z.ai key capture the anthropic wire of every team without a
// credential of its own and serve their claude nodes GLM, the platform's
// Claude forfait never sealed.

// sharedTierFixture seeds one z.ai key under keyScope and one claude_code
// forfait under forfaitOwner — the platform's or an org's stores.
func sharedTierFixture(t *testing.T, keyScope, forfaitOwner string) *Publisher {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, keyScope, secrets.ProviderZAI, "sk-zai-shared")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, forfaitOwner, "sk-ant-shared-forfait")
	return &Publisher{
		apiKeys:      keys,
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
	}
}

func assertForfaitHoldsTheWire(t *testing.T, tier string, b secrets.RunBundle) {
	t.Helper()
	if !contains(string(b.OAuthCredentials["claude_code"]), "sk-ant-shared-forfait") {
		t.Errorf("%s: claude_code = %q, want the %s forfait — a metered key filled first and the forfait was never sealed",
			tier, b.OAuthCredentials["claude_code"], tier)
	}
	if got, present := b.APIKeys[secrets.ProviderZAI]; present {
		t.Errorf("%s: APIKeys[zai] = %q — the z.ai key took the anthropic wire as the run's DEFAULT, so every unpinned claude node would spend GLM",
			tier, got)
	}
}

func TestPlatformTier_forfaitHoldsTheWireOverAMeteredKey(t *testing.T) {
	p := sharedTierFixture(t, secrets.PlatformTenantID, secrets.PlatformOwnerKey)
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundle(t, p, rs, p.sealer, "run-1", "team1", "webhook:cfg-1")

	assertForfaitHoldsTheWire(t, "platform", b)
	if got, present := b.PinnedAPIKeys[secrets.ProviderZAI]; present {
		t.Errorf("PinnedAPIKeys[zai] = %q — nothing pins zai, so nothing may fund it", got)
	}
}

// A route that NAMES zai (a GLM fallback) still gets the key — beside the
// forfait, where no default-precedence reader looks.
func TestPlatformTier_pinnedFacadeRouteKeepsItsKeyBesideTheForfait(t *testing.T) {
	p := sharedTierFixture(t, secrets.PlatformTenantID, secrets.PlatformOwnerKey)
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, p.sealer, "run-1", "team1", "webhook:cfg-1", []string{"zai"})

	assertForfaitHoldsTheWire(t, "platform", b)
	if got := b.PinnedAPIKeys[secrets.ProviderZAI]; got != "sk-zai-shared" {
		t.Errorf("PinnedAPIKeys[zai] = %q, want the platform key — the route that names zai has no credential", got)
	}
}

func TestOrgTier_forfaitHoldsTheWireOverAMeteredKey(t *testing.T) {
	const orgID = "org-1"
	p := sharedTierFixture(t, secrets.OrgTierTenantID(orgID), secrets.OrgTierOwnerKey(orgID))
	p.identity = &fakeTeamResolver{
		orgs:    map[string]string{"team-in": orgID},
		orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
	}
	b := resolveBundleForOrg(t, p, "run-in", orgID, "team-in", "webhook:cfg")

	assertForfaitHoldsTheWire(t, "org", b)
	if !b.OrgSourced["claude_code"] {
		t.Errorf("OrgSourced = %v, missing \"claude_code\" — the forfait did not come from the org tier", b.OrgSourced)
	}
}

// listFailingOAuthStore is a forfait store whose read fails — a degraded
// Mongo, an unreachable replica.
type listFailingOAuthStore struct{ secrets.OAuthStore }

func (listFailingOAuthStore) ListByUser(context.Context, string) ([]secrets.OAuthRecord, error) {
	return nil, context.DeadlineExceeded
}

// Forfaits are read FIRST, and that read giving up must end the forfait pass
// only: an early return there that also skipped the keys would leave a run
// with no credential on a wire a key could have served.
func TestPlatformTier_forfaitStoreFailureStillFillsTheKeys(t *testing.T) {
	p := sharedTierFixture(t, secrets.PlatformTenantID, secrets.PlatformOwnerKey)
	p.oauthForfait = listFailingOAuthStore{p.oauthForfait}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundle(t, p, rs, p.sealer, "run-1", "team1", "webhook:cfg-1")

	if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-shared" {
		t.Errorf("APIKeys[zai] = %q, want the platform key — a failed forfait read cost the run every platform key", got)
	}
}

// Both instruments of one wire closed: the run parks either way, and the
// restore hands back the one the fill order puts first — the forfait by
// default, the key when the deployment puts keys first — so the park and its
// retry key on the credential a healthy launch would have used. Under
// facade_default=always, where the z.ai key is eligible for the default at
// all (the truth table below covers the other policies).
func TestPlatformTier_bothClosedRestoresInTheFillOrder(t *testing.T) {
	for _, keysFirst := range []bool{false, true} {
		sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
		if err != nil {
			t.Fatalf("sealer: %v", err)
		}
		keys := secrets.NewMemoryApiKeyStore()
		seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-shared", "fp-zai-platform")
		oauth := secrets.NewMemoryOAuthStore()
		seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-shared-forfait")
		recs, err := oauth.ListByUser(context.Background(), secrets.PlatformOwnerKey)
		if err != nil || len(recs) != 1 {
			t.Fatalf("seeded platform forfait unreadable: %v (%d records)", err, len(recs))
		}
		st := usagecap.NewMemStore()
		recordRefusal(t, st, usagecap.ScopePlatform, "fp-zai-platform")
		if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, recs[0].Fingerprint), usagecap.Reading{
			Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
			ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		kf, always := keysFirst, string(platformcfg.FacadeAlways)
		p := &Publisher{
			apiKeys:          keys,
			oauthForfait:     oauth,
			runSecrets:       secrets.NewMemoryRunSecretsStore(),
			sealer:           sealer,
			logger:           testLogger(),
			usageCaps:        st,
			platformAudience: audienceResolver(&platformcfg.PlatformCredentials{KeysFirst: &kf, FacadeDefault: &always}, nil),
		}
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")

		gotForfait := len(b.OAuthCredentials["claude_code"]) != 0
		gotKey := b.APIKeys[secrets.ProviderZAI] != ""
		if keysFirst && (!gotKey || gotForfait) {
			t.Errorf("keys_first: restored forfait=%v key=%v, want the key alone", gotForfait, gotKey)
		}
		if !keysFirst && (!gotForfait || gotKey) {
			t.Errorf("forfaits first: restored forfait=%v key=%v, want the forfait alone", gotForfait, gotKey)
		}
	}
}

// A pin on the forfait's OWN provider is funded beside it, pinned-only: claw
// bills a Claude forfait as extra usage (refused outright under
// ITERION_FORBID_SUBSCRIPTION_OAUTH) and pi has no bridge to it, so for their
// `anthropic/…` nodes the pinned key is the only credential that serves on
// its terms. The forfait keeps the family for the default precedence, and
// claude_code prefers it over the pinned key under an `anthropic` hint
// (delegate precedence), so the subscription's own work stays on it.
func TestSharedTier_aPinOnTheForfaitsOwnProviderIsFundedPinnedOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider secrets.Provider
		kind     secrets.OAuthKind
	}{
		{"anthropic beside a Claude forfait", secrets.ProviderAnthropic, secrets.OAuthKindClaudeCode},
		{"openai beside a codex forfait", secrets.ProviderOpenAI, secrets.OAuthKindCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, tc.provider, "sk-shared-"+string(tc.provider))
			oauth := secrets.NewMemoryOAuthStore()
			seedOAuthKind(t, oauth, sealer, secrets.PlatformOwnerKey, tc.kind)
			p := &Publisher{
				apiKeys:      keys,
				oauthForfait: oauth,
				runSecrets:   secrets.NewMemoryRunSecretsStore(),
				sealer:       sealer,
				logger:       testLogger(),
			}
			rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
			b := resolveBundlePinned(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1", []string{string(tc.provider)})

			if len(b.OAuthCredentials[string(tc.kind)]) == 0 {
				t.Fatalf("inert bench: the %s forfait was not sealed", tc.kind)
			}
			if got := b.PinnedAPIKeys[tc.provider]; got != "sk-shared-"+string(tc.provider) {
				t.Errorf("PinnedAPIKeys[%s] = %q — the %s node that pins it has no other credential it can spend on its terms", tc.provider, got, tc.provider)
			}
			if got, present := b.APIKeys[tc.provider]; present {
				t.Errorf("APIKeys[%s] = %q — the key took the family the forfait holds", tc.provider, got)
			}
		})
	}
}

// seedOAuthKind seals one forfait of the given kind under ownerKey, in the
// payload shape each kind's consumers read.
func seedOAuthKind(t *testing.T, st secrets.OAuthStore, sealer secrets.Sealer, ownerKey string, kind secrets.OAuthKind) {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-forfait"}}`)
	if kind == secrets.OAuthKindCodex {
		blob = []byte(`{"tokens":{"access_token":"sk-forfait","account_id":"acct"}}`)
	}
	sealed, err := secrets.SealOAuthPayload(sealer, ownerKey, kind, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID: ownerKey, Kind: kind, SealedPayload: sealed, Fingerprint: "fp-" + ownerKey + "-" + string(kind),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// resolveCreds runs the real resolution and returns what it reports beside
// the sealed bundle — skippedReopensAt among it.
func resolveCreds(t *testing.T, p *Publisher, orgID, tenant string, pinned []string) credResolution {
	t.Helper()
	ctx := store.WithTenant(context.Background(), tenant)
	creds, err := p.resolveAndSealCredentials(ctx, "run-shadow", orgID, tenant, "webhook:cfg", "", "", nil, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, pinned, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	return creds
}

// A forfait holding the wire beside a usable key of its own tier stamps NO
// reopening. The stamp is run-wide: an instant stamped "now" makes every
// usage-window park of the run retry at the floor — including parks the
// re-resolution cannot move (a ChatGPT forfait, which no launch-time window
// check closes; another wire; routes pinned to a provider the key cannot
// serve) — and five floor-rate wakes retire a run in half an hour. The key
// is shadowed, not refused: the launch skipped nothing.
func TestSharedTier_aShadowedKeyStampsNoReopening(t *testing.T) {
	t.Run("platform", func(t *testing.T) {
		p := sharedTierFixture(t, secrets.PlatformTenantID, secrets.PlatformOwnerKey)
		if got := resolveCreds(t, p, "", "team1", nil).skippedReopensAt; !got.IsZero() {
			t.Errorf("skippedReopensAt = %v — a key the forfait shadows was reported as passed over", got)
		}
	})
	t.Run("org", func(t *testing.T) {
		const orgID = "org-1"
		p := sharedTierFixture(t, secrets.OrgTierTenantID(orgID), secrets.OrgTierOwnerKey(orgID))
		p.identity = &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
		}
		if got := resolveCreds(t, p, orgID, "team-in", nil).skippedReopensAt; !got.IsZero() {
			t.Errorf("skippedReopensAt = %v — a key the forfait shadows was reported as passed over", got)
		}
	})
}

// The org tier reads its forfaits first too, and a failed read there must end
// the forfait pass only — the org's keys still fund the run.
func TestOrgTier_forfaitStoreFailureStillFillsTheKeys(t *testing.T) {
	const orgID = "org-1"
	p := sharedTierFixture(t, secrets.OrgTierTenantID(orgID), secrets.OrgTierOwnerKey(orgID))
	p.oauthForfait = listFailingOAuthStore{p.oauthForfait}
	p.identity = &fakeTeamResolver{
		orgs:    map[string]string{"team-in": orgID},
		orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
	}
	b := resolveBundleForOrg(t, p, "run-in", orgID, "team-in", "webhook:cfg")

	if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-shared" {
		t.Errorf("APIKeys[zai] = %q, want the org key — a failed org forfait read cost the run every org key", got)
	}
	if !b.OrgSourced[string(secrets.ProviderZAI)] {
		t.Errorf("OrgSourced = %v, missing zai — the key did not come from the org tier", b.OrgSourced)
	}
}

// The openai wire follows the same order: a ChatGPT forfait of the tier takes
// the family over the tier's metered openai key, on both shared tiers.
func TestSharedTier_codexForfaitHoldsTheOpenAIWire(t *testing.T) {
	const orgID = "org-1"
	for _, tc := range []struct {
		name            string
		keyScope, owner string
		orgID, tenant   string
	}{
		{"platform", secrets.PlatformTenantID, secrets.PlatformOwnerKey, "", "team1"},
		{"org", secrets.OrgTierTenantID(orgID), secrets.OrgTierOwnerKey(orgID), orgID, "team-in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, tc.keyScope, secrets.ProviderOpenAI, "sk-openai-shared")
			oauth := secrets.NewMemoryOAuthStore()
			seedOAuthKind(t, oauth, sealer, tc.owner, secrets.OAuthKindCodex)
			p := &Publisher{
				apiKeys:      keys,
				oauthForfait: oauth,
				runSecrets:   secrets.NewMemoryRunSecretsStore(),
				sealer:       sealer,
				logger:       testLogger(),
				identity: &fakeTeamResolver{
					orgs:    map[string]string{"team-in": orgID},
					orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
				},
			}
			b := resolveBundleForOrg(t, p, "run-openai", tc.orgID, tc.tenant, "webhook:cfg")

			if len(b.OAuthCredentials[string(secrets.OAuthKindCodex)]) == 0 {
				t.Errorf("%s: the codex forfait was not sealed — the metered openai key took its wire", tc.name)
			}
			if got, present := b.APIKeys[secrets.ProviderOpenAI]; present {
				t.Errorf("%s: APIKeys[openai] = %q beside the codex forfait — every unpinned openai node would spend the key", tc.name, got)
			}
		})
	}
}

// The restore hands back, per wire family, the credential a healthy launch
// would have used: tier by tier as the walk visits them, the tenant's own
// first. Restoring every forfait before every key put a shared tier's closed
// forfait over the tenant's own refused key; every key before every forfait
// put a platform key over the tenant's own closed forfait.
func TestRestore_TheTenantsOwnCredentialComesBackFirst(t *testing.T) {
	closedFor48h := func(t *testing.T, st usagecap.Store, scope, fp string) {
		t.Helper()
		if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, scope, fp), usagecap.Reading{
			Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
			ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	bench := func(t *testing.T) (*Publisher, secrets.ApiKeyStore, secrets.OAuthStore, *usagecap.MemStore) {
		t.Helper()
		sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
		if err != nil {
			t.Fatalf("sealer: %v", err)
		}
		keys := secrets.NewMemoryApiKeyStore()
		oauth := secrets.NewMemoryOAuthStore()
		st := usagecap.NewMemStore()
		return &Publisher{
			apiKeys:      keys,
			oauthForfait: oauth,
			runSecrets:   secrets.NewMemoryRunSecretsStore(),
			sealer:       sealer,
			logger:       testLogger(),
			usageCaps:    st,
		}, keys, oauth, st
	}

	t.Run("the tenant's refused key over a closed platform forfait", func(t *testing.T) {
		p, keys, oauth, st := bench(t)
		seedKeyFP(t, keys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai-tenant", "fp-zai-tenant")
		recordRefusal(t, st, usagecap.TenantScope("team1"), "fp-zai-tenant")
		seedOAuth(t, oauth, p.sealer, secrets.PlatformOwnerKey, "sk-ant-platform-forfait")
		closedFor48h(t, st, usagecap.ScopePlatform, seededFP(secrets.PlatformOwnerKey))
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		b := resolveBundle(t, p, rs, p.sealer, "run-1", "team1", "webhook:cfg-1")

		if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-tenant" {
			t.Errorf("APIKeys[zai] = %q, want the tenant's own key restored", got)
		}
		if len(b.OAuthCredentials["claude_code"]) != 0 {
			t.Errorf("the platform's closed forfait was restored over the tenant's own refused key")
		}
	})

	t.Run("the tenant's closed forfait over a refused platform key", func(t *testing.T) {
		p, keys, oauth, st := bench(t)
		seedOAuth(t, oauth, p.sealer, secrets.OrgOwnerKey("team1"), "sk-ant-team-forfait")
		closedFor48h(t, st, usagecap.TenantScope("team1"), seededFP(secrets.OrgOwnerKey("team1")))
		seedKeyFP(t, keys, p.sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-zai-platform")
		recordRefusal(t, st, usagecap.ScopePlatform, "fp-zai-platform")
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		b := resolveBundle(t, p, rs, p.sealer, "run-1", "team1", "webhook:cfg-1")

		if !contains(string(b.OAuthCredentials["claude_code"]), "sk-ant-team-forfait") {
			t.Errorf("claude_code = %q, want the tenant's own forfait restored", b.OAuthCredentials["claude_code"])
		}
		if got, present := b.APIKeys[secrets.ProviderZAI]; present {
			t.Errorf("APIKeys[zai] = %q — the platform's refused key was restored over the tenant's own forfait", got)
		}
	})
}

// keys_first puts a tier's key before its forfait on one wire family — for a
// key of the wire's own vendor. An Anthropic key then holds the anthropic wire
// and the Claude forfait is its backstop.
func TestPlatformTier_keysFirstPutsTheSameVendorKeyOnTheWire(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-shared")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-shared-forfait")
	for _, keysFirst := range []bool{false, true} {
		kf := keysFirst
		p := &Publisher{
			apiKeys:          keys,
			oauthForfait:     oauth,
			runSecrets:       secrets.NewMemoryRunSecretsStore(),
			sealer:           sealer,
			logger:           testLogger(),
			platformAudience: audienceResolver(&platformcfg.PlatformCredentials{KeysFirst: &kf}, nil),
		}
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")
		gotKey := b.APIKeys[secrets.ProviderAnthropic] == "sk-ant-shared"
		gotForfait := len(b.OAuthCredentials["claude_code"]) != 0
		if keysFirst != gotKey || keysFirst == gotForfait {
			t.Errorf("keys_first=%v: anthropic key on the wire=%v, forfait sealed=%v", keysFirst, gotKey, gotForfait)
		}
	}
}

// The facade policy decides whether a z.ai key may answer the anthropic wire
// for nodes that never asked for it — a claude id served GLM — across the
// fill order, the forfait's window and the route's pin. Every row is a
// resolution through the real publisher.
func TestSharedTier_facadePolicyTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keysFirst     bool
		facade        platformcfg.FacadePolicy
		forfait       string // "open" | "closed" | "none"
		pinZAI        bool
		wantForfait   bool // the Claude forfait is sealed (live or restored)
		wantZAIDef    bool // z.ai key in the default channel
		wantZAIPinned bool // z.ai key in the pinned channel
	}{
		{"auto, forfait open", false, platformcfg.FacadeAuto, "open", false, true, false, false},
		{"auto, forfait open, zai pinned", false, platformcfg.FacadeAuto, "open", true, true, false, true},
		{"auto, forfait closed: park on it", false, platformcfg.FacadeAuto, "closed", false, true, false, false},
		{"always, forfait closed: falls through", false, platformcfg.FacadeAlways, "closed", false, false, true, false},
		{"never, forfait closed: park on it", false, platformcfg.FacadeNever, "closed", false, true, false, false},
		{"keys_first + auto: the facade stays off", true, platformcfg.FacadeAuto, "open", false, true, false, false},
		{"keys_first + always: pre-change order", true, platformcfg.FacadeAlways, "open", false, false, true, false},
		{"keys_first + never, zai pinned", true, platformcfg.FacadeNever, "open", true, true, false, true},
		{"auto, no forfait: a z.ai-only tier keeps serving", false, platformcfg.FacadeAuto, "none", false, false, true, false},
		{"never, no forfait, zai pinned: sealed on a free family", false, platformcfg.FacadeNever, "none", true, false, false, true},
		{"never, no forfait, nothing pinned: nothing sealed", false, platformcfg.FacadeNever, "none", false, false, false, false},
		// `tier` is the per-tier rule: a tier holding a native credential —
		// open or closed — keeps its own facade key off the default, and a
		// tier holding none falls through. It answers for THIS tier only,
		// whatever another tier holds.
		{"tier, forfait open", false, platformcfg.FacadeTier, "open", false, true, false, false},
		{"tier, forfait open, zai pinned", false, platformcfg.FacadeTier, "open", true, true, false, true},
		{"tier, forfait closed: park on it", false, platformcfg.FacadeTier, "closed", false, true, false, false},
		{"tier, no forfait: falls through", false, platformcfg.FacadeTier, "none", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-shared")
			oauth := secrets.NewMemoryOAuthStore()
			st := usagecap.NewMemStore()
			if tc.forfait != "none" {
				seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-shared-forfait")
			}
			if tc.forfait == "closed" {
				if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, seededFP(secrets.PlatformOwnerKey)), usagecap.Reading{
					Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
					ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
				}); err != nil {
					t.Fatalf("record: %v", err)
				}
			}
			kf, fd := tc.keysFirst, string(tc.facade)
			p := &Publisher{
				apiKeys:          keys,
				oauthForfait:     oauth,
				runSecrets:       secrets.NewMemoryRunSecretsStore(),
				sealer:           sealer,
				logger:           testLogger(),
				usageCaps:        st,
				platformAudience: audienceResolver(&platformcfg.PlatformCredentials{KeysFirst: &kf, FacadeDefault: &fd}, nil),
			}
			var pins []string
			if tc.pinZAI {
				pins = []string{"zai"}
			}
			rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
			b := resolveBundlePinned(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1", pins)

			if got := len(b.OAuthCredentials["claude_code"]) != 0; got != tc.wantForfait {
				t.Errorf("forfait sealed = %v, want %v", got, tc.wantForfait)
			}
			if got := b.APIKeys[secrets.ProviderZAI] != ""; got != tc.wantZAIDef {
				t.Errorf("z.ai key as the wire's default = %v, want %v", got, tc.wantZAIDef)
			}
			if got := b.PinnedAPIKeys[secrets.ProviderZAI] != ""; got != tc.wantZAIPinned {
				t.Errorf("z.ai key for the routes naming zai = %v, want %v", got, tc.wantZAIPinned)
			}
		})
	}
}

// The restore is where "nothing else serves the wire" would bypass the facade
// policy: a refused z.ai key is remembered, and with keys first the restore
// visits it before the tier's closed forfait. Under `auto` it must not come
// back as the wire's default — the forfait does, and the run parks on it. It
// does come back for the routes that NAME zai, in either fill order: they
// park on its own refusal instead of losing their credential, where a GLM
// route left to the forfait asks api.anthropic.com for a model it does not
// serve — a failure no retry heals.
func TestRestore_theFacadePolicyHoldsThere(t *testing.T) {
	for _, keysFirst := range []bool{true, false} {
		for _, pinned := range [][]string{{"zai"}, nil} {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-shared", "fp-zai-platform")
			oauth := secrets.NewMemoryOAuthStore()
			seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-shared-forfait")
			st := usagecap.NewMemStore()
			recordRefusal(t, st, usagecap.ScopePlatform, "fp-zai-platform")
			if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, seededFP(secrets.PlatformOwnerKey)), usagecap.Reading{
				Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			kf, auto := keysFirst, string(platformcfg.FacadeAuto)
			p := &Publisher{
				apiKeys:          keys,
				oauthForfait:     oauth,
				runSecrets:       secrets.NewMemoryRunSecretsStore(),
				sealer:           sealer,
				logger:           testLogger(),
				usageCaps:        st,
				platformAudience: audienceResolver(&platformcfg.PlatformCredentials{KeysFirst: &kf, FacadeDefault: &auto}, nil),
			}
			rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
			b := resolveBundlePinned(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1", pinned)

			name := map[bool]string{true: "keys first", false: "forfaits first"}[keysFirst] + map[bool]string{true: ", zai pinned", false: ""}[pinned != nil]
			if got := b.APIKeys[secrets.ProviderZAI]; got != "" {
				t.Errorf("%s: the refused z.ai key came back as the anthropic wire's default under facade_default=auto", name)
			}
			if len(b.OAuthCredentials["claude_code"]) == 0 {
				t.Errorf("%s: the tier's closed forfait was not restored — the run has nothing to park on", name)
			}
			switch got := b.PinnedAPIKeys[secrets.ProviderZAI]; {
			case pinned != nil && got != "sk-zai-shared":
				t.Errorf("%s: the routes naming zai lost their refused key — they reach the forfait instead", name)
			case pinned == nil && got != "":
				t.Errorf("%s: a z.ai key came back for routes nobody declared", name)
			}
		}
	}
}

// A key sealed for the routes naming its provider is stamped — and so holds
// a slot of its concurrency ceiling — only when some route spends it. Beside
// the platform's Claude forfait, a claude_code route pinned to `anthropic`
// spends the forfait (its declared precedence), so the pinned key is never
// reached; a claw route on `anthropic/…` spends the key.
func TestStamp_aPinnedKeyNoRouteSpendsIsNotCounted(t *testing.T) {
	node := func(id, backend, provider, model string) ir.Node {
		return &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: model}}
	}
	for _, tc := range []struct {
		name      string
		wf        *ir.Workflow
		wantStamp bool
	}{
		{"claude_code pinned anthropic (the forfait serves it)",
			&ir.Workflow{Nodes: map[string]ir.Node{"review": node("review", "claude_code", "anthropic", "claude-opus-5-5")}}, false},
		{"claw anthropic/ (the key serves it)",
			&ir.Workflow{Nodes: map[string]ir.Node{"review": node("review", "claw", "", "anthropic/claude-opus-5-5")}}, true},
		{"both",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"review": node("review", "claude_code", "anthropic", "claude-opus-5-5"),
				"digest": node("digest", "claw", "", "anthropic/claude-opus-5-5"),
			}}, true},
		// A supervisor calls its model in process, where a key pinned for
		// its `anthropic/…` route comes before the forfait.
		{"claude_code pinned anthropic beside a supervisor (the supervisor spends the key)",
			&ir.Workflow{
				Nodes:       map[string]ir.Node{"review": node("review", "claude_code", "anthropic", "claude-opus-5-5")},
				Supervisors: []*ir.Supervisor{{Name: "coach", Model: "anthropic/claude-haiku-4-5"}},
			}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
			oauth := secrets.NewMemoryOAuthStore()
			seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform-forfait")
			rs := secrets.NewMemoryRunSecretsStore()
			p := &Publisher{apiKeys: keys, oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger()}
			pinned := derivePinnedProviders(tc.wf, model.ModelOverrides{}, nil)
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-stamp", "", "team1", "webhook:cfg", "", "", tc.wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, pinned, nil)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			rec, err := rs.Get(ctx, res.secretsRef)
			if err != nil {
				t.Fatalf("run secrets: %v", err)
			}
			b, err := secrets.OpenRunBundle(sealer, rec.TenantID, "", "run-stamp", rec.KeyID, rec.SealedBundle)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if b.PinnedAPIKeys[secrets.ProviderAnthropic] == "" {
				t.Fatalf("inert bench: the anthropic key was not sealed for the pinned route")
			}
			if got := slices.Contains(res.fingerprints, "fp-ant-platform"); got != tc.wantStamp {
				t.Errorf("pinned key stamped = %v, want %v (stamp %v)", got, tc.wantStamp, res.fingerprints)
			}
			if !slices.Contains(res.fingerprints, seededFP(secrets.PlatformOwnerKey)) {
				t.Errorf("the forfait every claude_code node spends is missing from the stamp %v", res.fingerprints)
			}
		})
	}
}

// The restore visits the tiers in the walk's order: an org's closed forfait
// comes back before the platform's refused key on the same wire, so the run
// parks on the credential a healthy launch of its team would have used. (Two
// credentials of ONE slot never compete here: the refused-credential memory
// keeps the first tier's.)
func TestRestore_theOrgsCredentialComesBackBeforeThePlatforms(t *testing.T) {
	const orgID = "org-1"
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.OrgTierOwnerKey(orgID), "sk-ant-org-forfait")
	st := usagecap.NewMemStore()
	recordRefusal(t, st, usagecap.ScopePlatform, "fp-ant-platform")
	if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.OrgScope(orgID), seededFP(secrets.OrgTierOwnerKey(orgID))), usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	p := &Publisher{
		apiKeys:      keys,
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
		usageCaps:    st,
		identity: &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
		},
	}
	b := resolveBundleForOrg(t, p, "run-in", orgID, "team-in", "webhook:cfg")

	if !contains(string(b.OAuthCredentials["claude_code"]), "sk-ant-org-forfait") || !b.OrgSourced["claude_code"] {
		t.Errorf("the org's closed forfait was not restored (org-sourced=%v)", b.OrgSourced["claude_code"])
	}
	if b.APIKeys[secrets.ProviderAnthropic] != "" {
		t.Errorf("the platform's refused key came back over the org's forfait — the tier the walk visits first")
	}
}

// A refused shared key does not come back over a key another tier already
// sealed for the same routes: the org's refused z.ai key would otherwise
// replace the platform's healthy one on the routes that name zai, and they
// would spend a key the provider is refusing.
func TestRestore_aRouteKeyAnotherTierFundedIsNotReplaced(t *testing.T) {
	const orgID = "org-1"
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.OrgTierTenantID(orgID), secrets.ProviderZAI, "sk-zai-org", "fp-zai-org")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-zai-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.OrgTierOwnerKey(orgID), "sk-ant-org-forfait")
	st := usagecap.NewMemStore()
	recordRefusal(t, st, usagecap.OrgScope(orgID), "fp-zai-org")
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{
		apiKeys: keys, oauthForfait: oauth, runSecrets: rs, sealer: sealer, logger: testLogger(), usageCaps: st,
		identity: &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
		},
	}
	ctx := store.WithTenant(context.Background(), "team-in")
	res, err := p.resolveAndSealCredentials(ctx, "run-in", orgID, "team-in", "webhook:cfg", "", "", nil, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, []string{"zai"}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	rec, err := rs.Get(ctx, res.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(sealer, rec.TenantID, "", "run-in", rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	if got := b.PinnedAPIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Errorf("the routes naming zai hold the platform key: %v — want it, not the org's refused key", got == "sk-zai-platform")
	}
}

// The tenant's own refused z.ai key does not displace a healthy key a shared
// tier sealed for the routes naming zai: restored as the default, it would be
// what every such route reads first, and a run whose routes all pin zai would
// park on the tenant's refusal beside a key that serves it. The platform's
// closed forfait comes back as the wire's park point instead.
func TestRestore_aSharedRouteKeyKeepsItsRoutesOverTheTenantsRefusedKey(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderZAI, "sk-zai-tenant", "fp-zai-tenant")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-zai-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform-forfait")
	st := usagecap.NewMemStore()
	recordRefusal(t, st, usagecap.TenantScope("team1"), "fp-zai-tenant")
	if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, seededFP(secrets.PlatformOwnerKey)), usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	auto := string(platformcfg.FacadeAuto)
	p := &Publisher{
		apiKeys: keys, oauthForfait: oauth, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger(), usageCaps: st,
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &auto}, nil),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	b := resolveBundlePinned(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1", []string{"zai"})

	if got := b.PinnedAPIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Errorf("the routes naming zai lost the platform's healthy key (hold it: %v)", got == "sk-zai-platform")
	}
	if b.APIKeys[secrets.ProviderZAI] != "" {
		t.Error("the tenant's refused key came back as the default — every route naming zai reads it first")
	}
	if !b.PlatformSourced[string(secrets.ProviderZAI)] {
		t.Error("the zai route key lost its platform mark — it would be metered on the tenant's ledger")
	}
	if len(b.OAuthCredentials["claude_code"]) == 0 {
		t.Error("the platform's closed forfait was not restored — the wire's unpinned nodes have nothing to park on")
	}
}

// wfPinningBesideDefault is wfPinning(provider) plus a claude_code node with
// no hint: a route that reads the anthropic wire's default precedence. Its
// model carries a prefix, so the walk still resolves every route (NarrowSafe)
// and the reader is what decides.
func wfPinningBesideDefault(provider string) *ir.Workflow {
	return wfPinningBeside(provider, &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "review"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Model: "anthropic/claude-opus-5"},
	})
}

func wfPinningBeside(provider string, n *ir.AgentNode) *ir.Workflow {
	wf := wfPinning(provider)
	wf.Nodes[n.ID] = n
	return wf
}

// restoreBench seeds the tenant's own key of a facade provider, refused,
// beside a healthy platform key of the same provider, under
// facade_default=never and with no Claude credential anywhere: the platform
// key is sealed for the routes naming the provider only, and nothing but the
// tenant's key could refill the anthropic family.
func restoreBench(t *testing.T, prov secrets.Provider) (*Publisher, *secrets.MemoryRunSecretsStore, secrets.Sealer) {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", prov, "sk-"+string(prov)+"-tenant", "fp-tenant")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, prov, "sk-"+string(prov)+"-platform", "fp-platform")
	st := usagecap.NewMemStore()
	recordRefusal(t, st, usagecap.TenantScope("team1"), "fp-tenant")
	never := string(platformcfg.FacadeNever)
	rs := secrets.NewMemoryRunSecretsStore()
	return &Publisher{
		apiKeys: keys, oauthForfait: secrets.NewMemoryOAuthStore(), runSecrets: rs, sealer: sealer, logger: testLogger(), usageCaps: st,
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &never}, nil),
	}, rs, sealer
}

func resolveBundleForWorkflow(t *testing.T, p *Publisher, rs *secrets.MemoryRunSecretsStore, sealer secrets.Sealer, wf *ir.Workflow) secrets.RunBundle {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "team1")
	res, err := p.resolveAndSealCredentials(ctx, "run-1", "", "team1", "webhook:cfg-1", "", "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, derivePinnedProviders(wf, model.ModelOverrides{}, nil), nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if res.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := rs.Get(ctx, res.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(sealer, rec.TenantID, "", "run-1", rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return b
}

// With nothing else to refill the family and a route that reads its default,
// the tenant's refused key is still the wire's last park point: it comes back
// as the default, over the shared key sealed for its provider's routes — which
// leaves with its marks, so the slot names ONE credential, metered on its
// owner's ledger. A wire left empty would fail that route on a no-credential
// error nothing retries, or spend the pod's ambient env.
func TestRestore_theTenantsOwnKeyIsTheLastParkPointOfAnEmptyWire(t *testing.T) {
	for name, wf := range map[string]*ir.Workflow{
		"an unhinted claude_code route": wfPinningBesideDefault("zai"),
		// A route the walk cannot resolve may read any slot.
		"an unresolved route": wfPinningBeside("zai", &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "review"},
			LLMFields: ir.LLMFields{Backend: "pi", Model: "claude-opus-5"},
		}),
	} {
		p, rs, sealer := restoreBench(t, secrets.ProviderZAI)
		b := resolveBundleForWorkflow(t, p, rs, sealer, wf)

		if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-tenant" {
			t.Errorf("%s: the tenant's own refused key is not the restored default (holds the tenant key: %v)", name, got == "sk-zai-tenant")
		}
		if _, present := b.PinnedAPIKeys[secrets.ProviderZAI]; present {
			t.Errorf("%s: the platform's route key stayed beside the tenant's default — two credentials in one slot", name)
		}
		if b.PlatformSourced[string(secrets.ProviderZAI)] {
			t.Errorf("%s: the zai slot still reads as the platform's — the tenant's key would be metered on the platform ledger", name)
		}
	}
}

// A run whose every route names zai reads no default: the last park point
// would serve nobody and park routes the platform's key serves. The tenant's
// refused key stays out.
func TestRestore_aRunWhoseRoutesAllNameTheProviderKeepsTheSharedKey(t *testing.T) {
	p, rs, sealer := restoreBench(t, secrets.ProviderZAI)
	b := resolveBundleForWorkflow(t, p, rs, sealer, wfPinning("zai"))

	if got := b.PinnedAPIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Errorf("the routes naming zai lost the platform's healthy key (hold it: %v)", got == "sk-zai-platform")
	}
	if b.APIKeys[secrets.ProviderZAI] != "" {
		t.Error("the tenant's refused key came back as the default for a run no route of which reads it")
	}
}

// The reader has to read THAT slot: a claw `anthropic/…` route reads the
// wire's Anthropic and z.ai keys, never a Moonshot one, so the tenant's
// refused Moonshot key stays out beside it.
func TestRestore_aReaderOfAnotherSlotDoesNotBringTheTenantsKeyBack(t *testing.T) {
	p, rs, sealer := restoreBench(t, secrets.ProviderMoonshot)
	b := resolveBundleForWorkflow(t, p, rs, sealer, wfPinningBeside("moonshot", &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "review"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"},
	}))

	if got := b.PinnedAPIKeys[secrets.ProviderMoonshot]; got != "sk-moonshot-platform" {
		t.Errorf("the routes naming moonshot lost the platform's healthy key (hold it: %v)", got == "sk-moonshot-platform")
	}
	if b.APIKeys[secrets.ProviderMoonshot] != "" {
		t.Error("the tenant's refused Moonshot key came back for a claw route that never reads it")
	}
}

// `auto` spans the run (#1998): the org tier holds a Claude forfait whose
// window is closed — held, whatever its window — so the platform tier's z.ai
// key serves no default route and the run parks on the forfait the restore
// brings back. `tier` is the rule it replaces: the platform tier, holding no
// native credential of its own, falls through to its facade key.
func TestSharedTier_autoSpansTheRunFacadeStaysOffEveryTier(t *testing.T) {
	for _, tc := range []struct {
		name         string
		facade       platformcfg.FacadePolicy
		whoseForfait string // "org" | "team"
		wantZAIDef   bool
		wantForfait  bool // the closed forfait is restored onto the bundle
	}{
		{"auto: the run parks on the org forfait", platformcfg.FacadeAuto, "org", false, true},
		{"tier: the platform key falls through", platformcfg.FacadeTier, "org", true, false},
		// The ticket's own scenario: a TEAM holding a closed Claude forfait
		// and no org tier at all. The team's credential is the run's: the
		// platform's z.ai key stays off the default on every tier.
		{"auto: the run parks on the team's own forfait", platformcfg.FacadeAuto, "team", false, true},
		{"tier: the team's forfait is not the platform tier's", platformcfg.FacadeTier, "team", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
			oauth := secrets.NewMemoryOAuthStore()
			st := usagecap.NewMemStore()
			orgID := ""
			forfaitOwner := ""
			switch tc.whoseForfait {
			case "org":
				orgID = "org-1998"
				forfaitOwner = secrets.OrgTierOwnerKey(orgID)
			case "team":
				forfaitOwner = "webhook:cfg-1"
			}
			seedOAuth(t, oauth, sealer, forfaitOwner, "sk-ant-forfait")
			windowScope := usagecap.TenantScope("team1")
			if orgID != "" {
				windowScope = usagecap.OrgScope(orgID)
			}
			if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, windowScope, seededFP(forfaitOwner)), usagecap.Reading{
				Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			fd := string(tc.facade)
			p := &Publisher{
				apiKeys: keys, oauthForfait: oauth, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
				logger: testLogger(), usageCaps: st,
				platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil),
			}
			if orgID != "" {
				p.identity = &fakeTeamResolver{orgs: map[string]string{"team1": orgID}, orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team1"}}}}}
			}
			rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-1998", orgID, "team1", "webhook:cfg-1", "", "", nil, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if res.secretsRef == "" {
				t.Fatal("nothing was sealed: the test proves nothing")
			}
			rec, err := rs.Get(ctx, res.secretsRef)
			if err != nil {
				t.Fatal(err)
			}
			b, err := secrets.OpenRunBundle(sealer, rec.TenantID, "", "run-1998", rec.KeyID, rec.SealedBundle)
			if err != nil {
				t.Fatal(err)
			}
			if got := b.APIKeys[secrets.ProviderZAI] != ""; got != tc.wantZAIDef {
				t.Errorf("z.ai key as the wire's default = %v, want %v", got, tc.wantZAIDef)
			}
			if _, pinned := b.PinnedAPIKeys[secrets.ProviderZAI]; pinned {
				t.Error("nothing pins zai here: it must be simply off the default")
			}
			if got, want := len(b.OAuthCredentials["claude_code"]) != 0, tc.wantForfait; got != want {
				t.Errorf("the org's closed forfait restored = %v, want %v", got, want)
			}
			if tc.wantForfait && tc.whoseForfait == "org" {
				if got := b.OrgSourced["claude_code"]; !got {
					t.Error("the restored forfait is not org-sourced")
				}
			}
		})
	}
}
