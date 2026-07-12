package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

func newStreamTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, dir
}

func seedStreamRun(t *testing.T, svc *Service, runID string, nEvents int) {
	t.Helper()
	if _, err := svc.store.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for i := 0; i < nEvents; i++ {
		evt := store.Event{Type: store.EventNodeStarted, RunID: runID, NodeID: "n"}
		if _, err := svc.store.AppendEvent(context.Background(), runID, evt); err != nil {
			t.Fatalf("AppendEvent %d: %v", i, err)
		}
	}
}

// drainEventBatches reads batches until pred is satisfied or the
// timeout passes, returning every event received in order. prior is
// the accumulation from earlier calls on the same subscription.
func drainEventBatches(t *testing.T, sub runstream.EventSubscription, prior []*store.Event, timeout time.Duration, pred func([]*store.Event) bool) []*store.Event {
	t.Helper()
	got := prior
	deadline := time.After(timeout)
	for {
		if pred(got) {
			return got
		}
		select {
		case batch, ok := <-sub.Events():
			if !ok {
				return got
			}
			got = append(got, batch...)
		case <-deadline:
			t.Fatalf("timeout draining events; got %d", len(got))
		}
	}
}

// TestStreamSource_EventsReplayThenLiveDedup: the persisted backlog
// replays as a batch, the on-demand tailer's redundant re-publish of
// the same events is deduped by seq, and a genuinely new live event
// arrives exactly once.
func TestStreamSource_EventsReplayThenLiveDedup(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-splice"
	seedStreamRun(t, svc, runID, 3) // seq 0,1,2 persisted

	sub, err := svc.StreamSource().SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	got := drainEventBatches(t, sub, nil, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 3 })
	for i, ev := range got[:3] {
		if ev.Seq != int64(i) {
			t.Fatalf("replay event %d has seq %d, want %d", i, ev.Seq, i)
		}
	}

	// The run is not Active (no in-process manager entry), so an
	// events.jsonl tailer was attached and re-published seq 0..2 into
	// the broker — those must be deduped. A NEW live event must pass.
	svc.broker.Publish(store.Event{Seq: 3, Type: store.EventNodeFinished, RunID: runID, NodeID: "n"})

	got = drainEventBatches(t, sub, got, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 4 })
	if got[3].Seq != 3 {
		t.Fatalf("live event seq = %d, want 3", got[3].Seq)
	}
	// No duplicates of the replayed window may ever surface.
	time.Sleep(300 * time.Millisecond)
	select {
	case batch, ok := <-sub.Events():
		if ok {
			for _, ev := range batch {
				if ev.Seq <= 2 {
					t.Fatalf("replayed seq %d re-delivered on the live tail", ev.Seq)
				}
			}
		}
	default:
	}
}

// TestStreamSource_AlertBypassesDedup: unpersisted Seq==0 alert events
// must pass the dedup guard even when the replay floor is far past 0.
func TestStreamSource_AlertBypassesDedup(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-alert-seam"
	seedStreamRun(t, svc, runID, 3)

	sub, err := svc.StreamSource().SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	replayed := drainEventBatches(t, sub, nil, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 3 })

	svc.broker.Publish(store.Event{Seq: 0, Type: store.EventAlert, RunID: runID, NodeID: "n"})
	got := drainEventBatches(t, sub, replayed, 5*time.Second, func(evs []*store.Event) bool {
		return len(evs) >= 4 && evs[len(evs)-1].Type == store.EventAlert
	})
	if got[len(got)-1].Type != store.EventAlert {
		t.Fatal("alert event did not bypass the seq dedup guard")
	}
}

// TestStreamSource_EventsTerminalClosesStream: an external/dispatcher
// run (not Active in this process) never gets a broker CloseRun. When it
// flips terminal mid-stream, the svcSource terminal poll must flush the
// final events and close the subscription — the signal the WS layer
// turns into `terminated`. Before the fix the tail blocked forever.
func TestStreamSource_EventsTerminalClosesStream(t *testing.T) {
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })

	svc, _ := newStreamTestService(t)
	const runID = "run-evt-terminal"
	seedStreamRun(t, svc, runID, 2) // running run, seq 0,1

	sub, err := svc.StreamSource().SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	drainEventBatches(t, sub, nil, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 2 })

	// Persist a final event, then flip the run terminal WITHOUT any
	// broker CloseRun (the external-run reality). The poll must catch the
	// terminal status, replay the straggler, and close the channel.
	if _, err := svc.store.AppendEvent(context.Background(), runID,
		store.Event{Type: store.EventRunFinished, RunID: runID}); err != nil {
		t.Fatalf("AppendEvent final: %v", err)
	}
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	// The Events channel must close (stream over) within a few poll ticks.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sub.Events():
			if !ok {
				return // channel closed → terminated will be emitted by the WS
			}
		case <-deadline:
			t.Fatal("timeout: event stream did not close after terminal flip")
		}
	}
}

