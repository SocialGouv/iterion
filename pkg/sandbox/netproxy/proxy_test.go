package netproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// startEchoServer launches a simple TCP echo server on 127.0.0.1:0
// and returns its address + a cleanup func.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func newProxy(t *testing.T, mode Mode, rules []string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *Proxy {
	t.Helper()
	p, err := Compile(mode, rules)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := New(Options{
		Policy: p,
		Dial:   dial,
	})
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

func TestProxyConnectAllowedTunnels(t *testing.T) {
	echoAddr, stopEcho := startEchoServer(t)
	defer stopEcho()

	// dial redirects every CONNECT to our echo server regardless of host.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial(network, echoAddr)
	}
	prx := newProxy(t, ModeAllowlist, []string{"allowed.example.com"}, dial)

	// Open a CONNECT to allowed.example.com:443
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT allowed.example.com:443 HTTP/1.1\r\nHost: allowed.example.com:443\r\n\r\n"))
	buf := make([]byte, 256)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("expected 200 Connection Established, got %q", string(buf[:n]))
	}
	// Now the connection is a raw tunnel — write+read echo.
	_, _ = conn.Write([]byte("hello"))
	got := make([]byte, 5)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("echo got %q, want hello", got)
	}
}

func TestProxyConnectBlockedReturns403(t *testing.T) {
	called := 0
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		called++
		t.Errorf("dial should not be called for a blocked host (was: %q)", addr)
		return nil, net.ErrClosed
	}
	blockedHost := "evil.site"
	blockedFired := make(chan struct{}, 1)

	p, err := Compile(ModeAllowlist, []string{"good.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	prx, err := New(Options{
		Policy: p,
		Dial:   dial,
		OnBlocked: func(host, _ string) {
			if host == blockedHost {
				blockedFired <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = prx.Shutdown(context.Background())
	})

	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT evil.site:443 HTTP/1.1\r\nHost: evil.site:443\r\n\r\n"))
	buf := make([]byte, 512)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Errorf("expected 403 for blocked host, got %q", string(buf[:n]))
	}
	if called != 0 {
		t.Errorf("dial called %d times for a blocked host", called)
	}
	select {
	case <-blockedFired:
	case <-time.After(time.Second):
		t.Error("OnBlocked was not invoked for the denied host")
	}
}

func TestProxyAuthRequiredWhenTokenSet(t *testing.T) {
	p, _ := Compile(ModeAllowlist, []string{"good.example.com"})
	prx, _ := New(Options{Policy: p, Token: "secret123"})
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prx.Shutdown(context.Background()) })

	// Dial without auth.
	conn, _ := net.Dial("tcp", prx.Addr().String())
	defer conn.Close()
	_, _ = conn.Write([]byte("CONNECT good.example.com:443 HTTP/1.1\r\nHost: good.example.com:443\r\n\r\n"))
	buf := make([]byte, 256)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "407") {
		t.Errorf("expected 407 Proxy Authentication Required, got %q", string(buf[:n]))
	}
}

func TestProxyEndpointURL(t *testing.T) {
	p, _ := Compile(ModeAllowlist, nil)
	prx, _ := New(Options{Policy: p, Token: "tok"})
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prx.Shutdown(context.Background()) })

	got := prx.Endpoint("")
	if got == "" {
		t.Fatal("Endpoint returned empty after Start")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("Endpoint returned invalid URL %q: %v", got, err)
	}
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http", u.Scheme)
	}
	if pass, _ := u.User.Password(); pass != "tok" {
		t.Errorf("token in URL userinfo = %q, want tok", pass)
	}
}

func TestProxyShutdownIdempotent(t *testing.T) {
	p, _ := Compile(ModeAllowlist, nil)
	prx, _ := New(Options{Policy: p})
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := prx.Shutdown(context.Background()); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	if err := prx.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestNewTokenIsRandomAndStable(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("NewToken returned the same token twice: %q", a)
	}
	if len(a) != 64 { // 32 bytes hex-encoded
		t.Errorf("token length = %d, want 64", len(a))
	}
}

// fake an http.HandlerFunc test for the forward path — confirms plain
// HTTP works too.
func TestProxyForwardAllowedHTTP(t *testing.T) {
	upstream := http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go upstream.Serve(&singleHandlerListener{ln: ln, body: "hello-from-upstream"})

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial(network, ln.Addr().String())
	}
	prx := newProxy(t, ModeAllowlist, []string{"allowed.test"}, dial)

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: prx.Addr().String()}),
		},
		Timeout: 3 * time.Second,
	}
	resp, err := client.Get("http://allowed.test/x")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello-from-upstream") {
		t.Errorf("body = %q, want substring hello-from-upstream", body)
	}
}

// singleHandlerListener wraps a net.Listener and serves a fixed body
// for any HTTP request — a tiny test fixture to avoid pulling in
// httptest.NewServer (which makes its own listener).
type singleHandlerListener struct {
	ln   net.Listener
	body string
}

func (s *singleHandlerListener) Accept() (net.Conn, error) {
	c, err := s.ln.Accept()
	if err != nil {
		return nil, err
	}
	go func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		body := s.body
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " + itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body))
	}(c)
	// Avoid having http.Server serve actual handlers — return a closed
	// conn-like sentinel so http.Server backs off cleanly.
	return &deadConn{}, nil
}

func (s *singleHandlerListener) Close() error   { return s.ln.Close() }
func (s *singleHandlerListener) Addr() net.Addr { return s.ln.Addr() }

type deadConn struct{}

func (deadConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (deadConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (deadConn) Close() error                     { return nil }
func (deadConn) LocalAddr() net.Addr              { return nil }
func (deadConn) RemoteAddr() net.Addr             { return nil }
func (deadConn) SetDeadline(time.Time) error      { return nil }
func (deadConn) SetReadDeadline(time.Time) error  { return nil }
func (deadConn) SetWriteDeadline(time.Time) error { return nil }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
