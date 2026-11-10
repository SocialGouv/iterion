package cloudpublisher

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The busy-wait GUARANTEE: an unfunded refusal waits (Waits, the capacity
// class) exactly when the same request starts once every busy pool entry
// frees. Asserted as that property over a matrix, through the real walk
// (resolveAndSealCredentials) and broker on memory stores, rather than row by
// row: the refusal arm (the deployment knob, the team_gateway boundary's
// strict arm, the sovereign llm_fallback=none) x the pool (the legacy pool,
// which lends one credential per run, and an attached pool, per slot) x one
// or two named providers x each named provider's entry state — busy (another
// run holds the forfait's one slot; the lent key cools), absent (paused), or
// refused behind a clock (the forfait's pledge serves another bot and cools;
// the lent key's slot is held and the key has expired). Each row launches,
// records Waits, frees every busy entry (ends the holders' leases, clears the
// cooldowns) and launches the same request again. The preview, read before the
// launch, must predict the same class, and Busy must name a provider exactly
// when its entry would serve the launch once free.

// waitArm is the refusal arm a matrix row arms.
type waitArm string

const (
	waitArmKnob        waitArm = "knob"
	waitArmTeamGateway waitArm = "team_gateway"
	waitArmSovereign   waitArm = "sovereign"
)

// entryState is a named provider's pool entry at the launch.
type entryState string

const (
	// entryWouldServe: the entry would serve the launch once free — another
	// run holds the forfait's one slot; the lent key cools.
	entryWouldServe entryState = "busy"
	// entryParked: the donor paused the entry.
	entryParked entryState = "absent"
	// entryRefusedBehindAClock: the entry refuses the launch whatever frees,
	// behind a clock status — the forfait's pledge serves another bot and
	// cools; the lent key's slot is held and the key has expired.
	entryRefusedBehindAClock entryState = "refused-behind-a-clock"
)

// guaranteeProvider is a provider the matrix names: the node whose decided
// route names it, and the donor entry that lends it.
type guaranteeProvider struct {
	name   string
	node   string
	model  string
	pledge string
	want   credpool.Credential
}

var guaranteeProviders = []guaranteeProvider{
	{name: "anthropic", node: "review", model: "anthropic/claude-opus-5",
		pledge: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), want: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"}},
	{name: "openai", node: "verdict", model: "openai/gpt-6",
		pledge: credpool.PledgeID("donor2", credpool.SourceAPIKey, string(secrets.ProviderOpenAI)), want: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAI)}},
}

// waitWorld is one row's world: a pool holding a Claude forfait (anthropic)
// and a lent OpenAI key (openai), one slot each, and a team that holds its
// own gateway key on the team_gateway arm alone.
type waitWorld struct {
	pub      *Publisher
	pledges  *credpool.MemoryPledgeStore
	lentKeys *secrets.MemoryApiKeyStore
	// att is the frozen attachment of an attached-pool row, nil on the
	// legacy pool.
	att     *store.RunPoolAttachment
	arm     waitArm
	holders []string
}

func newWaitWorld(t *testing.T, arm waitArm, attached bool) *waitWorld {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	pool := credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}
	var att *store.RunPoolAttachment
	if attached {
		pool = credpool.Pool{ID: "pool-att", OrgID: poolOrg, Enabled: true, OwnerType: credpool.PoolOwnerUser, OwnerID: "ops", Visibility: credpool.PoolVisibilityPrivate}
		att = frozenAttachment("pool-att")
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, pool); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	lentKeys := secrets.NewMemoryApiKeyStore()
	oa := secrets.ApiKey{ID: "key-oa", TenantID: "team-donor2", ScopeTeamID: "team-donor2", ScopeUserID: "donor2", Provider: secrets.ProviderOpenAI, Name: "donor2 openai",
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAI, "sk-oa-0000", "")}
	sealed, err := secrets.SealAPIKeyFor(sealer, oa, []byte("sk-oa-0000"))
	if err != nil {
		t.Fatalf("seal the lent key: %v", err)
	}
	oa.SealedSecret = sealed
	if err := lentKeys.Create(ctx, oa); err != nil {
		t.Fatalf("seed the lent key: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	for _, p := range []credpool.Pledge{
		{ID: guaranteeProviders[0].pledge, PoolID: pool.ID, UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
			Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxConcurrentRuns: 1}},
		{ID: guaranteeProviders[1].pledge, PoolID: pool.ID, UserID: "donor2", Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAI), KeyID: "key-oa"},
			Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxConcurrentRuns: 1}},
	} {
		if err := pledges.Upsert(ctx, p); err != nil {
			t.Fatalf("seed pledge %s: %v", p.ID, err)
		}
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(), OAuth: oauth, APIKeys: lentKeys, Sealer: sealer, Logger: testLogger()})
	teamKeys := secrets.NewMemoryApiKeyStore()
	if arm == waitArmTeamGateway {
		seedGatewayKey(t, teamKeys, sealer, poolTeam, nil)
	}
	resolver := &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}}
	if arm == waitArmSovereign {
		resolver.llmFallback = map[string]string{poolTeam: identity.LLMFallbackNone}
	}
	return &waitWorld{
		pub: &Publisher{runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, apiKeys: teamKeys,
			logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude, identity: resolver, requireLLMCredential: arm == waitArmKnob},
		pledges: pledges, lentKeys: lentKeys, att: att, arm: arm,
	}
}

