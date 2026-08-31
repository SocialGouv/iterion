package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
)

func TestPublishWithRetryTransientFailureThenSuccess(t *testing.T) {
	var calls int
	p := &Publisher{
		logger:             iterlog.Nop(),
		publishRetryDelays: []time.Duration{0, 0, 0},
		publishRun: func(context.Context, *queue.RunMessage) error {
			calls++
			if calls == 1 {
				return natsgo.ErrNoResponders
			}
			return nil
		},
	}

	if err := p.publish(context.Background(), &queue.RunMessage{RunID: "run-retry"}); err != nil {
		t.Fatalf("publish after transient failure: %v", err)
	}
	if calls != 2 {
		t.Fatalf("publish calls = %d, want 2", calls)
	}
}

func TestPublishWithRetryPersistentFailureReturnsQueueUnavailable(t *testing.T) {
	var calls int
	p := &Publisher{
		logger:             iterlog.Nop(),
		publishRetryDelays: []time.Duration{0, 0, 0},
		publishRun: func(context.Context, *queue.RunMessage) error {
			calls++
			return jetstream.ErrNoStreamResponse
		},
	}

	err := p.publish(context.Background(), &queue.RunMessage{RunID: "run-down"})
	var unavailable *runview.QueueUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error type = %T (%v), want *runview.QueueUnavailableError", err, err)
	}
	if unavailable.Code() != runview.QueueUnavailableErrorCode || !unavailable.Retryable() {
		t.Fatalf("typed retry metadata = (%q, %v), want (%q, true)", unavailable.Code(), unavailable.Retryable(), runview.QueueUnavailableErrorCode)
	}
	if !errors.Is(err, jetstream.ErrNoStreamResponse) {
		t.Fatalf("typed error does not retain NATS cause: %v", err)
	}
	if calls != 4 {
		t.Fatalf("publish calls = %d, want 4", calls)
	}
}

func TestPublishWithRetrySlowSuccessfulAck(t *testing.T) {
	var calls int
	p := &Publisher{
		logger:             iterlog.Nop(),
		publishRetryDelays: []time.Duration{},
		publishRun: func(ctx context.Context, _ *queue.RunMessage) error {
			calls++
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	started := time.Now()
	err := p.publish(context.Background(), &queue.RunMessage{RunID: "run-slow-ack"})
	if err != nil {
		t.Fatalf("publish with a 3s successful acknowledgement: %v", err)
	}
	if calls != 1 {
		t.Fatalf("publish calls = %d, want 1", calls)
	}
	if elapsed := time.Since(started); elapsed < 3*time.Second {
		t.Errorf("publish returned after %s, want it to wait for the successful acknowledgement", elapsed)
	}
}
