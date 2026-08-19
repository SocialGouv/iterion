package runner

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

// errtrack.Init installs a client once per PROCESS, so the transport
// has to outlive any one test — a per-test transport would never be
// wired on a second call (or a `-count=2` re-run).
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

// A background loop of the runner pod dying is the incident an operator
// most needs to see — and the one Go gives the CLI top level no way to
// observe, since it cannot recover another goroutine's panic.
func TestTrackPanicCapturesAndRepanics(t *testing.T) {
	tr := enableTracker(t)
	before := len(tr.all())

	var escaped any
	func() {
		// Stands in for the process dying: the guard must let the panic
		// through, so the outer recover is what catches it.
		defer func() { escaped = recover() }()
		func() {
			defer trackPanic("runner.heartbeat")
			panic("lease refresh nil deref")
		}()
	}()

	if escaped != "lease refresh nil deref" {
		t.Fatalf("the guard swallowed the panic instead of re-panicking: %v", escaped)
	}

	events := tr.all()
	if len(events)-before != 1 {
		t.Fatalf("want 1 captured panic, got %d", len(events)-before)
	}
	ev := events[len(events)-1]
	if !strings.Contains(ev.Message, "lease refresh nil deref") &&
		(len(ev.Exception) == 0 || !strings.Contains(ev.Exception[0].Value, "lease refresh nil deref")) {
		t.Fatalf("captured event does not name the panic: %+v", ev)
	}
	if ev.Contexts["iterion"]["surface"] != "runner.heartbeat" {
		t.Fatalf("captured event lost the surface label: %+v", ev.Contexts)
	}
}

// goTracked keeps the guard's contract on the goroutine it spawns, and
// stays a plain `go fn()` when nothing panics.
func TestGoTrackedRunsTheFunction(t *testing.T) {
	done := make(chan struct{})
	goTracked("runner.test", func() { close(done) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goTracked never ran fn")
	}
}
