// The resume-side half of the mono_family pin (#1221): the launch refusal
// #2258 delivered is the same judgement at a different moment, so both
// moments call ONE read-only predicate — and the resume one must leave the
// run's frozen inputs byte-identical, because a resumed attempt replays the
// inputs the launch froze and that replay is the whole bug.
//
// Mutants (each seen red before the predicate shipped): the prefix of the
// refusal narrowed to an operator pin (`ov != ""` kept on the resume side)
// → the resolver-chosen half reds; the dual guard dropped → the dual case
// reds; the trim/lower-case normalisation dropped → the spelling case reds;
// the predicate made to write the resolved family back into inputs → the
// frozen-inputs case reds.

package reviewtopology

import (
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A resume whose frozen family no credential serves any more is REFUSED,
// and the refusal carries the three things an operator needs: which family
// the run is pinned to, which ones this resume could serve, and the fix —
// which at this moment is NOT "remove the pin" (a resume cannot: the next
// one replays the same value) but "launch a fresh run".
func TestFrozenPinNoCredentialServesRefusesTheResume(t *testing.T) {
	frozen := map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyGPT}
	err := RefuseFrozenPin(wfWithVars(VarReviewMode), frozen, fams(FamilyClaude))
	if err == nil {
		t.Fatal("a resume whose frozen mono_family no credential serves must be refused — it starts and dies at its first review node")
	}
	for _, want := range []string{`"gpt"`, "claude", "frozen at launch", "launch a fresh run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the resume refusal does not carry %q: %v", want, err)
		}
	}
	// The remedy of the OTHER moment must not leak into this one: "remove
	// the pin" is advice a resume cannot act on.
	if strings.Contains(err.Error(), "remove the pin") {
		t.Errorf("the resume refusal offers the launch's remedy, which a resume cannot take: %v", err)
	}
}

// The run's FROZEN inputs are the launch's, byte for byte, after the
// judgement — refused or not. This is the property the ticket names: a
// parked run resumes with its inputs frozen at launch, and a resume that
// rewrote them would answer a question nobody asked.
func TestFrozenPinJudgementRewritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frozen map[string]any
		fams   FamilySet
	}{
		{"refused", map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyGPT}, fams(FamilyClaude)},
		{"served", map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyGPT}, fams(FamilyGPT)},
		{"dual", map[string]any{VarReviewMode: ModeDual, VarMonoFamily: FamilyGPT}, fams(FamilyClaude)},
		{"no family named", map[string]any{VarReviewMode: ModeMono}, fams(FamilyClaude)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := maps.Clone(tc.frozen)
			_ = RefuseFrozenPin(wfWithVars(VarReviewMode), tc.frozen, tc.fams)
			if !reflect.DeepEqual(tc.frozen, before) {
				t.Fatalf("the resume judgement rewrote the run's frozen inputs: %+v, want %+v", tc.frozen, before)
			}
		})
	}
}

// A family the resume CAN serve passes, whatever its provenance, and so
// does a run that names none.
func TestFrozenPinServedOrUnnamedPasses(t *testing.T) {
	wf := wfWithVars(VarReviewMode)
	if err := RefuseFrozenPin(wf, map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyGPT}, fams(FamilyClaude, FamilyGPT)); err != nil {
		t.Errorf("a frozen family the resume serves must pass: %v", err)
	}
	if err := RefuseFrozenPin(wf, map[string]any{VarReviewMode: ModeMono}, fams(FamilyClaude)); err != nil {
		t.Errorf("a run naming no family has no pin to judge: %v", err)
	}
	if err := RefuseFrozenPin(wf, nil, fams(FamilyClaude)); err != nil {
		t.Errorf("a run with no inputs at all has no pin to judge: %v", err)
	}
}

