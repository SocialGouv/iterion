package llmroute

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestResolve_FirstSetWinsPerField(t *testing.T) {
	park := RefusedPinnedPark
	no := false
	got, src := Resolve(
		Layer{Source: "binding", Policy: Policy{RefusedPinnedKey: RefusedPinnedPark}},
		Layer{Source: "bot", Policy: Policy{
			PairOrder:        []string{Pair(HarnessClaw, "anthropic_key")},
			RefusedPinnedKey: RefusedPinnedForfait,
			Strict:           &no,
		}},
		Layer{Source: SourcePlatform, Policy: Policy{RefusedPinnedKey: park, Triggers: []string{TriggerUsageWindow}}},
		Layer{Source: SourceEnv, Policy: Policy{}},
	)

	if want := []string{Pair(HarnessClaw, "anthropic_key")}; !reflect.DeepEqual(got.PairOrder, want) {
		t.Fatalf("pair_order = %v, want the bot's (a level replaces, never merges)", got.PairOrder)
	}
	if src[FieldPairOrder] != "bot" {
		t.Fatalf("pair_order source = %q, want bot", src[FieldPairOrder])
	}
	if got.RefusedPinnedKey != RefusedPinnedPark {
		t.Fatalf("refused_pinned_key = %q, want the binding's park (highest setter wins even against the bot's forfait)", got.RefusedPinnedKey)
	}
	if src[FieldRefusedPinnedKey] != "binding" {
		t.Fatalf("refused_pinned_key source = %q, want binding", src[FieldRefusedPinnedKey])
	}
	if got.Strict == nil || *got.Strict {
		t.Fatalf("strict = %v, want the bot's explicit false (nil and false are different answers)", got.Strict)
	}
	if src[FieldStrict] != "bot" {
		t.Fatalf("strict source = %q, want bot", src[FieldStrict])
	}
}

func TestResolve_LocksStopTheDescent(t *testing.T) {
	park := RefusedPinnedPark
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{RefusedPinnedKey: RefusedPinnedForfait}},
		Layer{Source: "bot", Policy: Policy{Locks: []string{FieldRefusedPinnedKey}, RefusedPinnedKey: park}},
		Layer{Source: SourcePlatform, Policy: Policy{RefusedPinnedKey: park}},
	)
	// The author's lock is what stops the BINDING (and here the run): the
	// field is decided at or below the lock, so the bot's park beats the
	// run's forfait.
	if got.RefusedPinnedKey != RefusedPinnedPark {
		t.Fatalf("refused_pinned_key = %q, want the bot's park — its lock vetoes the setter above it", got.RefusedPinnedKey)
	}
	if src[FieldRefusedPinnedKey] != "bot" {
		t.Fatalf("refused_pinned_key source = %q, want bot", src[FieldRefusedPinnedKey])
	}
}

func TestResolve_LockWithoutValuePinsTheDefault(t *testing.T) {
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{PairOrder: []string{Pair(HarnessClaw, "anthropic_key")}}},
		Layer{Source: "bot", Policy: Policy{Locks: []string{FieldPairOrder}}},
		Layer{Source: SourcePlatform, Policy: Policy{PairOrder: []string{Pair(HarnessCodex, CredChatGPTForfait)}}},
	)
	// The lock vetoes the run above and decides at-or-below itself: the
	// bot sets nothing, so the PLATFORM's list answers (it is below the
	// lock and sets) — not the run's, not the default.
	want := []string{Pair(HarnessCodex, CredChatGPTForfait)}
	if !reflect.DeepEqual(got.PairOrder, want) {
		t.Fatalf("pair_order = %v, want the platform's (below the lock) — not the run's vetoed list", got.PairOrder)
	}
	if src[FieldPairOrder] != SourcePlatform {
		t.Fatalf("pair_order source = %q, want platform", src[FieldPairOrder])
	}
}

