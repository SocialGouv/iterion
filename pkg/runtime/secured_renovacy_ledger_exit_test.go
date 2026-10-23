package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// The bot-level witness of #2381: secured-renovacy's ledger exit carries the
// guard its iteration back-edge shares, so a malformed producer output —
// was_batch missing — matches NO outgoing edge of write_audit_md and dies
// NO_OUTGOING_EDGE. The mechanism on a program is
// TestAGuardedLoopExitRefusesAnOutputWithNoConditionField; this one holds the
// SHIPPED bot to the same shape (the compiled IR, not a hand-built graph).
// The bare exit this guard replaced routed whatever the producer answered
// into the Phase 2 solo ledger.
func TestTheSecuredRenovacyLedgerExitCarriesTheGuard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "bots", "secured-renovacy", "main.bot"))
	if err != nil {
		t.Fatalf("the shipped bot is unreadable: %v", err)
	}
	wf := compileBotText(t, string(src))
	// The exact three-edge contract, conditions in their simple form: an
	// Expression substitute (e.g. `outputs.write_audit_md.was_batch != true`,
	// which a MISSING field satisfies) must fail here as loudly as a lost
	// edge — the malformed-producer case is the one the guard exists for.
	want := map[string]struct {
		cond string
		neg  bool
	}{
		"select_candidate": {"was_batch", true},
		"bucket_families":  {"was_batch", false},
		"solo_banked":      {"was_batch", true},
	}
	seen := 0
	for _, e := range wf.Edges {
		if e.From != "write_audit_md" {
			continue
		}
		seen++
		w, ok := want[e.To]
		if !ok {
			t.Fatalf("unexpected outgoing edge write_audit_md -> %s", e.To)
		}
		if e.Condition != w.cond || e.Negated != w.neg {
			t.Fatalf("write_audit_md -> %s guarded by %q (negated=%v), want the simple %q (negated=%v)", e.To, e.Condition, e.Negated, w.cond, w.neg)
		}
		if e.Expression != nil {
			t.Fatalf("write_audit_md -> %s carries an Expression where the simple was_batch guard belongs", e.To)
		}
	}
	if seen != len(want) {
		t.Fatalf("write_audit_md has %d outgoing edges, want exactly the three of the #2381 contract", seen)
	}
}
