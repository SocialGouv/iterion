package webhooks

import (
	"reflect"
	"strings"
	"testing"
)

// The webhook PATCH distinguishes an ABSENT hold/allowlist (keep) from a
// non-nil EMPTY one (clear/widen) — a deliberate gesture that must survive a
// Mongo round-trip as EXPLICIT-empty ([]), not collapse into never-set:
// omitempty on the bson tag drops an empty slice on write, and a read-back
// nil is indistinguishable from "never set", so the provision-time adopt
// would resurrect the cleared value from the integration. These two fields
// therefore carry NO omitempty on their bson tags (the JSON tags keep it —
// the API shape is unchanged).
//
// Mutation that reddens this test: re-adding omitempty to either bson tag.
func TestConfigHoldAndAllowlistKeepExplicitEmptyOnMongo(t *testing.T) {
	for _, field := range []string{"HoldLabels", "LabelAllowlist"} {
		f, ok := reflect.TypeOf(Config{}).FieldByName(field)
		if !ok {
			t.Fatalf("Config.%s is gone — the adopt in pkg/forge depends on it", field)
		}
		if strings.Contains(f.Tag.Get("bson"), "omitempty") {
			t.Errorf("Config.%s bson tag %q carries omitempty: a deliberate [] write collapses to never-set on Mongo, and the cleared value is resurrected from the integration at the next silent re-provision", field, f.Tag.Get("bson"))
		}
		if !strings.Contains(f.Tag.Get("json"), "omitempty") {
			t.Errorf("Config.%s json tag lost omitempty — the API shape must stay as it was", field)
		}
	}
}
