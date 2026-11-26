package delegate

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// #3048: a gateway budget refusal whose type did not survive a hop (a layer
// that formatted with %v, a runner's text relay) is still the spend ceiling:
// the flattened-text readings the usage park and the chain reading fall back
// on recognise it as they recognise "spend limit".
func TestFlattenedBudgetRefusalIsASpendCeiling(t *testing.T) {
	spend, ok := SpendCeilingRefusal(BackendClaw, &api.APIError{Provider: "openai", StatusCode: 422,
		Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"})
	if !ok {
		t.Fatal("not typed as a spend ceiling")
	}
	flat := fmt.Errorf("node %q failed: %v", "review", spend)
	if !UsageWindowInFlattenedError(flat.Error()) {
		t.Errorf("UsageWindowInFlattenedError(%q) = false", flat)
	}
	if !IsAccountRefusal(flat) {
		t.Errorf("IsAccountRefusal(%q) = false", flat)
	}
}
