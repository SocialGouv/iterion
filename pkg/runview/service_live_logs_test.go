package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The bound log path (R54 slice 4, phase 2): a subscription reads the bytes of
// its retained selection, is woken by its run's log lane, and ends by one of
// exactly three sources — the producer's end value (E1), a retained closed
// epoch (E2), or a terminal status with no lane and no local producer (E3). A
// poll never ends a live lane. These tests drive the production seams:
// SubscribeLogs, the registry, the real producers.

// logSink drains one log subscription in the background, accumulating the
// text and the errors it publishes.
type logSink struct {
	mu   sync.Mutex
	text []byte
	errs []error
	done chan struct{}
}

func newLogSink(sub runstream.LogSubscription) *logSink {
	s := &logSink{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		chunks, errs := sub.Chunks(), sub.Errors()
		for chunks != nil || errs != nil {
			select {
			case chunk, ok := <-chunks:
				if !ok {
					chunks = nil
					continue
				}
				s.mu.Lock()
				if int64(len(s.text)) > chunk.Offset {
					s.text = s.text[:chunk.Offset]
				}
				if int64(len(s.text)) == chunk.Offset {
					s.text = append(s.text, chunk.Data...)
				}
				s.mu.Unlock()
			case err, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				s.mu.Lock()
				s.errs = append(s.errs, err)
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *logSink) textSoFar() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.text)
}

func (s *logSink) errorsSoFar() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.errs...)
}

// awaitText waits until the reassembled text equals want.
func (s *logSink) awaitText(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.textSoFar() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("log text = %q, want %q", s.textSoFar(), want)
}

// awaitEnd waits for the stream to close and returns its final text/errors.
func (s *logSink) awaitEnd(t *testing.T, timeout time.Duration) (string, []error) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(timeout):
		t.Fatalf("the log stream never ended; text so far %q", s.textSoFar())
	}
	return s.textSoFar(), s.errorsSoFar()
}

func (s *logSink) assertOpen(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-s.done:
		t.Fatalf("the log stream ended unexpectedly; text %q errors %v", s.textSoFar(), s.errorsSoFar())
	case <-time.After(d):
	}
}

// tightenBoundLogPoll shortens the terminal poll of the bound stream for the
// duration of a test.
func tightenBoundLogPoll(t *testing.T, d time.Duration) {
	t.Helper()
	prev := svcEventTerminalPollInterval
	svcEventTerminalPollInterval = d
	t.Cleanup(func() { svcEventTerminalPollInterval = prev })
}

// tightenLogLaneWait shortens the first-registration wait.
func tightenLogLaneWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := logLaneWaitBound
	logLaneWaitBound = d
	t.Cleanup(func() { logLaneWaitBound = prev })
}

