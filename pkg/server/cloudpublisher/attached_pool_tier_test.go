package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// attachedFixture is the pool fixture with the attachment surface wired: a
// PRIVATE pool (the attachment is the consent), an attachment store, and an
// api-key store so the gateway tier can travel.
type attachedFixture struct {
	pub     *Publisher
	rs      *secrets.MemoryRunSecretsStore
	sealer  secrets.Sealer
	pools   *credpool.MemoryPoolStore
	pledges *credpool.MemoryPledgeStore
	atts    *credpool.MemoryAttachmentStore
	leases  *credpool.MemoryLeaseStore
	oauth   *secrets.MemoryOAuthStore
}

func newAttachedFixture(t *testing.T) *attachedFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")

	pools := credpool.NewMemoryPoolStore()
	// PRIVATE, user-owned: the legacy walk must never reach it — only the
	// attachment names it (law 2's strongest form).
	if err := pools.Upsert(ctx, credpool.Pool{
		ID: "pool-att", OrgID: poolOrg, Enabled: true,
		OwnerType: credpool.PoolOwnerUser, OwnerID: "ops", Visibility: credpool.PoolVisibilityPrivate,
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-att",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Priority: 0,
	}); err != nil {
		t.Fatalf("seed forfait pledge: %v", err)
	}
	apiKeys := secrets.NewMemoryApiKeyStore()
	gw := secrets.ApiKey{
		ID: "key-gw", TenantID: poolTeam, ScopeTeamID: poolTeam, ScopeUserID: "gw-lender",
		Provider: secrets.ProviderOpenAICompatible, Name: "pliage-deepseek",
		BaseURL:     "https://gw.example",
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-0000", "https://gw.example"),
	}
	sealed, err := secrets.SealAPIKeyFor(sealer, gw, []byte("sk-gw-0000"))
	if err != nil {
		t.Fatalf("seal gateway key: %v", err)
	}
	gw.SealedSecret = sealed
	if err := apiKeys.Create(ctx, gw); err != nil {
		t.Fatalf("seed gateway key: %v", err)
	}
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("gw-lender", credpool.SourceAPIKey, string(secrets.ProviderOpenAICompatible)), PoolID: "pool-att",
		UserID:     "gw-lender",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible), KeyID: "key-gw"},
		Enabled:    true, Health: credpool.HealthOK,
		Model: "openai_compatible/scaleway/deepseek-v4.1-flash", Priority: 5,
	}); err != nil {
		t.Fatalf("seed gateway pledge: %v", err)
	}
	atts := credpool.NewMemoryAttachmentStore()
	if err := atts.Put(ctx, credpool.Attachment{
		ScopeID: credpool.TeamScopeID(poolTeam), PoolID: "pool-att", Enabled: true,
		Strategy: credpool.Strategy{Mode: credpool.StrategyPriority},
	}); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	leases := credpool.NewMemoryLeaseStore()
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: leases,
		Ledger: credpool.NewMemoryLedger(), OAuth: oauth, APIKeys: apiKeys,
		Sealer: sealer, Logger: testLogger(),
	})
	if broker == nil {
		t.Fatal("broker nil with every dependency wired")
	}
	rs := secrets.NewMemoryRunSecretsStore()
	return &attachedFixture{
		pub: &Publisher{
			runSecrets: rs, sealer: sealer, credPool: broker,
			logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude,
		},
		rs: rs, sealer: sealer, pools: pools, pledges: pledges, atts: atts, leases: leases, oauth: oauth,
	}
}

// resolveAttached runs the real resolution with the frozen attachment the
// launch would carry.
func (f *attachedFixture) resolveAttached(t *testing.T, runID string, wf *ir.Workflow, att *store.RunPoolAttachment) (secrets.RunBundle, credResolution) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, att)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}, creds
	}
	rec, err := f.rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle, creds
}

func frozenAttachment(poolID string) *store.RunPoolAttachment {
	return &store.RunPoolAttachment{
		PoolID:  poolID,
		ScopeID: credpool.TeamScopeID(poolTeam),
		Strategy: store.RunPoolStrategy{
			Triggers: []string{"usage_window"},
		},
	}
}

