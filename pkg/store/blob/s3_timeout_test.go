package blob

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A gateway that accepts the TCP connection and never answers must
// fail the operation within the response-header bound, not hang it:
// the 2026-10-31 incident wedged every cloud run for hours on exactly
// this shape (issue #2890 — PutAttachment sat in roundTrip with no
// client-side timeout while the storage layer black-holed requests).
func TestPutAttachmentFailsFastOnASilentGateway(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the response until the test releases the handler
	}))
	t.Cleanup(func() {
		close(release) // unblock the silent handlers FIRST, then Close
		silent.Close()
	})

	c, err := NewS3(context.Background(), Config{
		Endpoint:              silent.URL,
		Region:                "us-east-1",
		Bucket:                "run-bucket",
		AccessKeyID:           "key",
		SecretAccessKey:       "secret",
		UsePathStyle:          true,
		DialTimeout:           250 * time.Millisecond,
		ResponseHeaderTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		done <- c.PutAttachment(context.Background(), "run-1", "att", "f.txt", "text/plain", []byte("body"))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected PutAttachment to fail against a silent gateway, got nil")
		}
	case <-time.After(15 * time.Second):
		// Generous: the SDK retries the request 3 times with backoff, so the
		// total is several header bounds on a slow runner — the assertion is
		// "returns an error promptly", not a wall-clock ceiling.
		t.Fatal("PutAttachment still hung after 15 s — the response-header bound did not apply")
	}
}

// The bounds must not break the happy path: a responsive gateway
// answers a PUT within the same limits.
func TestPutAttachmentHappyPathUnderBounds(t *testing.T) {
	t.Parallel()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	c, err := NewS3(context.Background(), Config{
		Endpoint:              ok.URL,
		Region:                "us-east-1",
		Bucket:                "run-bucket",
		AccessKeyID:           "key",
		SecretAccessKey:       "secret",
		UsePathStyle:          true,
		DialTimeout:           250 * time.Millisecond,
		ResponseHeaderTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	defer c.Close()

	if err := c.PutAttachment(context.Background(), "run-1", "att", "f.txt", "text/plain", []byte("body")); err != nil {
		t.Fatalf("PutAttachment against a responsive gateway: %v", err)
	}
}

// ResponseHeaderTimeout stops at the headers: a gateway that starts a
// slow body after answering must still stream through unbounded, or
// GetAttachment readers would be cut mid-read.
func TestGetAttachmentStreamsPastTheHeaderBound(t *testing.T) {
	t.Parallel()

	chunks := strings.Repeat("a", 64)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 5; i++ {
			fmt.Fprint(w, chunks)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(150 * time.Millisecond) // ~750 ms total — 3x the header bound
		}
	}))
	defer slow.Close()

	c, err := NewS3(context.Background(), Config{
		Endpoint:              slow.URL,
		Region:                "us-east-1",
		Bucket:                "run-bucket",
		AccessKeyID:           "key",
		SecretAccessKey:       "secret",
		UsePathStyle:          true,
		DialTimeout:           250 * time.Millisecond,
		ResponseHeaderTimeout: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	defer c.Close()

	rc, meta, err := c.GetAttachment(context.Background(), "run-1", "att", "f.txt")
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	defer rc.Close()
	_ = meta
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatalf("read the streamed body: %v", err)
	}
	if buf.Len() != 5*64 {
		t.Fatalf("streamed %d bytes, want %d", buf.Len(), 5*64)
	}
}
