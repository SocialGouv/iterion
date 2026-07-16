package protocol

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTransport implements Transport for testing.
type mockTransport struct {
	mu       sync.Mutex
	messages [][]byte
	msgChan  chan map[string]any
	errChan  chan error
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		messages: make([][]byte, 0, 10),
		msgChan:  make(chan map[string]any, 10),
		errChan:  make(chan error, 1),
	}
}

func (m *mockTransport) ReadMessages(_ context.Context) (<-chan map[string]any, <-chan error) {
	return m.msgChan, m.errChan
}

func (m *mockTransport) SendMessage(_ context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, data)

	return nil
}

func (m *mockTransport) getMessages() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([][]byte, len(m.messages))
	copy(result, m.messages)

	return result
}

func (m *mockTransport) sendToController(msg map[string]any) {
	m.msgChan <- msg
}

func TestCancelRequest_InFlightOperation(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	defer ctrl.Stop()

	// Track when handler starts and if it was cancelled
	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})

	// Register a slow handler that checks for cancellation
	ctrl.RegisterHandler("slow_operation", func(ctx context.Context, _ *ControlRequest) (map[string]any, error) {
		close(handlerStarted)

		// Wait for cancellation or timeout
		select {
		case <-ctx.Done():
			close(handlerCancelled)

			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return map[string]any{"status": "completed"}, nil
		}
	})

	// Send a control request
	go func() {
		transport.sendToController(map[string]any{
			"type":       "control_request",
			"request_id": "req-123",
			"request": map[string]any{
				"subtype": "slow_operation",
			},
		})
	}()

	// Wait for handler to start
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Handler did not start in time")
	}

	// Give handler time to register in inFlight map
	time.Sleep(50 * time.Millisecond)

	// Send cancel request
	transport.sendToController(map[string]any{
		"type":       "control_cancel_request",
		"request_id": "req-123",
	})

	// Verify handler was cancelled
	select {
	case <-handlerCancelled:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Handler was not cancelled in time")
	}
}

func TestCancelRequest_AlreadyCompleted(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	defer ctrl.Stop()

	handlerDone := make(chan struct{})

	// Register a fast handler
	ctrl.RegisterHandler("fast_operation", func(_ context.Context, _ *ControlRequest) (map[string]any, error) {
		defer close(handlerDone)

		return map[string]any{"status": "done"}, nil
	})

	// Send a control request
	transport.sendToController(map[string]any{
		"type":       "control_request",
		"request_id": "req-456",
		"request": map[string]any{
			"subtype": "fast_operation",
		},
	})

	// Wait for handler to complete
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Handler did not complete in time")
	}

	// Give time for cleanup
	time.Sleep(50 * time.Millisecond)

	// Send cancel request for already completed operation
	transport.sendToController(map[string]any{
		"type":       "control_cancel_request",
		"request_id": "req-456",
	})

	// Give time for cancel acknowledgment
	time.Sleep(100 * time.Millisecond)

	// Should have received acknowledgment with found=false (already removed from map)
	messages := transport.getMessages()
	assert.NotEmpty(t, messages, "Should have sent responses")
}

func TestCancelRequest_UnknownRequestID(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	defer ctrl.Stop()

	// Send cancel request for unknown request ID
	transport.sendToController(map[string]any{
		"type":       "control_cancel_request",
		"request_id": "unknown-req",
	})

	// Give time for acknowledgment
	time.Sleep(100 * time.Millisecond)

	// Should have sent acknowledgment with found=false
	messages := transport.getMessages()
	require.NotEmpty(t, messages, "Should have sent cancel acknowledgment")
}

func TestCancelRequest_ContextPropagation(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	defer ctrl.Stop()

	var receivedCtx context.Context

	ctxReceived := make(chan struct{})

	// Register handler that captures context
	ctrl.RegisterHandler("ctx_test", func(ctx context.Context, _ *ControlRequest) (map[string]any, error) {
		receivedCtx = ctx

		close(ctxReceived)

		// Wait for context to be cancelled
		<-ctx.Done()

		return nil, ctx.Err()
	})

	// Send a control request
	transport.sendToController(map[string]any{
		"type":       "control_request",
		"request_id": "req-ctx",
		"request": map[string]any{
			"subtype": "ctx_test",
		},
	})

	// Wait for handler to receive context
	select {
	case <-ctxReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("Handler did not start in time")
	}

	// Verify context is not yet cancelled
	assert.NoError(t, receivedCtx.Err(), "Context should not be cancelled yet")

	// Send cancel request
	transport.sendToController(map[string]any{
		"type":       "control_cancel_request",
		"request_id": "req-ctx",
	})

	// Wait for context to be cancelled
	select {
	case <-receivedCtx.Done():
		assert.Equal(t, context.Canceled, receivedCtx.Err())
	case <-time.After(2 * time.Second):
		t.Fatal("Context was not cancelled in time")
	}
}

func TestCancelRequest_DataRace(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	defer ctrl.Stop()

	var wg sync.WaitGroup

	handlerCount := 10

	// Register handler
	ctrl.RegisterHandler("concurrent_op", func(ctx context.Context, _ *ControlRequest) (map[string]any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return map[string]any{"status": "done"}, nil
		}
	})

	// Start multiple concurrent operations
	for i := range handlerCount {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			reqID := "req-race-" + string(rune('a'+idx))

			transport.sendToController(map[string]any{
				"type":       "control_request",
				"request_id": reqID,
				"request": map[string]any{
					"subtype": "concurrent_op",
				},
			})

			// Randomly cancel some
			if idx%2 == 0 {
				time.Sleep(20 * time.Millisecond)

				transport.sendToController(map[string]any{
					"type":       "control_cancel_request",
					"request_id": reqID,
				})
			}
		}(i)
	}

	wg.Wait()

	// Allow operations to complete
	time.Sleep(200 * time.Millisecond)

	// If we get here without race detector complaining, test passes
}

func TestCancelAllInFlight(t *testing.T) {
	transport := newMockTransport()
	log := slog.Default()
	ctrl := NewController(log, transport)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ctrl.Start(ctx)
	require.NoError(t, err)

	var (
		cancelledCount int
		mu             sync.Mutex
	)

	handlerStarted := make(chan struct{}, 3)

	// Register slow handler
	ctrl.RegisterHandler("slow_op", func(ctx context.Context, _ *ControlRequest) (map[string]any, error) {
		handlerStarted <- struct{}{}

		<-ctx.Done()

		mu.Lock()
		defer mu.Unlock()

		cancelledCount++

		return nil, ctx.Err()
	})

	// Start multiple operations
	for i := range 3 {
		go func(idx int) {
			transport.sendToController(map[string]any{
				"type":       "control_request",
				"request_id": "req-all-" + string(rune('a'+idx)),
				"request": map[string]any{
					"subtype": "slow_op",
				},
			})
		}(i)
	}

	// Wait for all handlers to start
	for i := range 3 {
		select {
		case <-handlerStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("Handler %d did not start in time", i)
		}
	}

	// Stop controller (should cancel all in-flight)
	ctrl.Stop()

	// Give time for cancellation to propagate
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	count := cancelledCount

	assert.Equal(t, 3, count, "All handlers should have been cancelled")
}
