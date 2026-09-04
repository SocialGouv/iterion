package runner

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// TestRecordOrgSpendKey pins the usage key the runner charges spend to:
// the message's OrgID (the key the launch gate metered on) with a
// TenantID fallback for pre-orgid messages and org-less teams. Charging
// the team key when an org exists left the org's cost-cap document at
// zero forever — the multi-team cost-cap bug.
func TestRecordOrgSpendKey(t *testing.T) {
	cases := []struct {
		name    string
		orgID   string
		wantKey string
	}{
		{"org message charges the org key", "org-1", "org-1"},
		{"pre-orgid message falls back to the tenant key", "", "team-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			counter := orgusage.NewMemoryCounter()
			r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.New(iterlog.LevelError, nil)}}
			usage := newMetricsEmitter(nil, nil)
			usage.mu.Lock()
			usage.runCostUSD = 1.5
			usage.runInputTokens = 100
			usage.runOutputTokens = 50
			usage.mu.Unlock()

			r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: tc.orgID}, usage)

			now := time.Now().UTC()
			got, err := counter.Usage(context.Background(), tc.wantKey, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.CostUSD != 1.5 || got.InputTokens != 100 || got.OutputTokens != 50 {
				t.Fatalf("usage on %q = %+v, want cost 1.5 / 100 / 50", tc.wantKey, got)
			}
			// Nothing lands on the other key.
			other := "org-1"
			if tc.wantKey == "org-1" {
				other = "team-a"
			}
			if u, _ := counter.Usage(context.Background(), other, now); u.CostUSD != 0 {
				t.Fatalf("spend leaked onto %q: %+v", other, u)
			}
		})
	}
}

// TestGitOpTimeoutBoundsSubprocess proves runGit is wall-clock bounded
// even when the caller's ctx has no deadline — the exact shape of a run
// launched without --timeout, where a wedged clone used to pin the
// runner pod (one in-flight run each) forever.
func TestGitOpTimeoutBoundsSubprocess(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	// A fetch against RFC 5737 TEST-NET-1 blackholes (SYN never answered)
	// on typical hosts, wedging git in the TCP connect — the shape of a
	// stalled remote. On environments that instead answer/refuse fast the
	// command errors immediately and the elapsed assertion still holds:
	// the property under test is "runGit RETURNS promptly", not the error.
	// The init setup runs under the DEFAULT timeout: on a loaded CI
	// runner even `git init` can outlive a tight test override.
	dir := t.TempDir()
	if err := r.runGit(context.Background(), dir, "", "init", "-q"); err != nil {
		t.Fatalf("git init: %v", err)
	}

	old := gitOpTimeout
	gitOpTimeout = time.Second
	defer func() { gitOpTimeout = old }()
	start := time.Now()
	_ = r.runGit(context.Background(), dir, "", "fetch", "http://192.0.2.1/repo.git")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runGit returned after %s — the per-op timeout did not bound the subprocess", elapsed)
	}
}

// TestRecordOrgSpend_NoOpShapes pins the guard clauses: no counter wired,
// zero accumulated spend, and a tenant-less message all record nothing
// (and never panic).
func TestRecordOrgSpend_NoOpShapes(t *testing.T) {
	t.Run("nil counter", func(t *testing.T) {
		r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
		usage := newMetricsEmitter(nil, nil)
		usage.mu.Lock()
		usage.runCostUSD = 1
		usage.mu.Unlock()
		r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, usage)
	})
	t.Run("zero totals record nothing", func(t *testing.T) {
		counter := orgusage.NewMemoryCounter()
		r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.Nop()}}
		r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: "org-1"}, newMetricsEmitter(nil, nil))
		if u, _ := counter.Usage(context.Background(), "org-1", time.Now().UTC()); u.CostUSD != 0 || u.InputTokens != 0 {
			t.Fatalf("zero-spend attempt recorded usage: %+v", u)
		}
	})
	t.Run("tenant-less message records nothing", func(t *testing.T) {
		counter := orgusage.NewMemoryCounter()
		r := &Runner{cfg: Config{OrgUsage: counter, Logger: iterlog.Nop()}}
		usage := newMetricsEmitter(nil, nil)
		usage.mu.Lock()
		usage.runCostUSD = 2
		usage.mu.Unlock()
		r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run-1"}, usage)
		if u, _ := counter.Usage(context.Background(), "", time.Now().UTC()); u.CostUSD != 0 {
			t.Fatalf("tenant-less spend recorded: %+v", u)
		}
	})
	t.Run("nil usage", func(t *testing.T) {
		r := &Runner{cfg: Config{OrgUsage: orgusage.NewMemoryCounter(), Logger: iterlog.Nop()}}
		r.recordOrgSpend(context.Background(), &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, nil)
	})
}

