package netproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Proxy is the iterion HTTP / HTTPS CONNECT proxy. It enforces a
// [Policy] on the host portion of every CONNECT and plain-HTTP
// request, then tunnels accepted traffic transparently.
//
// The proxy never MITMs TLS — it only inspects the SNI / Host header.
// This preserves cert pinning in client SDKs and avoids the friction
// of injecting a CA into the sandbox.
//
// Proxy authentication is via Proxy-Authorization: Bearer <token>.
// Each run gets a fresh token (so a leaked token from one run cannot
// be replayed on another container against the same host port).
type Proxy struct {
	policy *Policy
	token  string

	// listener is bound on demand by [Proxy.Start].
	listener net.Listener
	server   *http.Server

	// hooks for observability (event emission)
	onBlocked func(host, reason string)

	// dialer used for upstream connections; tests can swap it.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	running bool
}

// Options configures a [Proxy].
type Options struct {
	// Policy is the compiled rule set the proxy enforces. Required.
	Policy *Policy

	// Token, if non-empty, requires every client to present it via
	// Proxy-Authorization: Bearer <token>. Empty disables auth (useful
	// for tests).
	Token string

	// OnBlocked, when non-nil, is called for every request the proxy
	// rejects. Engine integration uses this to emit the
	// `network_blocked` event into events.jsonl.
	OnBlocked func(host, reason string)

	// Dial overrides the upstream dialer. Tests use this to redirect
	// CONNECTs to a loopback echo server.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New constructs a Proxy. The proxy is not yet listening — call
// [Proxy.Start] when ready to accept clients.
func New(opts Options) (*Proxy, error) {
	if opts.Policy == nil {
		return nil, errors.New("netproxy: New: Policy is required")
	}
	dial := opts.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		dial = d.DialContext
	}
	return &Proxy{
		policy:    opts.Policy,
		token:     opts.Token,
		onBlocked: opts.OnBlocked,
		dial:      dial,
	}, nil
}

// NewToken generates a random opaque token suitable for [Options.Token].
// 32 bytes of entropy hex-encoded — collision-resistant under the
// number of concurrent runs iterion can plausibly host.
func NewToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// Endpoint returns the URL clients should set in HTTPS_PROXY/HTTP_PROXY.
// Includes the auth token when one is configured. Returns "" before
// [Proxy.Start] has bound a port.
func (p *Proxy) Endpoint(hostName string) string {
	if p.listener == nil {
		return ""
	}
	addr := p.listener.Addr().(*net.TCPAddr)
	host := hostName
	if host == "" {
		host = addr.IP.String()
	}
	if p.token != "" {
		return fmt.Sprintf("http://t:%s@%s:%d", p.token, host, addr.Port)
	}
	return fmt.Sprintf("http://%s:%d", host, addr.Port)
}

// Addr returns the bound network address (or nil before Start).
func (p *Proxy) Addr() net.Addr {
	if p.listener == nil {
		return nil
	}
	return p.listener.Addr()
}

// Start binds the proxy to the given address ("127.0.0.1:0" for an
// ephemeral port) and begins serving in a background goroutine. Stop
// gracefully via [Proxy.Shutdown].
func (p *Proxy) Start(addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return errors.New("netproxy: already running")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("netproxy: listen %s: %w", addr, err)
	}
	p.listener = ln
	p.server = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go p.server.Serve(ln)
	p.running = true
	return nil
}

// Shutdown stops the proxy server and closes the listener. Idempotent.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	p.running = false
	if p.server != nil {
		_ = p.server.Shutdown(ctx)
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	return nil
}

