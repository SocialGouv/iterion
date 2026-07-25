package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeRunLogStore records appended chunks and can fail the first N
// appends to exercise the retry-then-drop policy.
type fakeRunLogStore struct {
	mu       sync.Mutex
	chunks   []fakeChunk
	failNext int // number of upcoming AppendRunLog calls to fail
}

type fakeChunk struct {
	offset int64
	data   []byte
}

func (f *fakeRunLogStore) AppendRunLog(_ context.Context, _ string, offset int64, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("injected append failure")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.chunks = append(f.chunks, fakeChunk{offset: offset, data: cp})
	return nil
}

func (f *fakeRunLogStore) ReadRunLogRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}

func (f *fakeRunLogStore) RunLogSize(context.Context, string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var max int64
	for _, c := range f.chunks {
		if end := c.offset + int64(len(c.data)); end > max {
			max = end
		}
	}
	return max, nil
}

func (f *fakeRunLogStore) assembled() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var buf bytes.Buffer
	for _, c := range f.chunks {
		if c.offset > int64(buf.Len()) {
			// hole from a dropped batch — mark it
			buf.WriteString(strings.Repeat("_", int(c.offset)-buf.Len()))
		}
		b := buf.Bytes()[:c.offset]
		buf = *bytes.NewBuffer(append(b, c.data...))
	}
	return buf.String()
}

// syncBuffer is a mutex-guarded bytes.Buffer: the flusher goroutine
// writes log lines while the test polls String().
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestRunLogWriter_TickerFlushAndCloseFlush(t *testing.T) {
	fs := &fakeRunLogStore{}
	w := newRunLogWriter(context.Background(), fs, "r1", 0, iterlog.Nop())

	if _, err := w.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The 500ms ticker must flush without reaching the size threshold.
	waitForCondition(t, 3*time.Second, "ticker flush", func() bool { return fs.assembled() == "hello " })

	if _, err := w.Write([]byte("world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := fs.assembled(); got != "hello world" {
		t.Fatalf("after Close = %q, want %q", got, "hello world")
	}
	if got := w.Total(); got != 11 {
		t.Fatalf("Total = %d, want 11", got)
	}
}

func TestRunLogWriter_SizeThresholdFlushes(t *testing.T) {
	fs := &fakeRunLogStore{}
	w := newRunLogWriter(context.Background(), fs, "r1", 0, iterlog.Nop())
	defer w.Close()

	big := bytes.Repeat([]byte("x"), runLogFlushBytes+1)
	if _, err := w.Write(big); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Must flush well before the 500ms ticker.
	waitForCondition(t, 300*time.Millisecond, "size-threshold flush", func() bool {
		return len(fs.assembled()) == len(big)
	})
}

func TestRunLogWriter_OffsetSeedContinuity(t *testing.T) {
	fs := &fakeRunLogStore{}
	// Simulate a prior attempt having persisted 6 bytes.
	if err := fs.AppendRunLog(context.Background(), "r1", 0, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	seed, _ := fs.RunLogSize(context.Background(), "r1")
	w := newRunLogWriter(context.Background(), fs, "r1", seed, iterlog.Nop())

	if _, err := w.Write([]byte("again")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = w.Close()
	if got := fs.assembled(); got != "hello again" {
		t.Fatalf("resumed stream = %q, want %q", got, "hello again")
	}
}

func TestRunLogWriter_RetryThenDropKeepsOffsets(t *testing.T) {
	fs := &fakeRunLogStore{failNext: runLogFlushRetries} // first batch fails every attempt → dropped
	logBuf := &syncBuffer{}
	logger := iterlog.New(iterlog.LevelError, logBuf)
	w := newRunLogWriter(context.Background(), fs, "r1", 0, logger)

	if _, err := w.Write([]byte("lost!")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Wait until the failing batch has been retried and dropped (the
	// loud ERROR line is the observable signal; `dropped` is
	// flusher-private).
	waitForCondition(t, 5*time.Second, "drop after retries", func() bool {
		return strings.Contains(logBuf.String(), "DROPPING")
	})

	if _, err := w.Write([]byte("kept")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = w.Close()

	// The dropped batch leaves a hole; the next chunk keeps its true
	// absolute offset (5), never overlapping.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.chunks) != 1 || fs.chunks[0].offset != 5 || string(fs.chunks[0].data) != "kept" {
		t.Fatalf("chunks = %+v, want one chunk 'kept' at offset 5", fs.chunks)
	}
	if !strings.Contains(logBuf.String(), "DROPPING") {
		t.Errorf("drop was not loudly logged; log = %q", logBuf.String())
	}
}

func TestRunLogWriter_WriteNeverBlocksOnStore(t *testing.T) {
	// A store that hangs forever must not stall Write (only the flusher).
	blocked := make(chan struct{})
	fs := &hangingRunLogStore{unblock: blocked}
	w := newRunLogWriter(context.Background(), fs, "r1", 0, iterlog.New(iterlog.LevelError, os.Stderr))
	defer func() { close(blocked); _ = w.Close() }()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = fmt.Fprintf(w, "line %d\n", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Write blocked on a hanging store")
	}
}

type hangingRunLogStore struct{ unblock chan struct{} }

func (h *hangingRunLogStore) AppendRunLog(context.Context, string, int64, []byte) error {
	<-h.unblock
	return nil
}
func (h *hangingRunLogStore) ReadRunLogRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, nil
}
func (h *hangingRunLogStore) RunLogSize(context.Context, string) (int64, error) { return 0, nil }
