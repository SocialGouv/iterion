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

var (
	claimedPortsMu sync.Mutex
	claimedPorts   = map[int]bool{}
)

// claimLoopbackPort records a port as taken by this process, reporting
// whether the claim is the first. reserveLoopbackPort never releases what it
// claims: a port handed to a daemon stays in use for that daemon's whole life,
// and the ephemeral range is far larger than any test run's appetite.
func claimLoopbackPort(port int) bool {
	claimedPortsMu.Lock()
	defer claimedPortsMu.Unlock()
	if claimedPorts[port] {
		return false
	}
	claimedPorts[port] = true
	return true
}

// releaseLoopbackPort drops one claim. Only for a claim its caller made and
// never handed on — a probe of the claim set itself; a port given to a daemon
// is never released.
func releaseLoopbackPort(port int) {
	claimedPortsMu.Lock()
	defer claimedPortsMu.Unlock()
	delete(claimedPorts, port)
}
