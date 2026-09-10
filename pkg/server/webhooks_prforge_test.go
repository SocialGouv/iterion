package server

import "testing"

// An authorization outcome nobody set must never authorize: the zero value
// of the gate's result type is the refusing state, so a stub or an early
// return that forgets to set it fails closed.
func TestPRForgeGateOutcomeZeroValueRefuses(t *testing.T) {
	var zero prforgeGateOutcome
	if zero != gateRefused {
		t.Fatalf("the zero value of prforgeGateOutcome must be gateRefused, got %d", zero)
	}
	if zero == gateAuthorized {
		t.Fatalf("the zero value must never read as authorized")
	}
}