func TestResolve_LockPinningDefaultSaysSo(t *testing.T) {
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{RefusedPinnedKey: RefusedPinnedForfait}},
		Layer{Source: "bot", Policy: Policy{Locks: []string{FieldRefusedPinnedKey}}},
	)
	if got.RefusedPinnedKey != RefusedPinnedForfait {
		t.Fatalf("refused_pinned_key = %q, want the default (nobody below the lock set it)", got.RefusedPinnedKey)
	}
	if src[FieldRefusedPinnedKey] != "bot_lock" {
		t.Fatalf("refused_pinned_key source = %q, want bot_lock — 'default' alone would hide the veto", src[FieldRefusedPinnedKey])
	}
}

func TestResolve_LowestLockGoverns(t *testing.T) {
	park := RefusedPinnedPark
	got, _ := Resolve(
		Layer{Source: "run", Policy: Policy{RefusedPinnedKey: RefusedPinnedForfait}},
		Layer{Source: "bot", Policy: Policy{Locks: []string{FieldRefusedPinnedKey}, RefusedPinnedKey: RefusedPinnedForfait}},
		Layer{Source: SourcePlatform, Policy: Policy{Locks: []string{FieldRefusedPinnedKey}, RefusedPinnedKey: park}},
	)
	// Two locks: the lowest-priority one (platform) governs the range —
	// the most restrictive wins, and the platform's own value answers.
	if got.RefusedPinnedKey != park {
		t.Fatalf("refused_pinned_key = %q, want the platform's park (its lock is the lowest, its range decides)", got.RefusedPinnedKey)
	}
}

func TestResolve_StrictIsMonotoneWithoutLock(t *testing.T) {
	yes, no := true, false
	// A bot's true survives the run's false.
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{Strict: &no}},
		Layer{Source: "bot", Policy: Policy{Strict: &yes}},
	)
	if got.Strict == nil || !*got.Strict {
		t.Fatalf("strict = %v, want true — no level unsets it: the bot's true beats the run's false", got.Strict)
	}
	if src[FieldStrict] != "bot" {
		t.Fatalf("strict source = %q, want bot (the setter of the value that won)", src[FieldStrict])
	}

	// A platform false under no true anywhere stays false.
	got, _ = Resolve(
		Layer{Source: "bot", Policy: Policy{Strict: &no}},
		Layer{Source: SourcePlatform, Policy: Policy{Strict: &no}},
	)
	if got.Strict == nil || *got.Strict {
		t.Fatalf("strict = %v, want false — monotone lifts a true, it does not invent one", got.Strict)
	}
}

func TestResolve_StrictLockReopensForTheLauncher(t *testing.T) {
	yes, no := true, false
	got, src := Resolve(
		Layer{Source: "run", Launcher: true, Policy: Policy{Strict: &no, Locks: []string{FieldStrict}}},
		Layer{Source: "bot", Policy: Policy{Strict: &yes}},
	)
	if got.Strict == nil || *got.Strict {
		t.Fatalf("strict = %v, want false — the launcher's explicit lock is the one unset path", got.Strict)
	}
	if src[FieldStrict] != "run" {
		t.Fatalf("strict source = %q, want run", src[FieldStrict])
	}
}

func TestResolve_TriggersNotExtendedByLowerLevels(t *testing.T) {
	got, src := Resolve(
		Layer{Source: "bot", Policy: Policy{Triggers: []string{TriggerUsageWindow}}},
		Layer{Source: SourcePlatform, Policy: Policy{Triggers: []string{TriggerUsageWindow, TriggerAuth}}},
	)
	if !reflect.DeepEqual(got.Triggers, []string{TriggerUsageWindow}) {
		t.Fatalf("triggers = %v, want the bot's subset", got.Triggers)
	}
	if src[FieldTriggers] != "bot" {
		t.Fatalf("triggers source = %q, want bot", src[FieldTriggers])
	}
}

