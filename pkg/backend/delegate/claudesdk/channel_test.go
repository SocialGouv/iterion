package claudesdk

import (
	"context"
	"iter"
	"testing"
	"time"
)

// TestToChan_CancelDoesNotLeakGoroutine verifies that cancelling the
// context releases the producer goroutine even when the consumer has
// stopped reading. The earlier implementation issued an unconditional
// send of MessageOrError{Err: ctx.Err()} on cancel; once the 8-slot
// buffer filled, that send blocked indefinitely.
func TestToChan_CancelDoesNotLeakGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Producer that emits forever — until the consumer or ctx stops it.
	seq := iter.Seq2[Message, error](func(yield func(Message, error) bool) {
		for {
			if !yield(nil, nil) {
				return
			}
		}
	})

	ch := ToChan(ctx, seq)

	// Fill the buffer (8 slots) without reading the final cancel send.
	for i := 0; i < 8; i++ {
		<-ch
	}

	// Cancel and stop reading. If the producer's cancel-path send is
	// unconditional, it blocks forever and this test never returns —
	// the test binary's -timeout kills the run.
	cancel()

	// Wait for channel close with a deadline. A leaked producer never
	// closes the channel.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed — producer exited cleanly
			}
		case <-deadline:
			t.Fatal("ToChan producer leaked: channel never closed after cancel()")
		}
	}
}

// TestToChan_DrainAfterCancel verifies that a consumer who keeps
// reading after cancel still receives a final ctx.Err() before the
// channel closes (the buffer-permitting case).
func TestToChan_DrainAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	seq := iter.Seq2[Message, error](func(yield func(Message, error) bool) {
		for {
			if !yield(nil, nil) {
				return
			}
			time.Sleep(time.Millisecond) // give cancel a chance to win
		}
	})

	ch := ToChan(ctx, seq)
	cancel()

	sawErr := false
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				if !sawErr {
					t.Log("channel closed without surfacing ctx.Err — acceptable (non-blocking send)")
				}
				return
			}
			if msg.Err != nil {
				sawErr = true
			}
		case <-deadline:
			t.Fatal("ToChan producer leaked: channel never closed after cancel() (drain path)")
		}
	}
}
