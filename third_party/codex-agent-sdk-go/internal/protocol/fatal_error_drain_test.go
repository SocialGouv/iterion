package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestControllerDrainsTransportErrorOnMessagesClose reproduces the shutdown
// sequence of the subprocess transports: a fatal read error is buffered on
// the errs channel, then both channels close. Because both select cases are
// ready at once, the controller used to return on the closed messages
// channel about half the time without consuming the error — the query then
// ended silently with no result and no error. The drain makes the error
// deterministic; run with -count to exercise the former race.
func TestControllerDrainsTransportErrorOnMessagesClose(t *testing.T) {
	for i := 0; i < 50; i++ {
		transport := newMockTransport()
		ctrl := NewController(slog.Default(), transport)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		require.NoError(t, ctrl.Start(ctx))

		// Mirror the transport goroutine's exit path: buffer the error,
		// then close errs, then close messages.
		transport.errChan <- fmt.Errorf("scanner error: bufio.Scanner: token too long")
		close(transport.errChan)
		close(transport.msgChan)

		// The controller's own messages channel closes when its read loop
		// returns; the fatal error must be recorded by then.
		for range ctrl.Messages() { //nolint:revive // draining until close
		}

		require.Error(t, ctrl.FatalError(),
			"iteration %d: transport error lost when messages closed first", i)
		require.ErrorContains(t, ctrl.FatalError(), "token too long")

		ctrl.Stop()
		cancel()
	}
}

// TestControllerDeliversBufferedMessagesAfterErrsCloses covers the symmetric
// shutdown interleaving: transports close errs BEFORE messages, so buffered
// events (e.g. a late turn.completed) must still be delivered instead of
// being dropped when the select lands on the closed errs channel first.
func TestControllerDeliversBufferedMessagesAfterErrsCloses(t *testing.T) {
	for i := 0; i < 50; i++ {
		transport := newMockTransport()
		ctrl := NewController(slog.Default(), transport)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		require.NoError(t, ctrl.Start(ctx))

		const backlog = 5
		for j := 0; j < backlog; j++ {
			transport.msgChan <- map[string]any{"type": "result", "index": j}
		}

		close(transport.errChan)
		close(transport.msgChan)

		received := 0
		for range ctrl.Messages() {
			received++
		}

		require.Equal(t, backlog, received,
			"iteration %d: buffered messages dropped after errs closed", i)
		require.NoError(t, ctrl.FatalError())

		ctrl.Stop()
		cancel()
	}
}
