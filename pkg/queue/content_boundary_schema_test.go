package queue

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestRunMessage_ATeamGatewayMessageOlderThanTheProducerParks: a server older
// than ContentBoundaryProducerSchemaVersion seals no gateway key and arms no
// AmbientLLMRefused, but its resume replays the run document's frozen stamp —
// so a stale replica mid-rollout (or a server rolled back) resuming a run a
// v30 replica stamped team_gateway publishes a pre-v30 message carrying the
// stamp over an old-rules bundle, whose gateway routes would fall to the
// pod's environment. Each row is the JSON such a publisher writes, decoded
// and validated the way the runner's Delivery.Decode does; the team_gateway
// rows park as a version mismatch (ErrSchemaVersion: transient, a resume from
// an upgraded server recovers the run), every other row runs.
//
// Mutation: drop the rule from Validate → the two pre-v30 team_gateway rows
// validate.
func TestRunMessage_ATeamGatewayMessageOlderThanTheProducerParks(t *testing.T) {
	for _, row := range []struct {
		name   string
		body   string
		parked bool
	}{
		{"a v29 resume replaying a team_gateway stamp", `{"v":29,"run_id":"r","workflow_name":"w","ir_compiled":{},"content_boundary":{"kind":"team_gateway","key_id":"k","gateway_host":"gw.example"}}`, true},
		{"a v27 resume replaying a team_gateway stamp", `{"v":27,"run_id":"r","workflow_name":"w","ir_compiled":{},"content_boundary":{"kind":"team_gateway"}}`, true},
		{"a v29 own_keys stamp (the control)", `{"v":29,"run_id":"r","workflow_name":"w","ir_compiled":{},"content_boundary":{"kind":"own_keys"}}`, false},
		{"an unstamped v29 message (the control)", `{"v":29,"run_id":"r","workflow_name":"w","ir_compiled":{}}`, false},
		{"a v30 team_gateway message (the control)", `{"v":30,"run_id":"r","workflow_name":"w","ir_compiled":{},"content_boundary":{"kind":"team_gateway","key_id":"k","gateway_host":"gw.example"}}`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var m RunMessage
			if err := json.Unmarshal([]byte(row.body), &m); err != nil {
				t.Fatal(err)
			}
			err := m.Validate()
			if !row.parked {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrSchemaVersion) {
				t.Fatalf("Validate = %v, want %v — the run parks for a resume, never runs a team_gateway stamp over an old-rules bundle", err, ErrSchemaVersion)
			}
		})
	}
}
