package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/topology"
)

// primaryPinger is the slice of *mongo.Client that New needs to validate
// a connection — a seam so the retry policy below is testable without a
// broker.
type primaryPinger interface {
	Ping(ctx context.Context, rp *readpref.ReadPref) error
}

// pingPolicy bounds the connection validation in New.
type pingPolicy struct {
	// attempt bounds ONE ping. The driver's own server-selection timeout
	// is 30 s; a shorter attempt keeps a dead URI from stalling boot for
	// that long between retries.
	attempt time.Duration
	// pause separates two attempts.
	pause time.Duration
	// budget bounds the whole validation when the caller's context
	// carries no deadline (a server boot). A caller with a deadline sets
	// its own bound.
	budget time.Duration
}

var defaultPingPolicy = pingPolicy{
	attempt: 5 * time.Second,
	pause:   500 * time.Millisecond,
	budget:  30 * time.Second,
}

// pingPrimary validates the connection against the PRIMARY, retrying a
// server-selection failure until the context (or the policy budget) runs
// out. A fresh client's first server selection can outlast one attempt
// on a loaded host while the replica set is healthy — the handshake is
// late, not the primary absent — and a one-shot ping turns that lag into
// a boot failure, or into a conformance harness that ejects a green PR
// from the merge queue. A failure no retry can cure (bad credentials, a
// malformed URI) is returned from the first attempt. A primary that never
// appears still surfaces: the error names the attempts made and carries
// the driver's last topology report.
func pingPrimary(ctx context.Context, cli primaryPinger, pol pingPolicy) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.budget)
		defer cancel()
	}
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, pol.attempt)
		err := cli.Ping(attemptCtx, readpref.Primary())
		cancel()
		if err == nil {
			return nil
		}
		if !retryablePing(err) {
			return fmt.Errorf("store/mongo: ping: %w", err)
		}
		exhausted := fmt.Errorf("store/mongo: ping: no primary after %d attempt(s) (%s): %w",
			attempt, describeBound(ctx), err)
		if ctx.Err() != nil {
			return exhausted
		}
		select {
		case <-ctx.Done():
			return exhausted
		case <-time.After(pol.pause):
		}
	}
}

// retryablePing reports whether a ping failure means the topology is not
// ready yet (server selection still running, a timeout on the way) rather
// than a verdict on the connection itself.
func retryablePing(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		mongo.IsTimeout(err) ||
		errors.As(err, &topology.ServerSelectionError{})
}

// describeBound names the bound that ended the retries, so the operator
// can tell "the caller gave up" from "the boot budget ran out".
func describeBound(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "context cancelled"
	}
	if deadline, ok := ctx.Deadline(); ok {
		return "deadline " + deadline.UTC().Format(time.RFC3339)
	}
	return "no bound"
}
