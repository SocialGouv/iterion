package forge

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// For the replace-style fields of a parked request, nil and empty mean
// OPPOSITE things to the orchestrator: nil is "leave the stored value
// alone", empty is "clear it". bson's omitempty drops an empty slice or
// map, so tagging those fields with it turns `hold_labels: []` — the
// operator lifting every hold — into nil on the way back out of Mongo, and
// the approval silently replays as a no-op.
//
// Only the CLOUD store serializes, so the memory store used by tests and
// local mode cannot catch this: it has to be asserted on the encoding
// itself.
func TestProvisionApproval_EmptyIsNotNilThroughBSON(t *testing.T) {
	in := ProvisionApproval{
		ID:             "a1",
		OrgID:          "o1",
		TenantID:       "t1",
		HoldLabels:     []string{},
		LabelAllowlist: []string{},
		LaunchVars:     map[string]string{},
	}
	raw, err := bson.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ProvisionApproval
	if err := bson.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.HoldLabels == nil {
		t.Error(`HoldLabels: [] became nil — "lift every hold" would replay as "leave the holds alone"`)
	}
	if out.LabelAllowlist == nil {
		t.Error(`LabelAllowlist: [] became nil — "open the lane" would replay as "leave the allowlist alone"`)
	}
	if out.LaunchVars == nil {
		t.Error(`LaunchVars: {} became nil — "clear the launch vars" would replay as "leave them alone"`)
	}

	// The other direction must still hold: an absent field stays nil, or
	// every request would read as "clear everything".
	rawNil, err := bson.Marshal(ProvisionApproval{ID: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	var outNil ProvisionApproval
	if err := bson.Unmarshal(rawNil, &outNil); err != nil {
		t.Fatal(err)
	}
	if outNil.HoldLabels != nil || outNil.LabelAllowlist != nil || outNil.LaunchVars != nil {
		t.Errorf("an unmentioned field must stay nil: hold=%v labels=%v vars=%v",
			outNil.HoldLabels, outNil.LabelAllowlist, outNil.LaunchVars)
	}
}