func TestResolve_UnsetFieldsNormalizeWithDefaultProvenance(t *testing.T) {
	got, src := Resolve(Layer{Source: SourcePlatform, Policy: Policy{}})
	if !reflect.DeepEqual(got.PairOrder, DefaultPairOrder) {
		t.Fatalf("pair_order = %v, want the arbitrated default order", got.PairOrder)
	}
	if !reflect.DeepEqual(got.Triggers, Triggers) {
		t.Fatalf("triggers = %v, want the whole vocabulary", got.Triggers)
	}
	if got.RefusedPinnedKey != RefusedPinnedForfait {
		t.Fatalf("refused_pinned_key = %q, want the arbitrated default %q", got.RefusedPinnedKey, RefusedPinnedForfait)
	}
	if got.Strict != nil {
		t.Fatalf("strict = %v, want nil — Normalize must not invent a strict answer nobody set", *got.Strict)
	}
	for _, f := range Fields {
		if src[f] != SourceDefault {
			t.Fatalf("%s source = %q, want default", f, src[f])
		}
	}
}

func TestNormalize_Idempotent(t *testing.T) {
	once := Normalize(Policy{})
	twice := Normalize(once)
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("Normalize is not idempotent: %+v vs %+v", once, twice)
	}
	if once.PairOrder == nil || once.Triggers == nil || once.RefusedPinnedKey == "" {
		t.Fatalf("Normalize left a field empty: %+v", once)
	}
}

func TestNormalize_ExplicitEmptyTriggersSurvive(t *testing.T) {
	// The one legitimate empty: the ceiling's "never switch" answer must
	// not be re-inflated into the whole vocabulary by a defensive
	// Normalize — two readers would otherwise get opposite answers.
	p := Policy{Triggers: []string{}}
	got := Normalize(p)
	if got.Triggers == nil || len(got.Triggers) != 0 {
		t.Fatalf("triggers = %v, want the empty answer preserved", got.Triggers)
	}
}

func TestValidate_PairOrder(t *testing.T) {
	tests := []struct {
		name  string
		pairs []string
		want  string // substring of the expected error; "" = valid
	}{
		{"arbitrary default order", DefaultPairOrder, ""},
		{"every known provider key", providerKeyPairs(), ""},
		{"not a pair", []string{"claude_code"}, "not harness+credential"},
		{"empty side", []string{"+claude_forfait"}, "not harness+credential"},
		{"unknown harness", []string{Pair("notabackend", CredClaudeForfait)}, `unknown harness "notabackend"`},
		{"unknown credential", []string{Pair(HarnessClaw, "forfait_claude")}, `unknown credential "forfait_claude"`},
		{"duplicate", []string{Pair(HarnessClaw, "anthropic_key"), Pair(HarnessClaw, "anthropic_key")}, "twice"},
		{"explicitly empty", []string{}, "pair_order is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(Policy{PairOrder: tt.pairs})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want it to name %q", err, tt.want)
			}
		})
	}
}

func TestValidate_ExplicitEmptyTriggersRefused(t *testing.T) {
	err := Validate(Policy{Triggers: []string{}})
	if err == nil || !strings.Contains(err.Error(), "triggers is empty") {
		t.Fatalf("Validate = %v, want the empty-list refusal (a level that must not switch locks the field)", err)
	}
}

func providerKeyPairs() []string {
	var pairs []string
	for _, p := range []string{"anthropic", "openai", "bedrock", "vertex", "azure", "openrouter", "xai", "zai", "moonshot"} {
		pairs = append(pairs, Pair(HarnessClaw, p+"_key"))
	}
	return pairs
}

func TestValidate_TriggersClosedVocabulary(t *testing.T) {
	for _, tName := range Triggers {
		if err := Validate(Policy{Triggers: []string{tName}}); err != nil {
			t.Fatalf("Validate(%s): %v", tName, err)
		}
	}
	for _, refused := range []string{"budget", "schema", "any", "unclassified", "USAGE_WINDOW", ""} {
		err := Validate(Policy{Triggers: []string{refused}})
		if err == nil {
			t.Fatalf("Validate(%q) = nil, want a refusal — %q is excluded by rule, not silently dropped", refused, refused)
		}
	}
	if err := Validate(Policy{Triggers: []string{TriggerAuth, TriggerAuth}}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Validate(duplicate trigger) = %v, want a twice-error", err)
	}
}

