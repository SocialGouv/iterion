package nats

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// The boot wait of pkg/queue/nats, witnessed the way pkg/store/mongo's
// ping_test.go witnesses the store half: the loop against a scripted dialer,
// and the classifier against the errors the REAL library hands back — a
// stub that is gentler than natsgo would prove the policy on a producer
// nobody runs.
//
// A witness asserts only what its fixture fixes. The loop's witnesses run in
// a testing/synctest bubble, so their attempts, pauses and budgets are
// virtual: the retry counts, deadlines and durations they read are exact on
// any host, however loaded. A witness that needs natsgo's verdict on ONE
// transport outcome hands the real library that outcome through its
// CustomDialer — the kernel's refusal, a connection that never speaks, a
// broker that rejects the CONNECT — because a loaded host can turn a real
// loopback refusal into a dial timeout. The Connect wiring witness dials a
// real closed port under a real deadline, so it accepts every outcome such a
// dial can have and reads one duration only: a floor that load can lengthen,
// never shorten.
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

// fixtureBrokerURL is the address natsgo is given when a CustomDialer
// fixture answers in place of the network: an IP literal, so the library
// resolves nothing, and the fixture never dials it.
const fixtureBrokerURL = "nats://127.0.0.1:4222"

// refusingDialer answers every dial with the error net.Dialer builds for a
// port nothing listens on — the operation, the address, the connect syscall
// and ECONNREFUSED, term for term (TestRefusingDialerAnswersAsTheKernelDoes
// holds it to a real closed port). What the witnesses using it judge is
// natsgo's own reading of that error: a refused pool collapses into
// ErrNoServers.
type refusingDialer struct{}

func (refusingDialer) Dial(network, address string) (net.Conn, error) {
	addr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}
	return nil, &net.OpError{Op: "dial", Net: network, Addr: addr, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// pipeDialer hands natsgo one end of a net.Pipe; the broker is whatever the
// test plays on the other end. A net.Pipe honours the deadline natsgo sets
// on its handshake the way a TCP socket does, and inside a synctest bubble
// that deadline is virtual.
type pipeDialer struct{ conn net.Conn }

func (d pipeDialer) Dial(string, string) (net.Conn, error) { return d.conn, nil }

// isTimeout reports a bound that outlasted a dial or a handshake — the
// timeout class retryableDial reads.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// closedBrokerPort returns an address nothing listens on: the kernel refuses
// the connection, which is the boot failure measured on a fresh cluster
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

// The refusal refusingDialer hands natsgo is the kernel's. A dial to a
// closed loopback port with no deadline cannot end in a timeout, whatever
// the load, and both answers must read alike: natsgo recognises a refusal
// by that text.
func TestRefusingDialerAnswersAsTheKernelDoes(t *testing.T) {
	addr := closedBrokerPort(t)
	c, kernel := net.Dial("tcp", addr)
	if kernel == nil {
		_ = c.Close()
		t.Fatalf("a dial to the closed port %s connected — the comparison below would prove nothing", addr)
	}
	_, stub := refusingDialer{}.Dial("tcp", addr)
	if stub.Error() != kernel.Error() {
		t.Fatalf("fixture refusal %q, kernel refusal %q: the fixture no longer carries the real producer's terms", stub, kernel)
	}
	if !errors.Is(kernel, syscall.ECONNREFUSED) || !errors.Is(stub, syscall.ECONNREFUSED) {
		t.Fatalf("kernel %v, fixture %v: want ECONNREFUSED in both chains", kernel, stub)
	}
}

// A broker that is not up YET is waited for: the shape the ticket measured,
// where the kubelet serialised image pulls and the nats pod arrived 7 min
// after the processes that need it.
func TestWaitForBrokerRetriesABrokerThatIsNotUpYet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &scriptedDialer{errs: []error{natsgo.ErrNoServers, natsgo.ErrNoServers}}
		start := time.Now()
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
		if elapsed := time.Since(start); elapsed != 2*fastDialPolicy.pause {
			t.Fatalf("the wait took %v, want two pauses of %v between three attempts", elapsed, fastDialPolicy.pause)
		}
	})
}

