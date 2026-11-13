package runtime

import "testing"

func TestQualityTierOrdering(t *testing.T) {
	if TargetedTests >= PackageTests {
		t.Error("TargetedTests should be < PackageTests")
	}
	if PackageTests >= WorkspaceTests {
		t.Error("PackageTests should be < WorkspaceTests")
	}
	if WorkspaceTests >= MergeReady {
		t.Error("WorkspaceTests should be < MergeReady")
	}
}

func TestQualityTierString(t *testing.T) {
	tests := []struct {
		tier QualityTier
		want string
	}{
		{TargetedTests, "targeted_tests"},
		{PackageTests, "package"},
		{WorkspaceTests, "workspace"},
		{MergeReady, "merge_ready"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("%d.String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestParseQualityTier(t *testing.T) {
	for _, s := range []string{"targeted_tests", "package", "workspace", "merge_ready"} {
		tier, err := ParseQualityTier(s)
		if err != nil {
			t.Errorf("ParseQualityTier(%q): %v", s, err)
		}
		if tier.String() != s {
			t.Errorf("roundtrip: %q → %q", s, tier.String())
		}
	}

	_, err := ParseQualityTier("bogus")
	if err == nil {
		t.Error("expected error for unknown tier")
	}
}

func TestGreenContractSatisfied(t *testing.T) {
	tests := []struct {
		required QualityTier
		observed QualityTier
		want     bool
	}{
		{TargetedTests, TargetedTests, true},
		{TargetedTests, MergeReady, true},
		{PackageTests, TargetedTests, false},
		{PackageTests, PackageTests, true},
		{PackageTests, WorkspaceTests, true},
		{MergeReady, WorkspaceTests, false},
		{MergeReady, MergeReady, true},
		{WorkspaceTests, MergeReady, true},
	}
	for _, tt := range tests {
		c := NewGreenContract(tt.required)
		got := c.IsSatisfiedBy(tt.observed)
		if got != tt.want {
			t.Errorf("GreenContract(%s).IsSatisfiedBy(%s) = %v, want %v",
				tt.required, tt.observed, got, tt.want)
		}
	}
}

func TestGreenContractEvaluate(t *testing.T) {
	c := NewGreenContract(PackageTests)

	// Observed meets requirement.
	ws := WorkspaceTests
	outcome := c.Evaluate(&ws)
	if !outcome.IsSatisfied() {
		t.Error("WorkspaceTests should satisfy PackageTests")
	}
	if outcome.RequiredLevel != PackageTests {
		t.Errorf("RequiredLevel = %s", outcome.RequiredLevel)
	}

	// Observed below requirement.
	tt := TargetedTests
	outcome = c.Evaluate(&tt)
	if outcome.IsSatisfied() {
		t.Error("TargetedTests should not satisfy PackageTests")
	}

	// No observation.
	outcome = c.Evaluate(nil)
	if outcome.IsSatisfied() {
		t.Error("nil observed should not satisfy")
	}
	if outcome.ObservedLevel != nil {
		t.Error("ObservedLevel should be nil")
	}
}

func TestGreenContractAllTiers(t *testing.T) {
	// All 4 tiers: require MergeReady, only MergeReady satisfies.
	c := NewGreenContract(MergeReady)
	for _, tier := range []QualityTier{TargetedTests, PackageTests, WorkspaceTests} {
		if c.IsSatisfiedBy(tier) {
			t.Errorf("MergeReady should not be satisfied by %s", tier)
		}
	}
	if !c.IsSatisfiedBy(MergeReady) {
		t.Error("MergeReady should be satisfied by MergeReady")
	}
}
