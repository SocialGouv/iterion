package cloudpublisher

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// RVA probe (b): the pool lane's gateway refusal `break`s out of the switch
// only. Simulates the drift the comment names ("a pool vocabulary that
// drifted") by appending the gateway want to poolWantOrder for this test.
func rvaGatewayPoolLaunch(t *testing.T, drift bool) (msg *queue.RunMessage, open []credpool.Lease, bundle secrets.RunBundle, logs string, run *store.Run) {
	t.Helper()
	if drift {
		saved := poolWantOrder
		poolWantOrder = append(slices.Clone(saved), credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)})
		t.Cleanup(func() { poolWantOrder = saved })
	}
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The PLATFORM holds an anthropic key: the run's fallback if the pool lends nothing.
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	// The donor's GATEWAY key, sealed the way the key route seals it.
	gwURL, err := secrets.ValidateGatewayBaseURL("https://gw.example.test")
	if err != nil {
		t.Fatal(err)
	}
	donorKeys := secrets.NewMemoryApiKeyStore()
	dk := secrets.ApiKey{ID: secrets.NewApiKeyID(), TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderOpenAICompatible, BaseURL: gwURL, Name: "gw",
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-donated", gwURL), CreatedAt: time.Now().UTC()}
	dk.SealedSecret, err = secrets.SealAPIKeyFor(sealer, dk, []byte("sk-gw-donated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := donorKeys.Create(store.WithTenant(ctx, "donor-team"), dk); err != nil {
		t.Fatal(err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	// A pledge with Ref openai_compatible written straight into the store
	// (a pledge "written before the refusal").
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "openai_compatible"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "openai_compatible", KeyID: dk.ID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatal(err)
	}
	leases := credpool.NewMemoryLeaseStore()
	ledger := credpool.NewMemoryLedger()
	rvaLedger = ledger
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: leases, Ledger: ledger,
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: donorKeys, Sealer: sealer, Logger: testLogger(),
	})
	rvaBroker = broker
	var buf bytes.Buffer
	var published []*queue.RunMessage
	pub := &Publisher{
		identity:   &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}},
		apiKeys:    keys,
		usageCaps:  usagecap.NewMemStore(),
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   broker,
		logger:     iterlog.New(iterlog.LevelInfo, &buf),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			cp := *m
			published = append(published, &cp)
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw"}, Tools: []string{},
	}}}
	const runID = "run-gw-pool"
	lctx := store.WithIdentity(ctx, poolTeam, "requester")
	if _, err := pub.SubmitLaunch(lctx, runID, runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", BotID: "docs-refresh"}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v\n%s", err, buf.String())
	}
	if len(published) == 0 {
		t.Fatal("nothing published")
	}
	msg = published[len(published)-1]
	open, _ = leases.ListOpenByRun(ctx, runID)
	if msg.SecretsRef != "" {
		rec, err := pub.runSecrets.Get(ctx, msg.SecretsRef)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err = secrets.OpenRunBundle(mustDEKSealer(t, msg.BundleDEK), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := rs.LoadRun(lctx, runID)
	if err == nil {
		run = r
	}
	return msg, open, bundle, buf.String(), run
}

var (
	rvaBroker *credpool.Broker
	rvaLedger *credpool.MemoryLedger
)

func TestPoolGatewayRefusal_Control(t *testing.T) {
	msg, open, bundle, _, run := rvaGatewayPoolLaunch(t, false)
	t.Logf("CONTROL (no drift): PoolGrantless=%v open_leases=%d bundle.APIKeys=%v PlatformSourced=%v fps=%v tiers=%v",
		msg.PoolGrantless, len(open), keysOf(bundle.APIKeys), bundle.PlatformSourced, bundle.APIKeyFingerprints, tiersOf(run))
}

func TestPoolGatewayRefusal_Drift(t *testing.T) {
	msg, open, bundle, logs, run := rvaGatewayPoolLaunch(t, true)
	var budget float64
	if msg.Budget != nil {
		budget = msg.Budget.MaxCostUSD
	}
	t.Logf("DRIFT: PoolGrantless=%v open_leases=%d secretsRef=%q bundle.APIKeys=%v PlatformSourced=%v PoolSourced=%v tiers=%v budget.MaxCostUSD=%v",
		msg.PoolGrantless, len(open), msg.SecretsRef, keysOf(bundle.APIKeys), bundle.PlatformSourced, bundle.PoolSourced, tiersOf(run), budget)
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "REFUSED") || strings.Contains(l, "pool consulted") || strings.Contains(l, "no LLM credential") || strings.Contains(l, "GRANTED") || strings.Contains(l, "platform") {
			t.Logf("log: %s", l)
		}
	}
	// What the runner sends at the end of the attempt (pkg/runner loop_spend.go
	// recordPoolSpend: coarse totals, no ByFingerprint) for a run that spent $2.50
	// on whatever it ran on.
	pubAt, _ := time.Parse(time.RFC3339Nano, msg.PublishedAtRFC)
	if !msg.PoolGrantless {
		if err := rvaBroker.ReportAttempt(context.Background(), "run-gw-pool", pubAt, credpool.Outcome{CostUSD: 2.5, Condition: credpool.ConditionOK}); err != nil {
			t.Logf("ReportAttempt: %v", err)
		}
	}
	day, _, _ := rvaLedger.Usage(context.Background(), credpool.PledgeID("donor", credpool.SourceAPIKey, "openai_compatible"), time.Now().UTC())
	t.Logf("DONOR LEDGER after the attempt report: runs=%d cost_usd=%.2f", day.Runs, day.CostUSD)
	var fails []string
	if day.CostUSD > 0 {
		fails = append(fails, "the donor of the REFUSED gateway key is charged the run's spend")
	}
	// The refused grant must be RELEASED, not merely unsealed: a lease held
	// on it would outlive a credential the run never carried.
	if len(open) != 0 {
		fails = append(fails, "a REFUSED gateway grant still holds its pool lease after the launch")
	}
	if bundle.APIKeys[secrets.ProviderAnthropic] == "" {
		fails = append(fails, "the platform tier was skipped because res.grant is set by a refused grant: the run lost the platform anthropic key")
	}
	if len(fails) > 0 {
		t.Fatalf("defects:\n - %s", strings.Join(fails, "\n - "))
	}
}

