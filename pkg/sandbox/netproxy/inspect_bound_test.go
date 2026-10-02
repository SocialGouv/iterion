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
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

// A placeholder expands to its value, so a request under the inspection
// bound can stand for one far over it once substituted. The bound holds for
// what substitution makes too: a body whose placeholders would take it past
// the bound is refused (413) before the proxy builds it, and so are header
// values whose substitution would add more than the bound, all of them
// together; nothing reaches the upstream. A request that fits is substituted
// whole, and header bytes that carry no placeholder cost the bound nothing.
func TestSubstitutionStaysWithinTheInspectionBound(t *testing.T) {
	const bound = 4096
	value := strings.Repeat("v", 1000)
	g := secretguard.New([]secretguard.Secret{{Name: "tok", Value: value, Hosts: []string{"example.com"}}}, secretguard.DefaultConfig())
	ph := "__ITERION_SECRET_tok__"
	if out, _ := g.MaterializeForHostWithin(ph, "example.com", bound); out != value {
		t.Fatalf("scenario broken: the guard's placeholder is not %q", ph)
	}

	var mu sync.Mutex
	var gotBodies []string
	var gotHeaders []http.Header
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBodies = append(gotBodies, string(b))
		gotHeaders = append(gotHeaders, r.Header.Clone())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := New(Options{
		Policy:             pol,
		InspectCA:          ca,
		Rewriter:           g,
		InspectUpstreamTLS: &tls.Config{RootCAs: upstreamPool},
		MaxInspectedBody:   bound,
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) {
			return net.Dial(network, upstream.Listener.Addr().String())
		},
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
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(ca.CertPEM())
	proxyURL, _ := url.Parse("http://" + prx.Addr().String())
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientPool}}}
	send := func(body string, headers map[string]string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/upload", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	upstreamSaw := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(gotBodies)
	}

	// Ten placeholders: 220 bytes sent, 10 000 once substituted.
	if code := send(strings.Repeat(ph, 10), nil); code != http.StatusRequestEntityTooLarge || upstreamSaw() != 0 {
		t.Fatalf("a body whose substitution passes the bound: status %d, upstream saw %d request(s); want 413 and none", code, upstreamSaw())
	}
	// Five header values: 4 890 bytes added once substituted.
	many := map[string]string{"X-A": ph, "X-B": ph, "X-C": ph, "X-D": ph, "X-E": ph}
	if code := send("x", many); code != http.StatusRequestEntityTooLarge || upstreamSaw() != 0 {
		t.Fatalf("headers whose substitution adds more than the bound: status %d, upstream saw %d request(s); want 413 and none", code, upstreamSaw())
	}
	// Three placeholders in the body, three in headers, and a 5 000-byte
	// header with none: all within the bound.
	few := map[string]string{"X-A": ph, "X-B": ph, "X-C": ph, "Cookie": strings.Repeat("c", 5000)}
	if code := send(strings.Repeat(ph, 3), few); code != http.StatusOK {
		t.Fatalf("a request within the bound once substituted: status %d, want 200", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotBodies) != 1 || gotBodies[0] != strings.Repeat(value, 3) {
		t.Fatalf("the upstream got %d request(s), body %d bytes; want the body substituted whole", len(gotBodies), len(gotBodies[0]))
	}
	for _, k := range []string{"X-A", "X-B", "X-C"} {
		if got := gotHeaders[0].Get(k); got != value {
			t.Errorf("header %s reached the upstream as %d bytes, want the value", k, len(got))
		}
	}
}