// TestStreamSource_CloseReleasesTailer: closing the last subscription
// must tear down the on-demand events.jsonl tailer (refcount to zero,
// map entry removed) and unregister the broker subscriber.
func TestStreamSource_CloseReleasesTailer(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-close-release"
	seedStreamRun(t, svc, runID, 1)

	sub, err := svc.StreamSource().SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	drainEventBatches(t, sub, nil, 5*time.Second, func(evs []*store.Event) bool { return len(evs) >= 1 })

	svc.fileSrcMu.Lock()
	attached := svc.fileSrcs[runID] != nil
	svc.fileSrcMu.Unlock()
	if !attached {
		t.Fatal("expected an on-demand tailer for a non-active run")
	}

	_ = sub.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.fileSrcMu.Lock()
		_, present := svc.fileSrcs[runID]
		svc.fileSrcMu.Unlock()
		if !present && svc.broker.SubscriberCount(runID) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tailer/broker not released after Close (tailer present=%v, subscribers=%d)",
				present, svc.broker.SubscriberCount(runID))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// corruptEventsStore wraps the real store and fails LoadEventsRange the
// way a severely damaged events.jsonl does: partial page + the typed
// corruption sentinel.
type corruptEventsStore struct {
	store.RunStore
	partial []*store.Event
}

func (c *corruptEventsStore) LoadEventsRange(ctx context.Context, runID string, from, to int64, limit int) ([]*store.Event, error) {
	return c.partial, fmt.Errorf("%w: injected", store.ErrEventsCorrupted)
}