// The recipe's core: an attached run whose pool holds a forfait AND a
// gateway key seals BOTH slots — two grants, the forfait first (priority 0
// over 5), the gateway key traveling as the PAIRED key+URL the amendment
// defines, its endpoint-aware fingerprint riding the bundle.
func TestAttachedPool_sealsForfaitAndGatewaySlots(t *testing.T) {
	f := newAttachedFixture(t)
	bundle, creds := f.resolveAttached(t, "run-att", nil, frozenAttachment("pool-att"))
	if len(creds.grants) != 2 {
		t.Fatalf("sealed grants = %d, want 2 (forfait slot + gateway slot)", len(creds.grants))
	}
	if creds.grant == nil || creds.grant.Source != credpool.SourceOAuth {
		t.Fatalf("first sealed grant = %v, want the forfait (priority 0 outranks 5)", creds.grant)
	}
	if len(bundle.OAuthCredentials["claude_code"]) == 0 {
		t.Error("the forfait slot is empty — the pool tier did not seal it")
	}
	if bundle.APIKeys[secrets.ProviderOpenAICompatible] == "" {
		t.Fatal("the gateway slot is empty — a gateway entry the attachment brought in must seal")
	}
	if got := bundle.APIBaseURLs[secrets.ProviderOpenAICompatible]; got != "https://gw.example" {
		t.Errorf("gateway URL = %q, want the entry's canonical endpoint (the PAIR travels)", got)
	}
	wantFP := secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-0000", "https://gw.example")
	if bundle.APIKeyFingerprints[secrets.ProviderOpenAICompatible] != wantFP {
		t.Errorf("gateway fingerprint = %q, want the endpoint-aware stamp", bundle.APIKeyFingerprints[secrets.ProviderOpenAICompatible])
	}
	if !bundle.PoolSourced[string(secrets.ProviderOpenAICompatible)] || !bundle.PoolSourced["claude_code"] {
		t.Error("both sealed slots must carry the PoolSourced stamp (metering reaches the donor rows)")
	}
	// Two OPEN leases: one per served pledge — each slot drew its own
	// donor, and the gateway slot's acquisition is the forfait's SIBLING:
	// it must not supersede (close) the forfait's lease while both
	// credentials stay sealed and spent (R916b92). Counting every lease,
	// closed ones included, would miss exactly that.
	if open := openLeases(t, f, "run-att"); len(open) != 2 {
		t.Errorf("open leases = %d, want 2 (one per slot: forfait donor + gateway lender — neither superseded by its sibling)", len(open))
	}
}

// bundleFrom opens the sealed bundle a resolution produced.
func bundleFrom(t *testing.T, pub *Publisher, ctx context.Context, creds credResolution, runID string) secrets.RunBundle {
	t.Helper()
	rec, err := pub.runSecrets.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle
}

