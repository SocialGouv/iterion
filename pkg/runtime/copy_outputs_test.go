package runtime

import (
	"testing"
)

// TestCopyOutputs_DeepCopiesNestedMaps guards the data-integrity fix
// that promoted copyOutputs from a two-level shallow clone to a full
// recursive copy. Two parallel branches sharing a nested
// map[string]interface{} from an upstream node previously raced on
// the inner hashtable; the deep copy gives each branch its own
// independent tree.
func TestCopyOutputs_DeepCopiesNestedMaps(t *testing.T) {
	src := map[string]map[string]any{
		"node_a": {
			"obj": map[string]any{
				"k1": "v1",
				"k2": map[string]any{"nested": "value"},
			},
			"list": []any{
				"a",
				map[string]any{"in_list": true},
			},
		},
	}

	dst := copyOutputs(src)

	// Mutate the destination's deep structure and ensure the source
	// is untouched.
	dstObj := dst["node_a"]["obj"].(map[string]any)
	dstObj["k1"] = "mutated"
	dstObj["k2"].(map[string]any)["nested"] = "mutated"
	dst["node_a"]["list"].([]any)[1].(map[string]any)["in_list"] = false

	srcObj := src["node_a"]["obj"].(map[string]any)
	if srcObj["k1"] != "v1" {
		t.Errorf("source map mutated via shallow copy: k1=%v", srcObj["k1"])
	}
	if nested := srcObj["k2"].(map[string]any)["nested"]; nested != "value" {
		t.Errorf("source nested map mutated via shallow copy: nested=%v", nested)
	}
	if inList := src["node_a"]["list"].([]any)[1].(map[string]any)["in_list"]; inList != true {
		t.Errorf("source slice element mutated via shallow copy: in_list=%v", inList)
	}
}

// TestCopyOutputs_PreservesScalars is the inverse — scalars are
// safe to share by value, so the deep copy still returns them as-is.
func TestCopyOutputs_PreservesScalars(t *testing.T) {
	src := map[string]map[string]any{
		"node_b": {
			"s":  "string",
			"i":  42,
			"f":  3.14,
			"b":  true,
			"n":  nil,
			"by": []byte("bytes"),
		},
	}
	dst := copyOutputs(src)
	if got := dst["node_b"]["s"]; got != "string" {
		t.Errorf("scalar string copy: %v", got)
	}
	if got := dst["node_b"]["i"]; got != 42 {
		t.Errorf("scalar int copy: %v", got)
	}
}
