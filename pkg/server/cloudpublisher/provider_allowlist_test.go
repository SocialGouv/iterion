// The gate half of the tenant's provider allowlist (ADR-129 §5) and the
// wire half: a PRIMARY route outside the frozen list refuses the launch
// naming node, provider and level; a rescue outside it is pruned (no
// credential sealed, the pool not asked); a route the list cannot check
// refuses; and the allowlist rides the wire (v=25), replayed from the run
// doc on resume, never re-resolved.
// Mutants: the gate removed → W-refusals never fire; the prune disabled in
// the walk → Providers keeps the forbidden provider and the fill seals a
// z.ai key (W-no-credential reds); the stamp sourced from anything but the
// frozen policy → W-stamp reds.

package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The two routing fixtures: a gateway primary with a zai rescue (the
// rescue is pruned, the launch proceeds), and an anthropic-pinned primary
// (the launch is refused).
const (
	gatewayZaiRescueBot         = "agent draft:\n  model: \"openai_compatible/gw/m\"\n  backend: \"claw\"\n  fallbacks:\n    zai:\n      provider: \"zai\"\n\nworkflow main:\n  draft -> done\n"
	anthropicPinnedZaiRescueBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n  fallbacks:\n    zai:\n      provider: \"zai\"\n\nworkflow main:\n  draft -> done\n"
	templatedRescueBot          = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claude_code\"\n  fallbacks:\n    fb:\n      provider: \"{{vars.fb}}\"\n\nworkflow main:\n  draft -> done\n"
)

// providersPolicy builds the frozen policy the launch/replay tests stamp.
func providersPolicy(providers ...string) *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		// A resolved policy always carries a pair order (the fold fills the
		// default); the tests stamp the shipped one so the slot whitelist
		// stays the launch's.
		PairOrder: append([]string(nil), llmroute.DefaultPairOrder...),
		Providers: providers,
		Sources:   map[string]string{llmroute.FieldProviders: "org"},
	}
}

// allowlistPublisher is the minimal publisher for the gate tests: an empty
// BYOK store (nothing funded — a sealed key can only be the leak), the
// sealing stack wired.
func allowlistPublisher(t *testing.T, seeds ...secrets.Provider) (*Publisher, *bytes.Buffer) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		panic(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	for _, prov := range seeds {
		seedKeyFP(t, keys, sealer, "team1", prov, "sk-"+string(prov), "fp-"+string(prov))
	}
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: usagecap.NewMemStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	return p, &buf
}

