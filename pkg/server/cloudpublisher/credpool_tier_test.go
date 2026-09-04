package cloudpublisher

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// poolFixture wires a REAL broker over in-memory stores and a real sealer,
// with one donor whose credential is genuinely sealed in the OAuth store.
// Nothing here hands the publisher a credential directly: if the tier is
// not wired end to end, the bundle comes back empty.
type poolFixture struct {
	pub     *Publisher
	rs      *secrets.MemoryRunSecretsStore
	sealer  secrets.Sealer
	pledges *credpool.MemoryPledgeStore
	ledger  *credpool.MemoryLedger
}

const (
	poolOrg  = "org-1"
	poolTeam = "team-1"
)

func newPoolFixture(t *testing.T, limits credpool.Limits) *poolFixture {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()

	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")

	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Limits: limits,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	ledger := credpool.NewMemoryLedger()

	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: ledger,
		OAuth: oauth, Sealer: sealer, Logger: testLogger(),
	})
	if broker == nil {
		t.Fatal("broker is nil with every dependency wired")
	}
	rs := secrets.NewMemoryRunSecretsStore()
	return &poolFixture{
		pub: &Publisher{
			// Deliberately NO oauthForfait / apiKeys: this fixture is a
			// tenant with no credential of its own, which is the only
			// condition under which the pool is meant to step in.
			runSecrets: rs,
			sealer:     sealer,
			credPool:   broker,
			logger:     iterlog.New(iterlog.LevelError, nil),
		},
		rs: rs, sealer: sealer, pledges: pledges, ledger: ledger,
	}
}

// resolve runs the real credential-resolution path and opens the sealed
// bundle the runner would receive.
func (f *poolFixture) resolve(t *testing.T, runID string, wf *ir.Workflow) (secrets.RunBundle, credResolution) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", wf, nil, nil, model.ModelOverrides{})
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
	bundle, err := secrets.OpenRunBundle(f.sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle, creds
}

// The whole point of the tier: a tenant with no credential of its own ends
// up with a donor's, sealed into the bundle the runner will materialise.
func TestPoolTier_credentiallessRunGetsADonorsSubscription(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})

	bundle, creds := f.resolve(t, "run-1", nil)

	blob := bundle.OAuthCredentials["claude_code"]
	if len(blob) == 0 {
		t.Fatal("no pooled credential in the sealed bundle — the tier is not wired")
	}
	var got map[string]map[string]string
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("bundle credential is not the donor's stored blob: %v", err)
	}
	if got["claudeAiOauth"]["accessToken"] != "sk-ant-donated" {
		t.Errorf("token = %q, want the donor's", got["claudeAiOauth"]["accessToken"])
	}
	if creds.grant == nil || creds.grant.DonorID != "donor" {
		t.Fatalf("grant = %+v, want the donor's", creds.grant)
	}
}

// The enforcement half: the run's cost budget must be capped at what
// remains of the donor's allowance, or a single run could spend the whole
// pledge and the overspend would only be discovered afterwards.
func TestPoolTier_runBudgetIsCappedAtTheDonorsAllowance(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	// The donor has already given $3 today.
	if err := f.ledger.AddSpend(context.Background(), credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC(), 3, 0, 0); err != nil {
		t.Fatalf("seed spend: %v", err)
	}

	_, creds := f.resolve(t, "run-1", nil)
	if creds.grant == nil {
		t.Fatal("no grant")
	}
	budget := clampBudgetToGrant(nil, nil, creds.grant, 0, testLogger(), "run-1")
	if budget == nil || budget.MaxCostUSD != 2 {
		t.Fatalf("wire budget = %+v, want MaxCostUSD 2 (the $5 pledge minus the $3 already given)", budget)
	}
}

