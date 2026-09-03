package boardops

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// A bot's close_issue with no `to` on a card ALREADY in a terminal state
// must not re-file it into a different sink: `blocked` is the
// dispatcher's give-up lane, and "close whatever is first-terminal on
// the board" silently moved such cards to done — a re-classification the
// bot never asked for, through the terminal→terminal carve-out that was
// justified as an OPERATOR refiling. The acknowledgment half stays: the
// give-up stamp clears.
func TestClose_AlreadyTerminalCardIsNotRefiled(t *testing.T) {
	s, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := s.Create(native.Issue{Title: "gave up", State: native.StateBlocked})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGaveUp(iss.ID, &native.GiveUp{RunID: "r1", State: native.StateBlocked, Attempts: 3}); err != nil {
		t.Fatal(err)
	}
	capset := Capabilities{}
	for _, n := range AllCapabilities() {
		capset[n] = true
	}
	args, _ := json.Marshal(map[string]any{"id": iss.ID})
	out, err := Call(s, capset, "close_issue", args)
	if err != nil {
		t.Fatalf("close_issue: %v", err)
	}
	var got native.Issue
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != native.StateBlocked {
		t.Fatalf("a close with no target re-filed a terminal card: %q → %q", native.StateBlocked, got.State)
	}
	if got.GaveUp != nil {
		t.Fatalf("the acknowledgment half must still clear the give-up stamp")
	}
}