// A failure no retry can cure is returned from the first attempt: an
// operator with a malformed URI must not wait out the budget to learn it.
func TestWaitForBrokerFailsFastOnWhatNoRetryCures(t *testing.T) {
	malformed := malformedURLError(t)
	synctest.Test(t, func(t *testing.T) {
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
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("a fail-fast refusal spent %v of a %v budget, want none", elapsed, fastDialPolicy.budget)
		}
	})
}

// Without a caller deadline the policy budget bounds the wait, and the error
// names the attempts so a broker that never answers is not mistaken for a
// hang.
func TestWaitForBrokerIsBoundedByTheBudgetWhenTheContextHasNoDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		if elapsed != fastDialPolicy.budget {
			t.Fatalf("the wait ended after %v, want exactly the %v budget", elapsed, fastDialPolicy.budget)
		}
	})
}

// A caller's own deadline wins over the policy budget: a boot context that
// says when it gives up is honoured there, not 30 s later.
func TestWaitForBrokerHonoursTheCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callerBound = 60 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), callerBound)
		defer cancel()
		d := &stuckDialer{}
		pol := fastDialPolicy
		pol.budget = 10 * time.Second
		start := time.Now()
		_, err := waitForBroker(ctx, d.dial, pol)
		if err == nil {
			t.Fatal("expected an error at the caller deadline")
		}
		if elapsed := time.Since(start); elapsed != callerBound {
			t.Fatalf("the wait ended after %v, want the %v caller deadline", elapsed, callerBound)
		}
		if !strings.Contains(err.Error(), "deadline ") {
			t.Fatalf("err = %v, want the caller deadline named", err)
		}
	})
}