func keysOf(m map[secrets.Provider]string) []string {
	var out []string
	for k := range m {
		out = append(out, string(k))
	}
	slices.Sort(out)
	return out
}

func tiersOf(r *store.Run) []string {
	if r == nil {
		return nil
	}
	return r.CredentialTiers
}

// The fallback door's twin: a grant the door cannot seal — here its slot is
// refused wholesale — is RELEASED and not returned, so its lease does not
// outlive a credential the run never carried.
func TestAcquireDoorGrant_AnUnsealedGrantIsReleased(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	// The donor's zai rung is the ONLY rung, so the door's wants name it
	// and Acquire returns the donor's grant.
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "zai_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
	}
	wf := doorWf()
	ctx := context.Background()
	// Sanity arm: with its slot sealable, the door acquires the donor's
	// grant (the fixture proves the donor serves this policy's wants). The
	// bundle holds a default credential — the door's own gate: it opens
	// only for a run whose default slots are already funded.
	// The maps lane 0 initialises on every production bundle (the door's
	// donation marks write into them).
	bundle := &secrets.RunBundle{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-own"},
		PoolSourced: map[string]bool{}, PlatformSourced: map[string]bool{}}
	g, _ := f.pub.acquireDoorGrant(ctx, "r1", poolOrg, poolTeam, "requester", "bot", model.RouteView{Workflow: wf}, policy, nil, bundle, nil, false, false, map[secrets.Provider]string{}, func(string) bool { return true })
	if g == nil {
		t.Fatal("fixture: the door acquired nothing — the release witness would be vacuous")
	}
	// The sanity lease goes back, so the refused arm starts from a free slot.
	f.broker.ReleaseGrant(ctx, g)
	// The refused arm: every slot refused, nothing is sealed, the grant is
	// released and NOT returned (a returned grant books the run on the
	// donor and skips the tiers behind the door).
	refusedBundle := &secrets.RunBundle{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-own"},
		PoolSourced: map[string]bool{}, PlatformSourced: map[string]bool{}}
	g, released := f.pub.acquireDoorGrant(ctx, "r2", poolOrg, poolTeam, "requester", "bot", model.RouteView{Workflow: wf}, policy, nil, refusedBundle, nil, false, false, map[secrets.Provider]string{}, func(string) bool { return false })
	if g != nil {
		t.Fatal("an unsealed door grant was returned — its lease outlives a credential the run never carried")
	}
	// The released grant is RETURNED to the caller, not just released in
	// place: the rollback reopens what its acquisition superseded.
	if released == nil || released.DonorID != "donor" {
		t.Fatalf("the released door grant was dropped or is not the refused one: %+v", released)
	}
	// The slot must be FREE again: a third acquire grants. Under the leak,
	// the refused arm's lease would leave the pledge "serving" and this
	// arm would be declined.
	if g3, _ := f.pub.acquireDoorGrant(ctx, "r3", poolOrg, poolTeam, "requester", "bot", model.RouteView{Workflow: wf}, policy, nil, &secrets.RunBundle{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-own"}, PoolSourced: map[string]bool{}, PlatformSourced: map[string]bool{}}, nil, false, false, map[secrets.Provider]string{}, func(string) bool { return true }); g3 == nil {
		t.Fatal("the refused arm's lease was not released: the pledge is still serving it")
	}
	logs := f.logs.String()
	if n := strings.Count(logs, "not sealed"); n != 1 {
		t.Fatalf("the logs say \"not sealed\" %d times, want 1:\n%s", n, logs)
	}
	if n := strings.Count(logs, "fallback door granted"); n != 2 {
		t.Fatalf("the logs grant the door %d times, want 2 (the sanity arms only):\n%s", n, logs)
	}
	if strings.Contains(logs, "door declined") {
		t.Fatalf("the door declined an arm that should have been served:\n%s", logs)
	}
}

