package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestFileFinishedTicket_SaysWhenTheCardAlreadyMoved: the filing is a CAS
// on the state the sweep saw. When it finds the card already moved (an
// operator parked it in review while the run finished), nothing was
// written — and the log must say THAT, not "filed as done": an operator
// debugging a ticket that never reached done reads the old line as a
// filing that happened.
func TestFileFinishedTicket_SaysWhenTheCardAlreadyMoved(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := board.Create(native.Issue{Title: "finished run", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := *iss // the sweep's view: still in_progress
	if _, err := board.SetState(iss.ID, native.StateReview); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := &Server{logger: iterlog.New(iterlog.LevelInfo, &buf)}

	s.fileFinishedTicket(board, &snapshot, "run-1")

	if cur, _ := board.Get(iss.ID); cur.State != native.StateReview {
		t.Fatalf("the operator's move must stand: state %q", cur.State)
	}
	log := buf.String()
	if strings.Contains(log, "filed as done") {
		t.Fatalf("REPRODUCED: the log claims a filing that never happened:\n%s", log)
	}
	if !strings.Contains(log, "already left") {
		t.Fatalf("the drift must be named distinctly, log:\n%s", log)
	}
}