// sealedBundle opens what resolveAndSealCredentials sealed, the way the
// sibling tests do — the bundle, not the log line, is the evidence.
func sealedBundle(t *testing.T, p *Publisher, runID string, creds credResolution) secrets.RunBundle {
	t.Helper()
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	rec, err := rs.Get(store.WithTenant(context.Background(), "team1"), creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return b
}

// Witness 1, gate half. Under allowlist [anthropic] with BOTH keys seeded,
// the anthropic primary proceeds and the zai rescue funds NOTHING — the
// bundle seals the anthropic key and no z.ai key, though the tier holds
// one (the pruned rescue is not this tenant's spend). Under allowlist
// [openai_compatible], the same program REFUSES: the primary is outside
// the list with no allowed route left, naming node, provider and the
// level that set the list. Mutants: the prune disabled → the fill seals
// the z.ai key → the first half reds on its key assertion; the primary
// refusal disabled → the second half reds on its errors.Is.
func TestProviders_RescuePrunedPrimaryRefused(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	wf := compileTestSource(t, anthropicPinnedZaiRescueBot)
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-allow-1", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, providersPolicy("anthropic"))
	if err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the rescue is pruned, not refused", err)
	}
	b := sealedBundle(t, p, "run-allow-1", creds)
	if got := b.APIKeys[secrets.ProviderAnthropic]; got == "" {
		t.Fatalf("bundle = %+v, want the anthropic key sealed — the primary is inside the list", b.APIKeys)
	}
	if _, sealed := b.APIKeys[secrets.ProviderZAI]; sealed {
		t.Fatalf("bundle seals a z.ai key — the pruned rescue must fund no credential")
	}
	if _, sealed := b.PinnedAPIKeys[secrets.ProviderZAI]; sealed {
		t.Fatalf("bundle pins a z.ai key — the pruned rescue must fund no credential")
	}

	// The refusal half: the same program, the list narrowed past the primary.
	p2, _ := allowlistPublisher(t, secrets.ProviderZAI)
	wf2 := compileTestSource(t, anthropicPinnedZaiRescueBot)
	_, err = p2.resolveAndSealCredentials(ctx, "run-allow-2", "", "team1", "owner1", "", "", wf2, nil, nil, model.RouteView{Workflow: wf2}, nil, store.RunTrustDefault, nil, providersPolicy("openai_compatible"))
	if !errors.Is(err, model.ErrProviderOutsideAllowlist) {
		t.Fatalf("resolveAndSealCredentials = %v, want model.ErrProviderOutsideAllowlist — refused, never re-routed", err)
	}
	var outside *model.ProviderOutsideError
	if !errors.As(err, &outside) {
		t.Fatalf("error %v does not carry ProviderOutsideError", err)
	}
	if outside.Site != "draft" || outside.Provider != "anthropic" {
		t.Errorf("refusal names site=%q provider=%q, want draft/anthropic", outside.Site, outside.Provider)
	}
	if outside.Level != "org" {
		t.Errorf("refusal names level=%q, want org — the level that set the list", outside.Level)
	}
}

// Witness 2, gate half: a templated route the vars cannot answer, under a
// non-empty allowlist, refuses the launch naming the node and the field.
// Mutant: the readability obligation lifted (the gate ignores Unreadable)
// → the launch proceeds → this reds on errors.Is.
func TestProviders_UnreadableRouteRefusesUnderAnAllowlist(t *testing.T) {
	p, _ := allowlistPublisher(t)
	wf := compileTestSource(t, templatedRescueBot)
	ctx := store.WithTenant(context.Background(), "team1")
	_, err := p.resolveAndSealCredentials(ctx, "run-allow-3", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, providersPolicy("anthropic"))
	if !errors.Is(err, model.ErrRouteUnreadableUnderAllowlist) {
		t.Fatalf("resolveAndSealCredentials = %v, want model.ErrRouteUnreadableUnderAllowlist", err)
	}
	var unread *model.RouteUnreadableError
	if !errors.As(err, &unread) {
		t.Fatalf("error %v does not carry RouteUnreadableError", err)
	}
	if unread.Site != "draft#fb" {
		t.Errorf("refusal names site %q, want draft#fb", unread.Site)
	}
	if unread.Field != "provider" || unread.Hint != "{{vars.fb}}" {
		t.Errorf("refusal names field=%q hint=%q, want provider/{{vars.fb}}", unread.Field, unread.Hint)
	}
	// Without an allowlist the same launch proceeds (the refusal is the
	// list's doing, not the walk's default posture).
	p2, _ := allowlistPublisher(t)
	if _, err := p2.resolveAndSealCredentials(ctx, "run-allow-4", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil); err != nil {
		t.Fatalf("no-allowlist resolveAndSealCredentials = %v, want nil", err)
	}
}

// Witness 3: every reader of the walk's pruned set agrees — the pool want
// order never names the forbidden provider, and the preview's pinned set
// (its candidate filter) prunes it too. Mutant: any of the readers walked
// an un-pruned view → the want/pinned set names zai → this reds.
func TestProviders_NoCredentialSealedForAPrunedProvider(t *testing.T) {
	wf := compileTestSource(t, gatewayZaiRescueBot)
	view := model.RouteView{Workflow: wf, AllowedProviders: policyProviders(providersPolicy("openai_compatible"))}
	wants, res := wantsFor(view)
	if slices.ContainsFunc(wants, func(w credpool.Credential) bool { return strings.Contains(string(w.Source), "zai") }) {
		t.Errorf("pool wants = %v, want no zai — the pruned provider funds nothing", wants)
	}
	if slices.Contains(res.Providers, "zai") {
		t.Errorf("walk Providers = %v, want zai pruned", res.Providers)
	}
	if pinned := derivePinnedProviders(view); slices.Contains(pinned, "zai") {
		t.Errorf("pinned stamp = %v, want zai pruned — the fill and the gate must agree", pinned)
	}
}