func subscribeBoundLogs(t *testing.T, svc *Service, runID string) runstream.LogSubscription {
	t.Helper()
	bound := boundOriginCtx(t, svc, runID)
	sub, err := svc.StreamSource().SubscribeLogs(bound, runID, 0)
	if err != nil {
		t.Fatalf("bound log subscription: %v", err)
	}
	if sub == nil {
		t.Fatal("bound log subscription returned no pipe")
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

func legacyTailerCount(svc *Service) int {
	svc.fileSrcMu.Lock()
	defer svc.fileSrcMu.Unlock()
	return len(svc.logSrcs)
}

func writeNativeRunLog(t *testing.T, native *store.FilesystemRunStore, runID, content string) {
	t.Helper()
	dir := filepath.Join(native.Root(), "runs", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// T1 / T-ROUTE: the bytes of a bound subscription are the store's, never the
// producer ring's, and no legacy tailer is stood up under them.
func TestBoundLogBytesComeFromTheStoreNotTheRing(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	writeNativeRunLog(t, native, "r", "file-line\n")

	run, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	// A registered buffer under the same ID, a different birth-matching ring
	// content: the legacy carrier would ship it, the bound path must not.
	defer svc.InstallLogBufferForTest("r", run.BirthID, []byte("ring-line\n"))()

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	sink.awaitText(t, "file-line\n", 3*time.Second)
	if got := sink.textSoFar(); got != "file-line\n" {
		t.Fatalf("text = %q, want the store's bytes only", got)
	}
	if n := legacyTailerCount(svc); n != 0 {
		t.Fatalf("logSrcs = %d tailers, want 0 on the bound path", n)
	}
	_ = sub.Close()
}

// T-ROUTE-RETRY (F9): a transient admission followed by a success continues
// on the retained path — no ring byte, no legacy tailer (M22).
func TestBoundLogRetryReadsTheStoreNotTheLegacyCarrier(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	writeNativeRunLog(t, native, "r", "file-line\n")

	run, err := native.LoadRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.InstallLogBufferForTest("r", run.BirthID, []byte("ring-line\n"))()

	var calls int
	svc.logAdmissionHook = func(context.Context, string) error {
		calls++
		if calls == 1 {
			return os.ErrDeadlineExceeded // transient: not a permanent refusal
		}
		return nil
	}

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	sink.awaitText(t, "file-line\n", 5*time.Second)
	if got := sink.textSoFar(); got != "file-line\n" {
		t.Fatalf("text = %q, want the store's bytes after the retry", got)
	}
	if calls < 2 {
		t.Fatalf("admission calls = %d, want the retry to have run", calls)
	}
	if n := legacyTailerCount(svc); n != 0 {
		t.Fatalf("logSrcs = %d tailers, want 0 after the retry", n)
	}
	_ = sub.Close()
}

// T2: a live lane is never ended by a status poll — the finalize is delivered
// and the end is the producer's (endBy == lane).
func TestBoundLogEndsOnItsProducerLaneAfterTheStatus(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenBoundLogPoll(t, 20*time.Millisecond)

	var flipped atomic.Bool
	polled := make(chan struct{}, 1)
	svc.liveEvents.beforeLogStatusPoll = func() {
		if flipped.Load() {
			select {
			case polled <- struct{}{}:
			default:
			}
		}
	}

	buf, _ := svc.prepareRunLog(ctx, "r")
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)

	if _, err := buf.Write([]byte("mid\n")); err != nil {
		t.Fatal(err)
	}
	sink.awaitText(t, "mid\n", 3*time.Second)

	flipped.Store(true)
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polled:
	case <-time.After(3 * time.Second):
		t.Fatal("no status poll happened")
	}

	// Past the whole E3 settle window, the stream must still be open: the
	// status is terminal but its lane is alive, and the producer is writing
	// its finalize.
	time.Sleep(700 * time.Millisecond)
	sink.assertOpen(t, 20*time.Millisecond)
	if _, err := buf.Write([]byte("final-line\n")); err != nil {
		t.Fatal(err)
	}
	buf.setLogEnd(logEndTerminal)
	svc.dropRunLog("r", buf)

	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "mid\nfinal-line\n" {
		t.Fatalf("text = %q, want the finalize delivered", text)
	}
	if len(errs) != 0 {
		t.Fatalf("a lane end published errors: %v", errs)
	}
	if by, applies := svc.liveEvents.logEndBy("r"); by != logEndByLane || applies != 1 {
		t.Fatalf("endBy = %q applies = %d, want lane/1", by, applies)
	}
}

// T2b: the stall Warn of a live lane is a Warn, never a fin (M-WD).
func TestLiveLaneStallWarnNeverEndsTheStream(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenBoundLogPoll(t, 20*time.Millisecond)
	prevStall := logProducerStallWarn
	logProducerStallWarn = 50 * time.Millisecond
	t.Cleanup(func() { logProducerStallWarn = prevStall })

	buf, _ := svc.prepareRunLog(ctx, "r")
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)

	if _, err := buf.Write([]byte("mid\n")); err != nil {
		t.Fatal(err)
	}
	sink.awaitText(t, "mid\n", 3*time.Second)
	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	// Past the shared bound: the mutant ends the stream here.
	time.Sleep(120 * time.Millisecond)
	sink.assertOpen(t, 20*time.Millisecond)

	if _, err := buf.Write([]byte("late-line\n")); err != nil {
		t.Fatal(err)
	}
	buf.setLogEnd(logEndTerminal)
	svc.dropRunLog("r", buf)

	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "mid\nlate-line\n" {
		t.Fatalf("text = %q, want the late line delivered", text)
	}
	for _, err := range errs {
		if runstream.IsPermanentError(err) {
			t.Fatalf("the stall produced a permanent refusal: %v", err)
		}
	}
	if by, _ := svc.liveEvents.logEndBy("r"); by != logEndByLane {
		t.Fatalf("endBy = %q, want lane", by)
	}
}

// parkBoundLog hands the test the exact moment the initial observation
// returned with no lane: the merge then waits on `proceed`, so a short epoch
// registered and closed there is unseen by the observation and can only reach
// the merge through its watch — the deterministic E2 witness.
func parkBoundLog(t *testing.T, svc *Service) (observed <-chan struct{}, proceed chan<- struct{}) {
	t.Helper()
	parked := make(chan struct{})
	release := make(chan struct{})
	svc.liveEvents.afterLogObserved = func() {
		svc.liveEvents.afterLogObserved = nil
		close(parked)
		<-release
	}
	return parked, release
}

// T-EPOCH(a): an epoch that registers AND closes after the merge parked and
// before any poll is delivered by the RETAINED lane, never by a settle. The
// re-attach is held by the hook until after the close, so the retained voie
// is the one that delivers.
func TestShortRetainedTerminalEpochEndsByRetention(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour) // no poll may take part

	parked, proceed := parkBoundLog(t, svc)
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the initial observation never returned")
	}

	buf, _ := svc.prepareRunLog(ctx, "r")
	buf.setLogEnd(logEndTerminal)
	svc.dropRunLog("r", buf)
	close(proceed)

	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "" || len(errs) != 0 {
		t.Fatalf("text = %q errors = %v, want a clean retained end", text, errs)
	}
	if by, applies := svc.liveEvents.logEndBy("r"); by != logEndByRetained || applies != 1 {
		t.Fatalf("endBy = %q applies = %d, want retained/1 (M-LEAK, M-NOTIFY, M-STICKY)", by, applies)
	}
}

