package nats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// The boot wait of pkg/queue/nats, witnessed the way pkg/store/mongo's
// ping_test.go witnesses the store half: the loop against a scripted dialer,
// and the classifier against the errors the REAL library hands back — a
// stub that is gentler than natsgo would prove the policy on a producer
// nobody runs.
//
// No test in this file calls t.Parallel: the wiring witness replaces the
// package's dial policy for its duration, and nothing in pkg/queue/nats
// opts into parallel tests.

// dialedConn stands in for a live connection: waitForBroker returns the
// dialer's connection without touching it, so no broker is needed to prove
// the loop. Identity is asserted, so a loop that fabricated its own
// connection would be caught.
var dialedConn = &natsgo.Conn{}

// scriptedDialer answers the scripted errors in order, then connects.
type scriptedDialer struct {
	errs  []error
	calls int
}

func (s *scriptedDialer) dial(context.Context) (*natsgo.Conn, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return nil, s.errs[i]
	}
	return dialedConn, nil
}

// stuckDialer blocks until its attempt context expires: a host that accepts
// the TCP connection and then never completes the protocol handshake.
type stuckDialer struct{ calls int }

func (s *stuckDialer) dial(ctx context.Context) (*natsgo.Conn, error) {
	s.calls++
	<-ctx.Done()
	return nil, fmt.Errorf("dial: %w", ctx.Err())
}

var fastDialPolicy = dialPolicy{attempt: 20 * time.Millisecond, pause: time.Millisecond, budget: 300 * time.Millisecond}

// closedPort returns an address nothing listens on: the kernel refuses the
// connection, which is the boot failure measured on a fresh cluster
// ("connect NATS: nats: no servers available for connection").
func closedBrokerPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// malformedURLError is what natsgo returns for a URI no retry can fix. It is
// taken FROM the library rather than written down here, so the classifier is
// judged on the real producer's error.
func malformedURLError(t *testing.T) error {
	t.Helper()
	_, err := natsgo.Connect("nats://ho st:4222")
	if err == nil {
		t.Fatal("natsgo accepted a malformed URI — the fail-fast case below would prove nothing")
	}
	return err
}

// A broker that is not up YET is waited for: the shape the ticket measured,
// where the kubelet serialised image pulls and the nats pod arrived 7 min
// after the processes that need it.
func TestWaitForBrokerRetriesABrokerThatIsNotUpYet(t *testing.T) {
	d := &scriptedDialer{errs: []error{natsgo.ErrNoServers, natsgo.ErrNoServers}}
	nc, err := waitForBroker(context.Background(), d.dial, fastDialPolicy)
	if err != nil {
		t.Fatalf("waitForBroker: %v", err)
	}
	if nc != dialedConn {
		t.Fatalf("connection = %p, want the one the dialer returned", nc)
	}
	if d.calls != 3 {
		t.Fatalf("calls = %d, want 3 (two refused dials, then the broker)", d.calls)
	}
}

