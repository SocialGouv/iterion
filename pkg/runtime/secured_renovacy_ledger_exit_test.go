package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	var out []*ir.Edge
	for _, e := range wf.Edges {
		if e.From == "write_audit_md" {
			out = append(out, e)
		}
	}
	if len(out) < 3 {
		t.Fatalf("write_audit_md has %d outgoing edges, want the three the #2381 round mapped (back-edge, bucket, ledger exit)", len(out))
	}
	for _, e := range out {
		if !e.IsConditional() {
			t.Fatalf("write_audit_md -> %s is a bare edge: a malformed was_batch would be routed onward (the #2381 hazard)", e.To)
		}
	}
	var ledger *ir.Edge
	for _, e := range out {
		if e.To == "solo_banked" {
			ledger = e
		}
	}
	if ledger == nil {
		t.Fatal("the solo_banked ledger edge is gone — the #2381 guard moved")
	}
	if ledger.Condition != "was_batch" || !ledger.Negated {
		t.Fatalf("solo_banked is guarded by %q (negated=%v), want the negated was_batch of the #2381 guard", ledger.Condition, ledger.Negated)
	}
}