// Witness 5: the allowlist rides the wire FROZEN — the launch stamps its
// resolved policy's list, the resume replays the run doc's, never a
// re-resolution (a level's list that moved since the launch does not
// retroactively govern a running run). Mutants: the resume stamp read from
// anything but prior.LLMRoutePolicy (nil, or a re-resolve) → this reds.
func TestProviders_ResumeReplaysTheFrozenList(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store: st,
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	ctx := store.WithIdentity(context.Background(), "team-a", "u1")
	wf := compileTestSource(t, gatewayZaiRescueBot)
	cs := &runview.CompiledSource{Hash: "hash"}
	spec := runview.LaunchSpec{FilePath: "wf.bot", Source: gatewayZaiRescueBot, LLMRoutePolicy: providersPolicy("openai_compatible")}
	if _, err := p.SubmitLaunch(ctx, "run-frz-1", spec, wf, cs); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil || !slices.Equal(published.LLMProviders, []string{"openai_compatible"}) {
		t.Fatalf("launch wire LLMProviders = %v, want the resolved policy's list", published)
	}
	if err := st.UpdateRunStatus(ctx, "run-frz-1", store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	published = nil
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: "run-frz-1", FilePath: "wf.bot", Source: gatewayZaiRescueBot}, wf, cs); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil {
		t.Fatal("the resume published nothing")
	}
	if !slices.Equal(published.LLMProviders, []string{"openai_compatible"}) {
		t.Fatalf("resume wire LLMProviders = %v, want the run doc's frozen list replayed", published.LLMProviders)
	}
}

// The ladder honours the list like every other builder of rescue routes: a
// held pair whose credential the allowlist excludes never becomes a rung,
// while the pairs inside it do. Mutant: the pair filter dropped → the
// zai_key rung returns → this reds.
func TestProviders_LadderDropsAPairOutsideTheAllowlist(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	wf := compileTestSource(t, anthropicPinnedBot)
	policy := providersPolicy("anthropic")
	policy.Triggers = []string{"usage_window", "unavailable"}
	policy.PairOrder = []string{"claw+anthropic_key", "claw+zai_key"}
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-allow-5", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	for _, st := range creds.Ladder {
		if st.Provider == "zai_key" {
			t.Fatalf("ladder carries a zai_key rung (%+v) — the allowlist excludes the pair", creds.Ladder)
		}
	}
	found := false
	for _, st := range creds.Ladder {
		if st.Provider == "anthropic_key" {
			found = true
		}
	}
	if !found {
		t.Errorf("ladder = %+v, want the anthropic_key rung kept — the filter prunes the forbidden pair only", creds.Ladder)
	}
}

// The door honours the list like every builder of rescue routes: a pair
// whose credential the allowlist excludes is never probed, so no donor is
// ever asked to fund it (the bundle lacks it precisely because the seal
// gate kept it out). Mutant: the door's pair filter dropped → the want
// returns → this reds.
func TestProviders_DoorNeverAsksForAPairOutsideTheAllowlist(t *testing.T) {
	wf := compileTestSource(t, anthropicPinnedBot)
	view := model.RouteView{Workflow: wf, AllowedProviders: []string{"anthropic"}}
	policy := providersPolicy("anthropic")
	policy.Triggers = []string{"usage_window", "unavailable"}
	policy.PairOrder = []string{"claw+anthropic_key", "claw+zai_key"}
	wants, _, ok := doorWantsFor(policy, view, nil, func(_, _ string) bool { return false })
	if !ok {
		t.Fatal("doorWantsFor = not ok, want the anthropic want kept")
	}
	for _, w := range wants {
		if w.Ref == string(secrets.ProviderZAI) {
			t.Fatalf("door wants = %+v, want no zai donation — the allowlist excludes the pair", wants)
		}
	}
	found := false
	for _, w := range wants {
		if w.Ref == string(secrets.ProviderAnthropic) {
			found = true
		}
	}
	if !found {
		t.Errorf("door wants = %+v, want the anthropic want kept — the filter prunes the forbidden pair only", wants)
	}
}

