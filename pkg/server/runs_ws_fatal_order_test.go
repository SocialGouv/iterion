package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync/atomic"
	"testing"
	"time"
)

type fatalCompletionSub struct {
	events chan []*store.Event
	chunks chan runstream.LogChunk
	errs   chan error
	closes atomic.Int32
}

func (s *fatalCompletionSub) Events() <-chan []*store.Event     { return s.events }
func (s *fatalCompletionSub) Chunks() <-chan runstream.LogChunk { return s.chunks }
func (s *fatalCompletionSub) Errors() <-chan error              { return s.errs }
func (s *fatalCompletionSub) Close() error                      { s.closes.Add(1); return nil }
func fatalCompletionRun(t *testing.T, logs bool, errs chan error) (*runConn, *fatalCompletionSub, <-chan struct{}) {
	t.Helper()
	c := newRunConn(&Server{}, nil, "original")
	s := &fatalCompletionSub{events: make(chan []*store.Event), chunks: make(chan runstream.LogChunk), errs: errs}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if logs {
			c.pumpLogs(s)
		} else {
			c.pumpEvents(s)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
			close(c.closed)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("pump leaked")
		}
	})
	return c, s, done
}
func fatalCompletionFinish(t *testing.T, c *runConn, s *fatalCompletionSub, done <-chan struct{}, logs bool, want string) {
	t.Helper()
	if logs {
		close(s.chunks)
	} else {
		close(s.events)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("completion blocked on errors")
	}
	if n := s.closes.Load(); n != 1 {
		t.Fatalf("Close=%d", n)
	}
	select {
	case b := <-c.sendCh:
		var env runWSEnvelope
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != wsTypeError {
			t.Fatalf("completion=%s want error", env.Type)
		}
		var p wsErrorPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Code != want {
			t.Fatalf("code=%q want %q", p.Code, want)
		}
	default:
		t.Fatal("missing error")
	}
	select {
	case b := <-c.sendCh:
		t.Fatalf("second completion %s", b)
	default:
	}
}
func TestRunStreamConsumedFatalSurvivesWarnings(t *testing.T) {
	t.Parallel()
	for _, logs := range []bool{false, true} {
		for _, cause := range []error{store.ErrRunDataIdentityChanged, errors.Join(context.Canceled, store.ErrRunNotFound), store.ErrEventsCorrupted} {
			t.Run(fmt.Sprintf("logs=%v/%v", logs, cause), func(t *testing.T) {
				errs := make(chan error)
				c, s, done := fatalCompletionRun(t, logs, errs)
				for _, e := range []error{cause, errors.New("retry notice"), nil} {
					select {
					case errs <- e:
					case <-time.After(time.Second):
						t.Fatal("handshake blocked")
					}
				}
				want := "event_stream_failed"
				if logs {
					want = "log_stream_failed"
				} else if errors.Is(cause, store.ErrEventsCorrupted) {
					want = "events_corrupted"
				}
				// Errors intentionally remains open; all sends have been consumed before data closure.
				fatalCompletionFinish(t, c, s, done, logs, want)
			})
		}
	}
}
