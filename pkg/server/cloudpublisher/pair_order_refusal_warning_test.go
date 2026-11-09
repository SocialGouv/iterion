// ADR-131 §5's second half: a run replays its FROZEN routing pair order as
// stored, and says ONCE what that list held out. The replay-as-stored half
// was already covered (publisher.go hands the run document's own snapshot
// back to the walk on a resume); the warning did not exist. Seven sites
// passed a credential over because the frozen list names no pair for its
// slot, every one a silent continue/break — publisher.go's tenant key fill,
// tenant forfait fill, key restore, forfait restore and platform forfait
// fill, plus org_tier.go's two — and the eighth, publisher.go's platform key
// fill, refused the slot before reading its store, so it held nothing to
// name. All eight now ask the gate with the credential in hand, and the three
// KEY legs ask it one step earlier as well — inside their usability predicate
// (heldSlotUsable) — where a slot the frozen list refuses answers before its
// meter is read, so it arms no speculative usage-window retry (W12).
//
// Mutants these witnesses red on: the note removed from sealableHeldSlot (the
// chokepoint — main's silence put back) → W1/W3/W4/W6; the key fill AND key
// restore reduced to the pure gate → W1/W3/W4 (the key leg's two sites); the
// forfait fill reduced to it → W6 (the kind leg); the emission dropped from
// the walk's end → W1/W3/W4/W6; the emission no longer gated on a non-empty
// set → W2 (a line on every launch is one nobody reads); the default-source
// arm of the provenance collapsed → W3; the note keyed on the GATE instead of
// the frozen list → W7 (the provider allowlist's refusals would be reported
// as the pair order's); the dropped-pin guard removed from warnRefusedPins →
// W5 sees "the pin is honoured" about a key the walk never sealed; the
// allowlist arm dropped from the pin's switch, or excluderOfSlot made to
// answer the pair order always → W8 (and the preview's own allowlist
// witnesses, which read the same judgement); either provenance arm of the
// emission's LEVEL collapsed into the other → W1 (info) or W3 (warn); the
// empty-list arm put back into newPairOrderRefusals → W9; the allowlist
// clause dropped from the remedy, the allowlist never threaded into the
// record, or the clause made to name every held-out slot instead of the ones
// that list refuses → W10; the org and platform fills reduced to the pure
// gate → W11's org_forfait and platform_forfait rows; the gate dropped from
// one key leg's usability predicate → W12's row for that leg, each pinned
// alone; main's shape at the platform key leg (the gate back in the `missing`
// derivation, where the slot reaches no resolve) → W11's and W12's
// platform_key rows; the FUNDING leg of the level dropped → W1, its
// provenance leg dropped → W13; the pin's pair-order clause dropped, or the
// record not threaded into warnRefusedPins → W14; refuses() made to answer
// always → W7, W8 and W10, the three readers of that one judgement; a
// provider appended to allKnownProviders → W15, beside the package's own
// inertness tripwire.

package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// heldOutMarker is the one line §5 owes; every count below is of THIS
// substring, so a second emission anywhere in the walk is a failure.
const heldOutMarker = "held OUT by this run's frozen routing pair order"

// markerLevel is the LEVEL of the one §5 line in a capture, read off the
// prefix pkg/log writes. The level is part of what the line says: a warn is
// also a tracker breadcrumb (pkg/errtrack's warn+ hook), so a run whose
// configuration nobody got wrong must not produce one.
func markerLevel(t *testing.T, log string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, heldOutMarker) {
			continue
		}
		switch {
		case strings.Contains(line, "⚠️"):
			return "warn"
		case strings.Contains(line, "ℹ️"):
			return "info"
		}
		t.Fatalf("the §5 line carries no level prefix: %q", line)
	}
	t.Fatalf("no §5 line to read a level from:\n%s", log)
	return ""
}

const pairOrderBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"

// pairOrderPolicy is a frozen snapshot naming ONLY the Claude forfait pair:
// an anthropic key is resolved by the walk and sealed by no tier.
func pairOrderPolicy(source string) *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait)},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: source},
	}
}

// pairOrderPublisher is the sealing stack with ONE tenant anthropic key and a
// capturing logger; caps is returned so a witness can give that key refusal
// evidence.
func pairOrderPublisher(t *testing.T) (*Publisher, *usagecap.MemStore, *bytes.Buffer) {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-anthropic", "fp-anthropic")
	caps := usagecap.NewMemStore()
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: caps, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	return p, caps, &buf
}

