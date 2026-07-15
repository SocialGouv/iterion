package runner

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

// startEchoListener starts a loopback TCP listener standing in for an
// internal service an SSRF probe would target. It echoes one line back on
// the first accepted connection.
func startEchoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte(line))
	}()
	return ln.Addr().String()
}

// dialProxy opens a raw client connection to the clone-guard proxy endpoint
// and returns it with the parsed auth token.
func dialProxy(t *testing.T, endpoint string) (net.Conn, string) {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint %q: %v", endpoint, err)
	}
	token, _ := u.User.Password()
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", u.Host, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, token
}

// connectStatus issues a CONNECT for target through the proxy connection and
// returns the HTTP status line. withAuth controls the Proxy-Authorization
// header (Basic, as git/libcurl derives it from the proxy-URL userinfo).
func connectStatus(t *testing.T, conn net.Conn, target, token string, withAuth bool) (string, *bufio.Reader) {
	t.Helper()
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if withAuth {
		cred := base64.StdEncoding.EncodeToString([]byte("t:" + token))
		req += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	return strings.TrimSpace(status), r
}

// The rebinding scenario: the CONNECT host passes the hostname policy (it IS
// the repo host) but resolves to a non-public address. Strict mode must refuse
// at dial time — this is the connect-time layer the /etc/hosts pin cannot
// provide on non-root pods.
func TestCloneGuardProxyStrictRefusesNonPublicDial(t *testing.T) {
	echoAddr := startEchoListener(t)
	endpoint, shutdown, err := startCloneGuardProxy("127.0.0.1", true)
	if err != nil {
		t.Fatalf("startCloneGuardProxy: %v", err)
	}
	defer shutdown()

	conn, token := dialProxy(t, endpoint)
	status, _ := connectStatus(t, conn, echoAddr, token, true)
	if !strings.Contains(status, "502") {
		t.Fatalf("CONNECT %s status = %q; want 502 (dial refused: non-public IP)", echoAddr, status)
	}
}

// Off-host CONNECTs are refused by the single-host allowlist regardless of
// where they resolve — the redirect / alternate-URL vector.
func TestCloneGuardProxyRefusesOffHostConnect(t *testing.T) {
	endpoint, shutdown, err := startCloneGuardProxy("repo.example.com", true)
	if err != nil {
		t.Fatalf("startCloneGuardProxy: %v", err)
	}
	defer shutdown()

	conn, token := dialProxy(t, endpoint)
	status, _ := connectStatus(t, conn, "attacker.example.org:443", token, true)
	if !strings.Contains(status, "403") {
		t.Fatalf("off-host CONNECT status = %q; want 403 (policy)", status)
	}
}

// With the on-prem escape hatch (allowPrivate → strict=false) an internal
// forge stays reachable and bytes tunnel through.
func TestCloneGuardProxyAllowPrivateTunnels(t *testing.T) {
	echoAddr := startEchoListener(t)
	endpoint, shutdown, err := startCloneGuardProxy("127.0.0.1", false)
	if err != nil {
		t.Fatalf("startCloneGuardProxy: %v", err)
	}
	defer shutdown()

	conn, token := dialProxy(t, endpoint)
	status, r := connectStatus(t, conn, echoAddr, token, true)
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT %s status = %q; want 200", echoAddr, status)
	}
	// Drain the response headers (a lone CRLF after the status line).
	if line, err := r.ReadString('\n'); err != nil || strings.TrimSpace(line) != "" {
		t.Fatalf("expected empty line after 200, got %q err=%v", line, err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	echoed, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(echoed) != "ping" {
		t.Fatalf("tunnel echo = %q; want ping", echoed)
	}
}

// The per-clone token gates the proxy: a client without it gets 407, so
// another process on the pod cannot ride the guard's allowlist.
func TestCloneGuardProxyRequiresAuth(t *testing.T) {
	endpoint, shutdown, err := startCloneGuardProxy("repo.example.com", true)
	if err != nil {
		t.Fatalf("startCloneGuardProxy: %v", err)
	}
	defer shutdown()

	conn, _ := dialProxy(t, endpoint)
	status, _ := connectStatus(t, conn, "repo.example.com:443", "", false)
	if !strings.Contains(status, "407") {
		t.Fatalf("unauthenticated CONNECT status = %q; want 407", status)
	}
}

func TestCloneGuardEnv(t *testing.T) {
	env := cloneGuardEnv("http://t:tok@127.0.0.1:9")
	want := map[string]bool{
		"HTTPS_PROXY=http://t:tok@127.0.0.1:9": false,
		"https_proxy=http://t:tok@127.0.0.1:9": false,
		"HTTP_PROXY=http://t:tok@127.0.0.1:9":  false,
		"http_proxy=http://t:tok@127.0.0.1:9":  false,
		"NO_PROXY=":                            false,
		"no_proxy=":                            false,
	}
	for _, e := range env {
		if _, ok := want[e]; !ok {
			t.Fatalf("unexpected env entry %q", e)
		}
		want[e] = true
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("missing env entry %q", k)
		}
	}
}