// The clamp must only ever LOWER. A bot that declares a small budget under
// a generous allowance must keep its own figure, not be raised to it.
func TestClampBudgetToGrant_neverRaises(t *testing.T) {
	grant := &credpool.Grant{RemainingUSD: 50}
	cases := []struct {
		name     string
		override *ir.BudgetOverrides
		wf       *ir.Workflow
		want     float64
	}{
		{"no budget anywhere takes the allowance", nil, nil, 50},
		{
			"the workflow's own declared budget wins when tighter",
			nil, &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 2}}, 2,
		},
		{
			"a launch override wins when tighter",
			&ir.BudgetOverrides{MaxCostUSD: 1}, &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 8}}, 1,
		},
		{
			"the allowance wins when it is the tightest",
			&ir.BudgetOverrides{MaxCostUSD: 900}, &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 800}}, 50,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clampBudgetToGrant(tc.override, tc.wf, grant, 0, testLogger(), "run-1")
			if got == nil || got.MaxCostUSD != tc.want {
				t.Errorf("MaxCostUSD = %+v, want %v", got, tc.want)
			}
		})
	}
}

// A donor who set no spend cap grants an allowance of 0 — which means "no
// ceiling", not "nothing left". Reading it as a ceiling would publish a run
// budget of zero and stop the run before its first node.
func TestClampBudgetToGrant_uncappedDonorImposesNoCeiling(t *testing.T) {
	wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 7}}
	got := clampBudgetToGrant(nil, wf, &credpool.Grant{RemainingUSD: 0}, 0, testLogger(), "run-1")
	if got != nil && got.MaxCostUSD != 0 {
		t.Errorf("budget = %+v, want no cost override imposed", got)
	}
}

// No grant at all (no pool, or nobody available) must leave the caller's
// budget exactly as it was.
func TestClampBudgetToGrant_noGrantPassesThrough(t *testing.T) {
	in := &ir.BudgetOverrides{MaxCostUSD: 12, MaxTokens: 999}
	got := clampBudgetToGrant(in, nil, nil, 0, testLogger(), "run-1")
	if got == nil || got.MaxCostUSD != 12 || got.MaxTokens != 999 {
		t.Errorf("budget = %+v, want the caller's own overrides untouched", got)
	}
}

// The pool is a LAST resort: spending a contributor's lent subscription
// while the tenant holds a usable key of its own would take a donation
// nobody needed.
func TestPoolTier_skippedWhenTheTenantHasItsOwnCredential(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	// Give the tenant its own personal forfait.
	own := secrets.NewMemoryOAuthStore()
	seedOAuth(t, own, f.sealer, "requester", "sk-ant-own")
	f.pub.oauthForfait = own

	bundle, creds := f.resolve(t, "run-1", nil)
	if creds.grant != nil {
		t.Error("the pool was drawn on although the tenant had its own credential")
	}
	if got := string(bundle.OAuthCredentials["claude_code"]); !contains(got, "sk-ant-own") {
		t.Errorf("bundle carries %q, want the tenant's own credential", got)
	}
}

// A paused donor must not be served, and the run must proceed exactly as
// it would with no pool at all — never fail.
func TestPoolTier_pausedDonorLeavesTheRunCredentialless(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{})
	ctx := context.Background()
	p, err := f.pledges.Get(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatalf("get pledge: %v", err)
	}
	p.Enabled = false
	if err := f.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("pause pledge: %v", err)
	}

	bundle, creds := f.resolve(t, "run-1", nil)
	if creds.grant != nil {
		t.Error("a paused donor was served")
	}
	if len(bundle.OAuthCredentials) != 0 {
		t.Errorf("bundle = %+v, want no credentials", bundle.OAuthCredentials)
	}
}

