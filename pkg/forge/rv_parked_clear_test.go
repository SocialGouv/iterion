package forge

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The parked record must replay the request VERBATIM on approval — "what
// the org approved is what lands". Provision reads SPOKEN-EMPTY values as
// deliberate gestures on this record's fields: an empty launch_vars map
// beside launch_vars_replace is the whole-map clear, an empty hold_labels
// slice lifts the repo's automation pause, an empty label_allowlist slice
// widens dispatch back to any label. A parked request carrying any of
// those gestures must survive the store's persistence, or approve replays
// a SILENT request and the stored state stays in force. Marshal/unmarshal
// is the codec the mongo store itself drives: InsertOne marshals the
// struct, FindOne decodes it back through the same tags.
func TestRV_ParkedEmptyLaunchVarsMap_SurvivesPersistence(t *testing.T) {
	spoken := ProvisionApproval{
		ID:                 "pa-1",
		OrgID:              "o1",
		TenantID:           "t1",
		ConnectionID:       "conn-1",
		RepoFullName:       "org/api",
		BotIDs:             []string{"review-pr"},
		LaunchVars:         map[string]string{},
		LaunchVarsReplace:  true,
		HoldLabels:         []string{},
		LabelAllowlist:     []string{},
		LaunchVarsSet:      true,
		HoldLabelsSet:      true,
		LabelAllowlistSet:  true,
		Routing:            &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "judge", Backend: "claw"}}},
		RoutingSet:         true,
		BaseBotIDs:         []string{"review-pr"},
		BaseHoldLabels:     []string{"pause-a", "pause-b"},
		BaseLabelAllowlist: []string{"implement"},
	}
	raw, err := bson.Marshal(spoken)
	if err != nil {
		t.Fatal(err)
	}
	var back ProvisionApproval
	if err := bson.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.LaunchVars == nil || len(back.LaunchVars) != 0 {
		t.Fatalf("the spoken empty launch_vars map did not survive persistence: round-tripped record has LaunchVars %v (LaunchVarsReplace %v) — approve replays a silent request and the stored pins stay in force", back.LaunchVars, back.LaunchVarsReplace)
	}
	if !back.LaunchVarsReplace {
		t.Fatal("LaunchVarsReplace lost in the round-trip")
	}
	if back.HoldLabels == nil || len(back.HoldLabels) != 0 {
		t.Fatalf("the spoken empty hold_labels slice did not survive persistence: round-tripped record has HoldLabels %v — approve replays the hold the org approved lifting", back.HoldLabels)
	}
	if back.LabelAllowlist == nil || len(back.LabelAllowlist) != 0 {
		t.Fatalf("the spoken empty label_allowlist slice did not survive persistence: round-tripped record has LabelAllowlist %v — approve replays the narrowing the org approved widening", back.LabelAllowlist)
	}
	// The three Set flags are the SPOKEN facts the org wire's card renders
	// (the fields' own omitempty collapses an empty to an absent key), and
	// the base snapshots are what the card renders deltas against. A
	// struct round-trip alone cannot pin them: Marshal→Unmarshal through
	// the SAME struct is invariant under any consistent tag change, so a
	// renamed or retagged key stays green while every record persisted
	// BEFORE the refactor decodes with the flag false — the r5-F1 lie
	// resurrected through persistence. The pinned contract is the
	// PERSISTED SHAPE: the marshaled document must carry the exact keys.
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"launch_vars_set", "hold_labels_set", "label_allowlist_set", "routing_set", "base_bot_ids", "base_hold_labels", "base_label_allowlist"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("the persisted shape lost %q — records written before the change decode with the flag/base absent, and the approver's card reads a spoken gesture as a silent record", key)
		}
	}
	if v, ok := doc["launch_vars_set"].(bool); !ok || !v {
		t.Fatalf("launch_vars_set is not the boolean true in the persisted shape: %v", doc["launch_vars_set"])
	}
	// routing_set is the spoken fact for the routing block — the payload
	// this gate arbitrates. A dropped or renamed key would hide the gesture
	// from every record decoded after the change, exactly like the three
	// flags above.
	if v, ok := doc["routing_set"].(bool); !ok || !v {
		t.Fatalf("routing_set is not the boolean true in the persisted shape: %v", doc["routing_set"])
	}
	// And the bytes still decode back into the facts the write encoded:
	// the two halves of the codec contract stay in agreement.
	if !back.LaunchVarsSet || !back.HoldLabelsSet || !back.LabelAllowlistSet {
		t.Fatalf("the spoken facts did not survive persistence: launch_vars_set=%v hold_labels_set=%v label_allowlist_set=%v — the approver's card reads a spoken gesture as a silent record",
			back.LaunchVarsSet, back.HoldLabelsSet, back.LabelAllowlistSet)
	}
	if len(back.BaseHoldLabels) != 2 || back.BaseHoldLabels[0] != "pause-a" || back.BaseHoldLabels[1] != "pause-b" {
		t.Fatalf("base_hold_labels did not survive persistence: %v", back.BaseHoldLabels)
	}
	if len(back.BaseLabelAllowlist) != 1 || back.BaseLabelAllowlist[0] != "implement" {
		t.Fatalf("base_label_allowlist did not survive persistence: %v", back.BaseLabelAllowlist)
	}

	// The silent shapes are unchanged: ABSENT values (nil) must round-trip
	// as nil, so legacy records and silent requests keep their meaning.
	silent := ProvisionApproval{ID: "pa-2", LaunchVarsReplace: true}
	raw2, err := bson.Marshal(silent)
	if err != nil {
		t.Fatal(err)
	}
	var back2 ProvisionApproval
	if err := bson.Unmarshal(raw2, &back2); err != nil {
		t.Fatal(err)
	}
	if back2.LaunchVars != nil {
		t.Fatalf("a nil (silent) launch_vars map must round-trip as nil, got %v", back2.LaunchVars)
	}
	if back2.HoldLabels != nil {
		t.Fatalf("a nil (silent) hold_labels slice must round-trip as nil, got %v", back2.HoldLabels)
	}
	if back2.LabelAllowlist != nil {
		t.Fatalf("a nil (silent) label_allowlist slice must round-trip as nil, got %v", back2.LabelAllowlist)
	}
	// And the silent record's flags decode FALSE — an absent key is the
	// silent fact, the side every legacy record falls on. The absent keys
	// are pinned too: an omitempty dropped from any of the five would put
	// a silent/false key into the persisted shape and flip the meaning of
	// every record the next deploy reads.
	var silentDoc bson.M
	if err := bson.Unmarshal(raw2, &silentDoc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"launch_vars_set", "hold_labels_set", "label_allowlist_set", "routing_set", "base_bot_ids", "base_hold_labels", "base_label_allowlist"} {
		if _, ok := silentDoc[key]; ok {
			t.Fatalf("the silent record persisted a %q key — absence is the silent fact; a present key flips the next decode's meaning", key)
		}
	}
	if back2.LaunchVarsSet || back2.HoldLabelsSet || back2.LabelAllowlistSet {
		t.Fatalf("a silent record must decode with the Set flags false: launch_vars_set=%v hold_labels_set=%v label_allowlist_set=%v",
			back2.LaunchVarsSet, back2.HoldLabelsSet, back2.LabelAllowlistSet)
	}
	if back2.BaseHoldLabels != nil || back2.BaseLabelAllowlist != nil {
		t.Fatalf("a silent record's base snapshots must decode as nil: base_hold_labels=%v base_label_allowlist=%v",
			back2.BaseHoldLabels, back2.BaseLabelAllowlist)
	}
}
