package runstream

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// lockedBuffer is a log sink written by producer goroutines and read by the
// test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func appendType(t *testing.T, st *store.FilesystemRunStore, id string, typ store.EventType) {
	t.Helper()
	if _, err := st.AppendEvent(t.Context(), id, store.Event{Type: typ}); err != nil {
		t.Fatal(err)
	}
}

func setStatus(t *testing.T, st *store.FilesystemRunStore, id string, s store.RunStatus) {
	t.Helper()
	if err := st.UpdateRunStatus(t.Context(), id, s, ""); err != nil {
		t.Fatal(err)
	}
}

func typesOf(evs []*store.Event) []store.EventType {
	out := make([]store.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

// A run resumed while the stream waits for a terminal event that never
// comes (a status-only terminal write) is not reported ended when the settle
// window elapses: its status is read again and the stream keeps tailing.
func TestFileSourceSettleWindowRechecksAResumedRun(t *testing.T) {
	src, st := barrierFixture(t, 300*time.Millisecond, store.EventNodeStarted)
	sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusFailedResumable)
	// Many 10 ms polls see the status-only terminal: the settle loop runs.
	time.Sleep(100 * time.Millisecond)
	// The resume's order: the status, then its event.
	setStatus(t, st, "barrier", store.RunStatusRunning)
	appendType(t, st, "barrier", store.EventRunResumed)
	receiveSeq(t, sub, 1, 5*time.Second)
	expectOpen(t, sub, 600*time.Millisecond, "a resumed run ended at the settle window")
	appendType(t, st, "barrier", store.EventNodeFinished)
	receiveSeq(t, sub, 2, 5*time.Second)
}

// A Terminal hint received while waiting for a terminal event reads the status
// again: a run resumed meanwhile keeps its stream.
func TestFileSourceSettleTerminalHintRechecksAResumedRun(t *testing.T) {
	src, st := barrierFixture(t, time.Hour, store.EventNodeStarted)
	term := make(chan struct{}, 1)
	sub, err := src.SubscribeEventsHinted(t.Context(), "barrier", EventCursor{}, FileStreamHints{Terminal: term, TerminalPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusFailedResumable)
	time.Sleep(100 * time.Millisecond)
	setStatus(t, st, "barrier", store.RunStatusRunning)
	appendType(t, st, "barrier", store.EventRunResumed)
	receiveSeq(t, sub, 1, 5*time.Second)
	term <- struct{}{}
	expectOpen(t, sub, 500*time.Millisecond, "a Terminal hint ended a resumed run's stream")
	appendType(t, st, "barrier", store.EventNodeFinished)
	receiveSeq(t, sub, 2, 5*time.Second)
}

// K1: each terminal event type ends the events as soon as it is read, with a
// settle window out of reach.
func TestFileSourceEachTerminalEventTypeEndsTheEvents(t *testing.T) {
	for _, typ := range []store.EventType{store.EventRunFinished, store.EventRunFailed, store.EventRunCancelled, store.EventRunInterrupted} {
		t.Run(string(typ), func(t *testing.T) {
			src, st := barrierFixture(t, time.Hour, store.EventNodeStarted)
			sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			receiveSeq(t, sub, 0, 5*time.Second)
			setStatus(t, st, "barrier", store.RunStatusFailedResumable)
			time.Sleep(100 * time.Millisecond)
			appendType(t, st, "barrier", typ)
			got := readToEnd(t, sub, 3*time.Second)
			if len(got) != 1 || got[0].Type != typ {
				t.Fatalf("delivered %v, want only %s", typesOf(got), typ)
			}
		})
	}
}

// K2: run_resumed and run_rewound reopen the events a run_failed ended, so a
// later terminal status waits for the next terminal event. run_auto_resumed
// does not: it is written before the resume it announces, which reopens with
// its own run_resumed.
func TestFileSourceReopeningEventTypes(t *testing.T) {
	for _, tc := range []struct {
		typ     store.EventType
		reopens bool
	}{{store.EventRunResumed, true}, {store.EventRunRewound, true}, {store.EventRunAutoResumed, false}} {
		t.Run(string(tc.typ), func(t *testing.T) {
			src, st := barrierFixture(t, time.Hour, store.EventRunFailed, tc.typ)
			sub, err := src.SubscribeEvents(t.Context(), "barrier", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			receiveSeq(t, sub, 1, 5*time.Second)
			setStatus(t, st, "barrier", store.RunStatusFinished)
			time.Sleep(150 * time.Millisecond)
			if !tc.reopens {
				expectEnd(t, sub, 3*time.Second, string(tc.typ)+" reopened the events")
				return
			}
			appendType(t, st, "barrier", store.EventRunFinished)
			for _, ev := range readToEnd(t, sub, 3*time.Second) {
				if ev.Seq == 2 && ev.Type == store.EventRunFinished {
					return
				}
			}
			t.Fatalf("%s did not reopen: the stream ended before run_finished", tc.typ)
		})
	}
}

// K4: a Terminal hint received while waiting for a terminal event of a run
// still terminal ends the stream at once.
func TestFileSourceSettleEndsOnATerminalHint(t *testing.T) {
	src, st := barrierFixture(t, time.Hour, store.EventNodeStarted)
	term := make(chan struct{}, 1)
	sub, err := src.SubscribeEventsHinted(t.Context(), "barrier", EventCursor{}, FileStreamHints{Terminal: term, TerminalPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusCancelled)
	time.Sleep(200 * time.Millisecond)
	term <- struct{}{}
	expectEnd(t, sub, 3*time.Second, "Terminal hint during the settle wait")
}

// A closed Terminal hint channel is a hint that never fires again, not one
// that fires forever. With a far poll, the poller never sees the terminal
// status: only a closed channel taken for a hint could end the stream.
func TestFileSourceClosedTerminalHintIsDisabled(t *testing.T) {
	src, st := barrierFixture(t, time.Hour, store.EventNodeStarted)
	term := make(chan struct{})
	close(term)
	sub, err := src.SubscribeEventsHinted(t.Context(), "barrier", EventCursor{}, FileStreamHints{Terminal: term, TerminalPoll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusFinished)
	expectOpen(t, sub, 500*time.Millisecond, "the poller took a closed Terminal channel for a hint")
}

// The wait for a terminal event does not take a closed Terminal channel for
// a hint either: the poll sees the terminal status, and the stream still
// waits for its terminal event.
func TestFileSourceClosedTerminalHintIsDisabledWhileSettling(t *testing.T) {
	src, st := barrierFixture(t, time.Hour, store.EventNodeStarted)
	term := make(chan struct{})
	close(term)
	sub, err := src.SubscribeEventsHinted(t.Context(), "barrier", EventCursor{}, FileStreamHints{Terminal: term, TerminalPoll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusFinished)
	expectOpen(t, sub, 500*time.Millisecond, "the settle wait took a closed Terminal channel for a hint")
}

// A closed wake channel does not turn the watch into a busy drain loop.
func TestWatchFileDrainsClosedWakeIsDisabled(t *testing.T) {
	wake := make(chan struct{})
	close(wake)
	var drains atomic.Int64
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_ = watchFileDrains(ctx, t.TempDir(), "events.jsonl", func(context.Context) error { drains.Add(1); return nil }, wake, quietLogger())
	if n := drains.Load(); n > 3 {
		t.Fatalf("%d drains in 300 ms with a closed wake channel", n)
	}
}

// fixedTerminalSelection reports a terminal status for every check.
type fixedTerminalSelection struct {
	store.FilesystemStreamSelection
	dir string
}

func (s *fixedTerminalSelection) WatchHints() (string, string)           { return s.dir, s.dir }
func (s *fixedTerminalSelection) Terminal(context.Context) (bool, error) { return true, nil }

// Closed hint channels do not spin the wait for a terminal event either.
func TestFinalDrainClosedHintsAreDisabled(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	var drains atomic.Int64
	f := &FileSource{logger: quietLogger()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now()
	f.runSelectedFile(ctx, &fixedTerminalSelection{dir: t.TempDir()}, fileRun{
		drain:       func(context.Context) error { drains.Add(1); return nil },
		ended:       func() bool { return false },
		hints:       FileStreamHints{Drain: closed, TerminalPoll: time.Hour},
		settle:      300 * time.Millisecond,
		settleEvery: time.Hour,
		warn:        func(error) {},
		fail:        func(e error) { t.Errorf("fail: %v", e) },
	})
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("the settle wait ended after %v, before its window", elapsed)
	}
	if n := drains.Load(); n > 5 {
		t.Fatalf("%d drains during a 300 ms settle wait with a closed Drain channel", n)
	}
}

// A terminal status with no terminal event (a status-only writer) is a
// legitimate end: the settle window elapses without a warning.
func TestFileSourceStatusOnlyTerminalLogsNoWarning(t *testing.T) {
	prev, prevSettle := fileTerminalPollInterval, fileTerminalSettleWindow
	fileTerminalPollInterval, fileTerminalSettleWindow = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { fileTerminalPollInterval, fileTerminalSettleWindow = prev, prevSettle })
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	src := NewFileSource(st, dir, iterlog.New(iterlog.LevelWarn, logs))
	defer src.Close()
	seedHintRun(t, st, "quiet")
	sub, err := src.SubscribeEvents(t.Context(), "quiet", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "quiet", store.RunStatusCancelled)
	expectEnd(t, sub, 5*time.Second, "status-only terminal")
	if out := logs.String(); out != "" {
		t.Fatalf("warning logged for a status-only terminal run: %s", out)
	}
}

// Closing a FileSource ends its open subscriptions with ErrSourceClosed, not
// a clean completion.
func TestFileSourceCloseFailsOpenSubscriptions(t *testing.T) {
	src, st, dir := newFileSourceFixture(t)
	seedHintRun(t, st, "open")
	if err := os.WriteFile(filepath.Join(dir, "runs", "open", "run.log"), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := src.SubscribeEvents(t.Context(), "open", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	logs, err := src.SubscribeLogs(t.Context(), "open", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	receiveSeq(t, events, 0, 5*time.Second)
	select {
	case <-logs.Chunks():
	case <-time.After(5 * time.Second):
		t.Fatal("log chunk not delivered")
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	readToEnd(t, events, 5*time.Second)
	if !queuedError(events.Errors(), ErrSourceClosed) {
		t.Fatal("events subscription ended without ErrSourceClosed")
	}
	for range logs.Chunks() {
	}
	if !queuedError(logs.Errors(), ErrSourceClosed) {
		t.Fatal("logs subscription ended without ErrSourceClosed")
	}
}

// queuedError reports whether errs, already closed by its producer, held
// target.
func queuedError(errs <-chan error, target error) bool {
	found := false
	for err := range errs {
		found = found || errors.Is(err, target)
	}
	return found
}

// A transient failure to open the run (here: no permission on its directory)
// does not end a running run's stream: the open is retried and the stream
// resumes once the directory is readable again.
func TestFileSourceTransientOpenErrorIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	src, st, dir := newFileSourceFixture(t)
	defer src.Close()
	seedHintRun(t, st, "locked")
	runDir := filepath.Join(dir, "runs", "locked")
	if err := os.Chmod(runDir, 0); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(runDir, 0o700); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	t.Cleanup(restore)
	sub, err := src.SubscribeEvents(t.Context(), "locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	expectOpen(t, sub, 600*time.Millisecond, "a transient open error ended the stream")
	restore()
	receiveSeq(t, sub, 0, 5*time.Second)
	appendType(t, st, "locked", store.EventNodeFinished)
	receiveSeq(t, sub, 1, 5*time.Second)
}

// An invalid run ID can never be opened: the subscription ends at once with a
// permanent refusal instead of retrying the open.
func TestFileSourceInvalidRunIDIsRefused(t *testing.T) {
	src, _, _ := newFileSourceFixture(t)
	defer src.Close()
	sub, err := src.SubscribeEvents(t.Context(), "..", 0)
	if err != nil {
		t.Fatalf("subscribe = %v, want a subscription that fails", err)
	}
	defer sub.Close()
	readToEnd(t, sub, 2*time.Second)
	refused := false
	for err := range sub.Errors() {
		refused = refused || IsPermanentError(err) && errors.Is(err, store.ErrInvalidRunDataIdentity)
	}
	if !refused {
		t.Fatal("an invalid run ID did not end with a permanent ErrInvalidRunDataIdentity")
	}
}

// The same holds for a log stream whose run.log is momentarily unreadable:
// the existence check is retried, and the bytes arrive once it is readable.
func TestFileSourceTransientLogOpenErrorIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	src, st, dir := newFileSourceFixture(t)
	defer src.Close()
	seedHintRun(t, st, "locked-log")
	logPath := filepath.Join(dir, "runs", "locked-log", "run.log")
	if err := os.WriteFile(logPath, []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(logPath, 0o600); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
	t.Cleanup(restore)
	sub, err := src.SubscribeLogs(t.Context(), "locked-log", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case chunk, open := <-sub.Chunks():
		t.Fatalf("a transient log open error ended or fed the stream: %+v (open %v)", chunk, open)
	case <-time.After(600 * time.Millisecond):
	}
	restore()
	select {
	case chunk, open := <-sub.Chunks():
		if !open || string(chunk.Data) != "hello" {
			t.Fatalf("after access returned: %+v (open %v)", chunk, open)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("log bytes not delivered once readable")
	}
}

// countingSelection counts terminal checks of a run that stays running.
type countingSelection struct {
	store.FilesystemStreamSelection
	checks atomic.Int64
}

func (s *countingSelection) Terminal(context.Context) (bool, error) {
	s.checks.Add(1)
	return false, nil
}

// The poller does not spin on a closed Terminal channel: with a far poll it
// checks the status once, at the start of the round.
func TestFileRoundClosedTerminalHintDoesNotSpin(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	s := &countingSelection{}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	r := newFileRound(ctx, s, FileStreamHints{Terminal: closed, TerminalPoll: time.Hour})
	<-r.done
	if n := s.checks.Load(); n > 2 {
		t.Fatalf("%d terminal checks in 300 ms with a closed Terminal channel", n)
	}
}