// Witness 1: a narrowed frozen order plus a tenant key for an excluded slot
// → EXACTLY ONE line, naming the slot and the level that won the list. The
// walk refuses that slot at the fill and would refuse it again at every
// later stage, so the count is the assertion.
func TestPairOrder_HeldOutCredentialIsNamedOnceWithItsProvenance(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-1", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, pairOrderPolicy(llmroute.SourceTeam), nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — a narrowed pair order holds a credential out, it does not refuse the launch", err)
	}
	if creds.secretsRef != "" {
		t.Fatalf("a bundle was sealed (%q) although the frozen order names no pair for anthropic_key", creds.secretsRef)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1 — the walk refuses the slot at several stages and owes ONE line:\n%s", got, log)
	}
	for _, want := range []string{"run=run-po-1", "anthropic_key", "pair_order source=team", "ADR-131",
		// The remedy is bounded by the replay: widening the level lifts the
		// slot for the NEXT launch, and an operator who widens it for THIS
		// run seals nothing and loses the line that named the slot.
		"only a fresh launch picks the change up"} {
		if !strings.Contains(log, want) {
			t.Errorf("the §5 line does not carry %q:\n%s", want, log)
		}
	}
	// This run sealed NOTHING: the narrowing cost it its only credential, so
	// this line is the ONLY explanation of the "no credential resolved"
	// warning beside it. Said below that warning's level, a deployment
	// reading warn+ (a supported ITERION_LOG_LEVEL, and pkg/errtrack's hook)
	// gets the symptom with no cause at all.
	if !strings.Contains(log, "no credential resolved for run=run-po-1") {
		t.Fatalf("the fixture produces no unfunded symptom, so the level below proves nothing:\n%s", log)
	}
	if got := markerLevel(t, log); got != "warn" {
		t.Errorf("a narrowing that left the run unfunded is reported at %s level, want warn — its symptom is a warn:\n%s", got, log)
	}
}

// Witness 2: the SHIPPED default order holds nothing out — its entries cover
// every sealable slot — so the walk is silent and the key is sealed. Without
// this half, a line that fired on every launch would pass witness 1.
func TestPairOrder_TheShippedDefaultHoldsNothingOut(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	ctx := store.WithTenant(context.Background(), "team1")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: append([]string(nil), llmroute.DefaultPairOrder...),
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault},
	}
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-2", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if b := sealedBundle(t, p, "run-po-2", creds); b.APIKeys[secrets.ProviderAnthropic] != "sk-anthropic" {
		t.Fatalf("the default order sealed %+v, want the anthropic key — the probe must exercise a walk that CAN seal", b.APIKeys)
	}
	if strings.Contains(buf.String(), heldOutMarker) {
		t.Fatalf("the shipped default held a credential out:\n%s", buf.String())
	}
}

// Witness 3: the pre-upgrade case the clause is about — a list won by the
// DEFAULT of the launch day. The line says so, because the remedy differs: a
// fresh launch resolves today's default, where a tenant's own list must be
// widened where it is written.
func TestPairOrder_ADefaultSourcedListIsNamedAsTheLaunchDaysDefault(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-po-3", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, pairOrderPolicy(llmroute.SourceDefault), nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
	}
	for _, want := range []string{"SHIPPED DEFAULT", "pair_order source=default", "fresh launch"} {
		if !strings.Contains(log, want) {
			t.Errorf("a default-sourced list is not named as the launch day's default (%q missing):\n%s", want, log)
		}
	}
	// THIS is the shape §5 calls a warning: no level chose the list, so the
	// narrowing is the launch day's default and nobody stated it.
	if got := markerLevel(t, log); got != "warn" {
		t.Errorf("the pre-upgrade shape is reported at %s level, want warn:\n%s", got, log)
	}
}

// Witness 4: a RESUME replays the frozen list from the run document and says
// the same thing once. The resume is the half of §5 that cannot be read off
// a launch: nothing re-resolves the list, so the line must come from the
// stored snapshot.
func TestPairOrder_AResumeReplayingTheFrozenOrderSaysItOnce(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p, _, buf := pairOrderPublisher(t)
	p.store = st
	var published *queue.RunMessage
	p.publishRun = func(_ context.Context, m *queue.RunMessage) error {
		published = m
		return nil
	}
	ctx := store.WithIdentity(context.Background(), "team1", "owner1")
	const runID = "run-po-resume"
	wf := compileTestSource(t, pairOrderBot)
	cs := &runview.CompiledSource{Hash: "hash"}
	if _, err := p.SubmitLaunch(ctx, runID, runview.LaunchSpec{
		FilePath:       "wf.bot",
		Source:         pairOrderBot,
		LLMRoutePolicy: pairOrderPolicy(llmroute.SourceBot),
	}, wf, cs); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if published == nil {
		t.Fatal("the launch published nothing — the fixture must reach the queue for the resume to be the second act")
	}
	launched, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after launch: %v", err)
	}
	if launched.LLMRoutePolicy == nil || len(launched.LLMRoutePolicy.PairOrder) != 1 {
		t.Fatalf("the launch froze pair_order=%v, want the one-pair list — the resume replays THIS", launched.LLMRoutePolicy)
	}
	if got := strings.Count(buf.String(), heldOutMarker); got != 1 {
		t.Fatalf("the launch said the §5 line %d time(s), want exactly 1:\n%s", got, buf.String())
	}

	// The resume's own log: the frozen list comes back from the document.
	buf.Reset()
	if err := st.UpdateRunStatus(ctx, runID, store.RunStatusPausedWaitingHuman, ""); err != nil {
		t.Fatalf("park the run: %v", err)
	}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: pairOrderBot}, wf, cs); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the resume said the §5 line %d time(s), want exactly 1 — the frozen list is replayed as stored, and says so:\n%s", got, log)
	}
	for _, want := range []string{"run=" + runID, "anthropic_key", "pair_order source=bot"} {
		if !strings.Contains(log, want) {
			t.Errorf("the resume's §5 line does not carry %q:\n%s", want, log)
		}
	}
}

