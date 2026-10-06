package cloudpublisher

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The routing fallback door (ADR-121 § Delivery 2, slice 5): a donor who
// marked `fallback_use` serves a FALLBACK rung of a run that holds its own
// credential; nobody else ever does.

// TestBrokerHasFallbackDonors: the probe answers for a consenting donor of
// a WANTED kind, and no for everything else — including a consenting donor
// of an unwanted kind and a non-consenting donor of a wanted one.
func TestBrokerHasFallbackDonors(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	req := credpool.Request{OrgID: poolOrg, TenantID: poolTeam, UserID: "requester",
		Wants: []credpool.Credential{{Source: credpool.SourceAPIKey, Ref: "zai"}}, FallbackOnly: true}
	ok, err := f.broker.HasFallbackDonors(context.Background(), req)
	if err != nil || !ok {
		t.Fatalf("probe = %v, %v — want true (the consenting donor serves the wanted kind)", ok, err)
	}
	req.Wants = []credpool.Credential{{Source: credpool.SourceAPIKey, Ref: "moonshot"}}
	if ok, _ := f.broker.HasFallbackDonors(context.Background(), req); ok {
		t.Fatal("the probe answered for an UNWANTED kind — a consult would walk for nothing")
	}
	f2 := newDoorFixture(t, false, nil)
	req2 := credpool.Request{OrgID: poolOrg, TenantID: poolTeam, UserID: "requester",
		Wants: []credpool.Credential{{Source: credpool.SourceAPIKey, Ref: "zai"}}, FallbackOnly: true}
	if ok, _ := f2.broker.HasFallbackDonors(context.Background(), req2); ok {
		t.Fatal("the probe answered for a non-consenting pool — the zero-opt-in landscape must cost no walk")
	}
}

// doorFixture: a tenant holding ONLY an anthropic key, a policy whose
// ladder names a zai_key rung (missing), and a zai donor whose consent is
// the case's variable.
type doorFixture struct {
	pub      *Publisher
	broker   *credpool.Broker
	logs     *bytes.Buffer
	pledges  *credpool.MemoryPledgeStore
	donorKey *secrets.ApiKey
}

func newDoorFixture(t *testing.T, consenting bool, wfPrep func(*ir.Workflow)) *doorFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	// The TENANT's own key: the bundle is non-empty, which is the door's
	// premise (a run with its own money).
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, poolTeam, secrets.ProviderAnthropic, "sk-ant-tenant")
	// The DONOR's zai key, pledged the way the whole-bundle path pledges.
	apiKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, keyID, []byte("zai-donated-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderZAI, Name: "lent", SealedSecret: sealed,
		Fingerprint: secrets.FingerprintSHA256("zai-donated-key"),
	}); err != nil {
		t.Fatalf("seed donor key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "zai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai", KeyID: keyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
		FallbackUse: consenting,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: apiKeys, Sealer: sealer, Logger: testLogger(),
	})
	logs := &bytes.Buffer{}
	p := &Publisher{
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   broker,
		logger:     iterlog.New(iterlog.LevelInfo, logs),
	}
	return &doorFixture{pub: p, broker: broker, logs: logs, pledges: pledges, donorKey: &secrets.ApiKey{ID: keyID}}
}

// doorPolicy names a ladder whose zai rung the one-key tenant lacks. The
// anthropic rung is held and materializes through the ordinary path.
func doorPolicy() *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{
			llmroute.Pair(llmroute.HarnessClaw, "anthropic_key"),
			llmroute.Pair(llmroute.HarnessClaw, "zai_key"),
		},
		Triggers: []string{llmroute.TriggerUsageWindow},
	}
}

// doorWf is a node the claw rungs can land on: tools declared-empty (the
// tools-inversion predicate) and no session (the S3 contract's own).
func doorWf() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{
			Backend: "claude_code",
			Model:   "claude-opus-5-5",
		},
		Tools: []string{},
	}}}
}

func (f *doorFixture) resolve(t *testing.T, runID string, policy *store.RunLLMRoutePolicy, wf *ir.Workflow) (secrets.RunBundle, credResolution) {
	t.Helper()
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, runID, poolOrg, poolTeam, "requester", "docs-refresh", "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, policy)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}, creds
	}
	rec, err := f.pub.runSecrets.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle, creds
}