// T-EPOCH(b): the same short epoch ending LOST without a cause is a typed
// permanent error, delivered by the retained lane.
func TestShortRetainedLostEpochIsATypedError(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour)

	parked, proceed := parkBoundLog(t, svc)
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the initial observation never returned")
	}

	buf, _ := svc.prepareRunLog(ctx, "r")
	owner := svc.ownRunLog(ctx, "r", buf, logDurable)
	// The run is still running: the abandon classifies the epoch LOST, no
	// cause attached.
	owner.Abandon()
	close(proceed)

	_, errs := sink.awaitEnd(t, 3*time.Second)
	var typed error
	for _, err := range errs {
		if runstream.IsPermanentError(err) {
			typed = err
		}
	}
	if !errors.Is(typed, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want the typed source closure", errs)
	}
	if by, applies := svc.liveEvents.logEndBy("r"); by != logEndByRetained || applies != 1 {
		t.Fatalf("endBy = %q applies = %d, want retained/1", by, applies)
	}
}

// T-EPOCH(c): durability and header are checked at EVERY adoption — a resume
// epoch whose producer never reached the store is refused typed, never
// adopted and never settled over (M-ADMIT).
func TestMemoryOnlyResumeEpochIsRefusedAtAdoption(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour)

	parked, proceed := parkBoundLog(t, svc)
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the initial observation never returned")
	}

	buf := svc.prepareRunLogNoFile(ctx, "r")
	svc.ownRunLog(ctx, "r", buf, logMemoryOnly) // durable=false
	close(proceed)

	_, errs := sink.awaitEnd(t, 3*time.Second)
	found := false
	for _, err := range errs {
		if errors.Is(err, store.ErrRunDataCapability) {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %v, want the durability refusal", errs)
	}
	if n := svc.liveEvents.logStreams(); n != 0 {
		t.Fatalf("logActive = %d, want the refused merge unregistered", n)
	}
}

