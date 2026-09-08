package retrypolicy

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A code the engine declares but nobody classified falls back to today's
// behaviour on BOTH surfaces — the cloud redelivery resumes it, the CLI
// loop refuses it. That silent split is what let a deterministic compute
// failure be auto-resumed seven times. Every declared code gets a row.
func TestEveryReservedFailureCodeIsClassified(t *testing.T) {
	for _, code := range store.ReservedFailureCodes {
		if Classify(code) == DispositionUnknown {
			t.Errorf("%s has no row in the automatic-resume classification — "+
				"decide whether an automatic resume can change its outcome", code)
		}
	}
	if len(classification) != len(store.ReservedFailureCodes) {
		t.Errorf("the table has %d rows for %d declared codes — it carries a value the vocabulary does not",
			len(classification), len(store.ReservedFailureCodes))
	}
}

// The empty code is UNKNOWN, not a verdict: a legacy row with no code, and
// paused_operator (which never carries one), must keep resuming.
func TestUnknownCodeIsNeitherDeterministicNorAutoResumable(t *testing.T) {
	if IsDeterministic("") {
		t.Error(`IsDeterministic("") = true — every legacy failed_resumable row would stop coming back`)
	}
	if AutoResumable("") {
		t.Error(`AutoResumable("") = true — an unclassified failure would be re-driven blind`)
	}
	if IsDeterministic("A_CODE_FROM_A_NEWER_BINARY") {
		t.Error("an unknown code read as deterministic — a newer engine's vocabulary would strand runs here")
	}
}

// The two predicates answer different questions and must not collapse into
// each other: an infrastructure failure is neither hopeless (the cloud
// redelivery cures it) nor the CLI loop's business.
func TestDispositionsAreDistinct(t *testing.T) {
	cases := []struct {
		code                      store.FailureCode
		deterministic, autoResume bool
	}{
		{store.FailureExpressionFailed, true, false},
		{store.FailureIRUnloadable, true, false},
		{store.FailureFailNode, true, false},
		{store.FailureToolFailedPermanent, true, false},
		// The in-node recipe already compacted TWICE and gave up
		// ("compaction did not reduce context enough to fit the model
		// window"); a resume rehydrates the SAME persisted conversation for
		// the same node, so no attempt after this one is smaller.
		{store.FailureContextLengthExceeded, true, false},
		{store.FailureExecutionFailed, false, true},
		{store.FailureUsageLimitBlocked, false, true},
		{store.FailureBudgetExceeded, false, true},
		{store.FailureInterrupted, false, false},
		{store.FailureSandboxCapacity, false, false},
		// Re-executable, NOT deterministic: an agent's next sample may
		// conform, so parking these would strand runs that recover.
		{store.FailureSchemaValidation, false, false},
		{store.FailureNoOutgoingEdge, false, false},
		// Every claim re-MATERIALISES the sealed OAuth-forfait blob into a
		// fresh file and refreshes it on the spot when it is at/past its
		// expiry lead (runner.injectCredentials → startOAuthRefreshers). So
		// the effective token on the next attempt can differ from the one
		// that was refused, even though the sealed blob does not.
		{store.FailureAuthFailed, false, false},
		// The provider answered about the MODEL, not about the request:
		// no sample differs, no wait helps. The one provider rejection
		// that clears the deterministic bar.
		{store.FailureModelUnavailable, true, false},
		// The DECLARED schema, refused before any request was built —
		// unlike FailureSchemaValidation two rows above, where a request
		// was served and the next sample may conform.
		{store.FailureSchemaUnusable, true, false},
	}
	for _, c := range cases {
		if got := IsDeterministic(c.code); got != c.deterministic {
			t.Errorf("IsDeterministic(%s) = %v, want %v", c.code, got, c.deterministic)
		}
		if got := AutoResumable(c.code); got != c.autoResume {
			t.Errorf("AutoResumable(%s) = %v, want %v", c.code, got, c.autoResume)
		}
	}
}