// TestFallbackDoor_consentedDonorServesTheMissingRung: the door seals the
// donor's key into the missing slot AND the granted pair materializes —
// a door sealed after HeldPairs would grant a credential no rung
// dispatches, which is what the Ladder assertion pins.
// Mutant: the door moved after the ladder block → this test reds on the
// Ladder assertion; the filter dropped → the unconsenting case reds.
func TestFallbackDoor_consentedDonorServesTheMissingRung(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	wf := doorWf()
	bundle, creds := f.resolve(t, "run-door", doorPolicy(), wf)

	if creds.doorGrant == nil {
		t.Fatalf("no door grant; log:\n%s", f.logs.String())
	}
	if creds.grant != nil {
		t.Fatal("a door-served run also holds a whole-bundle grant — the platform-skip and budget-clamp sites read this field and would misfire")
	}
	if creds.doorGrant.Source != credpool.SourceAPIKey || creds.doorGrant.Ref != "zai" {
		t.Fatalf("door grant = %+v, want the donor's zai key", creds.doorGrant)
	}
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "zai-donated-key" {
		t.Fatalf("sealed zai key = %q, want the donor's", got)
	}
	if !bundle.PoolSourced["zai"] {
		t.Fatal("the door grant is not PoolSourced — the meter would bill the wrong record")
	}
	// The granted pair materialized: the ladder (and the wire) carries it.
	found := false
	for _, e := range creds.Ladder {
		if e.Backend == "claw" && e.Provider == "zai_key" && e.Policy {
			found = true
		}
	}
	if !found {
		t.Fatalf("ladder = %+v, want the granted claw+zai_key pair — a door sealed after HeldPairs grants a credential no rung dispatches", creds.Ladder)
	}
	// The program carries EXACTLY the two materialized rungs — the held
	// anthropic pair and the door-granted zai pair — and no probe
	// artifact: a strip that leaked would duplicate a rung here.
	node := wf.Nodes["implement"].(*ir.AgentNode)
	policyRungs := 0
	for _, fb := range node.Fallbacks {
		if fb.Policy {
			policyRungs++
		}
	}
	if policyRungs != 2 {
		t.Fatalf("policy rungs on the node = %d, want exactly 2 (held anthropic + door-granted zai) — a probe artifact leaked", policyRungs)
	}
}

// TestFallbackDoor_unconsentingDonorIsInvisible: a pool whose donor never
// marked fallback_use is exactly as invisible to the door as no pool —
// no grant, no sealed key, no rung, no consult log.
func TestFallbackDoor_unconsentingDonorIsInvisible(t *testing.T) {
	f := newDoorFixture(t, false, nil)
	wf := doorWf()
	bundle, creds := f.resolve(t, "run-silent", doorPolicy(), wf)

	if creds.doorGrant != nil {
		t.Fatalf("door grant = %+v — the donor never consented", creds.doorGrant)
	}
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "" {
		t.Fatalf("zai key sealed = %q — the door served an unconsenting pledge", got)
	}
	for _, e := range creds.Ladder {
		if e.Provider == "zai_key" {
			t.Fatalf("ladder carries %s — the missing kind must stay missing", e.Provider)
		}
	}
	if strings.Contains(f.logs.String(), "fallback door consulted") {
		t.Fatal("the door CONSULTED with zero opted-in donors — the probe must amortize the walk away")
	}
	// The tenant's own key is untouched.
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-tenant" {
		t.Fatalf("the tenant's own key = %q — the door must not disturb the tiers below", got)
	}
}

// TestFallbackDoor_screenRefusalArmsNothing: a pair the REAL screen
// refuses for every node (here: an unresolvable-tools claw crossing —
// the node's tools are UNDECLARED, so the tools-inversion refusal holds)
// arms no want, even with a consenting donor. Asking the pool for it
// would burn the one grant on a rung that never dispatches.
func TestFallbackDoor_screenRefusalArmsNothing(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	wf := doorWf()
	// Undeclared tools: the claw stage's tools-inversion refusal holds.
	wf.Nodes["implement"] = &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5-5"},
	}
	bundle, creds := f.resolve(t, "run-refused", doorPolicy(), wf)
	if creds.doorGrant != nil {
		t.Fatalf("door grant = %+v — a screen-refused pair must arm no want", creds.doorGrant)
	}
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "" {
		t.Fatalf("zai sealed = %q", got)
	}
}

// TestFallbackDoor_wholeBundleGrantShutsTheDoor: a run with nothing of
// its own is the whole-bundle tier's case — the door must not stack a
// second consult behind it.
func TestFallbackDoor_wholeBundleGrantShutsTheDoor(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	// No tenant key: strip the fixture's own credential.
	f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
	wf := doorWf()
	bundle, creds := f.resolve(t, "run-empty", doorPolicy(), wf)
	if creds.grant == nil {
		t.Fatalf("no whole-bundle grant; log:\n%s", f.logs.String())
	}
	if creds.doorGrant != nil {
		t.Fatal("the door fired behind a whole-bundle grant — the empty bundle is that tier's case, not the door's")
	}
	if len(bundle.OAuthCredentials) == 0 && len(bundle.APIKeys) == 0 {
		t.Fatal("nothing sealed at all — the fixture broke")
	}
}

