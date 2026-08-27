package ir

import "testing"

func TestBudgetClampToCeiling(t *testing.T) {
	ceiling := &Budget{MaxIterations: 100, MaxTokens: 1000, MaxCostUSD: 5.0, MaxDuration: "1h", MaxParallelBranches: 4}

	cases := []struct {
		name string
		in   Budget
		want Budget
	}{
		{
			name: "over-ceiling values clamped down",
			in:   Budget{MaxIterations: 500, MaxTokens: 9000, MaxCostUSD: 50, MaxDuration: "4h", MaxParallelBranches: 32},
			want: Budget{MaxIterations: 100, MaxTokens: 1000, MaxCostUSD: 5.0, MaxDuration: "1h", MaxParallelBranches: 4, CapImposed: true},
		},
		{
			name: "under-ceiling values preserved",
			in:   Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "10m", MaxParallelBranches: 2},
			want: Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "10m", MaxParallelBranches: 2},
		},
		{
			name: "zero (unlimited) fields raised to ceiling — unbudgeted bot inherits the cap",
			in:   Budget{},
			want: Budget{MaxIterations: 100, MaxTokens: 1000, MaxCostUSD: 5.0, MaxDuration: "1h", MaxParallelBranches: 4, CapImposed: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.in
			b.ClampToCeiling(ceiling)
			if b != tc.want {
				t.Errorf("ClampToCeiling = %+v, want %+v", b, tc.want)
			}
		})
	}
}

func TestBudgetClampToCeiling_PartialCeiling(t *testing.T) {
	// A ceiling with only MaxIterations set must not touch other dimensions.
	b := Budget{MaxIterations: 500, MaxCostUSD: 999, MaxDuration: "9h"}
	b.ClampToCeiling(&Budget{MaxIterations: 50})
	if b.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", b.MaxIterations)
	}
	if b.MaxCostUSD != 999 || b.MaxDuration != "9h" {
		t.Errorf("unset ceiling dimensions were modified: %+v", b)
	}
}

// TestBudgetClampToCeilingMarksImposedCap pins the marker the runtime's
// exit grace keys on: a clamp that actually changes something records
// that the resulting cap was imposed from outside the run; a no-op clamp
// (already under every ceiling) records nothing.
func TestBudgetClampToCeilingMarksImposedCap(t *testing.T) {
	b := &Budget{MaxCostUSD: 100}
	b.ClampToCeiling(&Budget{MaxCostUSD: 10})
	if !b.CapImposed {
		t.Fatal("a clamp that lowered the cap must mark it imposed")
	}

	under := &Budget{MaxCostUSD: 5}
	under.ClampToCeiling(&Budget{MaxCostUSD: 10})
	if under.CapImposed {
		t.Fatal("a no-op clamp must not mark the cap imposed")
	}

	unbudgeted := &Budget{}
	unbudgeted.ClampToCeiling(&Budget{MaxCostUSD: 10})
	if !unbudgeted.CapImposed {
		t.Fatal("imposing a cap on an unbudgeted run is an imposed cap")
	}
}
