package prforge

import "testing"

func TestNeedsAutoHeal(t *testing.T) {
	cases := []struct {
		action, reason string
		want           bool
	}{
		{"dequeued", "MERGE_CONFLICT", true},
		{"dequeued", "CI_FAILURE", true},
		{"dequeued", "INVALID_MERGE_COMMIT", true},
		{"dequeued", "DEQUEUED_MANUALLY", false},
		{"dequeued", "", false},
		{"opened", "MERGE_CONFLICT", false},
		{"synchronize", "", false},
	}
	for _, c := range cases {
		p := Parsed{Action: c.action, DequeueReason: c.reason}
		if got := p.NeedsAutoHeal(); got != c.want {
			t.Errorf("NeedsAutoHeal(action=%q reason=%q) = %v, want %v", c.action, c.reason, got, c.want)
		}
	}
}