// A failure no retry can cure is returned from the first attempt: an
// operator with a malformed URI must not wait out the budget to learn it.
func TestWaitForBrokerFailsFastOnWhatNoRetryCures(t *testing.T) {
	malformed := malformedURLError(t)
	d := &scriptedDialer{errs: []error{malformed, malformed, malformed}}
	start := time.Now()
	nc, err := waitForBroker(context.Background(), d.dial, fastDialPolicy)
	if nc != nil {
		t.Fatalf("connection = %p, want none", nc)
	}
	if !errors.Is(err, malformed) {
		t.Fatalf("err = %v, want the library's own refusal", err)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1", d.calls)
	}
	if strings.Contains(err.Error(), "attempt") {
		t.Fatalf("a first-attempt refusal must not read as an exhausted wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > fastDialPolicy.budget/2 {
		t.Fatalf("a fail-fast refusal burned %v of a %v budget", elapsed, fastDialPolicy.budget)
	}
}

// Without a caller deadline the policy budget bounds the wait, and the error
// names the attempts so a broker that never answers is not mistaken for a
// hang.
func TestWaitForBrokerIsBoundedByTheBudgetWhenTheContextHasNoDeadline(t *testing.T) {
	d := &stuckDialer{}
	start := time.Now()
	nc, err := waitForBroker(context.Background(), d.dial, fastDialPolicy)
	elapsed := time.Since(start)
	if nc != nil || err == nil {
		t.Fatalf("connection = %p, err = %v, want no connection and an error after the budget", nc, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the last attempt's deadline in the chain", err)
	}
	if !strings.Contains(err.Error(), "no broker after") || !strings.Contains(err.Error(), "attempt(s)") {
		t.Fatalf("err = %v, want the attempt count", err)
	}
	if d.calls < 2 {
		t.Fatalf("calls = %d, want at least 2 attempts within the budget", d.calls)
	}
	if elapsed > 3*fastDialPolicy.budget {
		t.Fatalf("the wait ran %v past a %v budget", elapsed, fastDialPolicy.budget)
	}
}

// A caller's own deadline wins over the policy budget: a boot context that
// says when it gives up is honoured there, not 30 s later.
func TestWaitForBrokerHonoursTheCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	d := &stuckDialer{}
	pol := fastDialPolicy
	pol.budget = 10 * time.Second
	start := time.Now()
	_, err := waitForBroker(ctx, d.dial, pol)
	if err == nil {
		t.Fatal("expected an error at the caller deadline")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the wait ran %v past a 60ms caller deadline", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline ") {
		t.Fatalf("err = %v, want the caller deadline named", err)
	}
}

// A caller cancelled BEFORE the call is owed no dial at all, and the error
// says so: zero attempts, the cancellation named.
func TestWaitForBrokerDialsNothingForACallerAlreadyDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &scriptedDialer{errs: []error{natsgo.ErrNoServers}}
	_, err := waitForBroker(ctx, d.dial, fastDialPolicy)
	if err == nil || !strings.Contains(err.Error(), "context cancelled") {
		t.Fatalf("err = %v, want the cancellation named", err)
	}
	if !strings.Contains(err.Error(), "after 0 attempt(s)") {
		t.Fatalf("err = %v, want zero attempts reported", err)
	}
	if d.calls != 0 {
		t.Fatalf("calls = %d, want 0 — a done caller is owed no dial", d.calls)
	}
}

// A caller cancelled DURING a dial is named by the exhausted error, which
// also carries the attempt that was in flight — a different branch from the
// one above, and it must not report "no bound".
func TestWaitForBrokerNamesACallerCancelledDuringADial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancellingDialer{cancel: cancel}
	_, err := waitForBroker(ctx, d.dial, fastDialPolicy)
	if err == nil || !strings.Contains(err.Error(), "context cancelled") {
		t.Fatalf("err = %v, want the cancellation named", err)
	}
	if !strings.Contains(err.Error(), "after 1 attempt(s)") {
		t.Fatalf("err = %v, want the in-flight attempt reported", err)
	}
	if !errors.Is(err, natsgo.ErrNoServers) {
		t.Fatalf("err = %v, want the dial failure kept in the chain", err)
	}
}

// cancellingDialer cancels the caller while its dial is in flight.
type cancellingDialer struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancellingDialer) dial(context.Context) (*natsgo.Conn, error) {
	c.calls++
	c.cancel()
	return nil, natsgo.ErrNoServers
}

// A deadline already in the past is reported as a caller that gave up, NOT
// as a configuration mistake: natsgo refuses a negative timeout with "nats:
// timeout invalid", which is what the real dialer would receive from such an
// attempt context, and an operator reading that would hunt a chart value
// that is perfectly fine.
func TestWaitForBrokerRefusesAnExpiredDeadlineWithoutDialling(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := waitForBroker(ctx, dialBroker("nats://"+closedBrokerPort(t)), defaultDialPolicy)
	if err == nil {
		t.Fatal("expected an error for a deadline already past")
	}
	if strings.Contains(err.Error(), "timeout invalid") {
		t.Fatalf("err = %v, want the expired caller named, not a natsgo option error", err)
	}
	if !strings.Contains(err.Error(), "after 0 attempt(s)") || !strings.Contains(err.Error(), "deadline ") {
		t.Fatalf("err = %v, want zero attempts and the deadline named", err)
	}
}