// The preview observes, never refuses — but a refusal the live launch will
// raise must be visible where the operator is looking (the same discipline
// that moved the fallback screen's refusals onto the timeline). Mutant:
// the warning block dropped → the preview stays silent → this reds.
func TestProviders_PreviewNamesTheRefusalTheLaunchWillRaise(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("openai_compatible")
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, anthropicPinnedZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	joined := strings.Join(v.Warnings, "\n")
	if !strings.Contains(joined, "The live launch refuses") || !strings.Contains(joined, "anthropic") {
		t.Errorf("warnings = %v, want the outside-allowlist refusal named with its provider", v.Warnings)
	}
}

// The seal gate is total over the slot vocabulary: a mapped slot answers
// its provider's membership, and a slot no provider maps to is not
// provably inside the list — refused (the same posture wantForSlot's
// refusal keeps), never silently admitted.
func TestProviders_SealGateIsTotalOverTheSlotVocabulary(t *testing.T) {
	pol := sharedTierPolicy{}.withProviderAllowlist(model.AllowedProviderSet([]string{"anthropic"}))
	if pol.sealableSlot("anthropic_key") != true {
		t.Error("anthropic_key must seal — its provider is inside the list")
	}
	if pol.sealableSlot("zai_key") != false {
		t.Error("zai_key must not seal — its provider is outside the list")
	}
	if pol.sealableSlot("not_a_slot") != false {
		t.Error("a slot no provider maps to must not seal — it is not provably inside the list")
	}
}

// The prefix-less model class at the gate: a launch whose route names
// nothing the walk can read refuses under a non-empty list (ADR-129 §5) —
// the run would otherwise ride whatever the pod's ambient precedence funds,
// providers the list forbids. Mutant: the widen tail records nothing → the
// walk leaves Unreadable empty → the launch proceeds → this reds.
func TestProviders_PrefixlessModelRefusesAtTheGate(t *testing.T) {
	const bareModelBot = "agent draft:\n  model: \"claude-opus-5\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic)
	wf := compileTestSource(t, bareModelBot)
	ctx := store.WithTenant(context.Background(), "team1")
	_, err := p.resolveAndSealCredentials(ctx, "run-bare", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, providersPolicy("openai"))
	if err == nil {
		t.Fatal("resolveAndSealCredentials = nil error, want the unreadable refusal — the route names no provider the list could check")
	}
	if !errors.Is(err, model.ErrRouteUnreadableUnderAllowlist) {
		t.Errorf("error = %v, want it to match ErrRouteUnreadableUnderAllowlist", err)
	}
	// The class twin: no list, the same launch walks (the gate is inert).
	p2, _ := allowlistPublisher(t, secrets.ProviderAnthropic)
	if _, err := p2.resolveAndSealCredentials(ctx, "run-bare-2", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil); err != nil {
		t.Errorf("no-allowlist resolveAndSealCredentials = %v, want nil", err)
	}
}

