package cloudpublisher

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The BYOK walk's evidence predicate, both faces: only a fresh quota-family
// refusal under the KEY'S OWN fingerprint skips it. Everything uncertain —
// no reading, another provider's meter, the pay-as-you-go overage channel —
// keeps the key usable, because a wrong skip silently moves spend onto a
// credential nobody chose.
func TestApiKeyUsable(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "primary", Fingerprint: "fp-zai-1"}

	usable := p.apiKeyUsable(context.Background(), scope, "run-x", nil)
	if !usable(key) {
		t.Fatal("no evidence must mean usable")
	}

	// The provider refused this key's account rate (the frequency window
	// #610 records) — the walk must pass it over.
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if usable(key) {
		t.Fatal("a fresh frequency refusal under the key's fingerprint must skip it")
	}

	// Hysteric guards: a rejected OVERAGE reading is money, not quota; and
	// a provider with no metered backend has no evidence to act on.
	st2 := usagecap.NewMemStore()
	p2 := &Publisher{usageCaps: st2, logger: iterlog.New(iterlog.LevelError, nil)}
	if err := st2.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowOverage, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !p2.apiKeyUsable(context.Background(), scope, "run-x", nil)(key) {
		t.Fatal("a rejected overage reading is no quota evidence — the key must stay usable")
	}
	other := secrets.ApiKey{Provider: secrets.ProviderOpenAI, Name: "m", Fingerprint: "fp-zai-1"}
	if !usable(other) {
		t.Fatal("a provider with no metered backend must never be skipped")
	}
}

// seedKeyFP seeds a sealed API key carrying an explicit fingerprint — the
// identity the evidence predicate matches refusals against.
func seedKeyFP(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, provider secrets.Provider, plaintext, fp string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := st.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider, Fingerprint: fp,
		Name: string(provider) + "-" + fp, SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

func recordRefusal(t *testing.T, st usagecap.Store, scope, fp string) {
	t.Helper()
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, fp),
		usagecap.Reading{Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record refusal: %v", err)
	}
}

// A provider whose ONLY key is refused must still fund the run when no
// other tier can serve its wire: the run then makes one refused call and
// parks on a durable usage-window retry, instead of being published with
// an empty wire that fails on a no-credential auth error nothing retries.
func TestApiKeySkip_refusedOnlyKeyIsRestored(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-only", "fp-a")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r1", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-only" {
		t.Fatalf("anthropic key = %q, want the refused key RESTORED — an empty wire is a stuck run", got)
	}
	if b.PlatformSourced[string(secrets.ProviderAnthropic)] {
		t.Fatal("a restored TENANT key must not be marked platform-sourced")
	}
}

// With a second key of the same provider, the walk serves it and the
// refused one stays out — restore is strictly the no-other-option path.
func TestApiKeySkip_secondKeyServesNoRestore(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-frozen", "fp-a")
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-healthy", "fp-b")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r2", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-healthy" {
		t.Fatalf("anthropic key = %q, want the healthy second key", got)
	}
}

// The refused tenant key must not block the fall-through: a healthy
// platform key on the same wire serves the run — the manual key-removal
// this PR retires. The tenant key is NOT restored over it.
func TestApiKeySkip_refusedTenantKeyFallsThroughToPlatform(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-frozen", "fp-a")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-platform", "fp-p")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r3", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-platform" {
		t.Fatalf("anthropic key = %q, want the platform key — the fallback chain must engage", got)
	}
	if !b.PlatformSourced[string(secrets.ProviderAnthropic)] {
		t.Fatal("the platform fill must keep its shared-meter scope")
	}
}

// The platform tier itself: its refused-but-only key is restored WITH its
// platform metering scope — the last DB-backed tier is never left empty,
// the rule its OAuth sibling states.
func TestApiKeySkip_refusedPlatformKeyIsRestoredPlatformSourced(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-z")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.ScopePlatform, "fp-z")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r4", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Fatalf("zai key = %q, want the refused platform key RESTORED", got)
	}
	if !b.PlatformSourced[string(secrets.ProviderZAI)] {
		t.Fatal("a restored PLATFORM key must keep its platform-sourced metering scope")
	}
}

