package nats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Cross-process run steering (bump_loop / raise_budget).
//
// Transport shape: the API pod publishes the JSON SteerCommand on
// `iterion.steer.<run_id>` and waits on the per-command reply subject
// `iterion.steer.<run_id>.ack.<command_id>`. Explicit subjects — not
// nc.Request INBOXes — so both directions are observable with
// `nats sub 'iterion.steer.>'` and stay symmetric with the cancel
// subject convention. Core NATS (transient): a runner that is not
// subscribed yields a timeout, and the KV lease pre-check turns the
// common "no runner holds this run" case into a fast typed error
// instead of a burned timeout.
const (
	SubjectSteerFmt    = "iterion.steer.%s"        // %s = run_id
	SubjectSteerAckFmt = "iterion.steer.%s.ack.%s" // run_id, command_id

	// HeaderSteerCommandID carries the dedup key on both directions
	// (defence in depth; the JSON body is authoritative).
	HeaderSteerCommandID = "Iterion-Command-Id"

	// steerTimeout bounds the wait for the runner's reply — matches the
	// cancel-flush and publish timeouts.
	steerTimeout = 5 * time.Second
)

// ErrSteerTimeout: the command was published but no runner replied in
// time (runner mid-restart, or the run terminated between the lease
// check and the publish). The API maps it to 504.
var ErrSteerTimeout = errors.New("queue/nats: steer command timed out waiting for the runner's reply")

// ErrSteerNoRunner: no runner holds the run's KV lease — nothing to
// steer. The API maps it to 409.
var ErrSteerNoRunner = errors.New("queue/nats: no runner holds this run's lease")

// SteerRun publishes cmdBody (the JSON SteerCommand) for runID and
// waits for the runner's reply body. commandID keys the reply subject
// and the dedup header.
func (c *Conn) SteerRun(ctx context.Context, runID string, cmdBody []byte, commandID string) ([]byte, error) {
	if c == nil || c.nc == nil {
		return nil, fmt.Errorf("queue/nats: connection not initialised")
	}
	if runID == "" || commandID == "" {
		return nil, fmt.Errorf("queue/nats: steer requires runID and commandID")
	}

	// Fast no-runner check: an absent lease means no pod is executing
	// the run right now (queued, terminal, or lease expired after a
	// crash) — fail typed instead of waiting out the timeout.
	if c.kv != nil {
		if _, err := c.kv.Get(ctx, runID); err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil, ErrSteerNoRunner
			}
			// Any other KV error: fall through to the publish path — the
			// lease check is an optimisation, not the authority.
		}
	}

	// Subscribe to the reply subject BEFORE publishing (same SUB-protocol
	// race the cancel path guards against), with a synchronous
	// subscription so we can wait bounded.
	ackSubject := fmt.Sprintf(SubjectSteerAckFmt, runID, commandID)
	sub, err := c.nc.SubscribeSync(ackSubject)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: subscribe steer ack %s: %w", ackSubject, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := c.nc.FlushTimeout(steerTimeout); err != nil {
		return nil, fmt.Errorf("queue/nats: flush steer ack sub %s: %w", ackSubject, err)
	}

	headers := nats.Header{}
	headers.Set(HeaderSteerCommandID, commandID)
	if err := c.nc.PublishMsg(&nats.Msg{
		Subject: fmt.Sprintf(SubjectSteerFmt, runID),
		Data:    cmdBody,
		Header:  headers,
	}); err != nil {
		return nil, fmt.Errorf("queue/nats: publish steer %s: %w", runID, err)
	}
	if err := c.nc.FlushTimeout(steerTimeout); err != nil {
		return nil, fmt.Errorf("queue/nats: flush steer %s: %w", runID, err)
	}

	wait := steerTimeout
	if dl, ok := ctx.Deadline(); ok {
		if until := time.Until(dl); until < wait {
			wait = until
		}
	}
	msg, err := sub.NextMsg(wait)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) {
			return nil, ErrSteerTimeout
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("queue/nats: await steer reply %s: %w", ackSubject, err)
	}
	return msg.Data, nil
}

// SubscribeSteer installs the per-run steering subscriber. handler
// receives each command's body + command id (from the header, falling
// back to ""); it MUST publish its reply via PublishSteerAck. Lifecycle
// mirrors SubscribeCancel: auto-unsubscribed when ctx ends.
func (c *Conn) SubscribeSteer(ctx context.Context, runID string, handler func(cmdBody []byte, commandID string)) (*nats.Subscription, error) {
	if c == nil || c.nc == nil {
		return nil, fmt.Errorf("queue/nats: connection not initialised")
	}
	if ctx == nil {
		return nil, fmt.Errorf("queue/nats: subscribe steer %s: nil context", runID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sub, err := c.nc.Subscribe(fmt.Sprintf(SubjectSteerFmt, runID), func(m *nats.Msg) {
		handler(m.Data, m.Header.Get(HeaderSteerCommandID))
	})
	if err != nil {
		return nil, fmt.Errorf("queue/nats: subscribe steer %s: %w", runID, err)
	}
	flushCtx := ctx
	var cancel context.CancelFunc
	if _, ok := flushCtx.Deadline(); !ok {
		flushCtx, cancel = context.WithTimeout(ctx, steerTimeout)
	} else {
		cancel = func() {}
	}
	flushErr := c.nc.FlushWithContext(flushCtx)
	cancel()
	if flushErr != nil {
		if unsubErr := sub.Unsubscribe(); unsubErr != nil {
			return nil, fmt.Errorf("queue/nats: subscribe steer %s flush: %w (unsubscribe: %v)", runID, flushErr, unsubErr)
		}
		return nil, fmt.Errorf("queue/nats: subscribe steer %s flush: %w", runID, flushErr)
	}
	go func() {
		<-ctx.Done()
		if err := sub.Unsubscribe(); err != nil && c.logger != nil {
			c.logger.Warn("queue/nats: unsubscribe steer %s: %v", runID, err)
		}
	}()
	return sub, nil
}

// PublishSteerAck publishes the runner's typed reply body on the
// per-command ack subject.
func (c *Conn) PublishSteerAck(runID, commandID string, body []byte) error {
	if c == nil || c.nc == nil {
		return fmt.Errorf("queue/nats: connection not initialised")
	}
	headers := nats.Header{}
	headers.Set(HeaderSteerCommandID, commandID)
	if err := c.nc.PublishMsg(&nats.Msg{
		Subject: fmt.Sprintf(SubjectSteerAckFmt, runID, commandID),
		Data:    body,
		Header:  headers,
	}); err != nil {
		return fmt.Errorf("queue/nats: publish steer ack %s/%s: %w", runID, commandID, err)
	}
	if err := c.nc.FlushTimeout(steerTimeout); err != nil {
		return fmt.Errorf("queue/nats: flush steer ack %s/%s: %w", runID, commandID, err)
	}
	return nil
}
