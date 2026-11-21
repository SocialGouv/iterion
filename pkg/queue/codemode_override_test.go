package queue

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The wire half of the launch-time codemode override (CM-5e, #3345, schema
// v33/v34): the mode and the field-by-field bounds survive a marshal
// round-trip, and an omitted pair reads as absent — the "caller expressed
// nothing" the resolver's inherit rule depends on.
func TestRunMessageCarriesTheCodemodeOverride(t *testing.T) {
	t.Parallel()
	msg := RunMessage{
		V:        SchemaVersion,
		Codemode: "off",
		CodemodeBounds: &scripthost.Limits{
			ToolCalls: 256, ConcurrentToolCalls: 3,
			Wall: 90 * time.Second,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"concurrent_tool_calls":3`)) {
		t.Fatalf("payload omitted concurrent_tool_calls: %s", data)
	}
	var back RunMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Codemode != "off" {
		t.Errorf("Codemode = %q after the round-trip, want off", back.Codemode)
	}
	if back.CodemodeBounds == nil || back.CodemodeBounds.ToolCalls != 256 || back.CodemodeBounds.ConcurrentToolCalls != 3 || back.CodemodeBounds.Wall != 90*time.Second {
		t.Errorf("CodemodeBounds = %+v after the round-trip, want ToolCalls 256 / ConcurrentToolCalls 3 / Wall 90s", back.CodemodeBounds)
	}

	// Omitted fields of the bounds stay zero — the resolver reads them as
	// "the launch did not choose" field by field.
	raw := `{"v":33,"codemode":"on","codemode_bounds":{"tool_calls":64}}`
	var partial RunMessage
	if err := json.Unmarshal([]byte(raw), &partial); err != nil {
		t.Fatalf("unmarshal partial: %v", err)
	}
	if partial.Codemode != "on" || partial.CodemodeBounds == nil || partial.CodemodeBounds.ToolCalls != 64 || partial.CodemodeBounds.Memory != 0 || partial.CodemodeBounds.ConcurrentToolCalls != 0 {
		t.Errorf("partial bounds read %+v, want mode on with ToolCalls 64 and the rest zero", partial.CodemodeBounds)
	}

	// The override absent altogether reads absent.
	var none RunMessage
	if err := json.Unmarshal([]byte(`{}`), &none); err != nil {
		t.Fatalf("unmarshal none: %v", err)
	}
	if none.Codemode != "" || none.CodemodeBounds != nil {
		t.Errorf("absent override read %+v / %+v, want empty and nil — an absent launch must inherit, never declare", none.Codemode, none.CodemodeBounds)
	}
}