// Witness 5, the latent defect beside the clause: a webhook pin on a key
// whose slot the frozen order refuses is DROPPED by the fill, and the pin
// warning used to say "the pin is honoured, so this run will meet that wall"
// about it — a wall the run never meets, since nothing of that key is
// sealed. With no refusal evidence at all, the operator heard nothing.
func TestPairOrder_ADroppedPinIsNamedAsDroppedNotHonoured(t *testing.T) {
	p, caps, buf := pairOrderPublisher(t)
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-anthropic")
	pinned := firstKeyWithFingerprint(t, p.apiKeys, "fp-anthropic")
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-pin", "", "team1", "owner1", "", "", nil,
		map[string]string{string(secrets.ProviderAnthropic): pinned.ID}, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, pairOrderPolicy(llmroute.SourceTeam), nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef != "" {
		t.Fatalf("the pinned key was sealed (%q) although the frozen order names no pair for its slot", creds.secretsRef)
	}
	log := buf.String()
	if strings.Contains(log, "the pin is honoured") {
		t.Fatalf("a pin the walk DROPPED is reported as honoured, promising a wall this run never meets:\n%s", log)
	}
	if !strings.Contains(log, "the pin is DROPPED") {
		t.Fatalf("the dropped pin is never named — the operator gets neither their key nor a word about it:\n%s", log)
	}
	for _, want := range []string{pinned.ID, "anthropic_key", "repin"} {
		if !strings.Contains(log, want) {
			t.Errorf("the dropped-pin line does not carry %q:\n%s", want, log)
		}
	}
}

// Witness 6, the other slot vocabulary: the frozen order names only the claw
// key pair, so the tenant's CLAUDE FORFAIT is held out while its key is
// sealed. The forfait sites gate on slotOfKind, the key sites on
// slotOfProvider — one leg green proves nothing about the other.
func TestPairOrder_AHeldOutForfaitIsNamedByItsSlot(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "tok-claude")
	ctx := store.WithTenant(context.Background(), "team1")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourcePlatform},
	}
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-6", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := sealedBundle(t, p, "run-po-6", creds)
	if b.APIKeys[secrets.ProviderAnthropic] != "sk-anthropic" {
		t.Fatalf("the admitted key slot sealed %+v, want the anthropic key", b.APIKeys)
	}
	if _, sealed := b.OAuthCredentials[string(secrets.OAuthKindClaudeCode)]; sealed {
		t.Fatal("the forfait was sealed although the frozen order names no claude_forfait pair")
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
	}
	if !strings.Contains(log, llmroute.CredClaudeForfait) {
		t.Errorf("the §5 line does not name the held-out forfait slot:\n%s", log)
	}
	if strings.Contains(log, "anthropic_key") {
		t.Errorf("the §5 line names a slot that WAS sealed:\n%s", log)
	}
	// FUNDED, and a named level chose the list: the configuration working.
	// Reported on every launch and every resume of it, so a warn would be a
	// per-launch tracker breadcrumb about a list nobody got wrong — the
	// ADR-129 §5 prune line's level, not an alarm.
	if strings.Contains(log, "no credential resolved") {
		t.Fatalf("the fixture left the run unfunded, so the level below is not the funded case:\n%s", log)
	}
	if got := markerLevel(t, log); got != "info" {
		t.Errorf("a FUNDED narrowing a named level chose is reported at %s level, want info:\n%s", got, log)
	}
}