func TestValidate_RefusedPinnedKeyAndLocks(t *testing.T) {
	if err := Validate(Policy{RefusedPinnedKey: "nope"}); err == nil {
		t.Fatal("Validate(refused_pinned_key=nope) = nil, want a refusal")
	}
	for _, v := range []string{"", RefusedPinnedForfait, RefusedPinnedPark} {
		if err := Validate(Policy{RefusedPinnedKey: v}); err != nil {
			t.Fatalf("Validate(refused_pinned_key=%q): %v", v, err)
		}
	}
	if err := Validate(Policy{Locks: []string{"not_a_field"}}); err == nil || !strings.Contains(err.Error(), "not a policy field") {
		t.Fatalf("Validate(locks) = %v, want a field-vocabulary refusal", err)
	}
	if err := Validate(Policy{Locks: Fields}); err != nil {
		t.Fatalf("Validate(all fields locked): %v", err)
	}
}

func TestClamp_PrunesOnlyTriggers(t *testing.T) {
	src := map[string]string{FieldTriggers: "bot"}
	got := Clamp(Policy{
		Triggers:         []string{TriggerUsageWindow, TriggerAuth},
		RefusedPinnedKey: RefusedPinnedPark,
	}, Ceiling{Triggers: []string{TriggerUsageWindow}}, src)
	if !reflect.DeepEqual(got.Triggers, []string{TriggerUsageWindow}) {
		t.Fatalf("triggers = %v, want pruned to the ceiling", got.Triggers)
	}
	if src[FieldTriggers] != SourceCeiling {
		t.Fatalf("provenance = %q, want platform_ceiling", src[FieldTriggers])
	}
	if got.RefusedPinnedKey != RefusedPinnedPark {
		t.Fatalf("the ceiling touched refused_pinned_key: %q", got.RefusedPinnedKey)
	}
}

func TestClamp_EmptyAnswerSurvivesReNormalize(t *testing.T) {
	// The ceiling forbids every trigger the winner named: the honest
	// answer is "never switch" — an empty list that a defensive
	// re-Normalize must NOT inflate back into the whole vocabulary.
	src := map[string]string{}
	got := Clamp(Policy{Triggers: []string{TriggerAuth}}, Ceiling{Triggers: []string{TriggerUsageWindow}}, src)
	if got.Triggers == nil || len(got.Triggers) != 0 {
		t.Fatalf("triggers = %v, want the empty answer", got.Triggers)
	}
	if src[FieldTriggers] != SourceCeiling {
		t.Fatalf("provenance = %q, want platform_ceiling", src[FieldTriggers])
	}
	again := Normalize(got)
	if again.Triggers == nil || len(again.Triggers) != 0 {
		t.Fatalf("re-Normalize inflated the empty answer into %v", again.Triggers)
	}
}

func TestClamp_NeverExtendsAndIsNoopWhenPlatformWonTheField(t *testing.T) {
	// A subset the ceiling admits passes through un-pruned.
	src := map[string]string{}
	got := Clamp(Policy{Triggers: []string{TriggerAuth}}, Ceiling{Triggers: Triggers}, src)
	if !reflect.DeepEqual(got.Triggers, []string{TriggerAuth}) || src[FieldTriggers] != "" {
		t.Fatalf("subset pruned: %v / %q", got.Triggers, src[FieldTriggers])
	}
	// Slice 1: the platform level is the only level, so the platform's own
	// list both wins the fold and forms the ceiling — Clamp must be a
	// proven no-op, provenance untouched.
	src = map[string]string{FieldTriggers: SourcePlatform}
	full := []string{TriggerUsageWindow, TriggerAuth}
	got = Clamp(Policy{Triggers: full}, Ceiling{Triggers: full}, src)
	if !reflect.DeepEqual(got.Triggers, full) || src[FieldTriggers] != SourcePlatform {
		t.Fatalf("self-ceiling moved the answer: %v / %q", got.Triggers, src[FieldTriggers])
	}
	// No platform word on triggers: no ceiling at all.
	got = Clamp(Policy{Triggers: full}, Ceiling{}, src)
	if !reflect.DeepEqual(got.Triggers, full) {
		t.Fatalf("empty ceiling pruned: %v", got.Triggers)
	}
}