// The inspected TLS path reads a request's line and headers within the bound
// net/http's own server applies (http.DefaultMaxHeaderBytes and its slack),
// as the plain-HTTP path does: a larger header is refused (431) before the
// proxy holds it and nothing reaches the upstream, while a header under it
// goes through. Without the bound, one request's headers hold unbounded
// memory, whatever ITERION_SANDBOX_INSPECT_MAX_BODY says.
func TestTheInspectedPathBoundsTheRequestHeaders(t *testing.T) {
	var mu sync.Mutex
	seen, lastBody := 0, 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen++
		lastBody = len(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	upstreamPool := x509.NewCertPool()
	upstreamPool.AddCert(upstream.Certificate())
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	prx, err := New(Options{
		Policy:             pol,
		InspectCA:          ca,
		Rewriter:           testRewriter{ph: "__ITERION_SECRET_tok__", real: "sk-REAL-abcdef0123456789", host: "example.com"},
		InspectUpstreamTLS: &tls.Config{RootCAs: upstreamPool},
		Dial: func(_ context.Context, network, _ string) (net.Conn, error) {
			return net.Dial(network, upstream.Listener.Addr().String())
		},
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
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(ca.CertPEM())
	proxyURL, _ := url.Parse("http://" + prx.Addr().String())
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: clientPool}}}
	sendWithBody := func(size, body int) (int, error) {
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/", strings.NewReader(strings.Repeat("b", body)))
		req.Header.Set("X-Big", strings.Repeat("h", size))
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	send := func(size int) (int, error) { return sendWithBody(size, 0) }
	upstreamSaw := func() int {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}

	// Over TCP, a 2 MiB header can break the client's own write before the
	// proxy's 431 reaches it, so only "nothing was forwarded" is observable
	// here. WHICH end refused is proven on a pipe, where a write blocks
	// instead of resetting: TestTheInspectedPathRefusesAnOversizedHeaderItself.
	code, err := send(2 << 20)
	if upstreamSaw() != 0 || (err == nil && code != http.StatusRequestHeaderFieldsTooLarge) {
		t.Fatalf("a 2 MiB header: status %d, err %v, upstream saw %d request(s); want 431 (or the connection cut) and none", code, err, upstreamSaw())
	}
	if code, err := send(512 << 10); err != nil || code != http.StatusOK || upstreamSaw() != 1 {
		t.Fatalf("a 512 KiB header: status %d, err %v, upstream saw %d request(s); want it forwarded", code, err, upstreamSaw())
	}
	// The header bound is the headers': a body far past it keeps its own
	// (the inspection bound) and arrives whole.
	const bodyBytes = 4 << 20
	if code, err := sendWithBody(0, bodyBytes); err != nil || code != http.StatusOK {
		t.Fatalf("a %d-byte body: status %d, err %v; want it forwarded", bodyBytes, code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if lastBody != bodyBytes {
		t.Errorf("the upstream got a %d-byte body, want %d: the header bound cut the body", lastBody, bodyBytes)
	}
}

// The PROXY is the end that refuses an oversized header, not the upstream:
// Go's own server refuses a 2 MiB header too, so an end-to-end reply does not
// say which end read it. Driven over a pipe, where a write blocks instead of
// resetting, the bound is reached deterministically: the proxy answers 431,
// reports the refusal, and opens no connection to any upstream.
func TestTheInspectedPathRefusesAnOversizedHeaderItself(t *testing.T) {
	ca, err := NewEphemeralCA()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := Compile(ModeOpen, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var blocked []string
	dialled := 0
	prx, err := New(Options{
		Policy:    pol,
		InspectCA: ca,
		Rewriter:  testRewriter{ph: "__ITERION_SECRET_tok__", real: "sk-REAL-abcdef0123456789", host: "example.com"},
		OnBlocked: func(_, reason string) {
			mu.Lock()
			defer mu.Unlock()
			blocked = append(blocked, reason)
		},
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			mu.Lock()
			dialled++
			mu.Unlock()
			return nil, errors.New("the proxy must not reach an upstream for a refused request")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clientSide, proxySide := net.Pipe()
	defer clientSide.Close()
	go prx.handleConnectInspect("example.com:443", proxySide,
		bufio.NewReadWriter(bufio.NewReader(proxySide), bufio.NewWriter(proxySide)))

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	tlsClient := tls.Client(clientSide, &tls.Config{RootCAs: pool, ServerName: "example.com"})
	defer tlsClient.Close()
	// The write blocks once the proxy stops reading at its bound, so it runs
	// on its own goroutine; the response is read here.
	go func() {
		_, _ = io.WriteString(tlsClient, "GET / HTTP/1.1\r\nHost: example.com\r\nX-Big: "+strings.Repeat("h", 2<<20)+"\r\n\r\n")
	}()
	_ = tlsClient.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(tlsClient), nil)
	if err != nil {
		t.Fatalf("reading the proxy's answer: %v", err)
	}
	defer resp.Body.Close()
	// Drained: the pipe is synchronous, so an unread body would leave the
	// proxy blocked mid-answer.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("the proxy answered %d, want 431", resp.StatusCode)
	}
	// The proxy reports the refusal after answering it, so the condition is
	// waited for: reading it straight after the response would pass or fail
	// on which goroutine ran first.
	reasons := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		mu.Lock()
		reasons = strings.Join(blocked, "; ")
		mu.Unlock()
		if reasons != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(reasons, "request header over the inspection bound") {
		t.Errorf("the proxy's refusals were %q, want the header bound named", reasons)
	}
	mu.Lock()
	defer mu.Unlock()
	if dialled != 0 {
		t.Errorf("the proxy dialled an upstream %d time(s) for a refused request", dialled)
	}
}