// Witness 7, the attribution: a slot the tenant's PROVIDER ALLOWLIST excludes
// (ADR-129 §5) is refused by the same gate, and the prune line at the head of
// the walk owns it — §5's line must stay silent, or two lists refusing one
// slot read as two refusals of the same kind.
func TestPairOrder_AnAllowlistRefusalIsNotThePairOrdersLine(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai", "fp-zai")
	ctx := store.WithTenant(context.Background(), "team1")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: append([]string(nil), llmroute.DefaultPairOrder...),
		Providers: []string{"anthropic"},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault, llmroute.FieldProviders: llmroute.SourceOrg},
	}
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-7", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := sealedBundle(t, p, "run-po-7", creds)
	if _, sealed := b.APIKeys[secrets.ProviderZAI]; sealed {
		t.Fatal("the z.ai key was sealed although the allowlist excludes its provider — the probe needs a slot the gate refuses")
	}
	if strings.Contains(buf.String(), heldOutMarker) {
		t.Fatalf("a provider-allowlist refusal is reported as the frozen pair order's:\n%s", buf.String())
	}
}

// Witness 8, the same attribution at the PIN leg: the pin's fate is judged
// against the gate the walk seals through — the pair order COMPOSED with the
// providers allowlist — so a pin the ALLOWLIST dropped must name the
// allowlist. Naming the pair order there prescribes widening a list that
// already admits the slot, and the fixture guard below proves it does.
func TestPairOrder_AnAllowlistDroppedPinNamesTheAllowlistNotThePairOrder(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai", "fp-zai")
	pinned := firstKeyWithFingerprint(t, p.apiKeys, "fp-zai")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: append([]string(nil), llmroute.DefaultPairOrder...),
		Providers: []string{"anthropic"},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault, llmroute.FieldProviders: llmroute.SourceOrg},
	}
	if !llmroute.SlotSealable(policy.PairOrder, "zai_key") {
		t.Fatalf("the fixture's pair order does not admit zai_key — the probe needs a slot ONLY the allowlist refuses")
	}
	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-po-8", "", "team1", "owner1", "", "", nil,
		map[string]string{string(secrets.ProviderZAI): pinned.ID}, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	log := buf.String()
	if strings.Contains(log, "frozen routing pair order names no pair") {
		t.Fatalf("a pin the PROVIDERS ALLOWLIST dropped is blamed on the frozen pair order, which admits the slot — the remedy named is on the wrong knob:\n%s", log)
	}
	if !strings.Contains(log, "providers allowlist") {
		t.Fatalf("the dropped pin does not name the allowlist that refused it:\n%s", log)
	}
	for _, want := range []string{pinned.ID, "the pin is DROPPED"} {
		if !strings.Contains(log, want) {
			t.Errorf("the dropped-pin line does not carry %q:\n%s", want, log)
		}
	}
}

// Witness 9, the maximal refusal: a frozen policy whose pair_order list is
// EMPTY seals nothing at all — llmroute.SlotSealable refuses every slot — so
// the run that held EVERYTHING out is the one that most owes the line. The
// record opens on the same condition that installs the gate, or this case is
// the silent one.
func TestPairOrder_AnEmptyFrozenListNamesWhatItHeldOut(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceTeam},
	}
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-9", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef != "" {
		t.Fatalf("a bundle was sealed (%q) although the frozen list names no pair at all", creds.secretsRef)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1 — an empty frozen list refuses every slot and must say which it held:\n%s", got, log)
	}
	if !strings.Contains(log, "anthropic_key") {
		t.Errorf("the §5 line does not name the slot the empty list held out:\n%s", log)
	}
}

