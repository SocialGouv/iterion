package runstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func quietLogger() *iterlog.Logger {
	return iterlog.New(iterlog.LevelError, os.Stderr)
}

func writeEventLine(t *testing.T, path string, evt store.Event) {
	t.Helper()
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
}

// collector accumulates emitted values under a mutex and lets tests
// wait for a predicate with a deadline.
type collector[T any] struct {
	mu   sync.Mutex
	got  []T
	cond func([]T) bool
}

func (c *collector[T]) add(v T) {
	c.mu.Lock()
	c.got = append(c.got, v)
	c.mu.Unlock()
}

func (c *collector[T]) waitFor(t *testing.T, timeout time.Duration, pred func([]T) bool, what string) []T {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		snapshot := append([]T(nil), c.got...)
		c.mu.Unlock()
		if pred(snapshot) {
			return snapshot
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("timeout waiting for %s; got %v", what, c.got)
	return nil
}

func TestTailEventsFile_DrainsBacklogThenTailsAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEventLine(t, path, store.Event{Seq: 0, RunID: "r", Type: store.EventRunStarted})
	writeEventLine(t, path, store.Event{Seq: 1, RunID: "r", Type: store.EventNodeStarted, NodeID: "a"})

	done := make(chan struct{})
	defer close(done)
	col := &collector[int64]{}
	go TailEventsFile(path, done, func(evt store.Event) { col.add(evt.Seq) }, quietLogger())

	col.waitFor(t, 3*time.Second, func(s []int64) bool { return len(s) == 2 }, "backlog drain")

	writeEventLine(t, path, store.Event{Seq: 2, RunID: "r", Type: store.EventNodeFinished, NodeID: "a"})
	got := col.waitFor(t, 3*time.Second, func(s []int64) bool { return len(s) == 3 }, "live append")
	for i, seq := range got {
		if seq != int64(i) {
			t.Errorf("event %d has seq %d, want %d", i, seq, i)
		}
	}
}

func TestTailEventsFile_PartialLineIsNotEmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	writeEventLine(t, path, store.Event{Seq: 0, RunID: "r", Type: store.EventRunStarted})

	done := make(chan struct{})
	defer close(done)
	col := &collector[int64]{}
	go TailEventsFile(path, done, func(evt store.Event) { col.add(evt.Seq) }, quietLogger())
	col.waitFor(t, 3*time.Second, func(s []int64) bool { return len(s) == 1 }, "initial drain")

	// Append a partial line (no trailing newline) — must NOT be emitted.
	half := `{"seq":1,"run_id":"r","type":"node_st`
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(half); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	_ = f.Close()

	time.Sleep(400 * time.Millisecond)
	col.mu.Lock()
	n := len(col.got)
	col.mu.Unlock()
	if n != 1 {
		t.Fatalf("partial line was emitted: %d events, want 1", n)
	}

	// Complete the line — now it must arrive.
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`arted","node_id":"a"}` + "\n"); err != nil {
		t.Fatalf("complete line: %v", err)
	}
	_ = f.Close()
	col.waitFor(t, 3*time.Second, func(s []int64) bool { return len(s) == 2 }, "completed line")
}

func TestTailEventsFile_FileAppearsLate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	done := make(chan struct{})
	defer close(done)
	col := &collector[int64]{}
	go TailEventsFile(path, done, func(evt store.Event) { col.add(evt.Seq) }, quietLogger())

	time.Sleep(150 * time.Millisecond) // tailer is waiting on a missing file
	writeEventLine(t, path, store.Event{Seq: 0, RunID: "r", Type: store.EventRunStarted})
	col.waitFor(t, 3*time.Second, func(s []int64) bool { return len(s) == 1 }, "late file creation")
}

type logSpan struct {
	offset int64
	data   string
}

func TestTailLogFile_DrainsAndTailsWithOffsets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	if err := os.WriteFile(path, []byte("hello "), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	defer close(done)
	col := &collector[logSpan]{}
	go TailLogFile(path, done, func(off int64, b []byte) { col.add(logSpan{off, string(b)}) }, quietLogger())

	assembled := func(spans []logSpan) string {
		out := []byte{}
		for _, s := range spans {
			if s.offset > int64(len(out)) {
				return fmt.Sprintf("GAP at %d", s.offset)
			}
			out = append(out[:s.offset], s.data...)
		}
		return string(out)
	}
	col.waitFor(t, 3*time.Second, func(s []logSpan) bool { return assembled(s) == "hello " }, "initial drain")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString("world"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
	col.waitFor(t, 3*time.Second, func(s []logSpan) bool { return assembled(s) == "hello world" }, "live append")
}

func TestTailLogFile_TruncationReanchorsAtZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	if err := os.WriteFile(path, []byte("original content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	done := make(chan struct{})
	defer close(done)
	col := &collector[logSpan]{}
	go TailLogFile(path, done, func(off int64, b []byte) { col.add(logSpan{off, string(b)}) }, quietLogger())

	col.waitFor(t, 3*time.Second, func(s []logSpan) bool {
		return len(s) > 0 && s[0].offset == 0 && s[0].data == "original content"
	}, "initial drain")

	// Rewrite the file shorter — the tailer must re-anchor at 0 and
	// re-emit from the top.
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	col.waitFor(t, 3*time.Second, func(s []logSpan) bool {
		last := s[len(s)-1]
		return last.offset == 0 && last.data == "new"
	}, "post-truncation re-anchor")
}