// The initial window: a registration and its close that land between the
// watch and the initial observation are still observed — by the observation,
// or by the watch token the merge reconciles first (F2).
func TestEpochRegisteredInTheInitialWindowIsRetained(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour)

	svc.liveEvents.beforeLogObserved = func() {
		buf, _ := svc.prepareRunLog(ctx, "r")
		buf.setLogEnd(logEndTerminal)
		svc.dropRunLog("r", buf)
		svc.liveEvents.beforeLogObserved = nil
	}

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	_, errs := sink.awaitEnd(t, 3*time.Second)
	if len(errs) != 0 {
		t.Fatalf("errors = %v, want a clean retained end", errs)
	}
	if by, applies := svc.liveEvents.logEndBy("r"); by != logEndByRetained || applies != 1 {
		t.Fatalf("endBy = %q applies = %d, want retained/1", by, applies)
	}
}

// The first fin of an epoch is sticky: E1 then E2 of one generation applies
// exactly once, whichever voie delivers first (M-STICKY).
func TestFirstFinOfAnEpochIsSticky(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	buf, _ := svc.prepareRunLog(ctx, "r")
	obs, _, ok := svc.logRegistry.currentFor("r")
	if !ok {
		t.Fatal("no epoch was registered")
	}
	sub, ok := svc.logRegistry.subscribe(obs)
	if !ok || sub.Closed {
		t.Fatal("the open epoch refused a subscription")
	}
	defer sub.Cancel()
	defer svc.dropRunLog("r", buf)

	m := &liveLogMerge{
		l: svc.liveEvents, runID: "r", entry: &liveLogEntry{},
		dataWake: make(chan struct{}, 1), finalHint: make(chan struct{}, 1),
		out: runstream.NewLogPipe(),
	}
	m.adoptLane(sub)

	m.applyEnd(sub.epoch, logEndTerminal, logEndByLane)     // E1 first
	m.applyEnd(sub.epoch, logEndTerminal, logEndByRetained) // the E2 duplicate
	if m.applies != 1 {
		t.Fatalf("applications = %d, want exactly 1", m.applies)
	}
	if m.entry.endBy != logEndByLane {
		t.Fatalf("endBy = %q, want the first voie (lane)", m.entry.endBy)
	}
}

// T-FAIL(a): a local capture failure is a fin. It refuses a subscription that
// starts after it, and it ends one that was already parked, typed.
func TestCaptureFailureEndsABoundLogStream(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)

	typed := fmt.Errorf("runview: run r log tee: %w", errors.Join(store.ErrRunDataCapability, syscall.ENOSPC))
	svc.recordLogProducerFailure("r", typed)

	bound := boundOriginCtx(t, svc, "r")
	sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatalf("subscribe after a capture failure returned synchronously: %v", err)
	}
	sink := newLogSink(sub)
	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "" {
		t.Fatalf("text = %q, want no byte before the refusal", text)
	}
	found := false
	for _, err := range errs {
		if errors.Is(err, store.ErrRunDataCapability) && errors.Is(err, syscall.ENOSPC) {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %v, want both members of the capture failure", errs)
	}
	_ = sub.Close()

	// A parked subscriber is told too, never left waiting. The handshake is
	// the initial observation having returned with no lane — never a timeout.
	svc2, native2 := newLogLaneService(t)
	seedLogLaneRun(t, native2, "r", store.RunStatusRunning)
	parked, proceed := parkBoundLog(t, svc2)
	sub2 := subscribeBoundLogs(t, svc2, "r")
	sink2 := newLogSink(sub2)
	select {
	case <-parked:
	case <-time.After(3 * time.Second):
		t.Fatal("the initial observation never returned")
	}
	failure := fmt.Errorf("runview: run r log tee: %w", errors.Join(store.ErrRunDataCapability, syscall.EIO))
	svc2.recordLogProducerFailure("r", failure)
	close(proceed)

	_, errs2 := sink2.awaitEnd(t, 3*time.Second)
	found2 := false
	for _, err := range errs2 {
		if errors.Is(err, store.ErrRunDataCapability) && errors.Is(err, syscall.EIO) {
			found2 = true
		}
	}
	if !found2 {
		t.Fatalf("errors = %v, want the typed capture failure", errs2)
	}
}