// Witness 10, the remedy's knob: a slot BOTH of the run's frozen lists hold
// out is this list's refusal — it stays on the line, which is the only
// statement such a slot gets — but widening pair_order would not lift it. The
// line asks the one judgement every message prescribing a remedy asks
// (excluderOfSlot) and names the second list, or the operator widens one
// list, still seals nothing, and loses the line that named the slot.
func TestPairOrder_ASlotBothFrozenListsHoldOutNamesTheAllowlistToo(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai", "fp-zai")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait)},
		Providers: []string{"anthropic"},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources: map[string]string{
			llmroute.FieldPairOrder: llmroute.SourceTeam,
			llmroute.FieldProviders: llmroute.SourceOrg,
		},
	}
	// The probe needs one slot BOTH lists refuse and one only the pair order
	// refuses, or the partition it asserts is untested.
	for _, slot := range []string{"anthropic_key", "zai_key"} {
		if llmroute.SlotSealable(policy.PairOrder, slot) {
			t.Fatalf("the fixture's frozen order admits %s — the probe needs a list that refuses both key slots", slot)
		}
	}
	if allowed := model.AllowedProviderSet(policy.Providers); !allowed[string(secrets.ProviderAnthropic)] || allowed[string(secrets.ProviderZAI)] {
		t.Fatalf("the fixture's allowlist is %v — the probe needs anthropic admitted and zai refused", allowed)
	}
	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-po-10", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
	}
	// Both slots stay named: dropping the doubly-refused one would hide a
	// credential the walk resolved and sealed nowhere.
	for _, want := range []string{"anthropic_key", "zai_key", "pair_order source=team"} {
		if !strings.Contains(log, want) {
			t.Errorf("the §5 line does not carry %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log, "providers allowlist (anthropic) holds zai_key out too") {
		t.Fatalf("the §5 line prescribes widening pair_order for zai_key, which the PROVIDERS ALLOWLIST also holds out — the operator is sent to the knob that cannot lift it:\n%s", log)
	}
	if !strings.Contains(log, "widening pair_order alone does not lift") {
		t.Errorf("the second list is named without saying the first list's remedy is not enough:\n%s", log)
	}
	// The slot only the pair order refuses keeps the pair order's own remedy:
	// a blanket "widen both" would send the operator to a list that already
	// admits it.
	if !strings.Contains(log, "holds zai_key out too") || strings.Contains(log, "holds anthropic_key") {
		t.Errorf("the allowlist clause does not partition the slots — it must name only what that list refuses:\n%s", log)
	}

	// The other half of the loop the clause closes, and why the clause has to
	// be said BEFORE the operator acts: widening pair_order for zai_key
	// yields what W7 states — this line is the FROZEN PAIR ORDER's, so a slot
	// only the allowlist still refuses leaves it. Breaking that silence is
	// ADR-129's (its prune line speaks of ROUTES, not of a credential a tier
	// held), not this record's; the warning above is what keeps an operator
	// from walking into it.
	buf.Reset()
	widened := *policy
	widened.PairOrder = append(append([]string(nil), policy.PairOrder...), llmroute.Pair(llmroute.HarnessClaw, "zai_key"))
	if !llmroute.SlotSealable(widened.PairOrder, "zai_key") {
		t.Fatalf("the widened order still refuses zai_key — this leg must model an operator who did what the line asked")
	}
	if _, err := p.resolveAndSealCredentials(ctx, "run-po-10b", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, &widened, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials after the widening: %v", err)
	}
	after := buf.String()
	if strings.Contains(after, "zai_key") {
		t.Errorf("the widened pair_order ADMITS zai_key, so the §5 line must not name it — the providers allowlist owns that refusal (W7):\n%s", after)
	}
	if !strings.Contains(after, "anthropic_key") {
		t.Errorf("the §5 line stops naming anthropic_key, which the widened order still refuses:\n%s", after)
	}
}

// pairOrderTierFixture is the sealing stack with NO credential: each tier row
// below seeds exactly the one its own site holds, so the §5 line it reads can
// only have come from that site.
func pairOrderTierFixture(t *testing.T) (*Publisher, *bytes.Buffer) {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var buf bytes.Buffer
	p := &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: usagecap.NewMemStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}
	return p, &buf
}

