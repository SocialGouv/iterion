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

// A panic in a fire-and-forget server goroutine is the exact incident
// the tracker exists for — and the one case where a nil logger would
// otherwise leave no trace at all.
func TestGoSafeCapturesPanicToTheTracker(t *testing.T) {
	tr := &memTransport{}
	// errtrack.Init is once-per-process; this package's suite has no
	// other caller, so this call owns it.
	if !errtrack.Init(errtrack.Config{
		DSN:       "https://publickey@localhost/1",
		Transport: tr,
		Logger:    iterlog.New(iterlog.LevelError, io.Discard),
	}) {
		t.Fatal("errtrack.Init returned false with a valid DSN")
	}
	t.Cleanup(func() { sentry.CurrentHub().BindClient(nil) })

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
	errtrack.Flush()

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 captured panic, got %d", len(events))
	}
	ev := events[0]
	if !strings.Contains(ev.Message, "nil map write") &&
		(len(ev.Exception) == 0 || !strings.Contains(ev.Exception[0].Value, "nil map write")) {
		t.Fatalf("captured event does not name the panic: %+v", ev)
	}
	if ev.Contexts["iterion"]["task"] != "audit-insert" {
		t.Fatalf("captured event lost the task label: %+v", ev.Contexts)
	}
}
