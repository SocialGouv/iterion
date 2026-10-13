//go:build linux

package compatgw

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A connect nobody answers — a rescheduled runner pod's stale IP, a dropped
// SYN — fails the request on the dial bound, not on the request's own
// deadline, and on the same bound whichever road it takes: a generation's
// request carries no deadline, so without the bound the node waits out the
// kernel's SYN retries. The proxy road dials through a dialer of its own
// (the guarded one would refuse the proxy's private address), so the field
// parity test cannot compare it — the two roads are compared here, by
// behaviour, side by side.
func TestGuardedTransport_AnUnansweredConnectIsBoundedOnBothRoads(t *testing.T) {
	roads := []struct {
		name, endpoint, url, failure string
	}{
		{"direct", "", "https://" + silentListener(t) + "/v1/models", "dial tcp"},
		{"proxy", "http://" + silentListener(t), "https://93.184.216.34/v1/models", "proxyconnect"},
	}
	transports := make([]*http.Transport, len(roads))
	for i, road := range roads {
		t.Setenv(SandboxProxyEndpointEnv, road.endpoint)
		tr, err := guardedTransport(false)
		if err != nil {
			t.Fatalf("%s road: guardedTransport: %v", road.name, err)
		}
		transports[i] = tr
	}

	type outcome struct {
		err     error
		elapsed time.Duration
		expired bool
	}
	outcomes := make([]outcome, len(roads))
	var wg sync.WaitGroup
	for i, road := range roads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, road.url, nil)
			if err != nil {
				outcomes[i] = outcome{err: err}
				return
			}
			start := time.Now()
			resp, err := (&http.Client{Transport: transports[i]}).Do(req)
			if err == nil {
				resp.Body.Close()
			}
			outcomes[i] = outcome{err: err, elapsed: time.Since(start), expired: ctx.Err() != nil}
		}()
	}
	wg.Wait()

	for i, road := range roads {
		o := outcomes[i]
		switch {
		case o.err == nil:
			t.Errorf("%s road: a connect nobody answers answered the request", road.name)
		case o.expired:
			t.Errorf("%s road: the unanswered connect held the request to its own deadline (%v) — no connect timeout", road.name, o.err)
		case !strings.Contains(o.err.Error(), road.failure):
			t.Errorf("%s road: err = %v, want the dial's own failure (%q)", road.name, o.err, road.failure)
		}
	}
	if direct, proxy := outcomes[0].elapsed, outcomes[1].elapsed; proxy > direct+2*time.Second {
		t.Errorf("the proxy road gave up on the connect after %v, the direct road after %v — the proxy road's dial is not bounded like the direct road's", proxy, direct)
	}
}

// silentListener returns the address of a loopback listener whose accept
// queue is full: the kernel drops every further SYN, so a connect there
// waits exactly like one to an address nobody answers.
func silentListener(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	f := os.NewFile(uintptr(fd), "silent-listener")
	defer f.Close()
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Backlog 0: one completed connection fills the accept queue.
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln, err := net.FileListener(f)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addr := ln.Addr().String()
	filler, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("filling the accept queue: %v", err)
	}
	t.Cleanup(func() { _ = filler.Close() })
	probe, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err == nil {
		_ = probe.Close()
		t.Fatal("precondition: the full listener answered a connect — this kernel does not drop the SYN, the witness cannot hold")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("precondition: the probe connect failed with %v, want a timeout — a refused connect proves no silence", err)
	}
	return addr
}