// Witness 11, the class at every TIER: the chokepoint converted eight sites
// and the witnesses above drive the TENANT's only — reverting the org tier's
// two fills and the platform forfait fill to the pure gate left the whole
// package green, and publisher.go is a file in-flight branches touch, so a
// silent revert has a way to arrive. One row per site, each holding exactly
// one credential, so the line a row reads comes from the tier it seeded.
//
// What a row PINS, measured by reverting ONE site at a time to the pure gate:
//   - each FORFAIT fill is the sole reporter of its tier, since a forfait the
//     gate refuses is remembered by no skip and no restore asks again:
//     publisher.go's tenant fill → tenant_forfait (and W6), org_tier.go's →
//     org_forfait, publisher.go's platform fill → platform_forfait;
//   - the KEY sites are redundant BY CONSTRUCTION — a refused key leaves its
//     wire empty, the fallback resolve remembers it, and the restore asks the
//     SAME gate again, the record keying on the slot — so no single one is
//     pinnable: reverting any ONE of publisher.go's tenant fill, its key
//     restore and org_tier.go's org fill leaves every assertion green, and
//     reverting the three together reddens tenant_key, org_key, W1, W3, W4,
//     W9 and W10;
//   - publisher.go's forfait RESTORE can record nothing at all: a forfait
//     reaches the skip set only after passing this same gate at its fill. It
//     keeps the recording variant for the class's symmetry — the restore does
//     hold a credential — and no witness can red on it;
//   - platform_key is the row no other assertion can carry: that leg asked
//     the gate in its `missing` derivation, before the store read, so the
//     slot reached no resolve and the restore was asked for no candidate —
//     the one tier whose refusal was named nowhere while its own forfait leg
//     and its org twin both spoke. Putting that pre-resolve gate back reddens
//     this row alone.
func TestPairOrder_EveryTierNamesWhatTheFrozenOrderHeldOut(t *testing.T) {
	const orgID = "org-1"
	orgResolver := func() *fakeTeamResolver {
		return &fakeTeamResolver{
			orgs:    map[string]string{"team-in": orgID},
			orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
		}
	}
	cases := []struct {
		tier     string
		site     string
		orgID    string
		tenantID string
		wantSlot string
		seed     func(t *testing.T, p *Publisher)
	}{
		{tier: "tenant_key", site: "publisher.go, the BYOK key fill", tenantID: "team1", wantSlot: "anthropic_key",
			seed: func(t *testing.T, p *Publisher) {
				seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-anthropic", "fp-anthropic")
			}},
		{tier: "tenant_forfait", site: "publisher.go, the tenant forfait fill", tenantID: "team1", wantSlot: llmroute.CredClaudeForfait,
			seed: func(t *testing.T, p *Publisher) { seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "tok-tenant") }},
		{tier: "org_forfait", site: "org_tier.go, the org forfait fill", orgID: orgID, tenantID: "team-in", wantSlot: llmroute.CredClaudeForfait,
			seed: func(t *testing.T, p *Publisher) {
				p.identity = orgResolver()
				seedOAuth(t, p.oauthForfait, p.sealer, secrets.OrgTierOwnerKey(orgID), "tok-org")
			}},
		{tier: "org_key", site: "org_tier.go, the org key fill", orgID: orgID, tenantID: "team-in", wantSlot: "anthropic_key",
			seed: func(t *testing.T, p *Publisher) {
				p.identity = orgResolver()
				seedKeyFP(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID(orgID), secrets.ProviderAnthropic, "sk-org", "fp-org")
			}},
		{tier: "platform_forfait", site: "publisher.go, the platform forfait fill", tenantID: "team1", wantSlot: llmroute.CredClaudeForfait,
			seed: func(t *testing.T, p *Publisher) {
				seedOAuth(t, p.oauthForfait, p.sealer, secrets.PlatformOwnerKey, "tok-platform")
			}},
		{tier: "platform_key", site: "publisher.go, the platform key fill", tenantID: "team1", wantSlot: "anthropic_key",
			seed: func(t *testing.T, p *Publisher) {
				seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-platform", "fp-platform")
			}},
	}
	// A frozen order naming ONE pair, for a slot no row holds: every row's
	// own credential is outside it, and the line must name that row's slot.
	order := []string{llmroute.Pair(llmroute.HarnessClaw, "openai_key")}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			p, buf := pairOrderTierFixture(t)
			tc.seed(t, p)
			if llmroute.SlotSealable(order, tc.wantSlot) {
				t.Fatalf("the fixture's frozen order admits %s — this row needs a slot it refuses", tc.wantSlot)
			}
			policy := &store.RunLLMRoutePolicy{PairOrder: order, Triggers: []string{llmroute.TriggerUsageWindow},
				Sources: map[string]string{llmroute.FieldPairOrder: llmroute.SourceTeam}}
			ctx := store.WithTenant(context.Background(), tc.tenantID)
			creds, err := p.resolveAndSealCredentials(ctx, "run-po-tier", tc.orgID, tc.tenantID, "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if creds.secretsRef != "" {
				t.Fatalf("the %s site sealed a bundle (%q) although the frozen order names no pair for %s", tc.site, creds.secretsRef, tc.wantSlot)
			}
			log := buf.String()
			if got := strings.Count(log, heldOutMarker); got != 1 {
				t.Fatalf("%s said the §5 line %d time(s), want exactly 1 — that site holds the credential and owes the record:\n%s", tc.site, got, log)
			}
			if !strings.Contains(log, tc.wantSlot) {
				t.Errorf("the §5 line does not name %s, the slot %s held out:\n%s", tc.wantSlot, tc.site, log)
			}
		})
	}
}

