package queue

import (
	"encoding/json"
	"testing"
)

// PeekEnvelope is the recovery path for messages this build REJECTS: it must
// decode the identity fields from any schema version — newer or older —
// without validating, because that is exactly the message a consumer needs to
// park on the DLQ with an actionable run status (issue #481).
func TestPeekEnvelope(t *testing.T) {
	for _, v := range []int{SchemaVersion - 1, SchemaVersion, SchemaVersion + 1} {
		payload, err := json.Marshal(&RunMessage{
			V:              v,
			RunID:          "run-env",
			TenantID:       "team-a",
			OwnerID:        "u1",
			PublishedAtRFC: "2026-08-26T08:00:00Z",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		env, err := PeekEnvelope(payload)
		if err != nil {
			t.Fatalf("v=%d: %v", v, err)
		}
		if env.V != v || env.RunID != "run-env" || env.TenantID != "team-a" || env.OwnerID != "u1" || env.PublishedAtRFC != "2026-08-26T08:00:00Z" {
			t.Errorf("v=%d: envelope = %+v", v, env)
		}
	}
}

// A payload too broken to even identify cannot be parked with a meaningful
// status flip — PeekEnvelope must say so, and the caller falls back to
// Term-and-log (the malformed branch of decodeOrTerm).
func TestPeekEnvelope_Errors(t *testing.T) {
	if _, err := PeekEnvelope([]byte("{not json")); err == nil {
		t.Error("malformed JSON must error")
	}
	if _, err := PeekEnvelope([]byte(`{"v":99,"tenant_id":"t"}`)); err == nil {
		t.Error("a payload with no run_id must error — nothing to recover against")
	}
}