// The third evidence family: a provider that rejected the CREDENTIAL
// ITSELF (dead token, malformed secret) must be walked past exactly like
// a quota refusal — without this, a structurally-broken credential keeps
// filling its slot on every re-resolution and gates the healthy tiers off.
func TestApiKeyUsable_authRefusalSkips(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "dead", Fingerprint: "fp-dead"}
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-dead"),
		usagecap.Reading{Window: usagecap.WindowAuth, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if p.apiKeyUsable(context.Background(), scope, "run-x", nil)(key) {
		t.Fatal("a fresh auth refusal under the key's fingerprint must skip it")
	}
}

// The forfait half of the auth family, end to end at the resolution: an
// auth-refused user forfait is skipped — its evidence lives under the SAME
// record fingerprint the runner keys "anthropic-oauth" readings by — and
// the platform forfait serves instead. This is the dead-on-arrival-fleet
// scenario: before, the dead record filled the slot on every re-resolution.
func TestOAuthForfait_authRefusalFallsThroughToPlatform(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "owner1", "sk-ant-dead")
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform")
	caps := usagecap.NewMemStore()
	if err := caps.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team1"), seededFP("owner1")),
		usagecap.Reading{Window: usagecap.WindowAuth, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}

	p := &Publisher{oauthForfait: oauth, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-a1", "team1", "owner1")
	if got := string(b.OAuthCredentials["claude_code"]); !contains(got, "sk-ant-platform") {
		t.Fatalf("claude_code blob = %q, want the PLATFORM forfait — the auth-dead record must be walked past", got)
	}
}

// The concurrency ceiling: a key with MaxConcurrentRuns at its count of
// alive stamped runs is passed over like a refused one — the walk serves
// the next key of the provider — and the run being resolved never counts
// toward its own ceiling.
func TestApiKeyUsable_concurrencyCeiling(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()
	mkRun := func(id string, status store.RunStatus, fp string) {
		if _, err := rs.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		r, _ := rs.LoadRun(ctx, id)
		r.Status = status
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if err := rs.SetRunCredStamp(ctx, id, store.RunCredStamp{Fingerprints: []string{fp}}); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	mkRun("alive-1", store.RunStatusRunning, "fp-capped")
	mkRun("alive-2", store.RunStatusQueued, "fp-capped")

	p := &Publisher{usageCaps: usagecap.NewMemStore(), store: rs, logger: testLogger()}
	usable := p.apiKeyUsable(ctx, usagecap.TenantScope("team"), "run-new", nil)

	capped := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "capped", Fingerprint: "fp-capped", MaxConcurrentRuns: 2}
	if usable(capped) {
		t.Fatal("a key at its ceiling (2/2 alive) must be passed over")
	}
	roomy := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "roomy", Fingerprint: "fp-capped", MaxConcurrentRuns: 3}
	if !usable(roomy) {
		t.Fatal("a key under its ceiling (2/3) must stay usable")
	}
	uncapped := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "uncapped", Fingerprint: "fp-capped"}
	if !usable(uncapped) {
		t.Fatal("MaxConcurrentRuns=0 means uncapped")
	}

	// The run being resolved is already persisted and stamped (a resume):
	// it must not consume its own slot.
	if !p.apiKeyUsable(ctx, usagecap.TenantScope("team"), "alive-1", nil)(capped) {
		t.Fatal("a resume must not count itself toward the key's ceiling")
	}
}

