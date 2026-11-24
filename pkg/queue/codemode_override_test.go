package queue

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The wire half of the launch-time codemode override (CM-5e, #3345, schema
// v33/v34/v35): the mode and the field-by-field bounds survive a marshal
// round-trip, and an omitted pair reads as absent — the "caller expressed
// nothing" the resolver's inherit rule depends on.
func TestRunMessageCarriesTheCodemodeOverride(t *testing.T) {
	t.Parallel()
	msg := RunMessage{
		V:        SchemaVersion,
		Codemode: "off",
		CodemodeBounds: &scripthost.Limits{
			ToolCalls: 256, ConcurrentToolCalls: 3, StoreEntries: 48, StoreBytes: 262144,
			Wall: 90 * time.Second,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte(`"concurrent_tool_calls":3`)) || !bytes.Contains(data, []byte(`"store_entries":48`)) || !bytes.Contains(data, []byte(`"store_bytes":262144`)) {
		t.Fatalf("payload omitted new limits: %s", data)
	}
	var back RunMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Codemode != "off" {
		t.Errorf("Codemode = %q after the round-trip, want off", back.Codemode)
	}
	if back.CodemodeBounds == nil || back.CodemodeBounds.ToolCalls != 256 || back.CodemodeBounds.ConcurrentToolCalls != 3 || back.CodemodeBounds.Wall != 90*time.Second || back.CodemodeBounds.StoreEntries != 48 || back.CodemodeBounds.StoreBytes != 262144 {
		t.Errorf("CodemodeBounds = %+v after the round-trip, want ToolCalls 256 / ConcurrentToolCalls 3 / Wall 90s / StoreEntries 48 / StoreBytes 262144", back.CodemodeBounds)
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

func TestCodemodeStoreQuotaRequiresVersion35(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		bounds   *scripthost.Limits
		wantsNew bool
	}{
		{&scripthost.Limits{StoreEntries: 48}, true},
		{&scripthost.Limits{StoreBytes: 262144}, true},
		{&scripthost.Limits{ConcurrentToolCalls: 3}, false},
		{nil, false},
	} {
		msg := RunMessage{V: 34, RunID: "quota", WorkflowName: "quota", IRCompiled: json.RawMessage(`{}`), CodemodeBounds: row.bounds}
		if err := msg.Validate(); row.wantsNew && !errors.Is(err, ErrSchemaVersion) || !row.wantsNew && err != nil {
			t.Errorf("v34 bounds %+v: error = %v, requires newer version = %t", row.bounds, err, row.wantsNew)
		}
		msg.V = 35
		if err := msg.Validate(); err != nil {
			t.Errorf("v35 bounds %+v refused: %v", row.bounds, err)
		}
	}
}

func TestResumeCodemodeQuotaPatchWire(t *testing.T) {
	t.Parallel()
	const markerRFC = "2026-01-01T00:00:00Z"
	marker, err := time.Parse(time.RFC3339, markerRFC)
	if err != nil {
		t.Fatal(err)
	}
	msg := RunMessage{V: SchemaVersion, RunID: "quota", WorkflowName: "quota", IRCompiled: json.RawMessage(`{}`),
		PublishedAtRFC: marker.Add(time.Second).Format(time.RFC3339Nano),
		Resume:         &ResumeSpec{CodemodeQuota: &CodemodeQuotaPatch{Bytes: 256, QueuedAt: marker}},
		CodemodeBounds: &scripthost.Limits{StoreEntries: 32, StoreBytes: 256}}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"codemode_quota":{"queued_at":"`+markerRFC+`","store_bytes":256}`)) {
		t.Fatalf("wire conflated the bytes-only patch with the replay bounds: %s", data)
	}
	var back RunMessage
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if patch := back.Resume.CodemodeQuota; patch == nil || *patch != (CodemodeQuotaPatch{Bytes: 256, QueuedAt: marker}) {
		t.Fatalf("decoded patch = %+v, want bytes only", patch)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("valid v35 with quota patch: %v", err)
	}
	published := back.PublishedAtRFC
	back.PublishedAtRFC = ""
	if err := back.Validate(); err == nil || errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("v35 quota patch without publication time = %v, want malformed message", err)
	}
	back.PublishedAtRFC = published
	back.Resume.CodemodeQuota.QueuedAt = time.Time{}
	if err := back.Validate(); err == nil || errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("v35 quota patch without an exact marker = %v, want malformed message", err)
	}
	back.Resume.CodemodeQuota.QueuedAt = marker
	back.Resume.CodemodeQuota.Bytes = 0
	if err := back.Validate(); err == nil || errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("v35 quota patch with zero dimensions = %v, want malformed message", err)
	}
	back.Resume.CodemodeQuota.Bytes = 256
	back.V = 34
	back.CodemodeBounds = nil // The patch alone requires v35.
	if err := back.Validate(); !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("v34 with quota patch = %v, want schema-version refusal", err)
	}
}
