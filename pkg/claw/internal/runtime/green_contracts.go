package runtime

import "fmt"

// QualityTier represents the level of testing rigor required or observed.
// Tiers are ordered: TargetedTests < PackageTests < WorkspaceTests < MergeReady.
type QualityTier int

const (
	// TargetedTests runs only the specific tests related to the change.
	// Go equivalent: go test ./specific/...
	TargetedTests QualityTier = iota
	// PackageTests runs all tests in the affected package.
	// Go equivalent: go test ./pkg/...
	PackageTests
	// WorkspaceTests runs all tests in the workspace.
	// Go equivalent: go test ./...
	WorkspaceTests
	// MergeReady requires all tests pass plus additional quality gates.
	// Go equivalent: go test ./... + go vet ./... + race detector
	MergeReady
)

// String returns the snake_case string for a QualityTier.
func (t QualityTier) String() string {
	switch t {
	case TargetedTests:
		return "targeted_tests"
	case PackageTests:
		return "package"
	case WorkspaceTests:
		return "workspace"
	case MergeReady:
		return "merge_ready"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// ParseQualityTier converts a string to a QualityTier.
func ParseQualityTier(s string) (QualityTier, error) {
	switch s {
	case "targeted_tests":
		return TargetedTests, nil
	case "package":
		return PackageTests, nil
	case "workspace":
		return WorkspaceTests, nil
	case "merge_ready":
		return MergeReady, nil
	default:
		return TargetedTests, fmt.Errorf("unknown quality tier %q", s)
	}
}

// GreenContract specifies the minimum quality tier required.
type GreenContract struct {
	RequiredLevel QualityTier `json:"required_level"`
}

// NewGreenContract creates a new GreenContract with the given required level.
func NewGreenContract(requiredLevel QualityTier) GreenContract {
	return GreenContract{RequiredLevel: requiredLevel}
}

// IsSatisfiedBy returns true if the observed level meets or exceeds the required level.
func (c GreenContract) IsSatisfiedBy(observedLevel QualityTier) bool {
	return observedLevel >= c.RequiredLevel
}

// Evaluate checks whether the observed level satisfies the contract.
// If observedLevel is nil (no testing was done), the contract is unsatisfied.
func (c GreenContract) Evaluate(observedLevel *QualityTier) GreenContractOutcome {
	if observedLevel == nil {
		return GreenContractOutcome{
			Satisfied:     false,
			RequiredLevel: c.RequiredLevel,
			ObservedLevel: nil,
		}
	}
	return GreenContractOutcome{
		Satisfied:     c.IsSatisfiedBy(*observedLevel),
		RequiredLevel: c.RequiredLevel,
		ObservedLevel: observedLevel,
	}
}

// GreenContractOutcome is the result of evaluating a GreenContract.
type GreenContractOutcome struct {
	Satisfied     bool         `json:"satisfied"`
	RequiredLevel QualityTier  `json:"required_level"`
	ObservedLevel *QualityTier `json:"observed_level,omitempty"`
}

// IsSatisfied returns whether the contract was satisfied.
func (o GreenContractOutcome) IsSatisfied() bool {
	return o.Satisfied
}
