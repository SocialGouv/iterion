package compatgw

// The engine-owned sandbox proxy channel: a policy-isolated sandbox only
// lets egress through the run's network proxy, and the gateway client by
// design never trusts an ambient HTTPS_PROXY. The engine hands the endpoint
// under SandboxProxyEndpointEnv; a request crosses it once its gateway host
// has passed the guard as the runner resolves it, on the direct road's own
// transport bounds — the proxy is a path, never an authority that bypasses
// the host guard.

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

func TestSandboxProxyFunc_SetDialsTheValidatedHostThroughTheProxy(t *testing.T) {
	t.Setenv(SandboxProxyEndpointEnv, "http://tok@10.2.200.59:41625")
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("proxy = nil, want the engine-provided endpoint — direct dial would drop on the sandbox policy")
	}
	// A public IP literal: resolved without DNS, accepted non-strict.
	req, _ := http.NewRequest(http.MethodPost, "http://93.184.216.34/v1/chat/completions", nil)
	proxy, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("proxy resolution failed: %v", err)
	}
	if got := proxy.String(); got != "http://tok@10.2.200.59:41625" {
		t.Fatalf("proxy = %q, want the engine endpoint", got)
	}
}

func TestSandboxProxyFunc_TheHostGuardStillSpeaks(t *testing.T) {
	t.Setenv(SandboxProxyEndpointEnv, "http://tok@10.2.200.59:41625")
	tr, err := guardedTransport(true)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	// Strict mode + a loopback gateway host: the host guard refuses BEFORE
	// any byte reaches the proxy — the proxy is a path, not an authority.
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8080/v1", nil)
	if _, err := tr.Proxy(req); err == nil {
		t.Fatal("strict resolution of a loopback host = nil error, want the guard's refusal")
	}
}

func TestSandboxProxyFunc_UnsetIsDirect(t *testing.T) {
	t.Setenv(SandboxProxyEndpointEnv, "")
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("unset variable left a proxy func — the direct guarded dial stays the default")
	}
}

func TestSandboxProxyFunc_MalformedRefusesLoud(t *testing.T) {
	t.Setenv(SandboxProxyEndpointEnv, "not a url")
	_, err := guardedTransport(false)
	if err == nil {
		t.Fatal("malformed engine value = nil error, want a named refusal — a silent direct dial reproduces the park")
	}
	if !strings.Contains(err.Error(), SandboxProxyEndpointEnv) {
		t.Fatalf("refusal %q does not name the variable to fix", err)
	}
}

// connectProxy is a CONNECT proxy the test controls: it records every
// CONNECT target, answers 200, and hands the hijacked client connection to
// serve, which owns it until it returns.
type connectProxy struct {
	*httptest.Server
	mu      sync.Mutex
	targets []string
}

func newConnectProxy(t *testing.T, serve func(client net.Conn, buffered *bufio.Reader)) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.targets = append(p.targets, r.Host)
		p.mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		client, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		serve(client, buf.Reader)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *connectProxy) connectTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// tunnelTo serves every CONNECT with a tunnel to addr, whatever target the
// CONNECT named.
func tunnelTo(addr string) func(net.Conn, *bufio.Reader) {
	return func(client net.Conn, buffered *bufio.Reader) {
		dst, err := net.Dial("tcp", addr)
		if err != nil {
			return
		}
		defer dst.Close()
		go func() { _, _ = io.Copy(dst, buffered) }()
		_, _ = io.Copy(client, dst)
	}
}

