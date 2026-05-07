package mcp

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// fakePipe is an in-memory io.ReadWriteCloser used to verify Detach
// closes the registered conn. The closed flag is exported via a
// helper.
type fakePipe struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed atomic.Bool
}

func (p *fakePipe) Read(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.EOF
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Read(b)
}

func (p *fakePipe) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, errors.New("write on closed pipe")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Write(b)
}

func (p *fakePipe) Close() error {
	p.closed.Store(true)
	return nil
}

func TestMemoryBrowserRegistry_AttachGetDetach(t *testing.T) {
	r := NewMemoryBrowserRegistry()

	pipe := &fakePipe{}
	sess := BrowserSession{
		SessionID: "s1",
		RunID:     "run-1",
		NodeID:    "n",
		CDPConn:   pipe,
	}
	if err := r.Attach(sess); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	got, ok := r.Get("run-1", "s1")
	if !ok {
		t.Fatal("Get returned not found after Attach")
	}
	if got.SessionID != "s1" {
		t.Fatalf("got session id %q", got.SessionID)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("Attach should stamp StartedAt when zero")
	}

	list := r.ListByRun("run-1")
	if len(list) != 1 {
		t.Fatalf("ListByRun returned %d entries", len(list))
	}

	if err := r.Detach("run-1", "s1"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if !pipe.closed.Load() {
		t.Fatal("Detach should close the CDPConn")
	}
	if _, ok := r.Get("run-1", "s1"); ok {
		t.Fatal("Get returned a value after Detach")
	}
}

func TestMemoryBrowserRegistry_DuplicateAttach(t *testing.T) {
	r := NewMemoryBrowserRegistry()
	sess := BrowserSession{SessionID: "s1", RunID: "run-1"}
	if err := r.Attach(sess); err != nil {
		t.Fatal(err)
	}
	if err := r.Attach(sess); !errors.Is(err, ErrSessionAlreadyAttached) {
		t.Fatalf("expected ErrSessionAlreadyAttached, got %v", err)
	}
}

func TestMemoryBrowserRegistry_DetachUnknown(t *testing.T) {
	r := NewMemoryBrowserRegistry()
	if err := r.Detach("nope", "s1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestMemoryBrowserRegistry_RequiresIDs(t *testing.T) {
	r := NewMemoryBrowserRegistry()
	if err := r.Attach(BrowserSession{}); err == nil {
		t.Fatal("expected error on empty session")
	}
	if err := r.Attach(BrowserSession{SessionID: "x"}); err == nil {
		t.Fatal("expected error when run id missing")
	}
}

func TestStubChromiumRunner_AlwaysErrors(t *testing.T) {
	runner := NewStubChromiumRunner()
	conn, err := runner.Start("r", "n")
	if conn != nil {
		t.Fatal("stub runner should return nil conn")
	}
	if !errors.Is(err, ErrChromiumNotImplemented) {
		t.Fatalf("expected ErrChromiumNotImplemented, got %v", err)
	}
}