// handle routes incoming requests: CONNECT for HTTPS tunneling, any
// other method for plain-HTTP forwarding (we honour the Host: header
// to derive the target). Both paths apply policy and auth before
// touching the upstream.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if !p.checkAuth(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="iterion-sandbox"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	host := canonicalHost(r.Host)
	if host == "" && r.Method != http.MethodConnect {
		host = canonicalHost(r.URL.Host)
	}
	if host == "" {
		http.Error(w, "no host", http.StatusBadRequest)
		return
	}
	if !p.policy.Allow(host) {
		p.notifyBlocked(host)
		w.Header().Set("X-Iterion-Reason", "blocked by sandbox network policy")
		http.Error(w, "host '"+host+"' blocked by sandbox network policy", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// checkAuth validates the Proxy-Authorization header against the
// configured token. When no token is set, every request is accepted
// (test mode).
func (p *Proxy) checkAuth(r *http.Request) bool {
	if p.token == "" {
		return true
	}
	header := r.Header.Get("Proxy-Authorization")
	if header == "" {
		return false
	}
	// Accept both "Bearer <token>" and "Basic ..." (HTTPS_PROXY URL
	// embeds the token in the user:pass form, which Go translates to
	// Basic). For Basic, the token may appear as the password.
	switch {
	case strings.HasPrefix(header, "Bearer "):
		return strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) == p.token
	case strings.HasPrefix(header, "Basic "):
		// Decode "user:pass"
		raw := strings.TrimPrefix(header, "Basic ")
		decoded, err := decodeBasic(raw)
		if err != nil {
			return false
		}
		// Token is encoded as the password portion when the URL is
		// http://t:<token>@host:port — but some clients invert it.
		_, pass, ok := splitUserPass(decoded)
		if !ok {
			return false
		}
		return pass == p.token
	}
	return false
}

// notifyBlocked fires the OnBlocked hook safely.
func (p *Proxy) notifyBlocked(host string) {
	if p.onBlocked != nil {
		defer func() { _ = recover() }()
		p.onBlocked(host, "policy denial")
	}
}

// handleConnect tunnels TCP traffic between the client and the
// upstream after sending a 200 OK. We do not inspect the bytes —
// the SNI check happens implicitly via the host extracted from the
// CONNECT request line.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	upstream, err := p.dial(r.Context(), "tcp", host)
	if err != nil {
		http.Error(w, "upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Send the 200 response by hand because we hijacked.
	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := bufrw.Flush(); err != nil {
		return
	}

	// Drain any client bytes already buffered by the http server.
	if n := bufrw.Reader.Buffered(); n > 0 {
		buf := make([]byte, n)
		if _, err := io.ReadFull(bufrw.Reader, buf); err == nil {
			_, _ = upstream.Write(buf)
		}
	}

	tunnel(clientConn, upstream)
}

// handleForward proxies plain-HTTP requests by re-dialling the
// upstream and copying the request through. The response is streamed
// back unchanged. We strip hop-by-hop headers per RFC 7230.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	target := r.URL
	if target.Host == "" {
		target.Host = r.Host
	}
	if target.Scheme == "" {
		target.Scheme = "http"
	}

	hostport := target.Host
	if !strings.Contains(hostport, ":") {
		hostport += ":80"
	}

	upstream, err := p.dial(r.Context(), "tcp", hostport)
	if err != nil {
		http.Error(w, "upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Rewrite request to use the path-only form (origin form).
	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = ""
	outReq.URL.Host = ""
	outReq.RequestURI = ""
	stripHopByHop(outReq.Header)

	if err := outReq.Write(upstream); err != nil {
		http.Error(w, "write upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(upstream), outReq)
	if err != nil {
		http.Error(w, "read upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	stripHopByHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// tunnel pipes bytes bidirectionally until either side closes,
// then forces the peer connection shut so the second goroutine
// unblocks promptly.
//
// Without the explicit Close + second receive, the losing goroutine
// would stay parked in io.Copy until its peer's TCP FIN arrived,
// which can take seconds — long enough that Proxy.Shutdown returns
// while goroutines are still in flight. Closing both ends inside
// tunnel turns "either side closes" into a deterministic teardown.
func tunnel(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

var hopByHopHeaders = [...]string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

// decodeBasic decodes a "Basic " auth value to its "user:pass" form.
func decodeBasic(b64 string) (string, error) {
	dec, err := base64Decode(b64)
	if err != nil {
		return "", err
	}
	return string(dec), nil
}

// splitUserPass splits "user:pass". Returns ok=false on no colon.
func splitUserPass(s string) (user, pass string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
