package nats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// brokerDialer opens ONE connection to the broker, bounded by the attempt
// context. It is the seam that makes the boot policy below testable without
// a broker — the shape `pkg/store/mongo/ping.go` gives the store half with
// primaryPinger. A dialer answers with a live connection or an error, never
// both: every failure path of natsgo's Options.Connect returns `nil, err`.
type brokerDialer func(ctx context.Context) (*natsgo.Conn, error)

// dialPolicy bounds the boot dial in Connect.
type dialPolicy struct {
	// attempt bounds ONE dial. natsgo's own dial timeout is 2 s per server
	// in the pool and its handshake has no separate bound, so an attempt
	// deadline keeps a host that accepts TCP and then goes quiet from
	// holding the whole budget.
	attempt time.Duration
	// pause separates two attempts.
	pause time.Duration
	// budget bounds the whole wait when the caller's context carries no
	// deadline (a server or a runner boot). A caller with a deadline sets
	// its own bound.
	budget time.Duration
}

// defaultDialPolicy mirrors mongo's defaultPingPolicy value for value: a
// booting pod waits for its queue the way it waits for its store, and both
// waits share ONE budget the chart gives them — the startupProbe's 30 × 5 s
// = 150 s (charts/iterion/values.yaml, `probes.startup`), which also has to
// cover the ~28-collection schema pass. Neither wait may claim it alone, so
// raising one of the two is a decision about that 150 s, not a local knob.
var defaultDialPolicy = dialPolicy{
	attempt: 5 * time.Second,
	pause:   500 * time.Millisecond,
	budget:  30 * time.Second,
}

// dialBroker is the real brokerDialer. natsgo.Connect takes no context, so
// the attempt deadline becomes its natsgo.Timeout — the per-server dial
// bound — and opts follow it, which leaves a caller free to pass an explicit
// timeout that then wins.
func dialBroker(url string, opts ...natsgo.Option) brokerDialer {
	return func(ctx context.Context) (*natsgo.Conn, error) {
		bounded := opts
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// The attempt expired between the loop's check and this
				// dial (a slow runner under the race detector measured
				// it): a non-positive natsgo.Timeout is an option error
				// reading like a configuration mistake, so answer as a
				// dialer that honours its context does and let the loop
				// name the bound.
				return nil, context.DeadlineExceeded
			}
			bounded = append([]natsgo.Option{natsgo.Timeout(remaining)}, opts...)
		}
		return natsgo.Connect(url, bounded...)
	}
}