// The *_key credential slots mirror pkg/secrets.Provider's stable wire
// identifiers. The lists live apart because llmroute must not drag a store
// dependency into every reader of the policy — this test is the guard that
// keeps them from drifting apart in silence.
func TestKeyProviderSlotsMirrorSecretsProviders(t *testing.T) {
	for _, p := range []secrets.Provider{
		secrets.ProviderAnthropic, secrets.ProviderOpenAI, secrets.ProviderBedrock,
		secrets.ProviderVertex, secrets.ProviderAzure, secrets.ProviderOpenRouter,
		secrets.ProviderXAI, secrets.ProviderZAI, secrets.ProviderMoonshot,
	} {
		if !validCredential(string(p) + "_key") {
			t.Errorf("secrets.Provider %q has no matching %q slot in the policy vocabulary", p, string(p)+"_key")
		}
	}
}

// A lock that is not THE launcher's cannot launder an unset past the
// author (ADR-121: "no level unsets it — only the launcher, through an
// explicit lock"): the run's false loses to the bot's true, and the
// binding's lock in the middle changes nothing. The seat is MARKED, not
// positional — an unmarked run layer is just another level.
func TestResolve_StrictNonHeadLockCannotUnset(t *testing.T) {
	yes, no := true, false
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{Strict: &no}},
		Layer{Source: "binding", Policy: Policy{Strict: &no, Locks: []string{FieldStrict}}},
		Layer{Source: "bot", Policy: Policy{Strict: &yes}},
	)
	if got.Strict == nil || !*got.Strict {
		t.Fatalf("strict = %v, want true — a mid-chain lock is not the launcher's explicit lock", got.Strict)
	}
	if src[FieldStrict] != "bot" {
		t.Fatalf("strict source = %q, want bot", src[FieldStrict])
	}
}

// The monotone lift scans only the range the locks allow: a true ABOVE the
// lock was vetoed by it and must not come back through the lift — the
// locked range (no true below) answers false.
func TestResolve_StrictLiftDoesNotCrossALock(t *testing.T) {
	yes, no := true, false
	got, src := Resolve(
		Layer{Source: "run", Policy: Policy{Strict: &yes}},
		Layer{Source: "binding", Policy: Policy{Strict: &no, Locks: []string{FieldStrict}}},
		Layer{Source: "bot", Policy: Policy{Strict: &no}},
	)
	if got.Strict == nil || *got.Strict {
		t.Fatalf("strict = %v, want false — the binding's lock vetoes the run's true and owns the field", got.Strict)
	}
	if src[FieldStrict] != "binding" {
		t.Fatalf("strict source = %q, want binding", src[FieldStrict])
	}
}

// THE critical case the slice-2 review found: a BINDING lock + false must
// not launder an unset past the author — no layer marked Launcher exists
// yet, so the monotone lift restores the author's true against the whole
// binding's machinery. The schedule write path additionally refuses locks
// (the binding holds no lock primitive); this fold test is the backstop
// for records that arrive by other means.
func TestResolve_BindingLockCannotUnsetStrict(t *testing.T) {
	yes, no := true, false
	got, src := Resolve(
		Layer{Source: SourceSchedule, Policy: Policy{Strict: &no, Locks: []string{FieldStrict}}},
		Layer{Source: SourceBot, Policy: Policy{Strict: &yes}},
		Layer{Source: SourcePlatform, Policy: Policy{}},
	)
	if got.Strict == nil || !*got.Strict {
		t.Fatalf("strict = %v, want true — no launcher marked: the author's true survives the binding's lock+false", got.Strict)
	}
	if src[FieldStrict] != SourceBot {
		t.Fatalf("strict source = %q, want bot", src[FieldStrict])
	}
}