// TestStreamSource_CorruptedEventsFatal: a corrupted event log must
// ship the salvageable partial page, surface ErrEventsCorrupted on
// Errors(), and close the stream (no live tail on a corrupt log).
func TestStreamSource_CorruptedEventsFatal(t *testing.T) {
	dir := t.TempDir()
	base, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-corrupt"
	if _, err := base.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	partial := []*store.Event{{Seq: 0, Type: store.EventRunStarted, RunID: runID}}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()),
		WithStore(&corruptEventsStore{RunStore: base, partial: partial}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()

	var gotPartial bool
	var fatal error
	deadline := time.After(5 * time.Second)
	events, errs := sub.Events(), sub.Errors()
	for events != nil || errs != nil {
		select {
		case batch, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if len(batch) == 1 && batch[0].Seq == 0 {
				gotPartial = true
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			fatal = err
		case <-deadline:
			t.Fatal("timeout waiting for corrupted-stream shutdown")
		}
	}
	if !gotPartial {
		t.Error("salvageable partial page was not shipped before the fatal error")
	}
	if !errors.Is(fatal, store.ErrEventsCorrupted) {
		t.Errorf("fatal error = %v, want ErrEventsCorrupted", fatal)
	}
}

// collectLogChunks reassembles the log stream by absolute offset until
// the wanted content arrives (or the subscription closes / times out).
// have is the prefix received by earlier calls on the same subscription
// (chunk offsets are absolute).
func collectLogChunks(t *testing.T, sub runstream.LogSubscription, have, want string, timeout time.Duration) string {
	t.Helper()
	buf := []byte(have)
	deadline := time.After(timeout)
	for {
		if string(buf) == want {
			return want
		}
		select {
		case chunk, ok := <-sub.Chunks():
			if !ok {
				return string(buf)
			}
			end := chunk.Offset + int64(len(chunk.Data))
			if end > int64(len(buf)) {
				if chunk.Offset > int64(len(buf)) {
					t.Fatalf("log gap: have %d bytes, chunk starts at %d", len(buf), chunk.Offset)
				}
				buf = append(buf[:chunk.Offset], chunk.Data...)
			}
		case <-deadline:
			t.Fatalf("timeout collecting log chunks; have %q", string(buf))
		}
	}
}

// TestStreamSource_LogsInProcessBufferCutoff: the live in-process
// buffer path — snapshot + live writes, with the cutoff slicing
// guaranteeing no byte is delivered twice even though the write lands
// both in the snapshot window and on the fan-out channel.
func TestStreamSource_LogsInProcessBufferCutoff(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-log-buffer"
	seedStreamRun(t, svc, runID, 0)

	buf, _ := svc.prepareRunLog(runID)
	defer svc.dropRunLog(runID)
	if _, err := buf.Write([]byte("hello ")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeLogs(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	if got := collectLogChunks(t, sub, "", "hello ", 5*time.Second); got != "hello " {
		t.Fatalf("snapshot = %q, want %q", got, "hello ")
	}
	if _, err := buf.Write([]byte("world")); err != nil {
		t.Fatalf("live write: %v", err)
	}
	if got := collectLogChunks(t, sub, "hello ", "hello world", 5*time.Second); got != "hello world" {
		t.Fatalf("after live write = %q, want %q", got, "hello world")
	}
}

// TestStreamSource_LogsRingEvictionGapFill: bytes evicted from the
// 1 MiB ring must be gap-filled from the persisted run.log so a
// subscriber anchored at 0 still receives a contiguous stream.
func TestStreamSource_LogsRingEvictionGapFill(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-log-evict"
	seedStreamRun(t, svc, runID, 0)

	buf, _ := svc.prepareRunLog(runID) // file-backed: writes tee to run.log
	defer svc.dropRunLog(runID)

	// Write > runLogRingCap so the head is evicted from the ring but
	// remains on disk.
	head := strings.Repeat("H", 64*1024)
	filler := strings.Repeat("f", runLogRingCap)
	if _, err := buf.Write([]byte(head)); err != nil {
		t.Fatalf("write head: %v", err)
	}
	if _, err := buf.Write([]byte(filler)); err != nil {
		t.Fatalf("write filler: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeLogs(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	want := head + filler
	got := collectLogChunks(t, sub, "", want, 10*time.Second)
	if got != want {
		t.Fatalf("gap-filled stream mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// TestStreamSource_LogsExternalActiveRunTailer: an active run with no
// in-process buffer gets the on-demand run.log tailer, released on
// subscription Close.
func TestStreamSource_LogsExternalActiveRunTailer(t *testing.T) {
	svc, dir := newStreamTestService(t)
	const runID = "run-log-external"
	seedStreamRun(t, svc, runID, 0) // status running

	logPath := filepath.Join(dir, "runs", runID, "run.log")
	if err := os.WriteFile(logPath, []byte("alpha "), 0o644); err != nil {
		t.Fatalf("seed run.log: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeLogs(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	if got := collectLogChunks(t, sub, "", "alpha ", 5*time.Second); got != "alpha " {
		t.Fatalf("initial drain = %q", got)
	}

	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("beta"); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
	if got := collectLogChunks(t, sub, "alpha ", "alpha beta", 5*time.Second); got != "alpha beta" {
		t.Fatalf("live tail = %q", got)
	}

	_ = sub.Close()
	deadline := time.Now().Add(5 * time.Second)
	for svc.GetLogBuffer(runID) != nil {
		if time.Now().After(deadline) {
			t.Fatal("on-demand log buffer not released after Close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStreamSource_LogsTerminalOneShot: a terminal run replays its
// persisted run.log once (honouring fromOffset) and the stream closes.
func TestStreamSource_LogsTerminalOneShot(t *testing.T) {
	svc, dir := newStreamTestService(t)
	const runID = "run-log-terminal"
	seedStreamRun(t, svc, runID, 0)
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	logPath := filepath.Join(dir, "runs", runID, "run.log")
	if err := os.WriteFile(logPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatalf("write run.log: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeLogs(context.Background(), runID, 4)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	var chunks []runstream.LogChunk
	deadline := time.After(5 * time.Second)
	for {
		select {
		case chunk, ok := <-sub.Chunks():
			if !ok {
				if len(chunks) != 1 || chunks[0].Offset != 4 || string(chunks[0].Data) != "456789" {
					t.Fatalf("one-shot replay = %+v, want single chunk offset 4 data 456789", chunks)
				}
				return
			}
			chunks = append(chunks, chunk)
		case <-deadline:
			t.Fatal("timeout waiting for one-shot replay + close")
		}
	}
}

// TestStreamSource_LogsMissingFileClosesImmediately: a terminal run
// without a run.log yields a stream that closes with zero chunks.
func TestStreamSource_LogsMissingFileClosesImmediately(t *testing.T) {
	svc, _ := newStreamTestService(t)
	const runID = "run-log-missing"
	seedStreamRun(t, svc, runID, 0)
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusFailed, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	sub, err := svc.StreamSource().SubscribeLogs(context.Background(), runID, 0)
	if err != nil {
		t.Fatalf("SubscribeLogs: %v", err)
	}
	defer sub.Close()

	select {
	case chunk, ok := <-sub.Chunks():
		if ok {
			t.Fatalf("unexpected chunk %+v for a missing run.log", chunk)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: stream did not close for a missing run.log")
	}
}
