package queue

import (
	"errors"
	"testing"
)

// A version mismatch must be DISTINGUISHABLE from a malformed payload, because
// the two deserve opposite handling on the consumer side: a malformed message
// is terminated (no consumer will ever decode it), while a message from a
// newer server is left on the queue for a runner that already carries the new
// build.
//
// Collapsing them is not theoretical. During a production schema bump the
// consumer terminated a message it could not decode, which destroyed the queue
// entry while the run document stayed `queued` forever — the refusal visible
// only in one pod's log, nothing in the operator's view. Two runs were lost
// that way before the fleet finished rolling.
func TestSchemaVersionMismatchIsATypedTransientError(t *testing.T) {
	newer := &RunMessage{V: SchemaVersion + 1, RunID: "r1", WorkflowName: "wf", IRCompiled: []byte("{}")}
	err := newer.Validate()
	if err == nil {
		t.Fatal("a message from a newer server must not validate")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Errorf("a consumer cannot tell this from a malformed payload: %v", err)
	}

	// Every OTHER validation failure must stay untyped, so nothing is mistaken
	// for the transient case and left looping on the queue.
	for _, tc := range []struct {
		name string
		msg  *RunMessage
	}{
		{"missing run id", &RunMessage{V: SchemaVersion, WorkflowName: "wf", IRCompiled: []byte("{}")}},
		{"missing workflow name", &RunMessage{V: SchemaVersion, RunID: "r1", IRCompiled: []byte("{}")}},
		{"no IR at all", &RunMessage{V: SchemaVersion, RunID: "r1", WorkflowName: "wf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verr := tc.msg.Validate()
			if verr == nil {
				t.Fatal("want an error")
			}
			if errors.Is(verr, ErrSchemaVersion) {
				t.Errorf("a permanent defect must not read as a version mismatch: %v", verr)
			}
		})
	}
}