// The classifier is judged on the errors natsgo ACTUALLY returns from a
// dial, each class separately so none covers another.
func TestRetryableDialClassifiesWhatTheLibraryReturns(t *testing.T) {
	_, refused := natsgo.Connect("nats://"+closedBrokerPort(t), natsgo.Timeout(300*time.Millisecond))
	if !errors.Is(refused, natsgo.ErrNoServers) {
		t.Fatalf("a refused port returned %v, want ErrNoServers — the measured boot failure changed shape", refused)
	}
	if !retryableDial(refused) {
		t.Fatalf("refused port: %v classified as final, want retryable", refused)
	}

	if malformed := malformedURLError(t); retryableDial(malformed) {
		t.Fatalf("malformed URI: %v classified as retryable, want final", malformed)
	}

	if quiet := quietBrokerError(t); !retryableDial(quiet) {
		t.Fatalf("quiet broker: %v classified as final, want retryable", quiet)
	}

	// Outside a proxying sandbox an unresolvable host reaches us as this
	// stdlib type (measured: "dial tcp: lookup iterion-nats.invalid on
	// 127.0.0.53:53: no such host"); a sandbox whose proxy answers the
	// resolution collapses the same case into ErrNoServers, which the
	// first class above already covers. The type is synthesized so the
	// witness does not depend on which of the two the machine produces.
	notFound := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
		Err: "no such host", Name: "iterion-nats.iterion.svc", IsNotFound: true,
	}}
	if nerr := net.Error(notFound); nerr.Timeout() {
		t.Fatal("a not-found DNS error now reports Timeout — the class below would be covered by the timeout one")
	}
	if !retryableDial(notFound) {
		t.Fatalf("unresolved service: %v classified as final, want retryable", notFound)
	}
}

// quietBrokerError is what natsgo returns against a listener that accepts
// the connection and never speaks the protocol — a broker container that
// bound its port before it could serve.
func quietBrokerError(t *testing.T) error {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := l.Accept()
		if aerr != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	_, err = natsgo.Connect("nats://"+l.Addr().String(), natsgo.Timeout(300*time.Millisecond))
	if c, ok := <-accepted; ok && c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("natsgo completed a handshake with a silent listener")
	}
	return err
}

// Connect itself waits — the chokepoint both boot callers cross
// (cmd/iterion/server.go, cmd/iterion/runner.go), so neither has to know
// about the policy. The policy is replaced for the duration, the way
// mongo's store tests bound their own ping.
func TestConnectWaitsForABrokerThatIsNotUpYet(t *testing.T) {
	restore := defaultDialPolicy
	defaultDialPolicy = dialPolicy{attempt: 100 * time.Millisecond, pause: 10 * time.Millisecond, budget: 400 * time.Millisecond}
	t.Cleanup(func() { defaultDialPolicy = restore })

	start := time.Now()
	conn, err := Connect(context.Background(), Config{URL: "nats://" + closedBrokerPort(t)})
	elapsed := time.Since(start)
	if conn != nil {
		t.Fatalf("conn = %p, want none against a closed port", conn)
	}
	if err == nil {
		t.Fatal("Connect returned no error against a closed port")
	}
	if !strings.Contains(err.Error(), "queue/nats: connect: no broker after") || !strings.Contains(err.Error(), "attempt(s)") {
		t.Fatalf("err = %v, want the bounded wait's wording — a one-shot dial reports the library error alone", err)
	}
	if !errors.Is(err, natsgo.ErrNoServers) {
		t.Fatalf("err = %v, want the library's refusal kept in the chain", err)
	}
	if elapsed < defaultDialPolicy.pause {
		t.Fatalf("Connect gave up in %v — no second attempt was made", elapsed)
	}
	if elapsed > 3*defaultDialPolicy.budget {
		t.Fatalf("Connect ran %v past a %v budget", elapsed, defaultDialPolicy.budget)
	}
}

// The real dialer is usable outside the loop: with no deadline on the
// context it keeps natsgo's own dial bound instead of computing one, which
// is what keeps a zero-valued deadline from reaching natsgo as a negative
// timeout.
func TestDialBrokerWithoutADeadlineKeepsTheLibrarysOwnBound(t *testing.T) {
	_, err := dialBroker("nats://" + closedBrokerPort(t))(context.Background())
	if !errors.Is(err, natsgo.ErrNoServers) {
		t.Fatalf("err = %v, want the refusal natsgo reports with its default timeout", err)
	}
	if strings.Contains(fmt.Sprint(err), "timeout invalid") {
		t.Fatalf("err = %v — a missing deadline became a negative natsgo timeout", err)
	}
}
