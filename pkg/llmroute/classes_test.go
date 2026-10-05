package llmroute

import (
	"testing"
)

// The shipped table's standard/openai cell IS the delivery-1 crossing
// default: an unknown model classifies standard, so every model the
// table is silent on crosses exactly where it crossed before. Break
// this equality and the first policy with no model_classes silently
// re-targets every wholesale crossing.
func TestShippedStandardOpenAIIsTheDelivery1Default(t *testing.T) {
	tbl := ResolveClasses(nil)
	got, ok := tbl.Target(ClassStandard, familyOpenAI)
	if !ok || got != shippedOpenAIDefault {
		t.Fatalf("standard/openai = %q (ok=%v), want the shipped default %q — the byte-identity anchor", got, ok, shippedOpenAIDefault)
	}
}

// The reverse index matches the PAIR (family, bare), first class in
// vocabulary order wins, and everything unnamed classifies standard.
// A bare-keyed index would re-classify anthropic/gpt-6-luna (openai
// family by modelFamily's rule) into fast and add un-named byte deltas.
func TestClassOfIsFamilyKeyed(t *testing.T) {
	tbl := ResolveClasses(nil)
	cases := []struct {
		family, bare, want string
	}{
		{familyAnthropicWire, "claude-opus-5-5", ClassTop},
		{familyAnthropicWire, "claude-sonnet-5", ClassStandard},
		{familyAnthropicWire, "claude-haiku-4-5", ClassFast},
		{familyAnthropicWire, "glm-5.3", ClassStandard}, // unnamed → standard
		{familyOpenAI, "gpt-6-sol", ClassTop},           // named by top FIRST
		{familyOpenAI, "gpt-6-luna", ClassFast},
		{familyOpenAI, "gpt-6-mini", ClassStandard}, // unnamed → standard
		// The family-keyed guard: gpt-6-luna UNDER the anthropic prefix is
		// ANTHROPIC-WIRE family (modelFamily's own rule), where the fast
		// cell names claude-haiku-4-5 — gpt-6-luna is unnamed there and
		// classifies standard, so its crossing stays gpt-6-sol (delivery
		// 1). A bare-keyed index would find fast and cross it to
		// gpt-6-luna — three un-named byte deltas. (Review R837bf2: the
		// first draft of this row duplicated the openai-family line above
		// and pinned nothing.)
		{familyAnthropicWire, "gpt-6-luna", ClassStandard},
	}
	for _, tc := range cases {
		if got := tbl.ClassOf(tc.family, tc.bare); got != tc.want {
			t.Errorf("ClassOf(%s, %s) = %s, want %s", tc.family, tc.bare, got, tc.want)
		}
	}
	// The empty family (familyNone) is unnamed → standard.
	if got := tbl.ClassOf("", "openai_compatible/mistral"); got != ClassStandard {
		t.Fatalf("ClassOf(familyNone) = %s, want standard", got)
	}
}

// The fold merges ENTRY-WISE per class×family: two levels each naming a
// different cell keep BOTH; a conflict resolves per cell to the more
// specific level; the provenance keys name the cells.
func TestResolveModelClassesEntryWise(t *testing.T) {
	org := Policy{ModelClasses: map[string]map[string]string{
		ClassFast: {familyOpenAI: "gpt-6-mini"},
	}}
	platform := Policy{ModelClasses: map[string]map[string]string{
		ClassTop: {familyAnthropicWire: "claude-opus-5-5"},
	}}
	got, src := Resolve(
		Layer{Source: SourceOrg, Policy: org},
		Layer{Source: SourcePlatform, Policy: platform},
	)
	if got.ModelClasses[ClassFast][familyOpenAI] != "gpt-6-mini" {
		t.Fatalf("org's fast/openai cell lost: %+v", got.ModelClasses)
	}
	if got.ModelClasses[ClassTop][familyAnthropicWire] != "claude-opus-5-5" {
		t.Fatalf("platform's top/anthropic-wire cell lost: %+v", got.ModelClasses)
	}
	if src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI] != SourceOrg {
		t.Fatalf("fast/openai provenance = %q, want org", src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI])
	}
	if src[FieldModelClasses+"."+ClassTop+"."+familyAnthropicWire] != SourcePlatform {
		t.Fatalf("top/anthropic-wire provenance = %q, want platform", src[FieldModelClasses+"."+ClassTop+"."+familyAnthropicWire])
	}

	// A conflict on ONE cell: the more specific level wins that cell, the
	// other level's other cells survive.
	team := Policy{ModelClasses: map[string]map[string]string{
		ClassFast: {familyOpenAI: "gpt-6-nano"},
	}}
	got, src = Resolve(
		Layer{Source: SourceTeam, Policy: team},
		Layer{Source: SourceOrg, Policy: org},
	)
	if got.ModelClasses[ClassFast][familyOpenAI] != "gpt-6-nano" {
		t.Fatalf("team must win the contested cell: %+v", got.ModelClasses)
	}
	if src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI] != SourceTeam {
		t.Fatalf("contested cell provenance = %q, want team", src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI])
	}
}