// A resume must come back with a ceiling the ENGINE can satisfy.
//
// runtime/checkpoint.go restores the checkpoint's CUMULATIVE cost into the
// same tracker that max_cost_usd is checked against, while a grant is what
// the donor will lend NEXT. Publishing the marginal figure made a run that
// had spent $6 of a $10 pledge resume with a $4 ceiling against $6 already
// counted — BUDGET_EXCEEDED before the first node, on an ordinary pause.
func TestClampBudgetToGrant_resumeCeilingIsInTheEnginesCumulativeFrame(t *testing.T) {
	// The donor's $10/day with $6 of it already given to THIS run.
	grant := &credpool.Grant{RemainingUSD: 4}
	const alreadySpent = 6

	got := clampBudgetToGrant(nil, nil, grant, alreadySpent, testLogger(), "run-1")
	if got == nil {
		t.Fatal("no budget published")
	}
	if got.MaxCostUSD <= alreadySpent {
		t.Fatalf("ceiling $%.2f <= the $%.2f already counted — the run dies on its own budget at resume",
			got.MaxCostUSD, float64(alreadySpent))
	}
	if got.MaxCostUSD != 10 {
		t.Errorf("ceiling = $%.2f, want $10 (what was banked plus what the donor still lends)", got.MaxCostUSD)
	}
}

// The offset must not become a loophole: a tighter declared budget still
// wins, so a resume cannot quietly raise a bot's own ceiling.
func TestClampBudgetToGrant_resumeOffsetStillNeverRaisesADeclaredBudget(t *testing.T) {
	wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 3}}
	got := clampBudgetToGrant(nil, wf, &credpool.Grant{RemainingUSD: 4}, 6, testLogger(), "run-1")
	if got == nil || got.MaxCostUSD != 3 {
		t.Errorf("MaxCostUSD = %+v, want the workflow's own 3", got)
	}
}

// A run that pins its models must not be handed a donation it cannot
// spend. Asking for every known provider meant a bot pinned on
// anthropic/… could be granted a lent z.ai key: the lease consumed a unit
// of the donor's daily runs and held a slot for a run that then failed at
// its first LLM call, and every retry re-picked the same wrong donation.
func TestWantsFor_narrowsToTheProvidersTheRunPinned(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.AgentNode{LLMFields: ir.LLMFields{Model: "anthropic/claude-opus-5"}},
	}}
	got := wantsFor(wf, model.ModelOverrides{})
	if len(got) == 0 {
		t.Fatal("a pinned run was left with nothing to ask for")
	}
	for _, w := range got {
		if p := providerOfWant(w); p != string(secrets.ProviderAnthropic) {
			t.Errorf("asked for %s (provider %q) on an anthropic-pinned run", w, p)
		}
	}
	// The Claude subscription is the natural donation for that pin, and it
	// must still come before the metered key.
	if got[0].Source != credpool.SourceOAuth || got[0].Ref != string(secrets.OAuthKindClaudeCode) {
		t.Errorf("first want = %s, want the lent Claude subscription", got[0])
	}
}

// A run that pins nothing takes whatever it is given: claw substitutes the
// first available provider, so every donation can serve it.
func TestWantsFor_unpinnedRunKeepsTheWholeOrder(t *testing.T) {
	for _, wf := range []*ir.Workflow{nil, {Nodes: map[string]ir.Node{
		"a": &ir.AgentNode{LLMFields: ir.LLMFields{Model: ""}},
	}}} {
		if got := wantsFor(wf, model.ModelOverrides{}); len(got) != len(poolWantOrder) {
			t.Errorf("unpinned run asks for %d want(s), want the full %d", len(got), len(poolWantOrder))
		}
	}
}

// A lent subscription is metered like any other: on the DONOR credential's
// own identity. Without it the borrower's meter is slot-shaped, so a donor
// who reconnects a fresh subscription is still parked by the readings of
// the account it replaced — the lived failure, borrowed.
func TestPoolTier_grantCarriesTheDonorCredentialsIdentity(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})

	bundle, creds := f.resolve(t, "run-1", nil)

	if creds.grant == nil {
		t.Fatal("no grant — the tier is not wired")
	}
	want := seededFP("donor")
	if creds.grant.Fingerprint != want {
		t.Errorf("grant.Fingerprint = %q, want the donor record's %q", creds.grant.Fingerprint, want)
	}
	if got := bundle.OAuthFingerprints["claude_code"]; got != want {
		t.Errorf("OAuthFingerprints[claude_code] = %q, want %q — the borrower's meter would follow the slot, not the lent credential", got, want)
	}
}


