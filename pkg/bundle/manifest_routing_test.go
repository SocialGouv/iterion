package bundle

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The author's routing block: decodes from the manifest, projects onto the
// shared record, and a block the fold cannot read fails the LOAD — a typo
// must surface at parse time, next to its source, not days later as a
// silently-ignored policy on a run nobody is watching.
func TestManifest_RoutingBlock(t *testing.T) {
	m, err := DecodeManifest([]byte("name: x\nrouting:\n  pair_order:\n    - claw+anthropic_key\n  triggers:\n    - usage_window\n  refused_pinned_key: park\n  strict: true\n  locks:\n    - pair_order\n"), "test")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	pol := m.RoutingPolicy()
	if len(pol.PairOrder) != 1 || pol.PairOrder[0] != "claw+anthropic_key" {
		t.Fatalf("pair_order = %v", pol.PairOrder)
	}
	if pol.RefusedPinnedKey != llmroute.RefusedPinnedPark || pol.Strict == nil || !*pol.Strict {
		t.Fatalf("block = %+v", pol)
	}
	if len(pol.Locks) != 1 || pol.Locks[0] != llmroute.FieldPairOrder {
		t.Fatalf("locks = %v", pol.Locks)
	}

	// A nil block projects the zero Policy — the layer sets nothing.
	empty, err := DecodeManifest([]byte("name: y\n"), "test")
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if got := empty.RoutingPolicy(); got.PairOrder != nil || got.Triggers != nil || got.RefusedPinnedKey != "" {
		t.Fatalf("nil block projected %+v, want the zero policy", got)
	}

	// An invalid block fails the load, naming the field.
	if _, err := DecodeManifest([]byte("name: z\nrouting:\n  triggers:\n    - budget\n"), "test"); err == nil || !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("decode(budget trigger) = %v, want the vocabulary refusal", err)
	}
	if _, err := DecodeManifest([]byte("name: z\nrouting:\n  pair_order:\n    - claude_code+not_a_slot\n"), "test"); err == nil || !strings.Contains(err.Error(), "unknown credential") {
		t.Fatalf("decode(bad slot) = %v, want the credential refusal", err)
	}
}
