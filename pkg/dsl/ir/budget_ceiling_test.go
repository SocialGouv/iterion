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
		{
			name: "a zero duration is unlimited to the runtime, so it is raised",
			in:   Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "0s", MaxParallelBranches: 2},
			want: Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "1h", MaxParallelBranches: 2, CapImposed: true},
		},
		{
			name: "a negative duration is raised too",
			in:   Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "-1h", MaxParallelBranches: 2},
			want: Budget{MaxIterations: 10, MaxTokens: 500, MaxCostUSD: 1.0, MaxDuration: "1h", MaxParallelBranches: 2, CapImposed: true},
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

// A zero or negative duration ceiling is no platform limit: the bot keeps its
// own.
func TestBudgetClampToCeiling_NonPositiveDurationCeilingIsNoLimit(t *testing.T) {
	for _, ceiling := range []string{"0s", "-1h"} {
		b := Budget{MaxDuration: "5h"}
		if b.ClampToCeiling(&Budget{MaxDuration: ceiling}) || b.MaxDuration != "5h" || b.CapImposed {
			t.Errorf("ClampToCeiling under a %s ceiling = %+v, want the bot's 5h kept and no cap", ceiling, b)
		}
	}
}

// A ceiling written as a template is read from the process env — never a
// stored bot var — and the value it caps a run to is that expansion, not the
// template read again later.
func TestClampToCeilingFreezesTheCeilingItApplies(t *testing.T) {
	t.Setenv("ITERION_TEST_CLAMP_CEILING", "")
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_TEST_CLAMP_CEILING" {
			return "12h", true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)

	b := &Budget{MaxDuration: "5h"}
	if !b.ClampToCeiling(&Budget{MaxDuration: "${ITERION_TEST_CLAMP_CEILING:-1h}"}) {
		t.Error("ClampToCeiling reported no cap for 5h under a 1h ceiling")
	}
	if b.MaxDuration != "1h" {
		t.Errorf("MaxDuration = %q, want the 1h the process env gives the ceiling — never the stored 12h", b.MaxDuration)
	}
}

// The duration the ceiling judged is the duration that runs. A bot's
// ${ITERION_…} budget is expanded again when the run's budget is built, and a
// stored bot var changed in between would otherwise escape the ceiling.
func TestClampToCeilingFreezesTheJudgedDuration(t *testing.T) {
	stored := "1h"
	SetEnvOverlay(func(name string) (string, bool) {
		if name == "ITERION_TEST_CLAMP_MAX_DURATION" {
			return stored, true
		}
		return "", false
	})
	defer SetEnvOverlay(nil)

	b := &Budget{MaxDuration: "${ITERION_TEST_CLAMP_MAX_DURATION:-30m}"}
	if b.ClampToCeiling(&Budget{MaxDuration: "2h"}) {
		t.Error("ClampToCeiling reported a cap for the bot's own 1h under a 2h ceiling")
	}
	if b.CapImposed {
		t.Error("CapImposed = true — keeping the bot's own 1h under a 2h ceiling imposes no cap")
	}
	stored = "12h"
	if got := ExpandEnvWithDefault(b.MaxDuration); got != "1h" {
		t.Errorf("MaxDuration expands to %q once the stored var changed, want the judged 1h", got)
	}
}