// workflow is the row's run: a decided route per named provider, plus a
// gateway route on the team_gateway arm (the route its boundary stamps on).
func (w *waitWorld) workflow(named []guaranteeProvider) *ir.Workflow {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{}}
	add := func(id, m string) {
		wf.Nodes[id] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, LLMFields: ir.LLMFields{Backend: "claw", Model: m}, Tools: []string{}}
	}
	if w.arm == waitArmTeamGateway {
		add("gw", "openai_compatible/team/m")
	}
	for _, prov := range named {
		add(prov.node, prov.model)
	}
	return wf
}

func (w *waitWorld) launch(runID string, named []guaranteeProvider) (credResolution, error) {
	ctx := store.WithTenant(context.Background(), poolTeam)
	wf := w.workflow(named)
	return w.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, w.att, boundaryDerived)
}

// rollback returns a launch's grants as SubmitLaunch does when it never
// publishes.
func (w *waitWorld) rollback(cr credResolution) {
	ctx := context.Background()
	if cr.grant != nil {
		w.pub.credPool.ReleaseGrant(ctx, cr.grant)
	}
	for i, g := range cr.grants {
		if i > 0 && g != nil {
			w.pub.credPool.ReleaseGrant(ctx, g)
		}
	}
	if cr.doorGrant != nil {
		w.pub.credPool.ReleaseGrant(ctx, cr.doorGrant)
	}
}

// hold has another run of the team take the entry's one slot.
func (w *waitWorld) hold(t *testing.T, prov guaranteeProvider) {
	t.Helper()
	runID := "run-holder-" + prov.name
	req := credpool.Request{RunID: runID, OrgID: poolOrg, TenantID: poolTeam, UserID: "another-requester", BotID: "docs-refresh",
		Wants: []credpool.Credential{prov.want}, AttemptID: runID + "#1"}
	if w.att != nil {
		req.PoolIDs, req.AttachmentID = []string{w.att.PoolID}, w.att.ScopeID
	}
	g, err := w.pub.credPool.Acquire(context.Background(), req)
	if err != nil || g == nil || g.PledgeID != prov.pledge {
		t.Fatalf("fixture: %s's hold of %s = %v, %v", runID, prov.pledge, g, err)
	}
	w.holders = append(w.holders, runID)
}

func (w *waitWorld) shape(t *testing.T, prov guaranteeProvider, st entryState) {
	t.Helper()
	switch st {
	case entryWouldServe:
		if prov.want.Source == credpool.SourceOAuth {
			w.hold(t, prov)
			return
		}
		shapePledge(t, w.pledges, prov.pledge, func(p *credpool.Pledge) { p.CooldownUntil = coolingFor(time.Hour) })
	case entryParked:
		shapePledge(t, w.pledges, prov.pledge, func(p *credpool.Pledge) { p.Enabled = false })
	case entryRefusedBehindAClock:
		if prov.want.Source == credpool.SourceOAuth {
			shapePledge(t, w.pledges, prov.pledge, func(p *credpool.Pledge) {
				p.Bots = []string{"another-bot"}
				p.CooldownUntil = coolingFor(time.Hour)
			})
			return
		}
		w.hold(t, prov)
		k, err := w.lentKeys.Get(context.Background(), "key-oa")
		if err != nil {
			t.Fatalf("get key-oa: %v", err)
		}
		expired := time.Now().Add(-time.Minute)
		k.ExpiresAt = &expired
		if err := w.lentKeys.Replace(context.Background(), k); err != nil {
			t.Fatalf("expire key-oa: %v", err)
		}
	}
}

// free ends every busy state a wait waits on: the holders' runs end, the
// cooldowns clear. What no clock clears stays.
func (w *waitWorld) free(t *testing.T) {
	t.Helper()
	for _, run := range w.holders {
		if err := w.pub.credPool.Report(context.Background(), run, credpool.Outcome{CostUSD: 0.01}); err != nil {
			t.Fatalf("free: %s's report: %v", run, err)
		}
	}
	for _, prov := range guaranteeProviders {
		shapePledge(t, w.pledges, prov.pledge, func(p *credpool.Pledge) { p.CooldownUntil = nil })
	}
}

