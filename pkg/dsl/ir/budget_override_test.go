package ir

import "testing"

func TestApplyBudgetOverrides(t *testing.T) {
	tests := []struct {
		name string
		base *Budget
		over BudgetOverrides
		want *Budget // nil = expect wf.Budget stays nil
	}{
		{
			name: "no override, nil base stays nil",
			base: nil,
			over: BudgetOverrides{},
			want: nil,
		},
		{
			name: "no override, existing base untouched",
			base: &Budget{MaxCostUSD: 60, MaxDuration: "2h"},
			over: BudgetOverrides{},
			want: &Budget{MaxCostUSD: 60, MaxDuration: "2h"},
		},
		{
			name: "cost+duration override on existing base, rest inherits",
			base: &Budget{MaxCostUSD: 60, MaxDuration: "2h", MaxParallelBranches: 1, MaxTokens: 5000},
			over: BudgetOverrides{MaxCostUSD: 120, MaxDuration: "4h"},
			want: &Budget{MaxCostUSD: 120, MaxDuration: "4h", MaxParallelBranches: 1, MaxTokens: 5000},
		},
		{
			name: "nil base allocated when an override is supplied",
			base: nil,
			over: BudgetOverrides{MaxCostUSD: 120},
			want: &Budget{MaxCostUSD: 120},
		},
		{
			name: "all five fields overridden",
			base: &Budget{MaxCostUSD: 1, MaxDuration: "1m", MaxTokens: 1, MaxIterations: 1, MaxParallelBranches: 1},
			over: BudgetOverrides{MaxCostUSD: 9, MaxTokens: 9, MaxDuration: "9h", MaxIterations: 9, MaxParallelBranches: 9},
			want: &Budget{MaxCostUSD: 9, MaxDuration: "9h", MaxTokens: 9, MaxIterations: 9, MaxParallelBranches: 9},
		},
		{
			name: "zero/negative fields do not override",
			base: &Budget{MaxCostUSD: 60, MaxTokens: 5000},
			over: BudgetOverrides{MaxCostUSD: -5, MaxTokens: 0},
			want: &Budget{MaxCostUSD: 60, MaxTokens: 5000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wf := &Workflow{Budget: tt.base}
			ApplyBudgetOverrides(wf, tt.over)
			switch {
			case tt.want == nil && wf.Budget != nil:
				t.Fatalf("expected nil budget, got %+v", wf.Budget)
			case tt.want != nil && wf.Budget == nil:
				t.Fatalf("expected %+v, got nil budget", tt.want)
			case tt.want != nil && *wf.Budget != *tt.want:
				t.Fatalf("budget mismatch:\n got  %+v\n want %+v", *wf.Budget, *tt.want)
			}
		})
	}
}

func TestApplyBudgetOverrides_NilWorkflow(t *testing.T) {
	// Must not panic.
	ApplyBudgetOverrides(nil, BudgetOverrides{MaxCostUSD: 10})
}

func TestBudgetOverridesValidate(t *testing.T) {
	tests := []struct {
		name    string
		over    BudgetOverrides
		wantErr bool
	}{
		{"empty is valid", BudgetOverrides{}, false},
		{"good duration", BudgetOverrides{MaxDuration: "90m"}, false},
		{"hours", BudgetOverrides{MaxDuration: "4h"}, false},
		{"bad duration", BudgetOverrides{MaxDuration: "4 hours"}, true},
		{"bare number invalid", BudgetOverrides{MaxDuration: "120"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.over.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestBudgetOverridesIsZero(t *testing.T) {
	if !(BudgetOverrides{}).IsZero() {
		t.Fatal("empty BudgetOverrides should be zero")
	}
	if (BudgetOverrides{MaxCostUSD: 1}).IsZero() {
		t.Fatal("non-empty BudgetOverrides should not be zero")
	}
	// The imposed-cap marker alone carries no cap: nothing to apply, put
	// on the wire, or persist (it used to persist as an empty object).
	if !(BudgetOverrides{CapImposed: true}).IsZero() {
		t.Fatal("CapImposed-only BudgetOverrides should be zero — the marker never travels without the cap it marks")
	}
	if !(BudgetOverrides{MaxCostUSD: 5, CapImposed: true}).IsZero() == false {
		t.Fatal("a clamped cap with its marker should not be zero")
	}
}