// The tenant levels (ADR-121 delivery 2) fold by the same rules: team
// outranks org, org outranks platform. Their PROVENANCE labels are the
// run snapshot's "which level decided this" answer.
func TestResolve_TenantLevelsOrderTeamOverOrgOverPlatform(t *testing.T) {
	team := Policy{PairOrder: []string{Pair(HarnessClaw, "zai_key")}}
	org := Policy{PairOrder: []string{Pair(HarnessClaw, "anthropic_key")}}
	platform := Policy{PairOrder: []string{Pair(HarnessCodex, CredChatGPTForfait)}}

	got, src := Resolve(
		Layer{Source: SourceTeam, Policy: team},
		Layer{Source: SourceOrg, Policy: org},
		Layer{Source: SourcePlatform, Policy: platform},
	)
	if got.PairOrder[0] != Pair(HarnessClaw, "zai_key") || src[FieldPairOrder] != SourceTeam {
		t.Fatalf("team must answer over org: %v / %q", got.PairOrder, src[FieldPairOrder])
	}

	got, src = Resolve(
		Layer{Source: SourceOrg, Policy: org},
		Layer{Source: SourcePlatform, Policy: platform},
	)
	if got.PairOrder[0] != Pair(HarnessClaw, "anthropic_key") || src[FieldPairOrder] != SourceOrg {
		t.Fatalf("org must answer over platform: %v / %q", got.PairOrder, src[FieldPairOrder])
	}
}

// The lock doctrine AS DELIVERED (Resolve's pre-pass, policy.go): a lock
// at a level vetoes every MORE SPECIFIC level's setter and answers
// at-or-below itself. A TEAM lock does not bind the org below it — the
// org's value answers with its own provenance. Only the PLATFORM's lock
// binds every tenant-configurable level, because nothing answers below
// it. (The old seal-reading — "the lock stops the descent" — would make
// team lock + org value answer team_lock; the delivered fold never does.)
func TestResolve_TeamLockVetoesAboveAnswersBelow(t *testing.T) {
	// (a) team lock + bot value above + org value below: the bot is
	// vetoed, the ORG answers.
	bot := Policy{PairOrder: []string{Pair(HarnessClaudeCode, "zai_key")}}
	org := Policy{PairOrder: []string{Pair(HarnessClaw, "anthropic_key")}}
	teamLock := Policy{Locks: []string{FieldPairOrder}}
	got, src := Resolve(
		Layer{Source: SourceRun, Policy: bot},
		Layer{Source: SourceBot, Policy: bot},
		Layer{Source: SourceTeam, Policy: teamLock},
		Layer{Source: SourceOrg, Policy: org},
	)
	if got.PairOrder[0] != Pair(HarnessClaw, "anthropic_key") || src[FieldPairOrder] != SourceOrg {
		t.Fatalf("team lock must let the org below answer: %v / %q", got.PairOrder, src[FieldPairOrder])
	}

	// (b) team lock + NOTHING at-or-below sets: the lock PINS THE DEFAULT
	// (Normalize's answer) and the provenance says which level locked it.
	got, src = Resolve(
		Layer{Source: SourceRun, Policy: bot},
		Layer{Source: SourceTeam, Policy: teamLock},
	)
	if len(got.PairOrder) == 0 {
		t.Fatal("pair_order = nil, want the default pinned by the lock")
	}
	if src[FieldPairOrder] != SourceTeam+"_lock" {
		t.Fatalf("pair_order provenance = %q, want team_lock", src[FieldPairOrder])
	}

	// The mirror case: an ORG lock binds the team above it — the team's
	// value is vetoed and the org answers.
	teamVal := Policy{PairOrder: []string{Pair(HarnessClaudeCode, "zai_key")}}
	orgLock := Policy{Locks: []string{FieldPairOrder}}
	got, src = Resolve(
		Layer{Source: SourceTeam, Policy: teamVal},
		Layer{Source: SourceOrg, Policy: orgLock},
		Layer{Source: SourcePlatform, Policy: Policy{PairOrder: []string{Pair(HarnessCodex, CredChatGPTForfait)}}},
	)
	if got.PairOrder[0] != Pair(HarnessCodex, CredChatGPTForfait) || src[FieldPairOrder] != SourcePlatform {
		t.Fatalf("org lock must veto the team and let the platform below answer: %v / %q", got.PairOrder, src[FieldPairOrder])
	}
}