// Witness 12, the run's RETRY CLOCK: a credential the run's frozen pair
// order can never let it seal must not arm the usage-window retry. Each key
// leg resolves its tier through a usability predicate whose skip exit
// records the credential's reopening into the walk's clock
// (apiKeyUsable → skipTracker.note → credResolution.skippedReopensAt →
// store.Run.SkippedCredReopensAt, a durable write). pkg/runner then wakes
// the run at that instant rather than at the wall it actually met, and every
// arming spends one attempt of a bounded budget (usageWindowRetryAt;
// ScheduleRunRetry refuses past max_attempts). For a slot the frozen list
// refuses the wake can change nothing — the resume replays that list as
// stored, which is §5's other half, so the slot is refused again — so the
// attempt buys a reopening this run cannot use. The list is therefore asked
// INSIDE the predicate, with the credential in hand: the §5 record still
// names the slot, and the credential's meter is never read for it.
//
// One row per key leg, because each resolves its own tier: the forfait legs
// already ask the gate BEFORE the window read, and this is the parity the
// key legs owed.
func TestPairOrder_ARefusedSlotArmsNoSpeculativeRetry(t *testing.T) {
	const orgID = "org-1"
	cases := []struct {
		tier     string
		site     string
		orgID    string
		tenantID string
		seed     func(t *testing.T, p *Publisher)
	}{
		{tier: "tenant_key", site: "publisher.go, the BYOK key resolve", tenantID: "team1",
			seed: func(t *testing.T, p *Publisher) {
				seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-anthropic", "fp-tenant")
				recordRefusal(t, p.usageCaps, usagecap.TenantScope("team1"), "fp-tenant")
			}},
		{tier: "org_key", site: "org_tier.go, the org key resolve", orgID: orgID, tenantID: "team-in",
			seed: func(t *testing.T, p *Publisher) {
				p.identity = &fakeTeamResolver{
					orgs:    map[string]string{"team-in": orgID},
					orgDocs: map[string]identity.Org{orgID: {ID: orgID, CredentialAudience: identity.CredentialAudience{Teams: []string{"team-in"}}}},
				}
				seedKeyFP(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID(orgID), secrets.ProviderAnthropic, "sk-org", "fp-org")
				recordRefusal(t, p.usageCaps, usagecap.OrgScope(orgID), "fp-org")
			}},
		{tier: "platform_key", site: "publisher.go, the platform key resolve", tenantID: "team1",
			seed: func(t *testing.T, p *Publisher) {
				seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-platform", "fp-platform")
				recordRefusal(t, p.usageCaps, usagecap.ScopePlatform, "fp-platform")
			}},
	}
	order := []string{llmroute.Pair(llmroute.HarnessClaw, "openai_key")}
	for _, tc := range cases {
		t.Run(tc.tier, func(t *testing.T) {
			p, buf := pairOrderTierFixture(t)
			tc.seed(t, p)
			if llmroute.SlotSealable(order, "anthropic_key") {
				t.Fatalf("the fixture's frozen order admits anthropic_key — this row needs a slot it refuses")
			}
			policy := &store.RunLLMRoutePolicy{PairOrder: order, Triggers: []string{llmroute.TriggerUsageWindow},
				Sources: map[string]string{llmroute.FieldPairOrder: llmroute.SourceTeam}}
			ctx := store.WithTenant(context.Background(), tc.tenantID)
			creds, err := p.resolveAndSealCredentials(ctx, "run-po-clock", tc.orgID, tc.tenantID, "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			log := buf.String()
			if !creds.skippedReopensAt.IsZero() {
				t.Fatalf("%s armed the run's retry clock at %s from a credential the frozen order refuses: the resume replays that list, so the wake cannot change the resolution and the attempt is spent for nothing\n%s",
					tc.site, creds.skippedReopensAt.UTC().Format(time.RFC3339), log)
			}
			if at := creds.stamp().SkippedReopensAt; at != nil {
				t.Fatalf("%s stamped SkippedCredReopensAt=%s on the run document for a slot outside the run's routing", tc.site, at.UTC().Format(time.RFC3339))
			}
			if strings.Contains(log, "SKIPPED for run=") {
				t.Errorf("%s read the credential's usage meter for a slot the frozen order refuses — the gate answers before the meter:\n%s", tc.site, log)
			}
			// The fix must not buy its silence by hiding the refusal: §5's
			// line is what the operator reads instead of a retry.
			if got := strings.Count(log, heldOutMarker); got != 1 {
				t.Fatalf("%s said the §5 line %d time(s), want exactly 1 — the slot is held out and the record owes the line:\n%s", tc.site, got, log)
			}
			if !strings.Contains(log, "anthropic_key") {
				t.Errorf("the §5 line does not name anthropic_key, the slot %s held out:\n%s", tc.site, log)
			}
		})
	}
}

// Witness 13, the provenance leg of the same level: a run the narrowing cost
// NOTHING — its key seals — but whose list no level chose is still §5's own
// subject, so it is still the warn. Without this row the funding leg alone
// would pass W1 and W9, and the pre-upgrade shape would quietly become an
// info the day a deployment holds one spare credential.
func TestPairOrder_AFundedRunUnderTheLaunchDaysDefaultIsStillTheWarning(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "tok-claude")
	ctx := store.WithTenant(context.Background(), "team1")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault},
	}
	creds, err := p.resolveAndSealCredentials(ctx, "run-po-13", "", "team1", "owner1", "", "", nil, nil, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if b := sealedBundle(t, p, "run-po-13", creds); b.APIKeys[secrets.ProviderAnthropic] != "sk-anthropic" {
		t.Fatalf("the admitted key slot sealed %+v, want the anthropic key — this row models a FUNDED run", b.APIKeys)
	}
	log := buf.String()
	if strings.Contains(log, "no credential resolved") {
		t.Fatalf("the fixture left the run unfunded, so the level below would be the funding leg's:\n%s", log)
	}
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
	}
	if got := markerLevel(t, log); got != "warn" {
		t.Errorf("a list no level chose is reported at %s level, want warn — §5's own subject is the warning whatever the run ended up holding:\n%s", got, log)
	}
}

