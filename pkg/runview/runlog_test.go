package runview

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRunLogBuffer_WriteAndSnapshot(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	if _, err := b.Write([]byte("hello ")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if _, err := b.Write([]byte("world")); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	off, data, total := b.Snapshot(0)
	if off != 0 {
		t.Errorf("offset: got %d, want 0", off)
	}
	if string(data) != "hello world" {
		t.Errorf("data: got %q, want %q", data, "hello world")
	}
	if total != 11 {
		t.Errorf("total: got %d, want 11", total)
	}

	// Snapshot from mid-stream should slice correctly.
	off, data, total = b.Snapshot(6)
	if off != 6 || string(data) != "world" || total != 11 {
		t.Errorf("mid snapshot: got off=%d data=%q total=%d", off, data, total)
	}

	// Snapshot from beyond end is empty but reports total.
	off, data, total = b.Snapshot(11)
	if off != 11 || len(data) != 0 || total != 11 {
		t.Errorf("eof snapshot: got off=%d data=%q total=%d", off, data, total)
	}
}

func TestRunLogBuffer_RingEviction(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	// Write 1.5x the cap so older bytes get evicted.
	chunk := strings.Repeat("a", runLogRingCap/2)
	for i := 0; i < 3; i++ {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	off, data, total := b.Snapshot(0)
	if total != int64(3*len(chunk)) {
		t.Errorf("total: got %d, want %d", total, 3*len(chunk))
	}
	if len(data) > runLogRingCap {
		t.Errorf("ring exceeded cap: %d > %d", len(data), runLogRingCap)
	}
	// Snapshot from 0 should get clamped to ring start, not byte 0.
	if off == 0 {
		t.Errorf("expected snapshot to advance past 0 after eviction; got 0")
	}
	// After 3 writes of cap/2, the ring holds writes 2+3 (== cap)
	// and write 1's bytes have been evicted, so the snapshot starts
	// at offset == len(chunk).
	if off != int64(len(chunk)) {
		t.Errorf("offset after eviction: got %d, want %d", off, len(chunk))
	}
}

func TestRunLogBuffer_SingleWriteLargerThanCap(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	// Single write of 2x the cap. Only the trailing window survives.
	big := strings.Repeat("x", 2*runLogRingCap)
	if _, err := b.Write([]byte(big)); err != nil {
		t.Fatalf("write: %v", err)
	}
	off, data, total := b.Snapshot(0)
	if total != int64(2*runLogRingCap) {
		t.Errorf("total: got %d, want %d", total, 2*runLogRingCap)
	}
	if len(data) != runLogRingCap {
		t.Errorf("kept window: got %d, want %d", len(data), runLogRingCap)
	}
	if off != int64(runLogRingCap) {
		t.Errorf("offset: got %d, want %d", off, runLogRingCap)
	}
}

func TestRunLogBuffer_SubscribeReceivesLiveChunks(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	sub := b.Subscribe()
	defer sub.Cancel()

	if _, err := b.Write([]byte("first")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	chunk := <-sub.C
	if chunk.Offset != 0 || string(chunk.Bytes) != "first" {
		t.Errorf("chunk 1: got off=%d bytes=%q", chunk.Offset, chunk.Bytes)
	}

	if _, err := b.Write([]byte("second")); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	chunk = <-sub.C
	if chunk.Offset != 5 || string(chunk.Bytes) != "second" {
		t.Errorf("chunk 2: got off=%d bytes=%q", chunk.Offset, chunk.Bytes)
	}
}

func TestRunLogBuffer_CloseTerminatesSubscribers(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")

	sub := b.Subscribe()
	b.Close()

	_, ok := <-sub.C
	if ok {
		t.Errorf("expected channel to be closed after Close")
	}

	// Writes after Close are silent no-ops.
	n, err := b.Write([]byte("ignored"))
	if err != nil || n != 7 {
		t.Errorf("write after close: got n=%d err=%v", n, err)
	}
}

func TestRunLogBuffer_FilePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")

	b := mustNewRunLogBuffer(t, path)
	if _, err := b.Write([]byte("persisted line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b.Close()

	got, err := readFile(t, path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if got != "persisted line\n" {
		t.Errorf("file contents: got %q, want %q", got, "persisted line\n")
	}
}

func TestRunLogBuffer_DropsCounter(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	sub := b.Subscribe()
	defer sub.Cancel()

	// Don't drain — flood the subscriber buffer past its capacity so
	// the buffer drops chunks rather than blocking the writer.
	for i := 0; i < runLogSubBufferSize+10; i++ {
		if _, err := b.Write([]byte("x")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if sub.Drops() == 0 {
		t.Errorf("expected drops > 0 after flooding sub buffer; got 0")
	}
}

func TestRunLogBuffer_ConcurrentWriters(t *testing.T) {
	b := mustNewRunLogBuffer(t, "")
	defer b.Close()

	const writers = 8
	const perWriter = 100

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_, _ = b.Write([]byte("z"))
			}
		}()
	}
	wg.Wait()

	if got := b.Total(); got != int64(writers*perWriter) {
		t.Errorf("total: got %d, want %d", got, writers*perWriter)
	}
}

func readFile(t *testing.T, path string) (string, error) {
	t.Helper()
	bs, err := os.ReadFile(path)
	return string(bs), err
}

func mustNewRunLogBuffer(t *testing.T, filePath string) *RunLogBuffer {
	t.Helper()
	b, err := NewRunLogBuffer(filePath)
	if err != nil {
		t.Fatalf("NewRunLogBuffer(%q): %v", filePath, err)
	}
	return b
}

func TestRunLogBuffer_SeedsFromExistingFile(t *testing.T) {
	// Regression: NewRunLogBuffer reopening an existing run.log (the
	// daemon-resume / restart-on-active-run case) used to start the
	// ring's logical stream at offset 0 even though the file already
	// had pre-restart history. Snapshot(from=0) returned offset=0
	// → the editor's /log gap-fill helper (handleGetRunLog) and the
	// WS subscribe_logs replayer both saw offset == from and did
	// nothing — the per-node Logs tab went empty for any node that
	// ran before the restart.
	//
	// The fix seeds start/written from f.Stat().Size() so:
	//   - Snapshot(from=0) returns offset = pre-existing size
	//   - the handler reads [0, pre-existing-size) from disk and
	//     prepends it to whatever the ring grows to post-reopen.
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	if err := os.WriteFile(path, []byte("old history bytes "), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	preSize := int64(len("old history bytes "))

	b := mustNewRunLogBuffer(t, path)
	defer b.Close()

	off, data, total := b.Snapshot(0)
	if off != preSize {
		t.Errorf("Snapshot offset before any new write: got %d, want %d (pre-existing file size)", off, preSize)
	}
	if len(data) != 0 {
		t.Errorf("Snapshot data before any new write: got %d bytes, want 0 (ring is empty; pre-existing content lives on disk and gets gap-filled by the handler)", len(data))
	}
	if total != preSize {
		t.Errorf("Snapshot total before any new write: got %d, want %d", total, preSize)
	}

	// New write extends the stream past the seeded baseline.
	if _, err := b.Write([]byte("new ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	off, data, total = b.Snapshot(0)
	if off != preSize {
		t.Errorf("Snapshot offset after one new write: got %d, want %d (ring still starts at seed)", off, preSize)
	}
	if string(data) != "new " {
		t.Errorf("Snapshot data after one new write: got %q, want %q", data, "new ")
	}
	if total != preSize+4 {
		t.Errorf("Snapshot total after one new write: got %d, want %d", total, preSize+4)
	}

	// Mid-stream snapshot starting INSIDE the ring window works as before.
	off, data, total = b.Snapshot(preSize)
	if off != preSize || string(data) != "new " || total != preSize+4 {
		t.Errorf("mid-stream snapshot: got off=%d data=%q total=%d", off, data, total)
	}
}