// A pruned rescue is named on the launch surface: the gate logs every
// Forbidden provider with its sites, and the preview carries the twin
// warning — the plan's stated mitigation ("the launch says it"). Mutant:
// the log removed → the buffer holds only the GRANTED line → this reds;
// the preview warning removed → the warnings name no prune → this reds.
func TestProviders_LaunchNamesThePrunedRescue(t *testing.T) {
	p, buf := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	wf := compileTestSource(t, anthropicPinnedZaiRescueBot)
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-named", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, providersPolicy("anthropic"))
	if err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the rescue is pruned, not refused", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "zai") || !strings.Contains(logged, "pruned") {
		t.Errorf("launch log = %s, want it to name the pruned provider (zai) and the prune", logged)
	}
	b := sealedBundle(t, p, "run-named", creds)
	if _, sealed := b.APIKeys[secrets.ProviderZAI]; sealed {
		t.Fatal("bundle seals a z.ai key — naming the prune must not un-prune it")
	}

	// The preview's twin.
	p2, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("anthropic")
	v, err := p2.PreviewCredentials(context.Background(), spec, compileTestSource(t, anthropicPinnedZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	joined := strings.Join(v.Warnings, " | ")
	if !strings.Contains(joined, "zai") || !strings.Contains(joined, "prune") {
		t.Errorf("preview warnings = %s, want the prune named", joined)
	}
}

// The preview's tier candidates read the PRUNED set: a slot whose provider
// the allowlist excludes is sealed by no tier, so no candidate is shown
// selected for a credential the live launch refuses to acquire — the same
// predicate the live key fill consults. Mutant: the allowlist join lifted
// from the preview's policy → the zai candidate reads "selected" → this
// reds on its selection assertion.
func TestProviders_PreviewCandidatesReadTheAllowlist(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("anthropic")
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, anthropicPinnedZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	sawZai := false
	for _, c := range v.Candidates {
		if c.Provider != string(secrets.ProviderZAI) {
			continue
		}
		sawZai = true
		if c.Selection == "selected" {
			t.Errorf("candidate provider=zai selection=%s — the live launch seals no zai key under the list", c.Selection)
		}
		if !strings.Contains(c.Reason, "allowlist") {
			t.Errorf("candidate provider=zai reason = %q, want the exclusion named", c.Reason)
		}
	}
	if !sawZai {
		t.Fatal("no zai candidate at all — the preview hides rather than explains; the tenant cannot read why")
	}
	// The inside candidate stays selected: the gate is the list's, not a
	// blanket refusal.
	sawAnthropicSelected := false
	for _, c := range v.Candidates {
		if c.Provider == string(secrets.ProviderAnthropic) && c.Selection == "selected" {
			sawAnthropicSelected = true
		}
	}
	if !sawAnthropicSelected {
		t.Error("no selected anthropic candidate — the gate must exclude only what the list forbids")
	}
}

// The keep twin of the candidate gate: under a list that ADMITS zai both
// candidates read selected — the gate excludes only what the list forbids,
// never the neighbor it carries. The [anthropic] witness above cannot see
// an over-broad gate: there zai is supposed to be excluded. Mutant: the
// allowed set built from a partial parse of the list (first entry only) →
// that witness stays green while this reds on the zai assertion.
func TestProviders_PreviewCandidatesKeepAnAdmittedProvider(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("anthropic", "zai")
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, anthropicPinnedZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	selected := map[string]bool{}
	for _, c := range v.Candidates {
		if c.Selection == "selected" {
			selected[c.Provider] = true
		}
	}
	for _, provider := range []string{string(secrets.ProviderAnthropic), string(secrets.ProviderZAI)} {
		if !selected[provider] {
			t.Errorf("provider %s not selected under a list that admits it — the gate excluded more than the list forbids", provider)
		}
	}
}

// providerOfSlot is the inverse the ladder, the door and the seal gate all
// read: total over the CLOSED pair vocabulary, like wantForSlot. A slot no
// provider maps to vanishes from all three under ANY non-empty allowlist —
// fail-closed but silent, the drift this test exists to catch when the
// vocabulary grows. Mutant: an arm dropped → this reds.
func TestProviderOfSlot_TotalOverThePairVocabulary(t *testing.T) {
	for _, pair := range llmroute.DefaultPairOrder {
		_, c, err := llmroute.ParsePair(pair)
		if err != nil {
			t.Fatalf("ParsePair(%q): %v", pair, err)
		}
		if providerOfSlot(c) == "" {
			t.Errorf("providerOfSlot(%q) = \"\" — map the slot or name the exception; under an allowlist its pairs are dropped from ladder, door and seal in silence", c)
		}
	}
	if got := providerOfSlot(llmroute.CredClaudeForfait); got != string(secrets.ProviderAnthropic) {
		t.Errorf("providerOfSlot(claude_forfait) = %q, want anthropic", got)
	}
	if got := providerOfSlot(llmroute.CredChatGPTForfait); got != string(secrets.ProviderOpenAI) {
		t.Errorf("providerOfSlot(chatgpt_forfait) = %q, want openai", got)
	}
}

// The oauth-class twin of the candidate gate: a forfait whose provider the
// allowlist excludes is sealed by no tier, so the preview marks it
// not_consulted and names the list — the same predicate the api candidates
// read, one slot vocabulary over (slotOfKind). Mutant: the gate lifted
// from the oauth stage → the claude_code candidate reads consulted → this
// reds on its selection assertion.
func TestProviders_PreviewOAuthCandidatesReadTheAllowlist(t *testing.T) {
	// No api keys seeded, and the bot's one route INSIDE the list but not
	// env-funded: the plan walks past the BYOK tier to the oauth one, whose
	// claude_code record spends anthropic — outside the list.
	const openaiBot = "agent draft:\n  model: \"openai/gpt-5.5\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	p, _ := allowlistPublisher(t)
	seedOAuthKind(t, p.oauthForfait, p.sealer, "owner1", secrets.OAuthKindClaudeCode)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("openai")
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, openaiBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	sawClaude := false
	for _, c := range v.Candidates {
		if c.Source != "oauth" || !strings.Contains(c.Provider, "claude") {
			continue
		}
		sawClaude = true
		if c.Selection == "selected" {
			t.Errorf("oauth candidate provider=%s selection=%s — the claude forfait spends anthropic, outside the list", c.Provider, c.Selection)
		}
		if !strings.Contains(c.Reason, "allowlist") {
			t.Errorf("oauth candidate provider=%s reason = %q, want the exclusion named", c.Provider, c.Reason)
		}
	}
	if !sawClaude {
		t.Fatal("no claude_code oauth candidate — the fixture did not reach the oauth stage")
	}
	// The class twin: no list, the same forfait is consulted.
	p2, _ := allowlistPublisher(t)
	seedOAuthKind(t, p2.oauthForfait, p2.sealer, "owner1", secrets.OAuthKindClaudeCode)
	spec2 := previewSpec("team1", "owner1")
	v2, err := p2.PreviewCredentials(context.Background(), spec2, compileTestSource(t, openaiBot))
	if err != nil {
		t.Fatalf("PreviewCredentials (no list): %v", err)
	}
	for _, c := range v2.Candidates {
		if c.Source == "oauth" && strings.Contains(c.Provider, "claude") && c.Selection == "not_consulted" {
			t.Errorf("no-allowlist oauth candidate provider=%s selection=%s — the gate must be the list's doing", c.Provider, c.Selection)
		}
	}
}

// An exclusion reason names the gate that fired: a slot the routing pair
// order holds out (a narrowed pair_order) reads "routing pair order", a
// slot the providers allowlist refuses reads "allowlist" — the operator
// widens the knob that actually holds the credential out. Mutant: the
// reason stops distinguishing the two gates (always the allowlist text) →
// this reds on its pair-order assertions.
func TestProviders_PreviewReasonNamesTheActualExcluder(t *testing.T) {
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic, secrets.ProviderZAI)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = &store.RunLLMRoutePolicy{
		PairOrder: []string{"claw+anthropic_key"},
		Providers: []string{"anthropic", "zai"},
		Sources:   map[string]string{},
	}
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, gatewayZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	sawZai := false
	for _, c := range v.Candidates {
		if c.Provider != string(secrets.ProviderZAI) {
			continue
		}
		sawZai = true
		if !strings.Contains(c.Reason, "routing pair order") || strings.Contains(c.Reason, "allowlist") {
			t.Errorf("zai candidate reason = %q — the allowlist ADMITS zai; the pair whitelist %v is the excluder", c.Reason, spec.LLMRoutePolicy.PairOrder)
		}
	}
	if !sawZai {
		t.Fatal("no zai candidate — the fixture did not reach the api stage")
	}

	// The whitelist-only arm: no providers list at all, the pair order
	// alone holds the slot out.
	spec2 := previewSpec("team1", "owner1")
	spec2.LLMRoutePolicy = &store.RunLLMRoutePolicy{
		PairOrder: []string{"claw+anthropic_key"},
		Sources:   map[string]string{},
	}
	v2, err := p.PreviewCredentials(context.Background(), spec2, compileTestSource(t, gatewayZaiRescueBot))
	if err != nil {
		t.Fatalf("PreviewCredentials (whitelist only): %v", err)
	}
	sawZai2 := false
	for _, c := range v2.Candidates {
		if c.Provider != string(secrets.ProviderZAI) {
			continue
		}
		sawZai2 = true
		if !strings.Contains(c.Reason, "routing pair order") || strings.Contains(c.Reason, "allowlist") {
			t.Errorf("whitelist-only zai candidate reason = %q — no list is set; the pair order is the excluder", c.Reason)
		}
	}
	if !sawZai2 {
		t.Fatal("no zai candidate in the whitelist-only arm")
	}
}

