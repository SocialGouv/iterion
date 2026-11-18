package runstream

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// expectLogOpen fails when the log stream delivers a chunk or ends within d.
func expectLogOpen(t *testing.T, sub LogSubscription, d time.Duration, what string) {
	t.Helper()
	select {
	case c, ok := <-sub.Chunks():
		t.Fatalf("%s: got chunk %q (open %v), want neither", what, c.Data, ok)
	case <-time.After(d):
	}
}

// expectLogEnd waits for the log stream to end, failing on any chunk.
func expectLogEnd(t *testing.T, sub LogSubscription, d time.Duration, what string) {
	t.Helper()
	timer := time.After(d)
	for {
		select {
		case c, ok := <-sub.Chunks():
			if !ok {
				return
			}
			t.Fatalf("%s: unexpected chunk %q before the end", what, c.Data)
		case <-timer:
			t.Fatalf("%s: the log stream did not end within %v", what, d)
		}
	}
}

// T7b(a): a log file that did not exist when the preflight looked is still
// read when it appears before the producer's end — Final takes one last drain
// instead of an empty end.
func TestFinalLogDrainReadsBytesWrittenBeforeFinal(t *testing.T) {
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	ctx, r := fsRetainedBirth(t, st, 1, 0)
	final := make(chan struct{})
	sub, e := src.SubscribeLogsHinted(ctx, r.ID, 0, FileStreamHints{Final: final})
	if e != nil {
		t.Fatalf("SubscribeLogsHinted: %v", e)
	}
	defer sub.Close()
	// The preflight holds the absence for the producer instead of ending on it.
	expectLogOpen(t, sub, 200*time.Millisecond, "the preflight holds for the producer")
	if e := st.AppendRunLog(ctx, r.ID, 0, []byte("LATE")); e != nil {
		t.Fatal(e)
	}
	close(final)
	if c := fsRetainedLog(t, sub); string(c.Data) != "LATE" {
		t.Fatalf("chunk = %q, want LATE", c.Data)
	}
	expectLogEnd(t, sub, 3*time.Second, "after the final drain")
}

// T7b(c): Final with no log file ever written is a single clean empty end —
// the preflight holds for the producer instead of ending on the absence.
func TestFinalLogStreamEndsEmptyWhenNothingWasWritten(t *testing.T) {
	src, st, _ := newFileSourceFixture(t)
	defer src.Close()
	ctx, r := fsRetainedBirth(t, st, 1, 0)
	final := make(chan struct{})
	sub, e := src.SubscribeLogsHinted(ctx, r.ID, 0, FileStreamHints{Final: final})
	if e != nil {
		t.Fatalf("SubscribeLogsHinted: %v", e)
	}
	defer sub.Close()
	expectLogOpen(t, sub, 300*time.Millisecond, "absence is not an end in Final mode")
	close(final)
	expectLogEnd(t, sub, 3*time.Second, "on Final without a file")
}

// T2r: with Final set, a terminal status is not the end of the stream — the
// writer keeps finalizing after it — and the producer's end takes the last
// drain, delivering what was written after the status.
func TestFinalHintIgnoresATerminalStatusUntilTheProducerEnds(t *testing.T) {
	src, st, root := newFileSourceFixture(t)
	defer src.Close()
	ctx, r := fsRetainedBirth(t, st, 1, 0)
	if e := st.AppendRunLog(ctx, r.ID, 0, []byte("A")); e != nil {
		t.Fatal(e)
	}
	final := make(chan struct{})
	sub, e := src.SubscribeLogsHinted(ctx, r.ID, 0, FileStreamHints{Final: final})
	if e != nil {
		t.Fatalf("SubscribeLogsHinted: %v", e)
	}
	defer sub.Close()
	if c := fsRetainedLog(t, sub); string(c.Data) != "A" {
		t.Fatalf("chunk = %q, want A", c.Data)
	}
	if e := st.UpdateRunStatus(ctx, r.ID, store.RunStatusFinished, ""); e != nil {
		t.Fatalf("UpdateRunStatus: %v", e)
	}
	// The terminal poll is off in Final mode: a status alone neither ends the
	// stream nor ships the finalize's bytes.
	expectLogOpen(t, sub, 1200*time.Millisecond, "a terminal status is not an end in Final mode")
	path := filepath.Join(root, "runs", r.ID, "run.log")
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if e != nil {
		t.Fatalf("append to the run log: %v", e)
	}
	if _, e := f.Write([]byte("B")); e != nil {
		f.Close()
		t.Fatalf("append to the run log: %v", e)
	}
	if e := f.Close(); e != nil {
		t.Fatalf("close the run log: %v", e)
	}
	close(final)
	if c := fsRetainedLog(t, sub); string(c.Data) != "B" {
		t.Fatalf("chunk after the status = %q, want B", c.Data)
	}
	expectLogEnd(t, sub, 3*time.Second, "after Final")
}
