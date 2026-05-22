package netproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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

	// forwardTransport pools idle connections for plain-HTTP forwards.
	// Built once in [New] over the same dialer so policy enforcement
	// at the dial layer still applies. CONNECT tunnels keep their own
	// fresh dial per request — keep-alive pooling is meaningless once
	// the bytes are opaque TLS.
	forwardTransport *http.Transport

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
	p := &Proxy{
		policy:    opts.Policy,
		token:     opts.Token,
		onBlocked: opts.OnBlocked,
		dial:      dial,
	}
	p.forwardTransport = &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// We set DisableCompression=true so the upstream's
		// Content-Encoding (if any) reaches the client unchanged —
		// the proxy is byte-transparent for non-CONNECT traffic.
		DisableCompression: true,
	}
	return p, nil
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
	go func() {
		// Serve always returns a non-nil error when it stops. The
		// ordinary shutdown path produces http.ErrServerClosed; any
		// other return is something we want to know about so the
		// proxy doesn't quietly stop accepting connections while the
		// rest of the run keeps trying to use it.
		if err := p.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "netproxy: serve stopped with error: %v\n", err)
		}
	}()
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
	if p.forwardTransport != nil {
		p.forwardTransport.CloseIdleConnections()
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
		presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		return constantTimeEqual(presented, p.token)
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
		return constantTimeEqual(pass, p.token)
	}
	return false
}

// constantTimeEqual compares two strings in time independent of the
// common prefix length, defending against timing side-channels on the
// proxy token. The threat is narrow (only containers that already
// received the token can connect), but the discipline is cheap.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// defaultSilentDenyHosts is the seed list of hostnames the proxy
// still denies but never emits a `network_blocked` event for. Each
// entry is matched as either an exact host or any subdomain (i.e. a
// host like `http-intake.logs.datadoghq.com` covers itself AND
// `*.http-intake.logs.datadoghq.com`).
//
// Seeded with the Datadog telemetry endpoints claude-code ships its
// diagnostics to. The CONNECT proxy correctly denies them (Datadog
// isn't in any default allowlist), but in a 14-minute live run those
// denials drove 68 `network_blocked` events — drowning out actual
// security signal. The deny still happens; only the noisy event is
// suppressed.
//
// Extending this list is a deliberate, code-level decision so it
// stays auditable. If a new telemetry host crops up, add it here and
// land it in a regular commit.
var defaultSilentDenyHosts = []string{
	// claude-code → Datadog SaaS regions
	"http-intake.logs.us5.datadoghq.com",
	"http-intake.logs.us3.datadoghq.com",
	"http-intake.logs.us.datadoghq.com",
	"http-intake.logs.eu.datadoghq.com",
	"http-intake.logs.datadoghq.com",
	"api.datadoghq.com",
	// claude-code → GitHub Copilot CDN (bug-report telemetry path)
	"api.githubcopilot.com",
	// claude-code's MCP proxy probe at startup (3× per run when no
	// remote MCP server is configured — noise on every allowlist
	// workflow that doesn't opt in to Anthropic's MCP feature)
	"mcp-proxy.anthropic.com",
	// Sentry SDKs ship to ingest.<region>.sentry.io / *.ingest.sentry.io
	"ingest.sentry.io",
	"ingest.de.sentry.io",
	"ingest.us.sentry.io",
}

// isSilentDenyHost reports whether the given (canonicalised) host is
// covered by the silent-deny list — either as an exact match or as a
// subdomain of a listed host. Matching against the canonical form
// (lowercase, no port, no trailing dot) keeps the logic robust to the
// usual variants in CONNECT request lines.
func isSilentDenyHost(host string) bool {
	if host == "" {
		return false
	}
	for _, entry := range defaultSilentDenyHosts {
		if host == entry {
			return true
		}
		if strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// notifyBlocked fires the OnBlocked hook safely, unless the host
// matches the silent-deny list — in which case the denial still
// happens at handle() (the 403 response was already sent before this
// call) but the event is suppressed to keep events.jsonl free of
// telemetry noise.
func (p *Proxy) notifyBlocked(host string) {
	if isSilentDenyHost(host) {
		return
	}
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
	// SplitHostPort distinguishes a host that already carries a port
	// (`example.com:443`, `[::1]:8080`) from a bare host that needs one
	// appended. A naive `strings.Contains(":")` check would treat a
	// bracketed-but-portless IPv6 literal (`[::1]`) as "already has a
	// port" and pass it through unmodified, causing the dial to fail.
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
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

// handleForward proxies plain-HTTP requests through the pooled
// [http.Transport] so keep-alive connections to the same upstream are
// reused across requests. Hop-by-hop headers are stripped per RFC 7230.
func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	target := r.URL
	if target.Host == "" {
		target.Host = r.Host
	}
	if target.Scheme == "" {
		target.Scheme = "http"
	}

	outReq := r.Clone(r.Context())
	outReq.URL.Scheme = "http"
	outReq.URL.Host = target.Host
	outReq.Host = target.Host
	outReq.RequestURI = ""
	stripHopByHop(outReq.Header)

	resp, err := p.forwardTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
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
	// Wait for the second goroutine, but cap the wait so a peer in a
	// pathological TCP state (half-closed, RST swallowed by netfilter,
	// etc.) can't keep an io.Copy parked indefinitely. The first Close
	// above should unblock it; if it doesn't within a few seconds the
	// goroutine is genuinely wedged and waiting longer would only leak
	// us along with it.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
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