// Law 1 with teeth: the team's OWN anthropic key fills its slot at tier 1;
// the attached pool holding an anthropic pledge never displaces it — the
// pool serves only the slot the tenant left empty.
func TestAttachedPool_explicitKeyKeepsItsSlot(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	apiKeys := secrets.NewMemoryApiKeyStore()
	teamKey := secrets.ApiKey{
		ID: "key-team", TenantID: poolTeam, ScopeTeamID: poolTeam, ScopeUserID: "owner1",
		Provider: secrets.ProviderAnthropic, Name: "team key",
	}
	sealed, err := secrets.SealAPIKey(sealer, teamKey.ID, []byte("sk-team-key-000"))
	if err != nil {
		t.Fatalf("seal team key: %v", err)
	}
	teamKey.SealedSecret = sealed
	if err := apiKeys.Create(ctx, teamKey); err != nil {
		t.Fatalf("seed team key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{
		ID: "pool-mix", OrgID: poolOrg, Enabled: true,
		OwnerType: credpool.PoolOwnerUser, OwnerID: "ops", Visibility: credpool.PoolVisibilityPrivate,
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-mix",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK,
	}); err != nil {
		t.Fatalf("seed forfait pledge: %v", err)
	}
	// The pool ALSO holds an anthropic key from another donor — the one the
	// team's own key must keep out of its slot.
	other := secrets.ApiKey{
		ID: "key-other", TenantID: "team-9", ScopeTeamID: "team-9", ScopeUserID: "other-donor",
		Provider: secrets.ProviderAnthropic, Name: "other key",
	}
	otherSealed, err := secrets.SealAPIKey(sealer, other.ID, []byte("sk-other-key-0"))
	if err != nil {
		t.Fatalf("seal other key: %v", err)
	}
	other.SealedSecret = otherSealed
	if err := apiKeys.Create(ctx, other); err != nil {
		t.Fatalf("seed other key: %v", err)
	}
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("other-donor", credpool.SourceAPIKey, "anthropic"), PoolID: "pool-mix",
		UserID: "other-donor", Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "anthropic", KeyID: "key-other"},
		Enabled: true, Health: credpool.HealthOK,
	}); err != nil {
		t.Fatalf("seed other pledge: %v", err)
	}
	atts := credpool.NewMemoryAttachmentStore()
	if err := atts.Put(ctx, credpool.Attachment{ScopeID: credpool.TeamScopeID(poolTeam), PoolID: "pool-mix", Enabled: true}); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(),
		Ledger: credpool.NewMemoryLedger(), OAuth: oauth, APIKeys: apiKeys,
		Sealer: sealer, Logger: testLogger(),
	})
	pub := &Publisher{
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker,
		apiKeys: apiKeys, logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude,
	}
	sctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := pub.resolveAndSealCredentials(sctx, "run-law1", poolOrg, poolTeam, "owner1", "docs-refresh", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil, frozenAttachment("pool-mix"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The team's own key sealed its slot at tier 1: the pool's anthropic
	// pledge was passed over, and only the empty forfait slot drew a grant.
	if bundle := bundleFrom(t, pub, sctx, creds, "run-law1"); bundle.APIKeys[secrets.ProviderAnthropic] != "sk-team-key-000" {
		t.Fatalf("anthropic slot = %q, want the TEAM key — law 1: an explicit credential outranks the pool", bundle.APIKeys[secrets.ProviderAnthropic])
	}
	if creds.grant == nil || creds.grant.Source != credpool.SourceOAuth || creds.grant.DonorID != "donor" {
		t.Fatalf("pool grant = %v, want only the forfait slot served (the anthropic slot stayed the team's)", creds.grant)
	}
	if len(creds.grants) != 1 {
		t.Errorf("grants = %d, want 1 — the filled slot was never asked of the pool", len(creds.grants))
	}
}

// Law 2's enforcement half (Revi 2026-11-09): a RESOLVED-DISABLED
// attachment blocks the LEGACY audience walk too — a scope that opted out
// of pools entirely gets no shared credential injected, even
// credential-less, even with an audience pool wide open.
func TestAttachedPool_disabledBlocksTheLegacyTier(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	// The LEGACY audience pool: enabled, audience admits its own org, and
	// a healthy donor who WOULD serve a credential-less run.
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-legacy", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatalf("seed legacy pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-legacy",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK,
	}); err != nil {
		t.Fatalf("seed legacy pledge: %v", err)
	}
	atts := credpool.NewMemoryAttachmentStore()
	// The team OPTED OUT: resolved-disabled at its own level.
	if err := atts.Put(ctx, credpool.Attachment{
		ScopeID: credpool.TeamScopeID(poolTeam), PoolID: "pool-att", Enabled: false,
	}); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(),
		Ledger: credpool.NewMemoryLedger(), OAuth: oauth,
		Sealer: sealer, Logger: testLogger(),
	})
	pub := &Publisher{
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker,
		logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude,
	}
	sctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := pub.resolveAndSealCredentials(sctx, "run-optout", poolOrg, poolTeam, "requester", "docs-refresh", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, nil,
		&store.RunPoolAttachment{PoolID: "", ScopeID: credpool.TeamScopeID(poolTeam), Strategy: store.RunPoolStrategy{Triggers: []string{}}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if creds.grant != nil || len(creds.grants) != 0 {
		t.Errorf("a disabled attachment must suppress the LEGACY tier too — grant = %v, grants = %d", creds.grant, len(creds.grants))
	}
	if creds.secretsRef != "" {
		if bundle := bundleFrom(t, pub, sctx, creds, "run-optout"); len(bundle.OAuthCredentials["claude_code"]) != 0 {
			t.Error("the legacy donation landed despite the opt-out — law 2 broken at the enforcement half")
		}
	}
}

// openLeases counts the run's leases still open.
func openLeases(t *testing.T, f *attachedFixture, runID string) []credpool.Lease {
	t.Helper()
	all, err := f.leases.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("leases: %v", err)
	}
	var open []credpool.Lease
	for _, l := range all {
		if !l.Closed {
			open = append(open, l)
		}
	}
	return open
}

