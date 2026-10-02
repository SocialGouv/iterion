package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The budget persisted on a run (and shown in its header) carries a duration
// or the source as written — never an environment value an expansion read.
func TestPersistedBudgetNeverCarriesAnExpandedNonDuration(t *testing.T) {
	t.Setenv("PROBE_PERSIST_TEXT", "persist-echo-probe-value")
	if got := SnapshotBudgetForPersist(&ir.Budget{MaxDuration: "${PROBE_PERSIST_TEXT}"}).MaxDuration; got != "${PROBE_PERSIST_TEXT}" {
		t.Errorf("persisted max_duration = %q, want the source as written", got)
	}
	if got := SnapshotBudgetForPersist(&ir.Budget{MaxDuration: "${PROBE_PERSIST_UNSET:-30m}"}).MaxDuration; got != "30m" {
		t.Errorf("persisted max_duration = %q, want the concrete 30m", got)
	}
}
