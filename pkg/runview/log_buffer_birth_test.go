package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A live log buffer records the birth of the run it is created for — the
// in-process path (prepareRunLog) and the tailer path (prepareRunLogNoFile)
// alike, from the run a prepared launch's context is bound to or else from a
// read with the caller's identity — so a reader bound to a birth can tell it
// from a predecessor's buffer still registered under the ID. A buffer
// created for no readable run belongs to no birth.
func TestLogBufferRecordsItsRunBirth(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-log-birth"
	seedStreamRun(t, svc, runID, 0)
	run, err := svc.store.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.BirthID == "" {
		t.Fatal("fixture run has no birth")
	}

	buf, _ := svc.prepareRunLog(t.Context(), runID)
	if !buf.BornOf(run.BirthID) || buf.BornOf("SOMEOTHERBIRTH") {
		t.Fatalf("prepareRunLog's buffer: BornOf(%s)=%v", run.BirthID, buf.BornOf(run.BirthID))
	}
	if got := svc.GetLogBufferOf(runID, run.BirthID); got != buf {
		t.Fatal("GetLogBufferOf does not serve the run's own buffer")
	}
	if svc.GetLogBufferOf(runID, "SOMEOTHERBIRTH") != nil {
		t.Fatal("GetLogBufferOf served the buffer to another birth")
	}
	svc.dropRunLog(runID, buf)

	tail := svc.prepareRunLogNoFile(t.Context(), runID)
	if !tail.BornOf(run.BirthID) {
		t.Fatal("prepareRunLogNoFile's buffer does not record the run's birth")
	}
	svc.dropRunLog(runID, tail)

	// A prepared launch's context is bound to the run it published: that
	// birth is recorded without a read.
	obs, err := store.CaptureRunObservation(store.RunDataHeader{RunID: runID, Format: store.ValueFormat(run.FormatVersion), Protocol: run.DataProtocol, BirthID: "PREPAREDBIRTH"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunObservation(t.Context(), obs)
	if err != nil {
		t.Fatal(err)
	}
	prepared := svc.prepareRunLogNoFile(bound, runID)
	defer svc.dropRunLog(runID, prepared)
	if !prepared.BornOf("PREPAREDBIRTH") {
		t.Fatal("a buffer prepared under a bound context does not record the bound birth")
	}

	orphan := svc.prepareRunLogNoFile(t.Context(), "never-created")
	defer svc.dropRunLog("never-created", orphan)
	if orphan.BornOf("") {
		t.Fatal("a buffer for no readable run claims a birth")
	}
}