// A RESUME after a dead pod: the previous attempt's attached leases are
// still open (nobody reported). The resume's FIRST acquisition takes the
// attempt over — the dead attempt's leases close — and its later slots
// are the new grant's siblings: two open leases, never four.
func TestAttachedPool_aResumeSupersedesTheDeadAttemptsLeases(t *testing.T) {
	f := newAttachedFixture(t)
	f.resolveAttached(t, "run-resume", nil, frozenAttachment("pool-att"))
	if got := len(openLeases(t, f, "run-resume")); got != 2 {
		t.Fatalf("first attempt: open leases = %d, want 2", got)
	}
	_, creds := f.resolveAttached(t, "run-resume", nil, frozenAttachment("pool-att"))
	if len(creds.grants) != 2 {
		t.Fatalf("resume sealed %d grants, want 2", len(creds.grants))
	}
	open := openLeases(t, f, "run-resume")
	if len(open) != 2 {
		t.Fatalf("after the resume: open leases = %d, want 2 — the dead attempt's leases must be superseded, not left holding the donors' slots", len(open))
	}
	for _, l := range open {
		if l.AcquiredAt.Before(creds.grants[0].AcquiredAt) {
			t.Errorf("lease %s (pledge %s) of the DEAD attempt is still open", l.ID, l.PledgeID)
		}
	}
}

// reset_hold on the attached walk: the forfait slot switches to the
// second forfait while the first cools down; the first recovers; the
// RESUME keeps the switched-to entry when the dial is on, and returns to
// the strategy's first choice when it is off.
func TestAttachedPool_resetHoldKeepsTheSwitchedEntryOnResume(t *testing.T) {
	for _, hold := range []bool{true, false} {
		f := newAttachedFixture(t)
		ctx := context.Background()
		seedOAuth(t, f.oauth, f.sealer, "donor2", "sk-ant-donated-2")
		second := credpool.Pledge{
			ID: credpool.PledgeID("donor2", credpool.SourceOAuth, "claude_code"), PoolID: "pool-att",
			UserID: "donor2", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
			Enabled: true, Health: credpool.HealthOK, Priority: 3,
		}
		if err := f.pledges.Upsert(ctx, second); err != nil {
			t.Fatal(err)
		}
		firstID := credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
		setCooldown := func(until *time.Time) {
			p, err := f.pledges.Get(ctx, firstID)
			if err != nil {
				t.Fatal(err)
			}
			p.CooldownUntil = until
			if err := f.pledges.Upsert(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		att := frozenAttachment("pool-att")
		att.Strategy.ResetHold = hold

		later := time.Now().Add(time.Hour)
		setCooldown(&later)
		_, creds := f.resolveAttached(t, "run-hold", nil, att)
		if creds.grant == nil || creds.grant.PledgeID != second.ID {
			t.Fatalf("hold=%v: with the first forfait cooling the slot drew %v, want the second forfait", hold, creds.grant)
		}
		if creds.grant.Held != hold {
			t.Fatalf("hold=%v: the switched-to grant held=%v", hold, creds.grant.Held)
		}

		setCooldown(nil) // the first forfait's window reset
		_, creds = f.resolveAttached(t, "run-hold", nil, att)
		want := firstID
		if hold {
			want = second.ID
		}
		if creds.grant == nil || creds.grant.PledgeID != want {
			t.Fatalf("hold=%v: the resume drew %v, want %s", hold, creds.grant, want)
		}
	}
}
