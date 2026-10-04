package platformcfg

import (
	"strings"
	"testing"
)

// The registry's own gate: grammar (one source of truth with the queue's
// subject suffix), duplicates, and the lifecycle vocabulary. Red when any
// row of the table loses its check.
func TestRunnerPoolsValidate(t *testing.T) {
	ok := func(pools ...RunnerPool) *RunnerPools { return &RunnerPools{Pools: pools} }
	if err := ok(RunnerPool{Name: "honorabilite", State: RunnerPoolActive}).Validate(); err != nil {
		t.Fatalf("a well-formed active pool validates: %v", err)
	}
	if err := ok(RunnerPool{Name: "no-state-yet"}).Validate(); err != nil {
		t.Fatalf("an empty state (provisioning default) validates: %v", err)
	}
	for _, bad := range []string{"", "-lead", "Upper", string(make([]byte, 32))} {
		if err := ok(RunnerPool{Name: bad}).Validate(); err == nil {
			t.Fatalf("Validate accepted name %q", bad)
		}
	}
	if err := ok(RunnerPool{Name: "a"}, RunnerPool{Name: "a"}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Fatalf("a duplicate name must be refused: %v", err)
	}
	if err := ok(RunnerPool{Name: "a", State: "live"}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "state") {
		t.Fatalf("an unknown state must be refused: %v", err)
	}
}
