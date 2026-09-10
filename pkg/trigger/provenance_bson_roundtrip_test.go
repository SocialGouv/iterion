package trigger

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// The machine-provenance gate reads the reason with a Go TYPE ASSERTION:
//
//	reason, _ := ev.Payload["reason"].(string)
//
// On the FS twin that value never leaves the process. On the cloud twin
// it is written into a board_events document and decoded back out of
// BSON into a map[string]any before NormalizeBoardEvent and machineCaused
// ever see it — and a type assertion that misses returns "" silently,
// which reads as "not machine-caused". That would re-open the
// column-rename mass-launch on exactly the twin ADR-096 says it closed,
// with every existing test green: they all hand the gate an in-process
// map.
//
// This pins the round trip itself. No database: what is under test is the
// ENCODING of native.Event (the type boardmongo's eventDoc embeds under
// `bson:"event"`), not any query.
func TestMachineProvenanceSurvivesTheBSONRoundTrip(t *testing.T) {
	// The doc shape boardmongo persists: the event nested under a key.
	type eventEnvelope struct {
		Tenant string       `bson:"tenant_id"`
		Event  native.Event `bson:"event"`
	}

	for _, reason := range []string{
		tracker.ReasonWatchdog,
		tracker.ReasonRunFinished,
		tracker.ReasonUnblocked,
	} {
		t.Run(reason, func(t *testing.T) {
			in := eventEnvelope{
				Tenant: "t1",
				Event: native.Event{
					Seq: 7, Timestamp: time.Now().UTC().Truncate(time.Millisecond),
					Type: native.EvtIssueState, IssueID: "native:1",
					Payload: map[string]any{"from": "in_progress", "to": "blocked", "reason": reason},
				},
			}
			raw, err := bson.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out eventEnvelope
			if err := bson.Unmarshal(raw, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			got, ok := out.Event.Payload["reason"].(string)
			if !ok {
				t.Fatalf("reason decoded as %T, not string — every machine-provenance gate reads it through a "+
					"type assertion, so it silently reads as \"\" and a machine repair fires the chain",
					out.Event.Payload["reason"])
			}
			if got != reason {
				t.Fatalf("reason round-tripped to %q, want %q", got, reason)
			}
			// The gate itself, fed the DECODED event — not a hand-built map.
			want := tracker.IsMachineReason(reason)
			if machineCaused(Event{Payload: out.Event.Payload}) != want {
				t.Fatalf("machineCaused on the decoded payload = %t, want %t", !want, want)
			}
		})
	}
}
