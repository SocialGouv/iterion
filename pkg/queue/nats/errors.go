package nats

import (
	"context"
	"errors"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// IsTransientPublishError reports whether a failed JetStream publish is
// expected to recover after the broker reconnects or responders return.
// Payload/schema errors are intentionally excluded: retrying those cannot
// make them valid and must preserve their original semantic error.
func IsTransientPublishError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, natsgo.ErrNoResponders) ||
		errors.Is(err, natsgo.ErrTimeout) ||
		errors.Is(err, natsgo.ErrDisconnected) ||
		errors.Is(err, natsgo.ErrConnectionClosed) ||
		errors.Is(err, natsgo.ErrInvalidConnection) ||
		errors.Is(err, natsgo.ErrNoStreamResponse) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, jetstream.ErrConnectionClosed) {
		return true
	}

	// JetStream can answer a publish with a server-side APIError while the
	// cluster is degraded. Retry only the explicitly temporary conditions;
	// schema, subject, and stream errors remain permanent by default.
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode {
	case jetstream.ErrorCode(10008), // cluster temporarily unavailable
		jetstream.ErrorCode(10023), // insufficient resources
		jetstream.ErrorCode(10040): // cluster peer not a member
		return true
	default:
		return false
	}
}