// TestCredResolution_poolLease: the lease disjunction the publication
// plumbing reads — the whole-bundle grant first, the door grant as the
// fallback, nil when neither.
// Mutant: poolLease returning creds.grant alone → the door-grant case
// reds (a door-served run would read grantless: its spend never reaches
// the broker and its lease closes as superseded at publication).
func TestCredResolution_poolLease(t *testing.T) {
	whole := &credpool.Grant{PledgeID: "whole"}
	door := &credpool.Grant{PledgeID: "door"}
	if got := (credResolution{grant: whole, doorGrant: door}).poolLease(); got != whole {
		t.Fatalf("poolLease = %v, want the whole-bundle grant first", got)
	}
	if got := (credResolution{doorGrant: door}).poolLease(); got != door {
		t.Fatalf("poolLease = %v, want the door grant when no whole-bundle grant exists", got)
	}
	if got := (credResolution{}).poolLease(); got != nil {
		t.Fatalf("poolLease = %v, want nil", got)
	}
}

// TestWantForSlot_totalOverTheVocabulary: the inverse of the sealing maps
// covers the CLOSED pair vocabulary — both forfaits, every known provider
// key, nothing else.
// Mutant: any mapping arm flipped to false (or a provider dropped) → this
// test reds; the exhaustiveness is what keeps a rung from arming silently
// for nothing after a vocabulary extension.
func TestWantForSlot_totalOverTheVocabulary(t *testing.T) {
	w, ok := wantForSlot(llmroute.CredClaudeForfait)
	if !ok || w != (credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}) {
		t.Fatalf("claude_forfait → %+v ok=%v, want the claude_code OAuth want", w, ok)
	}
	w, ok = wantForSlot(llmroute.CredChatGPTForfait)
	if !ok || w != (credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindCodex)}) {
		t.Fatalf("chatgpt_forfait → %+v ok=%v, want the codex OAuth want", w, ok)
	}
	for _, prov := range allKnownProviders {
		w, ok := wantForSlot(slotOfProvider(prov))
		if !ok || w != (credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(prov)}) {
			t.Fatalf("%s → %+v ok=%v, want the provider's api_key want", prov, w, ok)
		}
	}
	if _, ok := wantForSlot("pool"); ok {
		t.Fatal("the retired `pool` slot must not map")
	}
	if _, ok := wantForSlot("unknown_key"); ok {
		t.Fatal("an unknown provider key must not map")
	}
}

// TestDoorWantsFor_restoresTheProgramByteExactly: the probe rungs the
// derivation lands are stripped before it returns — the workflow reads
// exactly as it did on entry. This is what lets a read-only preview run
// the derivation, and what keeps a probe artifact from feeding the launch
// derivations that read the program after it.
// Mutant: the strip loop removed from doorWantsFor → this test reds on the
// DeepEqual (and nothing else in the suite would).
func TestDoorWantsFor_restoresTheProgramByteExactly(t *testing.T) {
	wf := doorWf()
	policy := doorPolicy()
	before := wf.Nodes["implement"].(*ir.AgentNode).Fallbacks
	snapshot := make([]ir.Fallback, len(before))
	copy(snapshot, before)

	wants, unmapped, ok := doorWantsFor(policy, wf, model.ModelOverrides{}, func(_, credential string) bool {
		// Both pairs held: NOTHING is missing, the derivation probes
		// nothing — the trivially-restoring case.
		return true
	})
	if ok || len(wants) != 0 || len(unmapped) != 0 {
		t.Fatalf("all-held derivation = %+v unmapped=%v ok=%v, want nothing armed", wants, unmapped, ok)
	}
	after := wf.Nodes["implement"].(*ir.AgentNode).Fallbacks
	if len(after) != len(snapshot) {
		t.Fatalf("fallbacks = %d, want %d — the all-held path mutated the program", len(after), len(snapshot))
	}
	for i := range snapshot {
		if !reflect.DeepEqual(snapshot[i], after[i]) {
			t.Fatalf("fallbacks[%d] changed: %+v → %+v", i, snapshot[i], after[i])
		}
	}

	// The PROBING case: zai missing, the rung lands on the node, the strip
	// restores. The pre-probe snapshot is the same two-rung-free program.
	wants, _, ok = doorWantsFor(policy, wf, model.ModelOverrides{}, func(_, credential string) bool {
		return credential == "anthropic_key"
	})
	if !ok || len(wants) != 1 || wants[0] != (credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai"}) {
		t.Fatalf("door wants = %+v ok=%v, want exactly the zai api_key", wants, ok)
	}
	after = wf.Nodes["implement"].(*ir.AgentNode).Fallbacks
	if len(after) != len(snapshot) {
		t.Fatalf("after probe: fallbacks = %d, want %d — a probe rung leaked onto the program", len(after), len(snapshot))
	}
	for i := range snapshot {
		if !reflect.DeepEqual(snapshot[i], after[i]) {
			t.Fatalf("after probe: fallbacks[%d] changed: %+v → %+v", i, snapshot[i], after[i])
		}
	}
}