// The transport-level witness: with the engine variable set, an HTTPS
// request crosses a real CONNECT proxy — the production shape (the gateway
// is https). Strict mode, a proxy on a private (loopback) address: the
// proxy's own address is dialed plainly, while the gateway host keeps its
// per-request validation (rva R727875 + R4cd6a7).
func TestGuardedTransport_RequestCrossesTheSandboxProxy(t *testing.T) {
	var served int32
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&served, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	proxy := newConnectProxy(t, tunnelTo(gateway.Listener.Addr().String()))

	t.Setenv(SandboxProxyEndpointEnv, proxy.URL)
	tr, err := guardedTransport(true)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	// The httptest gateway serves its own untrusted certificate — the
	// witness dials the TLS endpoint through the tunnel, it does not
	// vouch for the certificate.
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr}
	// A public IP literal passes the strict guard without DNS; the test's
	// proxy tunnels it to the local gateway.
	resp, err := client.Get("https://93.184.216.34/v1/models")
	if err != nil {
		t.Fatalf("request through the sandbox proxy: %v", err)
	}
	resp.Body.Close()
	if got := proxy.connectTargets(); len(got) != 1 || got[0] != "93.184.216.34:443" {
		t.Fatalf("CONNECT targets = %q, want [93.184.216.34:443] — the request did not cross the proxy", got)
	}
	if atomic.LoadInt32(&served) != 1 {
		t.Fatalf("gateway served = %d, want 1 — the tunnel did not reach the gateway", atomic.LoadInt32(&served))
	}
}

// The proxy road leaves the gateway's identity to the TLS handshake, the
// direct road's own check: a server that cannot present a certificate the
// runner trusts for the gateway's name — what a DNS answer pointing
// somewhere else reaches — is refused before any request is sent, on both
// roads, by the transport exactly as production builds it.
func TestGuardedTransport_AnUntrustedCertificateIsRefusedOnBothRoads(t *testing.T) {
	var served int32
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&served, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	proxy := newConnectProxy(t, tunnelTo(gateway.Listener.Addr().String()))

	roads := []struct{ name, endpoint, url string }{
		{"direct", "", gateway.URL + "/v1/models"},
		{"proxy", proxy.URL, "https://93.184.216.34/v1/models"},
	}
	for _, road := range roads {
		t.Setenv(SandboxProxyEndpointEnv, road.endpoint)
		tr, err := guardedTransport(false)
		if err != nil {
			t.Fatalf("%s road: guardedTransport: %v", road.name, err)
		}
		resp, err := (&http.Client{Transport: tr}).Get(road.url)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s road: a certificate the runner does not trust was accepted", road.name)
		}
		if !strings.Contains(err.Error(), "x509") {
			t.Fatalf("%s road: err = %v, want the certificate check's refusal", road.name, err)
		}
	}
	if got := proxy.connectTargets(); len(got) != 1 {
		t.Fatalf("CONNECT targets = %q, want one — the proxy road never reached the tunnel", got)
	}
	if n := atomic.LoadInt32(&served); n != 0 {
		t.Fatalf("the gateway served %d request(s) behind a certificate the runner refused", n)
	}
}

// The proxy road is the direct road with two fields of its own — which
// proxy a request crosses, and how the proxy's address is dialed (compared
// by behaviour in AnUnansweredConnectIsBoundedOnBothRoads) — so every
// setting the direct road keeps (TLS handshake, idle reap, HTTP/2, the
// response-header floor) holds on both, and the direct road must keep them
// for the parity to mean anything.
func TestGuardedTransport_TheProxyRoadKeepsTheDirectRoadsBounds(t *testing.T) {
	for _, strict := range []bool{true, false} {
		t.Setenv(SandboxProxyEndpointEnv, "")
		direct, err := guardedTransport(strict)
		if err != nil {
			t.Fatalf("strict=%v: direct road: %v", strict, err)
		}
		if direct.TLSHandshakeTimeout <= 0 || direct.IdleConnTimeout <= 0 || direct.ResponseHeaderTimeout <= 0 || !direct.ForceAttemptHTTP2 {
			t.Errorf("strict=%v: the direct road lost a setting (TLS handshake %v, idle %v, response header %v, HTTP/2 %v) — a parity with it proves nothing",
				strict, direct.TLSHandshakeTimeout, direct.IdleConnTimeout, direct.ResponseHeaderTimeout, direct.ForceAttemptHTTP2)
		}
		t.Setenv(SandboxProxyEndpointEnv, "http://tok@10.2.200.59:41625")
		proxied, err := guardedTransport(strict)
		if err != nil {
			t.Fatalf("strict=%v: proxy road: %v", strict, err)
		}
		if proxied.DialContext == nil {
			t.Errorf("strict=%v: the proxy road dials the proxy with no dialer of its own — no connect timeout", strict)
		}
		dv, pv := reflect.ValueOf(direct).Elem(), reflect.ValueOf(proxied).Elem()
		for i := range dv.NumField() {
			field := dv.Type().Field(i)
			if !field.IsExported() || field.Name == "Proxy" || field.Name == "DialContext" {
				continue
			}
			d, p := dv.Field(i), pv.Field(i)
			if d.Kind() == reflect.Func {
				if d.IsNil() != p.IsNil() {
					t.Errorf("strict=%v: %s is set on one road only", strict, field.Name)
				}
				continue
			}
			if !reflect.DeepEqual(d.Interface(), p.Interface()) {
				t.Errorf("strict=%v: %s = %v on the proxy road, %v on the direct road", strict, field.Name, p.Interface(), d.Interface())
			}
		}
	}
}