// A caller cancelled BEFORE the call is owed no dial at all, and the error
// says so: zero attempts, the cancellation named.
func TestWaitForBrokerDialsNothingForACallerAlreadyDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// A caller cancelled DURING a dial is named by the exhausted error, which
// also carries the attempt that was in flight — a different branch from the
// one above, and it must not report "no bound".
func TestWaitForBrokerNamesACallerCancelledDuringADial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
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
	_, refused := natsgo.Connect(fixtureBrokerURL, natsgo.SetCustomDialer(refusingDialer{}))
	if !errors.Is(refused, natsgo.ErrNoServers) {
		t.Fatalf("natsgo answered the kernel's refusal with %v, want ErrNoServers — the measured boot failure changed shape", refused)
	}
	if !retryableDial(refused) {
		t.Fatalf("refused port: %v classified as final, want retryable", refused)
	}

	if malformed := malformedURLError(t); retryableDial(malformed) {
		t.Fatalf("malformed URI: %v classified as retryable, want final", malformed)
	}

	if rejected := badCredentialsError(t); retryableDial(rejected) {
		t.Fatalf("bad credentials: %v classified as retryable, want final", rejected)
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

// quietBrokerError is what natsgo returns against a broker that accepts the
// connection and never speaks the protocol — a container that bound its port
// before it could serve. The library's own handshake bound ends the wait,
// virtual inside the bubble. The error must be that timeout and nothing the
// refusal class already covers, or the classifier's timeout class would go
// unjudged.
func quietBrokerError(t *testing.T) error {
	t.Helper()
	var err error
	synctest.Test(t, func(t *testing.T) {
		client, broker := net.Pipe()
		defer func() {
			_ = client.Close()
			_ = broker.Close()
		}()
		_, err = natsgo.Connect(fixtureBrokerURL, natsgo.SetCustomDialer(pipeDialer{client}), natsgo.Timeout(300*time.Millisecond))
	})
	if err == nil {
		t.Fatal("natsgo completed a handshake with a broker that never spoke")
	}
	if errors.Is(err, natsgo.ErrNoServers) || !isTimeout(err) {
		t.Fatalf("a silent broker returned %v, want the handshake's timeout alone", err)
	}
	return err
}

// badCredentialsError is what natsgo returns when the broker refuses the
// CONNECT — credentials an operator must fix. The broker's half of the
// handshake is played in a bubble: INFO, then, once the client's CONNECT and
// PING arrive, the -ERR a nats-server answers an authorization violation
// with. No listed class covers that error, so retryableDial's default verdict
// is what judges it — the malformed URI cannot stand in: it is a net.Error,
// which the timeout class answers for.
func badCredentialsError(t *testing.T) error {
	t.Helper()
	var err error
	synctest.Test(t, func(t *testing.T) {
		client, broker := net.Pipe()
		defer func() {
			_ = client.Close()
			_ = broker.Close()
		}()
		go func() {
			if _, werr := io.WriteString(broker, "INFO {\"server_id\":\"fixture\"}\r\n"); werr != nil {
				return
			}
			r := bufio.NewReader(broker)
			for {
				line, rerr := r.ReadString('\n')
				if rerr != nil {
					return
				}
				if strings.HasPrefix(line, "PING") {
					break
				}
			}
			_, _ = io.WriteString(broker, "-ERR 'Authorization Violation'\r\n")
		}()
		_, err = natsgo.Connect(fixtureBrokerURL, natsgo.SetCustomDialer(pipeDialer{client}))
	})
	if !errors.Is(err, natsgo.ErrAuthorization) {
		t.Fatalf("a broker refusing the CONNECT returned %v, want natsgo's ErrAuthorization", err)
	}
	var netErr net.Error
	if errors.Is(err, natsgo.ErrNoServers) || errors.As(err, &netErr) {
		t.Fatalf("bad credentials returned %v, which a listed class judges — the default verdict would go unjudged", err)
	}
	return err
}

// Connect itself waits — the chokepoint both boot callers cross
// (cmd/iterion/server.go, cmd/iterion/runner.go), so neither has to know
// about the policy. The policy is replaced for the duration, the way
// mongo's store tests bound their own ping.
//
// The dial is real — natsgo's own net.Dialer against a closed loopback
// port — so the witness reads only what every outcome of that dial shares.
// The kernel's refusal (natsgo's ErrNoServers) and, on a host too loaded to
// finish a dial inside its attempt, a timeout are both answers the wait
// retries; the retry count, the exact durations and which answer came last
// are the virtual-time witnesses' to judge.
//
// What every outcome shares includes a floor on the wait: the loop words
// its error "no broker after" only once the budget's context is done, the
// caller here carries no deadline and nothing cancels it, and a context
// deadline never fires early. Load can only lengthen the wait, so the floor
// holds on any host — and it is the one assertion that sees the policy
// Connect hands the loop: a zero or truncated budget gives up before it
// with the same wording.
func TestConnectWaitsForABrokerThatIsNotUpYet(t *testing.T) {
	restore := defaultDialPolicy
	defaultDialPolicy = dialPolicy{attempt: 100 * time.Millisecond, pause: 10 * time.Millisecond, budget: 400 * time.Millisecond}
	t.Cleanup(func() { defaultDialPolicy = restore })

	url := "nats://" + closedBrokerPort(t)
	start := time.Now()
	conn, err := Connect(context.Background(), Config{URL: url})
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
	if !errors.Is(err, natsgo.ErrNoServers) && !isTimeout(err) {
		t.Fatalf("err = %v, want the broker's answer in the chain: natsgo's refusal or a dial timeout", err)
	}
	if elapsed < defaultDialPolicy.budget {
		t.Fatalf("Connect gave up after %v, before the %v budget: it did not wait (err = %v)", elapsed, defaultDialPolicy.budget, err)
	}
}

// The real dialer is usable outside the loop: with no deadline on the
// context it keeps natsgo's own dial bound instead of computing one, which
// is what keeps a zero-valued deadline from reaching natsgo as a negative
// timeout. natsgo is handed the kernel's refusal, so the refusal it reports
// is its verdict on that answer and no bound of the host's.
func TestDialBrokerWithoutADeadlineKeepsTheLibrarysOwnBound(t *testing.T) {
	_, err := dialBroker(fixtureBrokerURL, natsgo.SetCustomDialer(refusingDialer{}))(context.Background())
	if !errors.Is(err, natsgo.ErrNoServers) {
		t.Fatalf("err = %v, want the refusal natsgo reports with its default timeout", err)
	}
	if strings.Contains(fmt.Sprint(err), "timeout invalid") {
		t.Fatalf("err = %v — a missing deadline became a negative natsgo timeout", err)
	}
}

// The attempt context can expire between the loop's check and the dial (a
// slow CI runner under the race detector measured it, run 40829856051). A
// non-positive natsgo.Timeout is an option error — "nats: timeout invalid" —
// that is not retryable, so Connect would report the library error alone;
// the dialer answers as one that honours its context instead, and the loop
// keeps naming the bound.
func TestDialBrokerWithAnExpiredAttemptReportsTheDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	_, err := dialBroker("nats://127.0.0.1:1")(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded: a negative natsgo.Timeout reads like a configuration mistake", err)
	}
	if !retryableDial(err) {
		t.Fatalf("err = %v must stay retryable so the loop reports the bounded wait", err)
	}
}