// TestDoorServes_pinnedKeyFundsItsSlot: a provider the run holds as a
// PINNED key is a held slot — the door never asks for it (a donation
// beside a pinned key would take the wire's default over).
// Mutant: the PinnedAPIKeys loop dropped from doorServes → this test reds.
func TestDoorServes_pinnedKeyFundsItsSlot(t *testing.T) {
	bundle := secrets.RunBundle{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "own"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "pinned-for-its-routes"},
	}
	serves := doorServes(bundle)
	if !serves("claw", "anthropic_key") || !serves("claw", "zai_key") {
		t.Fatal("a pinned key must fund its slot exactly like a sealed one")
	}
	if serves("claw", "chatgpt_forfait") {
		t.Fatal("an unfunded slot must read missing")
	}
}

// TestAcquireDoorGrant_guards: the three structural refusals — a
// whole-bundle grant, an env-funded run, an empty bundle — each shut the
// door before anything runs.
// Mutants: any guard dropped → its case reds.
func TestAcquireDoorGrant_guards(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	policy := doorPolicy()
	wf := doorWf()
	bundle := secrets.RunBundle{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "own"}}
	sealable := func(string) bool { return true }
	ctx := context.Background()
	args := []any{}
	_ = args

	// A whole-bundle grant: the whole-bundle tier's case, never the door's.
	if g := f.pub.acquireDoorGrant(ctx, "r", poolOrg, poolTeam, "requester", "bot", wf, model.ModelOverrides{}, policy, &bundle, &credpool.Grant{}, false, map[secrets.Provider]string{}, sealable); g != nil {
		t.Fatal("the door fired behind a whole-bundle grant")
	}
	// An env-funded run acquires nothing at all (#2038).
	if g := f.pub.acquireDoorGrant(ctx, "r", poolOrg, poolTeam, "requester", "bot", wf, model.ModelOverrides{}, policy, &bundle, nil, true, map[secrets.Provider]string{}, sealable); g != nil {
		t.Fatal("the door fired on an env-funded run")
	}
	// An empty bundle belongs to the whole-bundle tier.
	if g := f.pub.acquireDoorGrant(ctx, "r", poolOrg, poolTeam, "requester", "bot", wf, model.ModelOverrides{}, policy, &secrets.RunBundle{}, nil, false, map[secrets.Provider]string{}, sealable); g != nil {
		t.Fatal("the door fired on an empty bundle")
	}
}

// TestFallbackDoor_crossHarnessSessionRungArms: under an ACTIVE posture, a
// SESSION-bearing node's crossings land at the screen (the third state) —
// and the door's probe must see the same thing the real screen sees. A
// probe without the posture would refuse the crossing, arm no want, and
// the door would never serve the very posture slice 3 introduced.
// Mutant: the probe stages built without CrossHarness → this test reds.
func TestFallbackDoor_crossHarnessSessionRungArms(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	policy := doorPolicy()
	policy.CrossHarness = llmroute.CrossHarnessReuse
	wf := doorWf()
	// The session the third state is about: the crossings would refuse
	// without the posture riding the stages.
	wf.Nodes["implement"] = &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5-5"},
		Tools:     []string{},
		Session:   ir.SessionInherit,
	}
	bundle, creds := f.resolve(t, "run-ch", policy, wf)
	if creds.doorGrant == nil {
		t.Fatalf("no door grant under the active posture; log:\n%s", f.logs.String())
	}
	found := false
	for _, e := range creds.Ladder {
		if e.Backend == "claw" && e.Provider == "zai_key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ladder = %+v, want the granted pair through the session node", creds.Ladder)
	}
	_ = bundle
}