// TestMetricsEmitter_RunTotalsAccumulate pins the per-run accumulation the
// org-spend charge reads: claw step events add input+output tokens and a
// cost for priced models; delegate events add their aggregated count to
// the input side (no price table → cost floor untouched).
func TestMetricsEmitter_RunTotalsAccumulate(t *testing.T) {
	m := newMetricsEmitter(&recordingEmitter{}, nil) // nil registry: totals still accumulate

	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventLLMRequest, NodeID: "n1",
		Data: map[string]any{"model": "claude-sonnet-4-6"},
	})
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventLLMStepFinished, NodeID: "n1",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(500)},
	})
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type: store.EventDelegateFinished, NodeID: "n2",
		Data: map[string]any{"backend": "claude_code", "tokens": float64(420)},
	})

	cost, in, out := m.RunTotals()
	if in != 1420 {
		t.Errorf("input tokens = %d, want 1420 (claw 1000 + delegate 420)", in)
	}
	if out != 500 {
		t.Errorf("output tokens = %d, want 500", out)
	}
	if cost <= 0 {
		t.Errorf("cost = %v, want > 0 for a priced claw model", cost)
	}
}

// #659 pt 2: the runner bumps `last_used_at` on every API key whose
// fingerprint sits in the resolved credentials, at the START and at the
// END of each attempt — nothing moves it during a turn (there is no live
// per-call signal), so the studio can tell a key idle for hours from one
// an attempt is holding, to attempt granularity. The launch-grant-only
// bump left `last_used_at` frozen for hours on keys actively spending —
// measured live 2026-09-03 ("2.5h idle while serving a third run's
// delegate the whole time").
//
// The oracle is the STORE: after recordOrgSpend, the key's
// last_used_at moves to the metering timestamp — even though msg.SecretsRef
// was never touched (the runner reads Credentials from ctx, which
// injectCredentials stamped for the whole executeRun).
func TestRecordOrgSpend_BumpsApiKeyFingerprintUsed(t *testing.T) {
	apiKeys := secrets.NewMemoryApiKeyStore()
	tenantCtx := store.WithTenant(context.Background(), "team-a")
	id := secrets.NewApiKeyID()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.SealAPIKey(sealer, id, []byte("sk-ant-live"))
	if err != nil {
		t.Fatal(err)
	}
	fp := secrets.FingerprintSHA256("sk-ant-live")
	if err := apiKeys.Create(tenantCtx, secrets.ApiKey{
		ID: id, ScopeTeamID: "team-a", Provider: secrets.ProviderAnthropic,
		Name: "live", SealedSecret: sealed, Fingerprint: fp,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if got, _ := apiKeys.Get(tenantCtx, id); got.LastUsedAt != nil {
		t.Fatalf("fresh key must have no last_used_at")
	}

	r := &Runner{cfg: Config{
		ApiKeys:  apiKeys,
		OrgUsage: orgusage.NewMemoryCounter(),
		Logger:   iterlog.Nop(),
	}}
	usage := newMetricsEmitter(nil, nil)
	usage.mu.Lock()
	usage.runCostUSD = 1
	usage.mu.Unlock()

	// Simulate what injectCredentials stamps on the executeRun ctx.
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): fp},
	})
	r.recordOrgSpend(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: "org-1"}, usage)

	// Best-effort store writes settle synchronously in the memory store.
	got, err := apiKeys.Get(tenantCtx, id)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatalf("last_used_at not bumped — the metering-time bump did not run")
	}
	if time.Since(*got.LastUsedAt) > 5*time.Second {
		t.Fatalf("last_used_at stale (%s ago)", time.Since(*got.LastUsedAt))
	}
}

// seedFingerprintedKey stores one sealed anthropic key carrying the
// fingerprint of its plaintext, the way the BYOK route stamps it.
func seedFingerprintedKey(t *testing.T, apiKeys secrets.ApiKeyStore, sealer secrets.Sealer, plaintext string) (id, fp string) {
	t.Helper()
	tenantCtx := store.WithTenant(context.Background(), "team-a")
	id = secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	fp = secrets.FingerprintSHA256(plaintext)
	if err := apiKeys.Create(tenantCtx, secrets.ApiKey{
		ID: id, ScopeTeamID: "team-a", Provider: secrets.ProviderAnthropic,
		Name: "live", SealedSecret: sealed, Fingerprint: fp,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return id, fp
}

// An attempt that measured nothing still HELD the key — a delegate that
// streamed no usage, a run refused at its first call. RunTotals is lossy;
// the bump must not hide behind it (the 2.5h incident reproduces exactly
// when it does).
func TestRecordOrgSpend_BumpsFingerprintEvenWithZeroUsage(t *testing.T) {
	apiKeys := secrets.NewMemoryApiKeyStore()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	id, fp := seedFingerprintedKey(t, apiKeys, sealer, "sk-ant-quiet")
	r := &Runner{cfg: Config{ApiKeys: apiKeys, OrgUsage: orgusage.NewMemoryCounter(), Logger: iterlog.Nop()}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): fp},
	})
	// Zero totals: the gate that used to swallow the bump.
	r.recordOrgSpend(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a", OrgID: "org-1"}, newMetricsEmitter(nil, nil))
	got, err := apiKeys.Get(store.WithTenant(context.Background(), "team-a"), id)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at not bumped on a zero-usage attempt end — the bump is still behind the spend gate")
	}
}

