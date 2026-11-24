package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// A pool-attached Consumer refuses the runner-side recreate before any
// broker call: the pool consumer is the server reconciler's topology,
// and Recreate's delete would land on the SHARED fleet consumer the
// owner/cfg still name (revi round 1, R124668).
func TestRecreateRefusesAPoolAttachedConsumer(t *testing.T) {
	cons := &Consumer{pool: "honorabilite", owner: &Conn{cfg: Config{StreamName: StreamRuns, ConsumerName: ConsumerRunners}}}
	err := cons.Recreate(context.Background())
	if err == nil {
		t.Fatal("pool-attached Recreate: want the typed refusal, got nil")
	}
	if !strings.Contains(err.Error(), "reconciler") {
		t.Fatalf("pool-attached Recreate: want the reconciler ownership error, got %v", err)
	}
	// RecoverIfGone reads as no-gone for the same reason: the fetch-path
	// recovery must not route a pool pod onto the shared consumer.
	if cons.RecoverIfGone(context.Background(), errors.New("anything")) {
		t.Fatal("pool-attached RecoverIfGone: want false (the reconciler owns the topology)")
	}
}

// IsConsumerGone matches the two sentinel shapes and the no-responders
// descriptions through wrapped chains, and stays false on unrelated
// errors and on nil.
func TestIsConsumerGoneShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deleted sentinel", fmt.Errorf("fetch wait: %w", jetstream.ErrConsumerDeleted), true},
		{"not-found sentinel", fmt.Errorf("info: %w", jetstream.ErrConsumerNotFound), true},
		{"no responders text", errors.New("queue/nats: fetch error: nats: no responders available for request"), false},
		{"unrelated", errors.New("connection closed"), false},
	}
	for _, tc := range cases {
		if got := IsConsumerGone(tc.err); got != tc.want {
			t.Errorf("%s: IsConsumerGone=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// IsLeadershipChange matches the sentinel through the pull path's
// wrapping and the API's own description — the exact string the Oct 30
// runner logged ("nats: leadership change (backing off)", issue #2890) —
// and stays false on nil, on unrelated errors, and on the consumer-gone
// shapes the fetch loop routes to RecoverIfGone first.
func TestIsLeadershipChangeShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"wrapped sentinel", fmt.Errorf("queue/nats: fetch wait error: %w", jetstream.ErrConsumerLeadershipChanged), true},
		{"the production string", errors.New("nats: leadership change (backing off)"), true},
		{"upper-cased description", errors.New("Leadership Change"), true},
		{"consumer deleted is not leadership", fmt.Errorf("fetch: %w", jetstream.ErrConsumerDeleted), false},
		{"no responders is not leadership", errors.New("nats: no responders available for request"), false},
		{"unrelated", errors.New("connection closed"), false},
	}
	for _, tc := range cases {
		if got := IsLeadershipChange(tc.err); got != tc.want {
			t.Errorf("%s: IsLeadershipChange=%v, want %v", tc.name, got, tc.want)
		}
	}
}