// The judgement is on the VALUE and on anything that is not DUAL — never on
// who wrote the value, nor on the mode being spelled out.
//
// Provenance: at launch a resolver-chosen family never refuses (it came from
// the set by construction); at resume the set has moved, so a run the
// resolver put on claude is refused once the Anthropic credential is gone.
// Nothing in the frozen inputs distinguishes the two, which is deliberate.
//
// Mode: a run launched BEFORE the injection existed carries the operator's
// pin beside an "auto" — or no review_mode at all — and the declaring bots'
// own default is mono. Those are exactly the five parked runs that replayed
// a stale pin in the 2026-09-22 incident, so a judgement gated on the mode
// being literally "mono" would miss the population the ticket was filed
// over. Hence "not dual", not "== mono".
func TestFrozenPinIsJudgedWhateverItsProvenanceOrMode(t *testing.T) {
	wf := wfWithVars(VarReviewMode)
	for _, tc := range []struct {
		name   string
		frozen map[string]any
	}{
		{"resolver-chosen, its credential gone since", map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyClaude}},
		{"a pre-injection run frozen on auto", map[string]any{VarReviewMode: ModeAuto, VarMonoFamily: FamilyClaude}},
		{"a pre-injection run naming no mode at all", map[string]any{VarMonoFamily: FamilyClaude}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RefuseFrozenPin(wf, tc.frozen, fams(FamilyGPT)); err == nil {
				t.Fatalf("a frozen claude family no credential serves must refuse the resume: %+v", tc.frozen)
			}
		})
	}
}

// A DUAL run fans out to both families and its mono_family means nothing
// (a dual launch deletes it); a stale value left by a --var must not ground
// the resume.
func TestFrozenPinIsSilentOnADualRun(t *testing.T) {
	frozen := map[string]any{VarReviewMode: ModeDual, VarMonoFamily: FamilyGPT}
	if err := RefuseFrozenPin(wfWithVars(VarReviewMode), frozen, fams(FamilyClaude)); err != nil {
		t.Fatalf("a dual run has no mono family to judge: %v", err)
	}
}

// Opt-in, like every other injection this package owns: a bot that declares
// no review_mode gets no judgement, whatever its inputs happen to hold.
func TestFrozenPinIsSilentOnABotThatDeclaresNoReviewMode(t *testing.T) {
	frozen := map[string]any{VarReviewMode: ModeMono, VarMonoFamily: FamilyGPT}
	if err := RefuseFrozenPin(wfWithVars("workspace_dir"), frozen, fams(FamilyClaude)); err != nil {
		t.Fatalf("a bot that declares no review_mode is not this package's: %v", err)
	}
	if err := RefuseFrozenPin(nil, frozen, fams(FamilyClaude)); err != nil {
		t.Fatalf("no workflow at all declares nothing: %v", err)
	}
}

// The frozen value is read the way the family keys are written — trimmed
// and lower-case — so a run launched with `--var mono_family=GPT` (honoured
// at the canonical spelling by the launch) is not refused on resume for its
// spelling, and a padded value is not refused for its padding.
func TestFrozenPinReadsTheCanonicalSpelling(t *testing.T) {
	wf := wfWithVars(VarReviewMode)
	for _, spelling := range []string{"GPT", " gpt ", "Gpt"} {
		frozen := map[string]any{VarReviewMode: ModeMono, VarMonoFamily: spelling}
		if err := RefuseFrozenPin(wf, frozen, fams(FamilyGPT)); err != nil {
			t.Errorf("a frozen %q on a served family must pass: %v", spelling, err)
		}
	}
	// And the refusal names the family without its padding, so an operator
	// can match it against the credential list.
	frozen := map[string]any{VarReviewMode: ModeMono, VarMonoFamily: " gpt "}
	err := RefuseFrozenPin(wf, frozen, fams(FamilyClaude))
	if err == nil || !strings.Contains(err.Error(), `"gpt"`) {
		t.Fatalf("the refusal must name the trimmed family: %v", err)
	}
}

// The two moments are ONE predicate with two remedies: whatever else
// changes, the launch keeps saying "remove the pin" (it can) and the resume
// keeps saying it cannot. A single shared message would mis-advise one of
// them.
func TestThePinRemedyBelongsToItsMoment(t *testing.T) {
	launch := RefusePinnedFamily(FamilyGPT, fams(FamilyClaude), AtLaunch)
	resume := RefusePinnedFamily(FamilyGPT, fams(FamilyClaude), AtResume)
	if launch == nil || resume == nil {
		t.Fatal("both moments must refuse a family nothing serves")
	}
	if !strings.Contains(launch.Error(), "on this launch") || !strings.Contains(launch.Error(), "remove the pin, or add the credential") {
		t.Errorf("the launch refusal lost its moment or its remedy: %v", launch)
	}
	if !strings.Contains(resume.Error(), "on this resume") {
		t.Errorf("the resume refusal does not name its moment: %v", resume)
	}
	if launch.Error() == resume.Error() {
		t.Error("the two moments carry the same text — then one of them advises a move its operator cannot make")
	}
}
