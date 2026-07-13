package netproxy

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns a connected (server, client) TCP loopback pair.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("accept: %v", r.err)
	}
	return r.c, client
}

func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf)
}

// tunnel must pipe both directions and, when one side closes, reap BOTH
// copy goroutines and return promptly — not leak the second one.
func TestTunnelPipesBothDirectionsAndTearsDown(t *testing.T) {
	srvA, cliA := tcpPair(t)
	srvB, cliB := tcpPair(t)

	tunnelDone := make(chan struct{})
	go func() {
		tunnel(srvA, srvB)
		close(tunnelDone)
	}()

	// a -> b
	if _, err := cliA.Write([]byte("ping")); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if got := readN(t, cliB, 4); got != "ping" {
		t.Fatalf("a->b = %q; want ping", got)
	}
	// b -> a
	if _, err := cliB.Write([]byte("pong")); err != nil {
		t.Fatalf("write b: %v", err)
	}
	if got := readN(t, cliA, 4); got != "pong" {
		t.Fatalf("b->a = %q; want pong", got)
	}

	// Closing one side must tear the whole tunnel down promptly.
	_ = cliA.Close()
	select {
	case <-tunnelDone:
	case <-time.After(10 * time.Second):
		t.Fatal("tunnel did not tear down after one side closed (leaked copy goroutine)")
	}
}
