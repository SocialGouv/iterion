package nats

import (
	"context"
	"errors"
	"fmt"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestIsTransientPublishError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no responders", err: natsgo.ErrNoResponders, want: true},
		{name: "request timeout", err: natsgo.ErrTimeout, want: true},
		{name: "publish deadline", err: context.DeadlineExceeded, want: true},
		{name: "disconnected", err: natsgo.ErrDisconnected, want: true},
		{name: "connection closed", err: natsgo.ErrConnectionClosed, want: true},
		{name: "invalid connection", err: natsgo.ErrInvalidConnection, want: true},
		{name: "legacy stream unavailable", err: natsgo.ErrNoStreamResponse, want: true},
		{name: "stream unavailable", err: jetstream.ErrNoStreamResponse, want: true},
		{name: "wrapped transient", err: errors.Join(errors.New("publish failed"), jetstream.ErrConnectionClosed), want: true},
		{name: "api cluster temporarily unavailable", err: fmt.Errorf("publish failed: %w", &jetstream.APIError{Code: 503, ErrorCode: jetstream.ErrorCode(10008)}), want: true},
		{name: "api insufficient resources", err: fmt.Errorf("publish failed: %w", &jetstream.APIError{Code: 503, ErrorCode: jetstream.ErrorCode(10023)}), want: true},
		{name: "api cluster peer not member", err: fmt.Errorf("publish failed: %w", &jetstream.APIError{Code: 503, ErrorCode: jetstream.ErrorCode(10040)}), want: true},
		{name: "api stream not found", err: fmt.Errorf("publish failed: %w", &jetstream.APIError{Code: 404, ErrorCode: jetstream.ErrorCode(10059)}), want: false},
		{name: "api bad request", err: fmt.Errorf("publish failed: %w", &jetstream.APIError{Code: 400, ErrorCode: jetstream.ErrorCode(10003)}), want: false},
		{name: "payload refusal", err: errors.New("maximum payload exceeded"), want: false},
		{name: "caller cancellation", err: context.Canceled, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransientPublishError(tt.err); got != tt.want {
				t.Errorf("IsTransientPublishError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
