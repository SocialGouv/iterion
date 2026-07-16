package protocol

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestController_SetFatalError_ConcurrentWithStop(t *testing.T) {
	// This test verifies no panic occurs when SetFatalError and Stop race.
	// Run with: go test -race -count=100
	for range 100 {
		transport := newMockTransport()
		controller := NewController(slog.Default(), transport)

		ctx := context.Background()
		err := controller.Start(ctx)
		require.NoError(t, err)

		var wg sync.WaitGroup

		wg.Add(2)

		// Goroutine 1: SetFatalError
		go func() {
			defer wg.Done()

			controller.SetFatalError(errors.New("transport error"))
		}()

		// Goroutine 2: Stop
		go func() {
			defer wg.Done()

			controller.Stop()
		}()

		wg.Wait()

		// Verify done channel is closed
		select {
		case <-controller.Done():
			// Expected
		default:
			t.Fatal("done channel should be closed")
		}
	}
}

func TestController_SetFatalError_MultipleCalls(t *testing.T) {
	// Verify multiple SetFatalError calls don't panic
	transport := newMockTransport()
	controller := NewController(slog.Default(), transport)

	ctx := context.Background()
	err := controller.Start(ctx)
	require.NoError(t, err)

	defer controller.Stop()

	// First error should be stored
	controller.SetFatalError(errors.New("first error"))
	require.EqualError(t, controller.FatalError(), "first error")

	// Second call should not panic, and first error is preserved
	controller.SetFatalError(errors.New("second error"))
	require.EqualError(t, controller.FatalError(), "first error")
}

func TestController_Stop_MultipleCalls(t *testing.T) {
	// Verify multiple Stop calls don't panic
	transport := newMockTransport()
	controller := NewController(slog.Default(), transport)

	ctx := context.Background()
	err := controller.Start(ctx)
	require.NoError(t, err)

	// Multiple Stop calls should not panic
	controller.Stop()
	controller.Stop()
	controller.Stop()

	// Verify done channel is closed
	select {
	case <-controller.Done():
		// Expected
	default:
		t.Fatal("done channel should be closed")
	}
}

func TestController_SendRequest_ResponseAfterTimeout_Race(t *testing.T) {
	// This test attempts to trigger a race between SendRequest timing out
	// and handleControlResponse delivering the response.
	//
	// The race window:
	// 1. SendRequest is waiting in select for response
	// 2. Response arrives, handleControlResponse looks up pending (found)
	// 3. SendRequest times out, defer runs, deletes pending from map
	// 4. handleControlResponse tries to send to response channel
	//
	// Run with: go test -race -count=100 -run TestController_SendRequest_ResponseAfterTimeout_Race
	for range 100 {
		transport := newMockTransport()
		controller := NewController(slog.Default(), transport)

		ctx := context.Background()
		err := controller.Start(ctx)
		require.NoError(t, err)

		// Use very short timeout to maximize chance of hitting race window
		timeout := 1 * time.Millisecond

		var wg sync.WaitGroup

		wg.Add(2)

		// Goroutine 1: Send request (will timeout)
		go func() {
			defer wg.Done()

			_, _ = controller.SendRequest(ctx, "test", map[string]any{}, timeout)
			// We expect this to timeout - ignore the error
		}()

		// Goroutine 2: Send response after a tiny delay
		// This tries to hit the window where pending exists but SendRequest is about to return
		go func() {
			defer wg.Done()

			// Small delay to let SendRequest register the pending request
			time.Sleep(500 * time.Microsecond)

			// Inject response - this will race with the timeout
			transport.sendToController(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"request_id": findPendingRequestID(controller),
					"subtype":    "success",
				},
			})
		}()

		wg.Wait()
		controller.Stop()
	}
}

// findPendingRequestID extracts a pending request ID from the controller.
// This is a test helper that peeks into pending requests.
func findPendingRequestID(c *Controller) string {
	c.pendingMu.RLock()
	defer c.pendingMu.RUnlock()

	for id := range c.pending {
		return id
	}

	return "unknown-request-id"
}

func TestController_SendRequest_ResponseDeliveryRace(t *testing.T) {
	// More aggressive test: many concurrent requests with immediate responses.
	// Run with: go test -race -count=10 -run TestController_SendRequest_ResponseDeliveryRace
	transport := newMockTransport()
	controller := NewController(slog.Default(), transport)

	ctx := context.Background()
	err := controller.Start(ctx)
	require.NoError(t, err)

	defer controller.Stop()

	var wg sync.WaitGroup

	numRequests := 50

	for range numRequests {
		wg.Go(func() {
			// Very short timeout
			timeout := 100 * time.Microsecond

			// Start request
			responseChan := make(chan struct{})

			go func() {
				_, _ = controller.SendRequest(ctx, "test", map[string]any{}, timeout)

				close(responseChan)
			}()

			// Immediately try to inject a response
			time.Sleep(50 * time.Microsecond)

			reqID := findPendingRequestID(controller)
			if reqID != "unknown-request-id" {
				transport.sendToController(map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"request_id": reqID,
						"subtype":    "success",
					},
				})
			}

			<-responseChan
		})
	}

	wg.Wait()
}

func TestController_SendRequest_ResponseChannelRace(t *testing.T) {
	// This test targets the specific race window where handleControlResponse
	// has already looked up the pending request but hasn't sent yet, while
	// SendRequest's defer is removing it from the map.
	//
	// The race is between:
	// - handleControlResponse: pending.response <- resp (line 407)
	// - SendRequest defer: delete(c.pending, requestID) (line 215)
	//
	// Run with: go test -race -count=1000 -run TestController_SendRequest_ResponseChannelRace
	for range 100 {
		transport := newMockTransport()
		controller := NewController(slog.Default(), transport)

		ctx := context.Background()
		err := controller.Start(ctx)
		require.NoError(t, err)

		// Capture the request ID as soon as it's registered
		var capturedReqID string

		reqIDCaptured := make(chan struct{})

		// Monitor for pending requests
		go func() {
			for {
				controller.pendingMu.RLock()

				for id := range controller.pending {
					capturedReqID = id

					controller.pendingMu.RUnlock()

					close(reqIDCaptured)

					return
				}

				controller.pendingMu.RUnlock()

				time.Sleep(10 * time.Microsecond)
			}
		}()

		var wg sync.WaitGroup

		// Start the request with a timeout that will fire
		wg.Go(func() {
			_, _ = controller.SendRequest(ctx, "test", map[string]any{}, 500*time.Microsecond)
		})

		// Wait for request to be registered, then immediately send response
		select {
		case <-reqIDCaptured:
			// Spam responses to maximize chance of hitting the race window
			for range 10 {
				transport.sendToController(map[string]any{
					"type": "control_response",
					"response": map[string]any{
						"request_id": capturedReqID,
						"subtype":    "success",
					},
				})
			}
		case <-time.After(10 * time.Millisecond):
			// Request might have already completed
		}

		wg.Wait()
		controller.Stop()
	}
}
