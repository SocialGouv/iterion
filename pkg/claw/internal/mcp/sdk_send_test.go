package mcp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// fakeSDKTransport builds an SDKTransport with a pre-wired responses channel
// (no real subprocess). The caller feeds responses into the channel.
// It sets up an io.Pipe for stdin and drains the reader side so writes don't block.
func fakeSDKTransport(t *testing.T) (*SDKTransport, chan Response) {
	t.Helper()
	ch := make(chan Response, 16)
	done := make(chan struct{})
	r, w := io.Pipe()
	t.Cleanup(func() {
		w.Close()
		r.Close()
	})
	// Drain the pipe reader so WriteLSPFrameTo doesn't block.
	go func() {
		io.Copy(io.Discard, r)
	}()
	return &SDKTransport{
		name:      "test",
		stdin:     w,
		responses: ch,
		done:      done,
	}, ch
}

func TestSDKSendMatchesRequestID(t *testing.T) {
	tr, ch := fakeSDKTransport(t)

	// Push a response with matching ID.
	ch <- Response{JSONRPC: "2.0", ID: float64(42), Result: "ok"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := tr.Send(ctx, Request{JSONRPC: "2.0", ID: 42, Method: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %v", resp.Error)
	}
	if resp.Result != "ok" {
		t.Errorf("expected result 'ok', got %v", resp.Result)
	}
}

func TestSDKSendSkipsMismatchedID(t *testing.T) {
	tr, ch := fakeSDKTransport(t)

	// First push a response with a different ID (simulating an unsolicited
	// notification or out-of-order response), then the correct one.
	ch <- Response{JSONRPC: "2.0", ID: float64(99), Result: "wrong"}
	ch <- Response{JSONRPC: "2.0", ID: float64(7), Result: "right"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := tr.Send(ctx, Request{JSONRPC: "2.0", ID: 7, Method: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %v", resp.Error)
	}
	if resp.Result != "right" {
		t.Errorf("expected result 'right', got %v", resp.Result)
	}
}

func TestSDKSendRespectsContextCancellation(t *testing.T) {
	tr, _ := fakeSDKTransport(t)

	// Create an already-cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := tr.Send(ctx, Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected RPC error due to context cancellation")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("expected error code -32603, got %d", resp.Error.Code)
	}
}

func TestSDKSendContextTimeout(t *testing.T) {
	tr, _ := fakeSDKTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	resp, err := tr.Send(ctx, Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected RPC error due to timeout")
	}
}

func TestIdsMatch(t *testing.T) {
	tests := []struct {
		a, b any
		want bool
	}{
		{1, float64(1), true},
		{int64(2), float64(2), true},
		{float64(3), float64(3), true},
		{"abc", "abc", true},
		{1, float64(2), false},
		{"a", "b", false},
		{1, "1", false},
		{nil, nil, true},
		{json.Number("5"), float64(5), true},
	}
	for _, tt := range tests {
		got := idsMatch(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("idsMatch(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
