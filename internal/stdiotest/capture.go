// Package stdiotest captures process stdout/stderr without depending on pipe
// capacity. Captures change process globals: do not use them in parallel tests,
// and join all writers before reading or restoring. Nested captures must close
// in reverse order. This captures writes through os.Stdout/os.Stderr, not writes
// to saved file pointers or the operating system's original file descriptors.
package stdiotest

import (
	"errors"
	"io"
	"os"
	"testing"
)

type result struct {
	text string
	err  error
}

// Capture owns a redirected stream. Read returns an interval; Close restores
// the original stream and drains any unread tail. Both require idle writers.
type Capture struct {
	t        testing.TB
	stream   **os.File
	original *os.File
	writer   *os.File
	done     chan result
	closed   bool
}

// Start redirects stream and starts draining BEFORE returning to its writers.
// Cleanup is registered as a fallback; callers should also defer Close when
// the capture's scope can end before the test does (including on panic).
func Start(t testing.TB, stream **os.File) *Capture {
	t.Helper()
	c := &Capture{t: t, stream: stream, original: *stream}
	c.open()
	t.Cleanup(c.Close)
	return c
}

func (c *Capture) open() {
	c.t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		c.closed = true
		c.t.Fatalf("capture pipe: %v", err)
	}
	c.writer = w
	c.done = make(chan result, 1)
	go func() {
		b, readErr := io.ReadAll(r)
		closeErr := r.Close()
		c.done <- result{string(b), errors.Join(readErr, closeErr)}
	}()
	*c.stream = w
}

func (c *Capture) drain() string {
	c.t.Helper()
	*c.stream = c.original
	closeErr := c.writer.Close()
	out := <-c.done
	if err := errors.Join(closeErr, out.err); err != nil {
		c.t.Errorf("capture drain: %v", err)
	}
	return out.text
}

// Read returns only the bytes since Start or the preceding Read, and starts a
// fresh interval. Close must not have been called yet.
func (c *Capture) Read() string {
	c.t.Helper()
	if c.closed {
		c.t.Fatal("read of a closed capture")
	}
	out := c.drain()
	c.open()
	return out
}

// Close restores the original stream, joins the reader and closes both pipe
// ends, even when the caller never read. Repeated calls are harmless.
func (c *Capture) Close() {
	c.t.Helper()
	if c.closed {
		return
	}
	c.closed = true
	c.drain()
}

// Run captures everything fn writes and restores the stream even on panic or
// runtime.Goexit. fn must join any writers it starts before returning.
func Run(t testing.TB, stream **os.File, fn func()) string {
	t.Helper()
	c := Start(t, stream)
	defer c.Close()
	fn()
	c.closed = true
	return c.drain()
}