// The attempt-START half, and its order: admitAttempt bumps the held keys
// as soon as the run is admitted, so a multi-hour attempt does not read as
// an idle key until it ends — and NOT before the pre-flight, so a run
// parked on a ceiling never dates a key it will not spend. Proven through
// the real sealed-bundle path, not by calling the bump directly.
func TestAdmitAttempt_BumpsHeldKeysOnceAdmitted(t *testing.T) {
	sealer := testSealer(t)
	apiKeys := secrets.NewMemoryApiKeyStore()
	id, _ := seedFingerprintedKey(t, apiKeys, sealer, "sk-ant-held")
	rs := secrets.NewMemoryRunSecretsStore()
	sealed, err := secrets.SealRunBundle(sealer, "run-1", secrets.RunBundle{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-held"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "team-a", RunID: "run-1", SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs, Sealer: sealer, ApiKeys: apiKeys}}
	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1"}
	ctx, cleanup, err := r.injectCredentials(context.Background(), msg)
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	defer cleanup()

	read := func(t *testing.T) *secrets.ApiKey {
		t.Helper()
		got, err := apiKeys.Get(store.WithTenant(context.Background(), "team-a"), id)
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		return &got
	}
	// Holding the plaintext is not spending it: nothing is stamped until
	// the run is admitted.
	if k := read(t); k.LastUsedAt != nil {
		t.Fatal("last_used_at stamped by injectCredentials alone — a run the pre-flight parks would date a key it never spent")
	}
	if err := r.admitAttempt(ctx, nil, msg); err != nil {
		t.Fatalf("admitAttempt on an uncapped runner: %v", err)
	}
	if k := read(t); k.LastUsedAt == nil {
		t.Fatal("last_used_at not bumped once the attempt was admitted — the key reads idle until the attempt ends")
	}
}

// The other side of the order: a run the pre-flight parks must leave every
// key exactly as idle as it found it.
func TestAdmitAttempt_ParkedRunStampsNothing(t *testing.T) {
	sealer := testSealer(t)
	apiKeys := secrets.NewMemoryApiKeyStore()
	id, fp := seedFingerprintedKey(t, apiKeys, sealer, "sk-ant-capped")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-capped"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): fp},
	})
	msg := &queue.RunMessage{RunID: "run-capped", TenantID: "team-a"}

	caps := usagecap.NewMemStore()
	if err := caps.Record(context.Background(), usageCapKey(ctx, msg), usagecap.Reading{
		Window:      usagecap.WindowSevenDay,
		Utilization: 0.95,
		Status:      usagecap.StatusRejected,
		ResetsAt:    time.Now().UTC().Add(5 * 24 * time.Hour),
		ObservedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	r := capRunner(capTestPolicy(), caps, &capStatusStore{})
	r.cfg.ApiKeys = apiKeys
	if err := r.admitAttempt(ctx, capLLMWorkflow(), msg); err == nil {
		t.Fatal("a capped run must be parked by the pre-flight")
	}
	got, err := apiKeys.Get(store.WithTenant(context.Background(), "team-a"), id)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if got.LastUsedAt != nil {
		t.Fatalf("a parked run stamped last_used_at (%v) on a key it never spent", got.LastUsedAt)
	}
}

// fingerprintSpyStore records which fingerprints the runner asked the
// api_keys store to bump.
type fingerprintSpyStore struct {
	secrets.ApiKeyStore
	asked []string
}

func (s *fingerprintSpyStore) MarkFingerprintUsed(ctx context.Context, fingerprint string, at time.Time) error {
	s.asked = append(s.asked, fingerprint)
	return s.ApiKeyStore.MarkFingerprintUsed(ctx, fingerprint, at)
}

// An OAuth slot's fingerprint is a subscription's identity in the OAuth
// store; asking the api_keys collection for it is a lookup that matches
// nothing. Only API-key slots reach the store.
func TestMarkCredFingerprintsUsed_SkipsOAuthSlots(t *testing.T) {
	spy := &fingerprintSpyStore{ApiKeyStore: secrets.NewMemoryApiKeyStore()}
	r := &Runner{cfg: Config{ApiKeys: spy, Logger: iterlog.Nop()}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		Fingerprints: map[string]string{
			string(secrets.ProviderAnthropic):   "aaaa000011112222",
			string(secrets.ProviderOpenAI):      "aaaa000011112222", // same secret on two slots: asked once
			string(secrets.OAuthKindClaudeCode): "cccc333344445555",
			string(secrets.OAuthKindCodex):      "dddd333344445555",
		},
	})
	r.markCredFingerprintsUsed(ctx, &queue.RunMessage{RunID: "run-1"}, time.Now().UTC())
	if len(spy.asked) != 1 || spy.asked[0] != "aaaa000011112222" {
		t.Fatalf("store asked for %v, want exactly the one API-key fingerprint", spy.asked)
	}
}