// previewRefusal is the credential preview's refusal line for the row's run,
// "" when it predicts none.
func (w *waitWorld) previewRefusal(t *testing.T, named []guaranteeProvider) string {
	t.Helper()
	q := previewReadOnly(w.pub)
	q.requireLLMCredential = w.pub.requireLLMCredential
	spec := previewSpec(poolTeam, "requester")
	spec.PoolAttachment = w.att
	v, err := q.PreviewCredentials(store.WithTenant(context.Background(), poolTeam), spec, w.workflow(named))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return previewWarning(v, "The live launch refuses")
}

type guaranteeRow struct {
	arm      waitArm
	attached bool
	named    []guaranteeProvider
	states   []entryState
}

func (r guaranteeRow) String() string {
	pool := "legacy"
	if r.attached {
		pool = "attached"
	}
	parts := make([]string, len(r.named))
	for i, prov := range r.named {
		parts[i] = prov.name + "=" + string(r.states[i])
	}
	return fmt.Sprintf("%s/%s/%s", r.arm, pool, strings.Join(parts, ","))
}

func guaranteeMatrix() []guaranteeRow {
	states := []entryState{entryWouldServe, entryParked, entryRefusedBehindAClock}
	var rows []guaranteeRow
	for _, arm := range []waitArm{waitArmKnob, waitArmTeamGateway, waitArmSovereign} {
		for _, attached := range []bool{false, true} {
			for _, prov := range guaranteeProviders {
				for _, st := range states {
					rows = append(rows, guaranteeRow{arm: arm, attached: attached, named: []guaranteeProvider{prov}, states: []entryState{st}})
				}
			}
			for _, a := range states {
				for _, o := range states {
					rows = append(rows, guaranteeRow{arm: arm, attached: attached, named: guaranteeProviders, states: []entryState{a, o}})
				}
			}
		}
	}
	return rows
}

func TestBusyWaitGuarantee_ARefusalWaitsExactlyWhenTheSameRequestStartsOnceEveryBusyEntryFrees(t *testing.T) {
	rows := guaranteeMatrix()
	if len(rows) != 90 {
		t.Fatalf("the matrix holds %d rows, want 90 (3 arms x 2 pools x (2x3 single + 3x3 pair))", len(rows))
	}
	for _, row := range rows {
		t.Run(row.String(), func(t *testing.T) {
			w := newWaitWorld(t, row.arm, row.attached)
			for i, prov := range row.named {
				w.shape(t, prov, row.states[i])
			}
			wantNames := make([]string, len(row.named))
			for i, prov := range row.named {
				wantNames[i] = prov.name
			}
			previewed := w.previewRefusal(t, row.named)

			cr, err := w.launch("run-guarantee", row.named)
			var refusal *UnfundedProvidersError
			if !errors.As(err, &refusal) || !slices.Equal(refusal.Providers, wantNames) {
				t.Fatalf("premise: the launch = %v, want the unfunded refusal naming %v", err, wantNames)
			}
			w.rollback(cr)
			waits := refusal.Waits()
			if got := errors.Is(err, ErrCredentialAtCapacity); got != waits {
				t.Fatalf("errors.Is(ErrCredentialAtCapacity) = %v, Waits() = %v: %v", got, waits, err)
			}
			if waits && (strings.Contains(err.Error(), "provision") || strings.Contains(err.Error(), "one credential per run")) {
				t.Errorf("the wait's sentence asks for a credential to be provisioned, or says no wait funds the run: %v", err)
			}
			// Busy names a provider exactly when its entry would serve the
			// launch once free; a walk that consults no pool notes none.
			for i, prov := range row.named {
				want := row.states[i] == entryWouldServe && row.arm != waitArmSovereign
				if got := len(refusal.Busy[prov.name]) > 0; got != want {
					t.Errorf("Busy[%s] = %v, want noted=%v (state %s)", prov.name, refusal.Busy[prov.name], want, row.states[i])
				}
			}
			if previewed == "" {
				t.Fatalf("the preview predicts no refusal; the launch refused: %v", err)
			}
			if got := strings.Contains(previewed, ErrCredentialAtCapacity.Error()); got != waits {
				t.Errorf("the preview predicts the capacity class = %v, the launch's Waits() = %v\n  preview: %s\n  launch:  %v", got, waits, previewed, err)
			}

			w.free(t)
			_, again := w.launch("run-guarantee", row.named)
			var still *UnfundedProvidersError
			if again != nil && !errors.As(again, &still) {
				t.Fatalf("the relaunch failed outside the unfunded refusal: %v", again)
			}
			if started := again == nil; started != waits {
				t.Errorf("Waits() = %v, but once every busy entry freed the same request started = %v\n  refusal:  %v\n  relaunch: %v", waits, started, err, again)
			}
		})
	}
}
