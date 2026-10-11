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
// would resurrect the cleared value from the integration. These fields
// therefore carry NO omitempty on their bson tags (the JSON tags keep it —
// the API shape is unchanged).
//
// The third field's failure mode differs only in the consequence: a cleared
// AuthorAllowlist reading back nil is still allow-all to every reader (the
// forge orchestrator's len==0 path), so nothing resurrects a narrowing — but
// the explicit-empty property is what the store contract owes, and the
// round-trip proof lives in TestConfigStoreAuthorAllowlistClearRoundTrips
// below.
//
// Mutation that reddens this test: re-adding omitempty to any bson tag.
func TestConfigHoldAndAllowlistKeepExplicitEmptyOnMongo(t *testing.T) {
	for _, field := range []string{"HoldLabels", "LabelAllowlist", "AuthorAllowlist"} {
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