// When the budget runs out while an attempt is in flight, the dialer answers
// context.DeadlineExceeded (see TestDialBrokerWithAnExpiredAttemptReportsTheDeadline);
// the error Connect returns must still carry the library's last refusal in its
// chain, or a caller reading natsgo.ErrNoServers through errors.Is loses the
// one fact that says "nothing is listening" (CI race-shard(1) on 6c7a08b8f).
// In the bubble the second attempt deterministically outlives the budget, so
// this is the in-flight exit, never the top-of-loop one.
func TestAnExpiredLastAttemptKeepsTheLibrarysRefusalInTheChain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pol := dialPolicy{attempt: 20 * time.Millisecond, pause: time.Millisecond, budget: 40 * time.Millisecond}
		calls := 0
		dial := func(ctx context.Context) (*natsgo.Conn, error) {
			calls++
			if calls == 1 {
				return nil, natsgo.ErrNoServers
			}
			time.Sleep(pol.budget) // outlive the budget, as a slow runner does
			return nil, context.DeadlineExceeded
		}
		_, err := waitForBroker(context.Background(), dial, pol)
		if err == nil {
			t.Fatal("waitForBroker returned no error against a dialer that never connects")
		}
		if !strings.Contains(err.Error(), "queue/nats: connect: no broker after 2 attempt(s)") {
			t.Fatalf("err = %v, want the bounded wait's wording after the in-flight second attempt", err)
		}
		if !errors.Is(err, natsgo.ErrNoServers) {
			t.Fatalf("err = %v, want the library's refusal kept in the chain", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the expired attempt kept in the chain too", err)
		}
	})
}