// The BLOCK provenance key answers only when no cell spoke: "default",
// or "<level>_lock" for a lock that pinned the block without setting
// cells. When cells spoke, the block key is OMITTED — a "default" beside
// named cells would lie about who decided.
func TestResolveModelClassesBlockProvenance(t *testing.T) {
	// No level speaks → the block key is "default".
	_, src := Resolve(Layer{Source: SourcePlatform, Policy: Policy{}})
	if src[FieldModelClasses] != SourceDefault {
		t.Fatalf("silent block provenance = %q, want default", src[FieldModelClasses])
	}
	// Cells spoke → the block key is omitted, the cells name themselves.
	_, src = Resolve(Layer{Source: SourceOrg, Policy: Policy{ModelClasses: map[string]map[string]string{
		ClassFast: {familyOpenAI: "gpt-6-mini"},
	}}})
	if _, spoken := src[FieldModelClasses]; spoken {
		t.Fatalf("block key present beside named cells: %q", src[FieldModelClasses])
	}
	if src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI] != SourceOrg {
		t.Fatalf("cell provenance = %q, want org", src[FieldModelClasses+"."+ClassFast+"."+familyOpenAI])
	}
	// A lock WITHOUT cells pins the block under the lock's provenance.
	_, src = Resolve(
		Layer{Source: SourceRun, Policy: Policy{ModelClasses: map[string]map[string]string{
			ClassTop: {familyOpenAI: "gpt-6-astra"},
		}}},
		Layer{Source: SourceTeam, Policy: Policy{Locks: []string{FieldModelClasses}}},
	)
	if src[FieldModelClasses] != SourceTeam+"_lock" {
		t.Fatalf("locked-without-cells provenance = %q, want team_lock", src[FieldModelClasses])
	}
	if got := src[FieldModelClasses+"."+ClassTop+"."+familyOpenAI]; got == SourceRun {
		t.Fatalf("the run's cell must be vetoed by the team lock: %q", got)
	}
	// The lock stops the descent at-or-below itself: the run's cell above
	// is vetoed, the shipped default answers — the reverse index built
	// from the resolved overrides must NOT carry it.
	got, _ := Resolve(
		Layer{Source: SourceRun, Policy: Policy{ModelClasses: map[string]map[string]string{
			ClassTop: {familyOpenAI: "gpt-6-astra"},
		}}},
		Layer{Source: SourceTeam, Policy: Policy{Locks: []string{FieldModelClasses}}},
	)
	tbl := ResolveClasses(got.ModelClasses)
	if target, ok := tbl.Target(ClassTop, familyOpenAI); ok && target == "gpt-6-astra" {
		t.Fatalf("the vetoed cell leaked into the table: %q", target)
	}
}

// Validation: the class vocabulary is closed; empty strings and
// prefixed specs refuse; unknown families and unknown model ids are
// accepted (families grow; a bogus id fails at dispatch, named).
func TestValidateModelClasses(t *testing.T) {
	ok := Policy{ModelClasses: map[string]map[string]string{
		ClassTop: {familyOpenAI: "gpt-6-astra", "gemini": "gemini-x"},
	}}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid overrides refused: %v", err)
	}
	// The BLOCK lock is expressible: model_classes must be IN the Fields
	// vocabulary Validate's lock check enforces, or the ADR's "locks bind
	// the whole block" ships unexpressible in production (review
	// R8e0e36 — the const existed, the slice did not).
	if err := Validate(Policy{Locks: []string{FieldModelClasses}}); err != nil {
		t.Fatalf("the model_classes block lock refused: %v", err)
	}
	if !validField(FieldModelClasses) {
		t.Fatal("model_classes is not a valid lock field")
	}
	bad := []struct {
		name string
		p    Policy
	}{
		{"unknown class", Policy{ModelClasses: map[string]map[string]string{"ultra": {familyOpenAI: "gpt-6-astra"}}}},
		{"empty class block", Policy{ModelClasses: map[string]map[string]string{ClassTop: {}}}},
		{"empty family", Policy{ModelClasses: map[string]map[string]string{ClassTop: {"": "x"}}}},
		{"empty spec", Policy{ModelClasses: map[string]map[string]string{ClassTop: {familyOpenAI: ""}}}},
		{"prefixed spec", Policy{ModelClasses: map[string]map[string]string{ClassTop: {familyOpenAI: "openai/gpt-6-astra"}}}},
	}
	for _, tc := range bad {
		if err := Validate(tc.p); err == nil {
			t.Errorf("%s: accepted, want refused", tc.name)
		}
	}
}

// An override that re-NAMES a model re-classifies it (first-wins on the
// resolved table): standard/anthropic-wire=claude-haiku-4-5 reverts
// haiku's crossings to the standard rung — coherent, named in the field
// doc.
func TestResolveClassesReclassification(t *testing.T) {
	tbl := ResolveClasses(map[string]map[string]string{
		ClassStandard: {familyAnthropicWire: "claude-haiku-4-5"},
	})
	if got := tbl.ClassOf(familyAnthropicWire, "claude-haiku-4-5"); got != ClassStandard {
		t.Fatalf("re-classified haiku = %s, want standard (standard names it first in the resolved reverse index)", got)
	}
	target, ok := tbl.Target(ClassStandard, familyOpenAI)
	if !ok || target != shippedOpenAIDefault {
		t.Fatalf("haiku's crossing = %q (ok=%v), want the standard rung", target, ok)
	}
}