// The oauth twin: a forfait slot the pair order excludes while the
// allowlist admits reads "routing pair order".
func TestProviders_PreviewOAuthReasonNamesTheActualExcluder(t *testing.T) {
	const openaiBot = "agent draft:\n  model: \"openai/gpt-5.5\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	p, _ := allowlistPublisher(t)
	seedOAuthKind(t, p.oauthForfait, p.sealer, "owner1", secrets.OAuthKindClaudeCode)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = &store.RunLLMRoutePolicy{
		PairOrder: []string{"claw+anthropic_key"},
		Providers: []string{"anthropic", "openai"},
		Sources:   map[string]string{},
	}
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, openaiBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	sawClaude := false
	for _, c := range v.Candidates {
		if c.Source != "oauth" || !strings.Contains(c.Provider, "claude") {
			continue
		}
		sawClaude = true
		if !strings.Contains(c.Reason, "routing pair order") || strings.Contains(c.Reason, "allowlist") {
			t.Errorf("oauth candidate provider=%s reason = %q — the allowlist admits it; the pair whitelist excluded the slot", c.Provider, c.Reason)
		}
	}
	if !sawClaude {
		t.Fatal("no claude_code oauth candidate — the fixture did not reach the oauth stage")
	}
}

// The preview's unreadable warning carries the live refusal's remedies,
// observed here: the field is made readable, or the launch goes without
// the list. Widening cures no unreadable shape (the same route is
// unreadable under the widest list), so the warning must not suggest it —
// an operator following it re-launches into the same refusal. Mutant: the
// old text back → this reds on the no-widening assertion.
func TestProviders_PreviewUnreadableWarningNamesTheRealRemedy(t *testing.T) {
	const bareModelBot = "agent draft:\n  model: \"claude-opus-5\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"
	p, _ := allowlistPublisher(t, secrets.ProviderAnthropic)
	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = providersPolicy("openai")
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, bareModelBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	joined := strings.Join(v.Warnings, "\n")
	if !strings.Contains(joined, "The live launch refuses") || !strings.Contains(joined, "unreadable") {
		t.Fatalf("warnings = %v, want the unreadable refusal named", v.Warnings)
	}
	if strings.Contains(joined, "widen") {
		t.Errorf("warnings = %s, want them free of the remedy that cannot work", joined)
	}
	if !strings.Contains(joined, "resolve the field") || !strings.Contains(joined, "launch without the providers list") {
		t.Errorf("warnings = %s, want the two real remedies named", joined)
	}
}