// The reopen witness with a real pool lease at stake: the LAUNCH takes
// donor A's xai grant through the pool lane (a lease is open, the run
// stamped), and the resume — donor A gone, a gateway pledge drifted into
// the pool's vocabulary — has its re-acquisition REFUSED and released
// before the publication fails. The rollback must reopen donor A's lease
// anyway: the refused re-acquisition superseded it all the same, and a nil
// ReopenSuperseded would hold A's slot to the TTL for a run that never
// relaunched (and silence the superseded attempt's spend report).
func rvaPoolLeaseLaunch(t *testing.T) (pub *Publisher, leases *credpool.MemoryLeaseStore, pledges *credpool.MemoryPledgeStore, rs *store.FilesystemRunStore, logs *bytes.Buffer, setPublishOK func(bool), gatewayKeyID string, wf *ir.Workflow) {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	rs, err = store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	// Donor A's xai key and pledge: what the LAUNCH takes through the pool.
	xaiKeyID := secrets.NewApiKeyID()
	xaiSealed, err := secrets.SealAPIKey(sealer, xaiKeyID, []byte("xai-donated-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Create(store.WithTenant(ctx, "donor-a"), secrets.ApiKey{ID: xaiKeyID, TenantID: "donor-a", ScopeTeamID: "donor-a",
		ScopeUserID: "donor-a", Provider: secrets.ProviderXAI, Name: "lent",
		SealedSecret: xaiSealed, Fingerprint: secrets.FingerprintSHA256("xai-donated-key"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	// The gateway donor key a DRIFTED gateway pledge would name (the
	// broker reads the donor's key by id from this store).
	gwURL, err := secrets.ValidateGatewayBaseURL("https://gw.example.test")
	if err != nil {
		t.Fatal(err)
	}
	gatewayKeyID = secrets.NewApiKeyID()
	gwSealed, err := secrets.SealAPIKeyFor(sealer, secrets.ApiKey{ID: gatewayKeyID, TenantID: "donor-gw", ScopeTeamID: "donor-gw",
		ScopeUserID: "donor-gw", Provider: secrets.ProviderOpenAICompatible, BaseURL: gwURL, Name: "gw",
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-donated", gwURL)}, []byte("sk-gw-donated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Create(store.WithTenant(ctx, "donor-gw"), secrets.ApiKey{ID: gatewayKeyID, TenantID: "donor-gw", ScopeTeamID: "donor-gw",
		ScopeUserID: "donor-gw", Provider: secrets.ProviderOpenAICompatible, BaseURL: gwURL, Name: "gw",
		SealedSecret: gwSealed, Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-donated", gwURL), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges = credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor-a", credpool.SourceAPIKey, "xai"), PoolID: "pool-1", UserID: "donor-a",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "xai", KeyID: xaiKeyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatal(err)
	}
	leases = credpool.NewMemoryLeaseStore()
	var buf bytes.Buffer
	publishOK := true
	pub = &Publisher{
		identity:   &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}},
		apiKeys:    keys,
		usageCaps:  usagecap.NewMemStore(),
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   credpool.NewBroker(credpool.BrokerConfig{Pools: pools, Pledges: pledges, Leases: leases, Ledger: credpool.NewMemoryLedger(), OAuth: secrets.NewMemoryOAuthStore(), APIKeys: keys, Sealer: sealer, Logger: testLogger()}),
		logger:     iterlog.New(iterlog.LevelInfo, &buf),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			if !publishOK {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	wf = &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw"}, Tools: []string{},
	}}}
	lctx := store.WithIdentity(ctx, poolTeam, "requester")
	if _, err := pub.SubmitLaunch(lctx, "run-lease", runview.LaunchSpec{FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n", BotID: "docs-refresh"}, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v\n%s", err, buf.String())
	}
	return pub, leases, pledges, rs, &buf, func(ok bool) { publishOK = ok }, gatewayKeyID, wf
}

func TestFailedResumeWithARefusedGrantReopensTheSupersededLease(t *testing.T) {
	pub, leases, pledges, rs, logs, setPublishOK, gatewayKeyID, wf := rvaPoolLeaseLaunch(t)
	// The launch holds donor A's pool lease.
	open, _ := leases.ListOpenByRun(context.Background(), "run-lease")
	if len(open) != 1 || open[0].DonorID != "donor-a" {
		t.Fatalf("fixture: the launch holds %v, want donor A's pool lease — the witness would be vacuous", open)
	}

	// The failure cycle: the run pauses, the publish seam is armed to fail,
	// donor A's pledge is disabled, and a gateway pledge drifts into the
	// pool's vocabulary. The resume's re-acquisition lands on it, the walk
	// refuses to seal it, and the publication fails on the seam.
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	run, err := rs.LoadRun(ctx, "run-lease")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusPausedOperator
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	setPublishOK(false)
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor-a", credpool.SourceAPIKey, "xai"), PoolID: "pool-1", UserID: "donor-a",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "xai"},
		Enabled:    false, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor-gw", credpool.SourceAPIKey, "openai_compatible"), PoolID: "pool-1", UserID: "donor-gw",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "openai_compatible", KeyID: gatewayKeyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatal(err)
	}
	saved := poolWantOrder
	poolWantOrder = append(slices.Clone(saved), credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)})
	t.Cleanup(func() { poolWantOrder = saved })
	if err := pub.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-lease", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
	}, wf, &runview.CompiledSource{Hash: "hash"}); err == nil {
		t.Fatal("SubmitResume succeeded despite the publish failure — the fault seam broke")
	}
	var fails []string
	open2, _ := leases.ListOpenByRun(context.Background(), "run-lease")
	for _, l := range open2 {
		if l.DonorID == "donor-gw" {
			fails = append(fails, "the refused gateway grant still holds a lease")
		}
	}
	reopened := false
	for _, l := range open2 {
		if l.DonorID == "donor-a" {
			reopened = true
		}
	}
	if !reopened {
		fails = append(fails, "donor A's superseded lease was not reopened across a refused re-acquisition (a nil ReopenSuperseded is a silent no-op)")
	}
	if len(fails) > 0 {
		t.Fatalf("defects:\n - %s\nlogs:\n%s", strings.Join(fails, "\n - "), logs.String())
	}
}