// End to end at the resolution: the capped key's provider slot walks to
// the SECOND key of the same provider, and the run-doc stamp records the
// credentials the bundle actually sealed.
func TestResolve_ceilingWalksToNextKeyAndStampsFingerprints(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := store.WithTenant(context.Background(), "team1")
	keys := secrets.NewMemoryApiKeyStore()
	// Two zai keys: the first is capped at 1 with one alive run holding it.
	id1 := secrets.NewApiKeyID()
	sealed1, _ := secrets.SealAPIKey(sealer, id1, []byte("zai-capped"))
	if err := keys.Create(ctx, secrets.ApiKey{ID: id1, ScopeTeamID: "team1", Provider: secrets.ProviderZAI,
		Name: "zai-a", SealedSecret: sealed1, Fingerprint: "fp-zai-a", MaxConcurrentRuns: 1, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create: %v", err)
	}
	id2 := secrets.NewApiKeyID()
	sealed2, _ := secrets.SealAPIKey(sealer, id2, []byte("zai-roomy"))
	if err := keys.Create(ctx, secrets.ApiKey{ID: id2, ScopeTeamID: "team1", Provider: secrets.ProviderZAI,
		Name: "zai-b", SealedSecret: sealed2, Fingerprint: "fp-zai-b", CreatedAt: time.Now().UTC().Add(time.Second)}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := rs.CreateRun(ctx, "holder", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hr, _ := rs.LoadRun(ctx, "holder")
	hr.Status = store.RunStatusRunning
	_ = rs.SaveRun(ctx, hr)
	_ = rs.SetRunCredStamp(ctx, "holder", store.RunCredStamp{Fingerprints: []string{"fp-zai-a"}})
	// The run being resolved must pre-exist for the stamp.
	if _, err := rs.CreateRun(ctx, "run-c1", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	p := &Publisher{apiKeys: keys, usageCaps: usagecap.NewMemStore(), store: rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rsec := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rsec, sealer, "run-c1", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderZAI]; got != "zai-roomy" {
		t.Fatalf("zai key = %q, want the second key — the capped one is full", got)
	}
	// The harvest names the credential the bundle ACTUALLY sealed — the
	// meter would count the wrong key otherwise.
	creds, err := p.resolveAndSealCredentials(ctx, "run-c2", "", "team1", "owner1", "", nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	found := false
	for _, fp := range creds.fingerprints {
		if fp == "fp-zai-a" {
			t.Fatalf("fingerprints %v name the capped key the bundle did not seal", creds.fingerprints)
		}
		if fp == "fp-zai-b" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fingerprints = %v, want fp-zai-b harvested", creds.fingerprints)
	}
}

// The wiring layer — the exact gap the launch-side stamp fell through
// (written before the run document existed, warn-and-lose): a LAUNCHED
// run's document must carry the sealed credentials' fingerprints via the
// launch's single SaveRun, and a RESUME whose re-resolution finds nothing
// must CLEAR the stamp, not keep metering a credential the run no longer
// holds.
func TestSubmitLaunchAndResume_credFingerprintsRideTheRunDocument(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	ctx := store.WithIdentity(context.Background(), "team1", "owner1")
	kid := secrets.NewApiKeyID()
	sealed, _ := secrets.SealAPIKey(sealer, kid, []byte("zai-key"))
	if err := keys.Create(ctx, secrets.ApiKey{ID: kid, ScopeTeamID: "team1", Provider: secrets.ProviderZAI,
		Name: "zai", SealedSecret: sealed, Fingerprint: "fp-wired", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	p := &Publisher{
		apiKeys:    keys,
		usageCaps:  usagecap.NewMemStore(),
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
		publishRun: func(context.Context, *queue.RunMessage) error { return nil },
	}
	wf := &ir.Workflow{Name: "wf"}
	if _, err := p.SubmitLaunch(ctx, "run-w1", runview.LaunchSpec{
		FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	run, err := rs.LoadRun(ctx, "run-w1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	found := false
	for _, fp := range run.CredFingerprints {
		if fp == "fp-wired" {
			found = true
		}
	}
	if !found {
		t.Fatalf("launched run carries %v, want fp-wired — the ceiling is blind to launches otherwise", run.CredFingerprints)
	}

	// The key disappears; the resume's re-resolution finds nothing and
	// must CLEAR the stamp.
	if err := keys.Delete(ctx, kid); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	run.Status = store.RunStatusPausedOperator
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-w1", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
	}, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	run, err = rs.LoadRun(ctx, "run-w1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.CredFingerprints) != 0 {
		t.Fatalf("resumed run still carries %v — a stale stamp holds a slot on a credential the run no longer has", run.CredFingerprints)
	}
}
