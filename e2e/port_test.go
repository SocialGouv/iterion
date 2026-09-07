package e2e

import (
	"net"
	"sync"
	"testing"
)

// reserveLoopbackPort returns a loopback port number nothing in this process
// has handed out before, for the subprocesses and daemons that must be TOLD
// their port instead of binding `:0` themselves.
//
// Reserve-then-release is a TOCTOU window by construction; what makes it a
// real collision rather than a theoretical one is a parallel suite, where two
// tests can be inside that window at the same time and the kernel is free to
// re-offer the port it just took back (observed: "dispatcher http server:
// listen tcp 127.0.0.1:33613: bind: address already in use"). The claimed set
// closes the in-process half of the race, which is the half this package
// created; a collision with an unrelated process on the host is the same risk
// it always had.
func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	const attempts = 20
	for i := 0; i < attempts; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		if err := l.Close(); err != nil {
			t.Fatalf("release port: %v", err)
		}
		if claimLoopbackPort(port) {
			return port
		}
	}
	t.Fatalf("the kernel re-offered an already-claimed loopback port %d times running", attempts)
	return 0
}

// reserveBusyLoopbackPort returns a listener still HOLDING its loopback port,
// for the rows that need a port to be OCCUPIED rather than free.
//
// It goes through the same claimed set, which is what makes the guarantee
// two-sided. A blocker binding `127.0.0.1:0` on its own can be handed the very
// port a sibling reserved and released moments earlier but whose daemon has
// not bound yet; the blocker then holds it for its whole run and the sibling
// dies with the exact "bind: address already in use" this helper exists to
// prevent — attributed, as ever, to the wrong test. Claiming after the bind
// and backing off on a lost claim leaves the port with whoever asked first.
//
// The listener is the caller's to close.
func reserveBusyLoopbackPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	const attempts = 20
	for i := 0; i < attempts; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("occupy port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		if claimLoopbackPort(port) {
			return l, port
		}
		if err := l.Close(); err != nil {
			t.Fatalf("release port: %v", err)
		}
	}
	t.Fatalf("the kernel re-offered an already-claimed loopback port %d times running", attempts)
	return nil, 0
}

var (
	claimedPortsMu sync.Mutex
	claimedPorts   = map[int]bool{}
)

// claimLoopbackPort records a port as taken by this process, reporting
// whether the claim is the first. Deliberately never released: a port handed
// to a daemon stays in use for that daemon's whole life, and the ephemeral
// range is far larger than any test run's appetite.
func claimLoopbackPort(port int) bool {
	claimedPortsMu.Lock()
	defer claimedPortsMu.Unlock()
	if claimedPorts[port] {
		return false
	}
	claimedPorts[port] = true
	return true
}