// A proxy that accepts the CONNECT and then stalls the tunnel — the
// gateway's side of the handshake never comes — fails the request on the
// TLS handshake bound, not on the request's own deadline: a generation's
// request carries none, so without the bound the node hangs.
func TestGuardedTransport_AStalledTunnelIsBoundedByTheHandshakeTimeout(t *testing.T) {
	release := make(chan struct{})
	proxy := newConnectProxy(t, func(net.Conn, *bufio.Reader) { <-release })
	t.Cleanup(func() { close(release) })

	t.Setenv(SandboxProxyEndpointEnv, proxy.URL)
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://93.184.216.34/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a stalled tunnel answered the request")
	}
	if ctx.Err() != nil {
		t.Fatalf("the stalled tunnel held the request to its own deadline (%v) — the proxy road has no TLS handshake bound", err)
	}
	if !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("err = %v, want the TLS handshake bound", err)
	}
}

// The engine's proxy matches its policy on the gateway's NAME — an
// allowlist refuses an IP literal no rule names — so the request hands the
// proxy the hostname, and the proxy resolves it again itself: the address
// the runner validated cannot ride the CONNECT, the proxy pins the name
// instead (sandboxProxyFunc). Through the real proxy, its allowlist naming
// the gateway.
func TestGuardedTransport_TheEngineProxyIsHandedTheGatewayName(t *testing.T) {
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()
	_, port, err := net.SplitHostPort(gateway.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := netproxy.Compile(netproxy.ModeAllowlist, []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := netproxy.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var dialed []string
	prx, err := netproxy.New(netproxy.Options{
		Policy: policy,
		Token:  token,
		// Stands in for the proxy's own dialer, which refuses a loopback
		// destination: it records what the proxy was asked to reach, then
		// reaches the test's gateway.
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, addr)
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, gateway.Listener.Addr().String())
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prx.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prx.Shutdown(context.Background()) })

	t.Setenv(SandboxProxyEndpointEnv, prx.Endpoint("127.0.0.1"))
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr}

	resp, err := client.Get("https://localhost:" + port + "/v1/models")
	if err != nil {
		t.Fatalf("the gateway's name through the allowlist: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 through the allowlist", resp.StatusCode)
	}
	mu.Lock()
	got := append([]string(nil), dialed...)
	mu.Unlock()
	if want := "localhost:" + port; len(got) != 1 || got[0] != want {
		t.Fatalf("the proxy was asked to reach %q, want [%s] — the name, which the proxy resolves itself", got, want)
	}

	// The address the name resolves to is refused by the same allowlist:
	// a CONNECT pinned to the runner's validated address never reaches the
	// gateway.
	resp, err = client.Get("https://127.0.0.1:" + port + "/v1/models")
	if err == nil {
		resp.Body.Close()
		t.Fatal("the allowlist admitted the gateway's address literal — a CONNECT pinned by the runner would cross: revisit sandboxProxyFunc's note on who pins")
	}
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("the address literal failed with %v, want the allowlist's 403", err)
	}
}