// T9: a lane registered after the subscription is adopted on its wake, with no
// poll, and its end is the stream's end (M12).
func TestLateLaneIsAdoptedWithoutAPoll(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour) // no poll may deliver the adoption

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)

	buf, _ := svc.prepareRunLog(ctx, "r")
	if _, err := buf.Write([]byte("late-lane\n")); err != nil {
		t.Fatal(err)
	}
	sink.awaitText(t, "late-lane\n", 3*time.Second)
	buf.setLogEnd(logEndTerminal)
	svc.dropRunLog("r", buf)

	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "late-lane\n" || len(errs) != 0 {
		t.Fatalf("text = %q errors = %v", text, errs)
	}
	if by, _ := svc.liveEvents.logEndBy("r"); by != logEndByLane {
		t.Fatalf("endBy = %q, want lane", by)
	}
}

// T6-mem: a producer whose tee never opened is memory-only: refused typed at
// adoption, no registered merge left behind, Stop bounded (M6).
func TestMemoryOnlyProducerIsRefusedAtAdoption(t *testing.T) {
	svc, native := newLogLaneService(t)
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)

	// Pre-create the run.log path as a DIRECTORY: the tee's OpenFile fails
	// (EISDIR) and fileTee() is false.
	if err := os.MkdirAll(filepath.Join(native.Root(), "runs", "r", "run.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	buf, _ := svc.prepareRunLog(ctx, "r")
	if buf.fileTee() {
		t.Fatal("the forced tee failure did not happen")
	}
	defer svc.dropRunLog("r", buf)

	bound := boundOriginCtx(t, svc, "r")
	sub, err := svc.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatalf("memory-only subscription returned synchronously: %v", err)
	}
	sink := newLogSink(sub)
	if text, errs := sink.awaitEnd(t, 3*time.Second); text != "" || !hasError(errs, store.ErrRunDataCapability) {
		t.Fatalf("text = %q errors = %v, want the capability refusal", text, errs)
	}
	_ = sub.Close()
	if n := svc.liveEvents.logStreams(); n != 0 {
		t.Fatalf("logActive = %d, want 0", n)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.liveEvents.stop(stopCtx); err != nil {
		t.Fatalf("stop did not join: %v", err)
	}
}

// T6b: a store without RunLogStore has no lane path: the merge refuses typed
// before touching the selection (M-RO).
func TestStoreWithoutRunLogsRefusesALaneSubscription(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &originRaceStore{RunStore: native} // wraps, does not implement RunLogStore
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive(), WithStore(wrapper))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	// The Service was built over the wrapper, so no liveEventStreams: mount one
	// by hand to exercise the capacity guard of the lane path.
	svc.liveEvents = &liveEventStreams{
		svc:       svc,
		files:     runstream.NewFileSource(wrapper, "", iterlog.Nop()),
		active:    make(map[uint64]*liveEventEntry),
		logActive: make(map[uint64]*liveLogEntry),
		logEnds:   make(map[string]logEndRecord),
	}
	bound := boundOriginCtx(t, svc, "r")
	out := runstream.NewLogPipe()
	err = svc.liveEvents.subscribeLogs(bound, "r", 0, out)
	if !errors.Is(err, store.ErrRunDataCapability) {
		t.Fatalf("lane subscription on a log-less store = %v, want the capability refusal", err)
	}
	out.Finish(nil)
	if n := svc.liveEvents.logStreams(); n != 0 {
		t.Fatalf("logActive = %d, want 0", n)
	}
}

// A terminal run with no lane replays its persisted log at once and ends
// after the E3 settle — never waiting a whole poll interval for the first
// status check.
func TestTerminalRunWithALogReplaysThenEnds(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, time.Hour) // only the immediate check may end it

	writeNativeRunLog(t, native, "r", "replayed\n")
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	sink.awaitText(t, "replayed\n", 2*time.Second)
	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "replayed\n" || len(errs) != 0 {
		t.Fatalf("text = %q errors = %v", text, errs)
	}
	if by, _ := svc.liveEvents.logEndBy("r"); by != logEndBySettle {
		t.Fatalf("endBy = %q, want settle", by)
	}
}