// Witness 14, the pin's remedy and the OTHER list: a pinned key whose slot
// BOTH frozen lists refuse is named by the allowlist arm — the list named
// first, since widening the pair order alone would not lift it — and that
// arm must carry the same insufficiency clause the §5 line carries (W10), or
// the operator widens the providers list, relaunches, and the pin is dropped
// again by a list this line never named. Both knobs for a doubly-refused
// pin; the allowlist alone for an allowlist-only one (W8).
func TestPairOrder_ADoublyRefusedPinNamesBothKnobs(t *testing.T) {
	p, _, buf := pairOrderPublisher(t)
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai", "fp-zai")
	pinned := firstKeyWithFingerprint(t, p.apiKeys, "fp-zai")
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait)},
		Providers: []string{"anthropic"},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources: map[string]string{
			llmroute.FieldPairOrder: llmroute.SourceTeam,
			llmroute.FieldProviders: llmroute.SourceOrg,
		},
	}
	// Both guards, or the probe proves nothing about a DOUBLY refused slot.
	if llmroute.SlotSealable(policy.PairOrder, "zai_key") {
		t.Fatalf("the fixture's frozen order admits zai_key — this probe needs a slot BOTH lists refuse")
	}
	if allowed := model.AllowedProviderSet(policy.Providers); allowed[string(secrets.ProviderZAI)] {
		t.Fatalf("the fixture's allowlist admits zai — this probe needs a slot BOTH lists refuse")
	}
	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-po-14", "", "team1", "owner1", "", "", nil,
		map[string]string{string(secrets.ProviderZAI): pinned.ID}, nil, model.RouteView{}, nil, store.RunTrustDefault, nil, policy, nil); err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	log := buf.String()
	var pinLine string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "the pin is DROPPED") {
			pinLine = line
		}
	}
	if pinLine == "" {
		t.Fatalf("the dropped pin is never named:\n%s", log)
	}
	if !strings.Contains(pinLine, "providers allowlist") {
		t.Fatalf("the dropped pin does not name the allowlist that refused it:\n%s", pinLine)
	}
	if !strings.Contains(pinLine, "frozen routing pair order names no pair for the zai_key slot either") {
		t.Fatalf("the pin's remedy prescribes ONE knob for a slot BOTH frozen lists refuse: widening the providers list leaves the pair order refusing it, and this line never named that list — the loop the §5 line's own clause (W10) exists to prevent:\n%s", pinLine)
	}
	if !strings.Contains(pinLine, "alone does not lift") {
		t.Errorf("the second list is named without saying the first knob is not enough:\n%s", pinLine)
	}
}

// Witness 15, the silence claim's own coupling: §5's line is designed to be
// SILENT under the shipped default (pair_order_refusal.go, warn), and that
// silence holds only while every slot the WALK can ask the gate about is
// named by llmroute.DefaultPairOrder. The two sets live in different
// packages and nothing couples them — llmroute's
// TestDefaultPairOrderCoversEverySealableSlot checks the default against
// hand-written slot literals, while the slots that reach the gate are
// DERIVED here, from allKnownProviders through slotOfProvider and the OAuth
// kinds through slotOfKind. The day a provider joins the walk (#2005 PR-C'
// does exactly that for the gateway, whose slot llmroute's own vocabulary
// refuses — pkg/llmroute's TestGatewayPairStaysOutOfTheWhitelistUntilThe...)
// the §5 record would note it under the SHIPPED DEFAULT, and the line would
// fire at warn claiming a fresh launch resolves a list that refuses the slot
// just as much. Derived, never listed: this reds in the package that depends
// on the claim, the day the walk's vocabulary grows.
func TestPairOrder_TheWalksSlotVocabularyIsNamedByTheShippedDefault(t *testing.T) {
	slots := make([]string, 0, len(allKnownProviders)+2)
	for _, prov := range allKnownProviders {
		slots = append(slots, slotOfProvider(prov))
	}
	for _, kind := range []secrets.OAuthKind{secrets.OAuthKindClaudeCode, secrets.OAuthKindCodex} {
		slots = append(slots, slotOfKind(string(kind)))
	}
	for _, slot := range slots {
		if !llmroute.SlotSealable(llmroute.DefaultPairOrder, slot) {
			t.Errorf("the walk can ask the seal gate about %q, and llmroute.DefaultPairOrder names no pair for it: ADR-131 §5's line claims silence under the shipped default, so it would now fire at warn on every launch of that default — telling the operator a fresh launch resolves a list that refuses the slot too. Either the default names the slot, or §5's silence claim is what the change must lift", slot)
		}
	}
}
