package compatgw

// The engine-owned sandbox proxy channel: a policy-isolated sandbox only
// lets egress through the run's network proxy, and the gateway client by
// design never trusts an ambient HTTPS_PROXY. The engine hands the endpoint
// under SandboxProxyEndpointEnv; the transport dials the VALIDATED gateway
// host through it — the proxy is a path, never an authority that bypasses
// the host guard.

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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

// The transport-level witness the unit tests cannot give: with the engine
// variable set, an HTTPS request through guardedTransport's client crosses
// a real CONNECT proxy — the production shape (the gateway is https) — and
// the proxy's own (private) address is dialed plainly, while the gateway
// host keeps the per-request validation (rva R727875 + R4cd6a7).
func TestGuardedTransport_RequestCrossesTheSandboxProxy(t *testing.T) {
	var connects, served int32
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&served, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		atomic.AddInt32(&connects, 1)
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		defer dst.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		go func() { _, _ = io.Copy(dst, buf) }()
		_, _ = io.Copy(conn, dst)
	}))
	defer proxy.Close()

	t.Setenv(SandboxProxyEndpointEnv, proxy.URL)
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	if tr.DialContext != nil {
		t.Fatal("a proxified transport kept the guarded DialContext — it validates the DIAL target and would refuse the proxy's own private address (rva R727875); the boundary is the per-request host check")
	}
	// The httptest gateway serves its own untrusted certificate — the
	// witness dials the TLS endpoint through the tunnel, it does not
	// vouch for the certificate.
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(gateway.URL + "/v1/models")
	if err != nil {
		t.Fatalf("request through the sandbox proxy: %v", err)
	}
	resp.Body.Close()
	if atomic.LoadInt32(&connects) != 1 {
		t.Fatalf("CONNECT count = %d, want 1 — the request did not cross the proxy", atomic.LoadInt32(&connects))
	}
	if atomic.LoadInt32(&served) != 1 {
		t.Fatalf("gateway served = %d, want 1 — the tunnel did not reach the gateway", atomic.LoadInt32(&served))
	}
}