// A Service stop ends a parked bound log stream with the typed source
// closure and joins it before the FileSource closes — never a clean
// completion.
func TestStopEndsAndJoinsAParkedBoundLogStream(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	tightenLogLaneWait(t, 100*time.Millisecond)

	// The merge must be RUNNING before the stop, or the subscription would be
	// refused before its merge and no end source would exist. A live lane
	// plus one shipped line is the handshake.
	ctx := t.Context()
	buf, _ := svc.prepareRunLog(ctx, "r")
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	if _, err := buf.Write([]byte("live\n")); err != nil {
		t.Fatal(err)
	}
	sink.awaitText(t, "live\n", 3*time.Second)
	defer svc.dropRunLog("r", buf)

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := svc.liveEvents.stop(stopCtx); err != nil {
		t.Fatalf("stop did not join the log stream: %v", err)
	}
	if n := svc.liveEvents.logStreams(); n != 0 {
		t.Fatalf("logActive = %d after the stop, want 0", n)
	}
	_, errs := sink.awaitEnd(t, 2*time.Second)
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("errors = %v, want the typed source closure", errs)
	}
	if by, _ := svc.liveEvents.logEndBy("r"); by != logEndByStop {
		t.Fatalf("endBy = %q, want stop", by)
	}
}

// T7b(b): a terminal run with no lane and no log file ends after the E3
// settle, without waiting for a first drain (M-E3).
func TestTerminalRunWithoutLaneEndsAfterASettle(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusFinished)
	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, 20*time.Millisecond)

	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "" || len(errs) != 0 {
		t.Fatalf("text = %q errors = %v, want a clean empty end", text, errs)
	}
	if by, _ := svc.liveEvents.logEndBy("r"); by != logEndBySettle {
		t.Fatalf("endBy = %q, want settle", by)
	}
}

// T-HOTSWAP: the lane of the OLD Service is neither transferred nor
// adoptable. A subscriber of the new Service sees no lane and ends on E3; a
// write the old producer lands more than the settle window after the status
// is not delivered. This is the documented limitation of slices 6/7.
func TestHotSwapLaneIsNotAdoptable(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	newSvc := func() *Service {
		svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithRecoveryPassive())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			svc.Stop(ctx)
		})
		return svc
	}
	producer, subscriber := newSvc(), newSvc()
	ctx := t.Context()
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)

	buf, _ := producer.prepareRunLog(ctx, "r")
	if _, err := buf.Write([]byte("before\n")); err != nil {
		t.Fatal(err)
	}

	tightenLogLaneWait(t, 100*time.Millisecond)
	tightenBoundLogPoll(t, 20*time.Millisecond)
	bound := boundOriginCtx(t, subscriber, "r")
	sub, err := subscriber.StreamSource().SubscribeLogs(bound, "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	sink := newLogSink(sub)
	sink.awaitText(t, "before\n", 3*time.Second)

	if err := native.UpdateRunStatus(ctx, "r", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	text, errs := sink.awaitEnd(t, 3*time.Second)
	if text != "before\n" || len(errs) != 0 {
		t.Fatalf("text = %q errors = %v, want the warm bytes only", text, errs)
	}
	if by, _ := subscriber.liveEvents.logEndBy("r"); by != logEndBySettle {
		t.Fatalf("endBy = %q, want settle (the old Service's lane is not adoptable)", by)
	}

	// Post-status write by the old producer, past the settle window: not
	// delivered to the new Service's subscriber.
	time.Sleep(600 * time.Millisecond)
	if _, err := buf.Write([]byte("late\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := sink.textSoFar(); got != "before\n" {
		t.Fatalf("text after the hot-swap write = %q, want the limitation", got)
	}
	producer.dropRunLog("r", buf)
}
