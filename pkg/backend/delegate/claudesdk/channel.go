package claudesdk

import (
	"context"
	"iter"
)

// MessageOrError pairs a Message with an optional error for channel-based consumption.
type MessageOrError struct {
	Message Message
	Err     error
}

// ToChan converts an iter.Seq2[Message, error] into a channel.
// The channel is closed when the iterator is exhausted or the context is cancelled.
func ToChan(ctx context.Context, seq iter.Seq2[Message, error]) <-chan MessageOrError {
	ch := make(chan MessageOrError, 8)
	go func() {
		defer close(ch)
		for msg, err := range seq {
			select {
			case <-ctx.Done():
				ch <- MessageOrError{Err: ctx.Err()}
				return
			case ch <- MessageOrError{Message: msg, Err: err}:
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}