// The loop gives up with a refusal in hand at more than one exit, and the
// caller's errors.Is(err, natsgo.ErrNoServers) must not depend on which one
// the scheduler picked. The in-flight exit is the witness above's; each case
// here drives one other exit on purpose and pins that it is the one reached,
// so a fixture that drifts to another exit fails instead of going blind.
func TestWaitForBrokerKeepsTheRefusalAtEveryGiveUp(t *testing.T) {
	t.Run("the bound falls during the pause", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// The budget ends 10 ms into a 50 ms pause: the select's
			// ctx.Done case ends the wait, before any second dial.
			pol := dialPolicy{attempt: 20 * time.Millisecond, pause: 50 * time.Millisecond, budget: 40 * time.Millisecond}
			d := &scriptedDialer{errs: []error{natsgo.ErrNoServers, natsgo.ErrNoServers}}
			start := time.Now()
			_, err := waitForBroker(context.Background(), d.dial, pol)
			if elapsed := time.Since(start); d.calls != 1 || elapsed != pol.budget {
				t.Fatalf("calls = %d, ended after %v; want 1 dial and the end at the %v budget, inside the pause", d.calls, elapsed, pol.budget)
			}
			if err == nil || !strings.Contains(err.Error(), "no broker after 1 attempt(s)") {
				t.Fatalf("err = %v, want the bounded wait's wording after one attempt", err)
			}
			if !errors.Is(err, natsgo.ErrNoServers) {
				t.Fatalf("err = %v, want the library's refusal kept in the chain", err)
			}
		})
	})
	t.Run("the bound is seen at the top of the loop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pol := dialPolicy{attempt: 20 * time.Millisecond, pause: 40 * time.Millisecond, budget: time.Hour}
			ctx := newDeadlineSeenLate(pol.pause)
			d := &scriptedDialer{errs: []error{natsgo.ErrNoServers, natsgo.ErrNoServers}}
			start := time.Now()
			_, err := waitForBroker(ctx, d.dial, pol)
			// The pause ran to its end and no second dial followed: the
			// top-of-loop check is the only code between the two.
			if elapsed := time.Since(start); d.calls != 1 || elapsed != pol.pause {
				t.Fatalf("calls = %d, ended after %v; want 1 dial and the end after the %v pause", d.calls, elapsed, pol.pause)
			}
			if err == nil || !strings.Contains(err.Error(), "no broker after 1 attempt(s)") {
				t.Fatalf("err = %v, want the bounded wait's wording after one attempt", err)
			}
			if !errors.Is(err, natsgo.ErrNoServers) {
				t.Fatalf("err = %v, want the library's refusal kept in the chain", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want the bound that ended the wait in the chain too", err)
			}
		})
	})
}

// deadlineSeenLate is a caller deadline whose Done channel closes when Err
// is asked past the deadline, never on a timer of its own. With a stdlib
// deadline that falls where a pause ends, both timers fire at the same
// instant and the loop's select picks either; this context fixes the pick
// that leads to the top-of-loop exit — the pause ends, then the loop's next
// check finds the deadline passed. Err closes Done before it reports, so no
// reader sees Err set while Done is open.
type deadlineSeenLate struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newDeadlineSeenLate(d time.Duration) *deadlineSeenLate {
	return &deadlineSeenLate{Context: context.Background(), deadline: time.Now().Add(d), done: make(chan struct{})}
}

func (c *deadlineSeenLate) Deadline() (time.Time, bool) { return c.deadline, true }

func (c *deadlineSeenLate) Done() <-chan struct{} { return c.done }

func (c *deadlineSeenLate) Err() error {
	if time.Now().Before(c.deadline) {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.DeadlineExceeded
}

// gaveUp is what the top-of-loop exit hands the caller. The case above pins
// that site; this pins the helper's contract on its own, including the
// shapes that case does not reach: the bound and the refusal travel
// together, and a nil refusal adds nothing.
func TestGaveUpCarriesTheBoundAndTheRefusalTogether(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bound   error
		refusal error
		want    []error
		absent  string
	}{
		{
			name:  "deadline with a refusal in hand",
			bound: context.DeadlineExceeded, refusal: natsgo.ErrNoServers,
			want: []error{context.DeadlineExceeded, natsgo.ErrNoServers},
		},
		{
			name:  "cancellation with a refusal in hand",
			bound: context.Canceled, refusal: natsgo.ErrNoServers,
			want: []error{context.Canceled, natsgo.ErrNoServers},
		},
		{
			name:  "no dial was owed, so no refusal to carry",
			bound: context.Canceled, refusal: nil,
			want: []error{context.Canceled}, absent: "refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gaveUp(tc.bound, tc.refusal)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("err = %v, want %v in the chain", err, w)
				}
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("err = %q, want no %q with a nil refusal", err.Error(), tc.absent)
			}
		})
	}
}