// waitForBroker opens the boot connection, retrying a broker that is not up
// YET until the context (or the policy budget) runs out. On a fresh cluster
// the queue pod routinely starts after the processes that need it — the
// kubelet serialises image pulls, and a Service's DNS record appears when
// the Service does — and a one-shot dial turns that lag into a boot failure
// whose crash-loop backoff then grows to minutes, long after the broker came
// up (measured: `helm install --wait` failing on a kind node where the nats
// image waited 7 min behind the pinned mongo and seaweedfs pulls).
//
// A failure no retry can cure (bad credentials, a malformed URI) is returned
// from the first attempt. A broker that never answers still surfaces: the
// error names the attempts made and the bound that ended them.
//
// natsgo.RetryOnFailedConnect is deliberately NOT used for this: it hands
// back a non-nil connection that is not connected, so the process would boot
// "successfully" into a degraded state where the failure is invisible and
// the readiness probe passes on it.
func waitForBroker(ctx context.Context, dial brokerDialer, pol dialPolicy) (*natsgo.Conn, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pol.budget)
		defer cancel()
	}
	var lastRefusal error // the last answer natsgo itself gave; an expired attempt alone says nothing about the broker
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			// The caller is already done, so no attempt is owed — and a
			// dial bounded by a deadline in the past would reach natsgo
			// as a negative timeout, i.e. an option error reading like a
			// configuration mistake instead of a caller that gave up.
			//
			// This is the loop's OTHER give-up, and it is reached with a
			// refusal already in hand whenever the bound falls between the
			// end of a pause and this check — a window a loaded runner
			// opens wide. The refusal travels here too, so the caller's
			// errors.Is answer does not depend on which of the two exits
			// the scheduler picked.
			return nil, fmt.Errorf("queue/nats: connect: no broker after %d attempt(s) (%s): %w",
				attempt-1, describeDialBound(ctx), gaveUp(ctx.Err(), lastRefusal))
		}
		attemptCtx, cancel := context.WithTimeout(ctx, pol.attempt)
		nc, err := dial(attemptCtx)
		cancel()
		if err == nil {
			return nc, nil
		}
		if errors.Is(err, context.DeadlineExceeded) && lastRefusal != nil {
			// The attempt expired before natsgo answered (a slow runner under the
			// race detector measured it): the library's last refusal is the
			// informative one, so both stay in the chain — the caller reads
			// natsgo.ErrNoServers through errors.Is, and the bound through the text.
			err = fmt.Errorf("%w; the last attempt expired: %w", lastRefusal, err)
		}
		if !retryableDial(err) {
			return nil, fmt.Errorf("queue/nats: connect: %w", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			lastRefusal = err
		}
		exhausted := fmt.Errorf("queue/nats: connect: no broker after %d attempt(s) (%s): %w",
			attempt, describeDialBound(ctx), err)
		if ctx.Err() != nil {
			return nil, exhausted
		}
		select {
		case <-ctx.Done():
			return nil, exhausted
		case <-time.After(pol.pause):
		}
	}
}

// retryableDial reports whether a failed boot dial means the broker is not
// up yet rather than a verdict on the connection itself. The default is NO:
// a class that is not listed here is reported from the first attempt, so a
// mistake an operator must fix is never hidden behind a wait. Each class
// below is one natsgo actually hands back on a dial — measured, not guessed:
// natsgo.ErrTimeout, for one, only comes from a request, an unsubscribe or a
// flush, so listing it here would be an untestable shape.
//
// This answers a different question from IsTransientPublishError, which
// judges a publish on a live connection; the two sets are not
// interchangeable (a no-responders publish says nothing about a dial, and
// ErrNoServers cannot come back from a publish).
func retryableDial(err error) bool {
	// Nothing is listening yet: natsgo collapses a refused connection on
	// every server of the pool into this one.
	if errors.Is(err, natsgo.ErrNoServers) {
		return true
	}
	// A Service whose record is not published yet answers NXDOMAIN, which
	// is indistinguishable from a host name nobody will ever resolve. The
	// cluster case is the one a boot must survive; the typo costs the
	// budget and is then named in the error. The class is checked BEFORE
	// the timeout one: a not-found DNS error is a net.Error whose Timeout
	// is false.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// A dial or a handshake that outlasted its bound: a broker whose
	// container accepts TCP before it serves the protocol, or a host that
	// blackholes SYN (a NetworkPolicy applied before the broker's pod
	// exists). context.DeadlineExceeded — what a dialer that honours its
	// attempt context returns — belongs to this class too.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// describeDialBound names the bound that ended the retries, so the operator
// can tell "the caller gave up" from "the boot budget ran out". The store
// half carries the same four lines (mongo's describeBound); a third bounded
// wait extracts them rather than copying again.
// gaveUp names why a wait ended: the bound that ended it and, when natsgo
// had already refused, that refusal — two facts a caller reads through
// errors.Is, never one replacing the other. A nil refusal is left out, so a
// caller owed no dial reports the bound alone.
func gaveUp(bound, refusal error) error {
	if refusal == nil {
		return bound
	}
	return fmt.Errorf("%w; the broker had refused: %w", bound, refusal)
}

func describeDialBound(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "context cancelled"
	}
	if deadline, ok := ctx.Deadline(); ok {
		return "deadline " + deadline.UTC().Format(time.RFC3339)
	}
	return "no bound"
}