// acquireFromPool must LOG A WARN naming the reason at the moment an
// abstention becomes final. The mute prod incident (2026-09-03, half a
// day of investigation) had the pool return nil to the caller and the
// server logs carry no line — an operator could not distinguish "no
// pool" from "audience refused" from "every donor cooling". The Warn
// line is the fix: one line per abstention, at the level that survives
// `ITERION_LOG_LEVEL=info` (Debug was the mute channel).
//
// The oracle is the LOG output the publisher emitted, not the returned
// value — the caller (resolveAndSealCredentials) has always fallen
// through on nil, so the ABSENCE of a log was the bug.
func TestAcquireFromPool_LogsWarnWithReasonOnAbstention(t *testing.T) {
	bufFor := func(t *testing.T) *bytes.Buffer {
		t.Helper()
		var buf bytes.Buffer
		return &buf
	}

	t.Run("nil broker → warn(pool_disabled)", func(t *testing.T) {
		buf := bufFor(t)
		p := &Publisher{logger: iterlog.New(iterlog.LevelInfo, buf)}
		if g := p.acquireFromPool(context.Background(), "run-1", "org", "team", "user", "bot", &ir.Workflow{}, model.ModelOverrides{}); g != nil {
			t.Fatalf("nil broker must not grant, got %+v", g)
		}
		log := buf.String()
		if !strings.Contains(log, "⚠️") || !strings.Contains(log, "run-1") || !strings.Contains(log, "pool_disabled") {
			t.Fatalf("want a Warn line naming run-1 and pool_disabled; got:\n%s", log)
		}
	})

	t.Run("wants == 0 → warn(bot pins ...)", func(t *testing.T) {
		buf := bufFor(t)
		// Broker present but the wants-derivation returns empty: build a
		// workflow with a model pin the pool never lends (a fake provider).
		f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
		f.pub.logger = iterlog.New(iterlog.LevelInfo, buf)
		wf := &ir.Workflow{Nodes: map[string]ir.Node{
			"a": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "a"},
				LLMFields: ir.LLMFields{Model: "fake-provider/some-model"},
			},
		}}
		if g := f.pub.acquireFromPool(context.Background(), "run-2", poolOrg, poolTeam, "u", "bot", wf, model.ModelOverrides{}); g != nil {
			t.Fatalf("wants==0 must not grant, got %+v", g)
		}
		log := buf.String()
		if !strings.Contains(log, "⚠️") || !strings.Contains(log, "run-2") || !strings.Contains(log, "pinned=fake-provider") {
			t.Fatalf("want a Warn line naming run-2 and the pinned provider; got:\n%s", log)
		}
	})

	t.Run("no eligible pledge → warn(no_eligible_pledge)", func(t *testing.T) {
		buf := bufFor(t)
		f := newPoolFixture(t, credpool.Limits{})
		f.pub.logger = iterlog.New(iterlog.LevelInfo, buf)
		// Disable the donor: audience opens, walk finds nothing eligible.
		pledge, _ := f.pledges.Get(context.Background(), credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
		pledge.Enabled = false
		_ = f.pledges.Upsert(context.Background(), pledge)

		if g := f.pub.acquireFromPool(context.Background(), "run-3", poolOrg, poolTeam, "u", "bot", &ir.Workflow{}, model.ModelOverrides{}); g != nil {
			t.Fatalf("no eligible pledge must not grant, got %+v", g)
		}
		log := buf.String()
		if !strings.Contains(log, "⚠️") || !strings.Contains(log, "run-3") || !strings.Contains(log, "no_eligible_pledge") {
			t.Fatalf("want a Warn line naming run-3 and no_eligible_pledge; got:\n%s", log)
		}
	})
}
