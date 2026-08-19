package server

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// memTransport records events in-process; no DSN of a real project is
// ever contacted.
type memTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *memTransport) Configure(sentry.ClientOptions) {}
func (t *memTransport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}
func (t *memTransport) Flush(time.Duration) bool              { return true }
func (t *memTransport) FlushWithContext(context.Context) bool { return true }
func (t *memTransport) Close()                                {}

func (t *memTransport) all() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*sentry.Event, len(t.events))
	copy(out, t.events)
	return out
}

// trackerTransport is the single in-memory transport of this test
// binary. errtrack.Init installs a client once per PROCESS, so the
// transport has to outlive any one test — a per-test transport would
// simply never be wired on a second call (or a `-count=2` re-run).
var (
	trackerTransport   = &memTransport{}
	trackerTransportOn sync.Once
)

func enableTracker(t *testing.T) *memTransport {
	t.Helper()
	trackerTransportOn.Do(func() {
		errtrack.Init(errtrack.Config{
			DSN:       "https://publickey@localhost/1",
			Transport: trackerTransport,
			Logger:    iterlog.New(iterlog.LevelError, io.Discard),
		})
	})
	if !errtrack.Enabled() {
		t.Fatal("errtrack.Init returned false with a valid DSN")
	}
	return trackerTransport
}

// A panic in a fire-and-forget server goroutine is the exact incident
// the tracker exists for — and the one case where a nil logger would
// otherwise leave no trace at all.
func TestGoSafeCapturesPanicToTheTracker(t *testing.T) {
	tr := enableTracker(t)
	before := len(tr.all())

	s := &Server{} // nil logger: the tracker is the only witness
	done := make(chan struct{})
	s.goSafe("audit-insert", func() {
		defer close(done)
		panic("nil map write")
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goSafe fn never ran")
	}

	// `done` closes as fn unwinds, i.e. BEFORE goSafe's own recover
	// defer has run — so poll for the capture rather than racing it.
	var events []*sentry.Event
	deadline := time.Now().Add(10 * time.Second)
	for {
		errtrack.Flush()
		if events = tr.all(); len(events) > before || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events)-before != 1 {
		t.Fatalf("want 1 captured panic, got %d", len(events)-before)
	}
	ev := events[len(events)-1]
	if !strings.Contains(ev.Message, "nil map write") &&
		(len(ev.Exception) == 0 || !strings.Contains(ev.Exception[0].Value, "nil map write")) {
		t.Fatalf("captured event does not name the panic: %+v", ev)
	}
	if ev.Contexts["iterion"]["task"] != "audit-insert" {
		t.Fatalf("captured event lost the task label: %+v", ev.Contexts)
	}
}
