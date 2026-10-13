package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The egress allowlist governs DESTINATIONS — and the proxy host's own
// services are not one of them: the proxy's dialer refuses a connect to its
// loopback, the unspecified address (which lands on the loopback) or a
// link-local address, judged on the address it is about to connect to.
func TestTheDialControlRefusesTheProxyHostsOwnAddresses(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1:80", "127.1.2.3:80", "[::1]:80",
		"0.0.0.0:80", "[::]:80", "[::ffff:127.0.0.1]:80", "[::ffff:0.0.0.0]:80",
		"169.254.169.254:80", "[fe80::1]:80", "[fe80::1%eth0]:80",
	} {
		if err := refuseLocalDestination("tcp", addr, nil); !errors.Is(err, errLocalDestination) {
			t.Errorf("%s: err = %v, want the local-destination refusal", addr, err)
		}
	}
	for _, addr := range []string{"8.8.8.8:443", "10.0.0.1:443", "192.168.1.10:80", "[fd00::1]:443", "[2606:4700::1111]:443"} {
		if err := refuseLocalDestination("tcp", addr, nil); err != nil {
			t.Errorf("%s must NOT be refused: %v", addr, err)
		}
	}
	// An address the hook cannot read is refused, never waved through.
	if err := refuseLocalDestination("tcp", "localhost:80", nil); err == nil {
		t.Error("a connect address that is no IP was let through")
	}
}

// localService stands in for a service on the proxy host's loopback: it
// counts the connections that reach it and answers each with a 200.
func localService(t *testing.T) (port string, hits *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	hits = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_, _ = http.ReadRequest(bufio.NewReader(c))
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nlocal")
			}(c)
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, hits
}

// openProxy starts a proxy on its DEFAULT dialer, policy open, inspecting
// TLS when ca is set.
func openProxy(t *testing.T, ca *EphemeralCA) *Proxy {
	t.Helper()
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Policy: pol}
	if ca != nil {
		opts.InspectCA = ca
	}
	prx, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = prx.Shutdown(ctx)
	})
	return prx
}

// viaProxy sends one request to rawURL through prx, trusting pool for an
// inspected https URL, and returns the status and body.
func viaProxy(t *testing.T, prx *Proxy, rawURL string, pool *x509.CertPool) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse("http://" + prx.Addr().String())
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// connectVia opens a CONNECT tunnel to target through prx and returns the
// proxy's answer — status and body; once the tunnel is up, the answer of
// the service at its end to a request sent through it.
func connectVia(t *testing.T, prx *Proxy, target string) string {
	t.Helper()
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("CONNECT %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.Status + " " + string(body)
	}
	// The tunnel is up: what follows the status is the tunnel, not a body.
	if _, err := io.WriteString(conn, "GET /x HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	inner, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("through the tunnel to %s: %v", target, err)
	}
	body, _ := io.ReadAll(inner.Body)
	_ = inner.Body.Close()
	return resp.Status + " | " + inner.Status + " " + string(body)
}

// End to end on the proxy's default dialer, on each way out — a plain-HTTP
// forward, a CONNECT tunnel, an inspected CONNECT: a destination on the proxy
// host's loopback is refused before any connection reaches the service
// there, whatever spelling names it — a loopback literal, the unspecified
// address, a resolvable loopback name.
func TestALocalDestinationIsRefusedOnEveryWayOut(t *testing.T) {
	if addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost."); err != nil || len(addrs) == 0 {
		t.Fatalf("scenario broken: localhost. does not resolve here (%v)", err)
	}
	port, hits := localService(t)
	plain := openProxy(t, nil)
	for _, host := range []string{"127.0.0.1", "0.0.0.0", "localhost."} {
		target := net.JoinHostPort(host, port)
		if status, body := viaProxy(t, plain, "http://"+target+"/x", nil); status != http.StatusBadGateway || !strings.Contains(body, errLocalDestination.Error()) {
			t.Errorf("forward to %s: status %d, body %q; want 502 naming the refusal", target, status, body)
		}
		if answer := connectVia(t, plain, target); !strings.HasPrefix(answer, "502") || !strings.Contains(answer, errLocalDestination.Error()) {
			t.Errorf("CONNECT to %s: %q; want 502 naming the refusal", target, answer)
		}
	}
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	inspecting := openProxy(t, ca)
	target := net.JoinHostPort("localhost", port)
	if status, body := viaProxy(t, inspecting, "https://"+target+"/x", pool); status != http.StatusBadGateway || !strings.Contains(body, errLocalDestination.Error()) {
		t.Errorf("inspected CONNECT to %s: status %d, body %q; want 502 naming the refusal", target, status, body)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the proxy host's local service took %d connection(s); want none", n)
	}
}

// The sanctioned container alias for the host, host.docker.internal, still
// reaches the host's loopback through the proxy (the per-run MCP listeners):
// the refusal holds for every other way there.
func TestTheHostAliasReachesTheHostLoopback(t *testing.T) {
	port, hits := localService(t)
	prx := openProxy(t, nil)
	target := net.JoinHostPort("host.docker.internal", port)
	if status, body := viaProxy(t, prx, "http://"+target+"/x", nil); status != http.StatusOK || body != "local" {
		t.Errorf("forward to %s: status %d, body %q; want the host service's 200", target, status, body)
	}
	if answer := connectVia(t, prx, target); !strings.HasPrefix(answer, "200") || !strings.HasSuffix(answer, "| 200 OK local") {
		t.Errorf("CONNECT to %s: %q; want a tunnel to the host service", target, answer)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("the host service took %d connection(s); want 2", n)
	}
}
