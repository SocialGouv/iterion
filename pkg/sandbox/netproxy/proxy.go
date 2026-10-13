package netproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// Proxy is the iterion HTTP / HTTPS CONNECT proxy. It enforces a
// [Policy] on the host portion of every CONNECT and plain-HTTP
// request, then tunnels accepted traffic transparently.
//
// By default the proxy does NOT terminate TLS — it inspects only the
// CONNECT host:port (and the plain-HTTP Host header) and shuttles the
// encrypted bytes through untouched. The reason is cost and simplicity,
// NOT client cert pinning: the clients iterion actually runs (the Claude
// Code CLI, the Anthropic/OpenAI SDKs, npm/pip/go/git) are standard
// trust-store clients with no certificate pinning — they work behind
// TLS-inspecting proxies (Zscaler, CrowdStrike, mitmproxy) once the
// proxy CA is trusted, which is exactly how the opt-in TLS-inspection
// mode (secret egress substitution, Layer 2) works. Transparent
// tunnelling is the default because it needs no CA minted, no CA private
// key to custody, and no per-runtime trust-store injection
// (NODE_EXTRA_CA_CERTS, certifi, the system store, …).
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

	// pins is the pin (Options.PinnedHosts, Options.PinSource); nil pins
	// nothing. A pinned host's forwards ride pinnedForward, and its
	// inspected requests the inspection's pinnedUpstream: neither keeps a
	// connection alive, so each request is dialed through the pin as it
	// stands — never a connection pooled before its host was pinned, or
	// under an escape hatch since withdrawn.
	pins          *pinSet
	pinnedForward *http.Transport

	// inspect holds the TLS-inspection state (Layer 2 secret egress
	// substitution). Nil keeps the proxy a transparent CONNECT tunnel.
	inspect *inspectConfig

	// maxBody is Options.MaxInspectedBody (0: maxInspectedBody).
	maxBody int64

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

	// Dial overrides the upstream dialer — and with it the proxy's own
	// refusal of a loopback, link-local or unspecified destination: the
	// caller's dialer owns its guard (the runner's clone guard dials through
	// pkg/secure/httpdial, whose escape hatch admits an internal forge).
	// PinnedHosts still apply: Dial is handed their validated address.
	// Tests use this to redirect CONNECTs to a loopback echo server.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// InspectCA, when non-nil, enables TLS-inspection mode (Layer 2):
	// CONNECT tunnels are terminated with leaves minted by this CA so the
	// proxy can rewrite the plaintext request. The CA's public cert must
	// be trusted inside the sandbox.
	InspectCA *EphemeralCA

	// Rewriter is consulted in inspection mode to substitute secret
	// placeholders and to block exfiltration. Nil leaves inspection a
	// pure decrypt/re-encrypt passthrough.
	Rewriter SecretRewriter

	// ModelHosts names hosts serving a model API beside the built-in ones
	// (a gateway at a path of its own), in the network rules' syntax; a base
	// URL or host:port names its host. In inspection mode a request's body
	// to such a host keeps its placeholders, like a request to a provider.
	// A `!` exclusion lifts the model detection for the hosts it matches, a
	// model API's path included — save a built-in provider host, which no
	// entry lifts; the last matching entry wins. An entry that is not a
	// valid pattern, or an exclusion naming a built-in provider host, fails
	// New.
	ModelHosts []string

	// MaxInspectedBody bounds the request body the proxy holds to inspect
	// (content DLP, substitution) — plain HTTP and inspected tunnels alike;
	// a larger one is refused (413), never cut. 0 keeps the default, 64 MiB;
	// a negative bound fails New. The runtime sets it from
	// ITERION_SANDBOX_INSPECT_MAX_BODY.
	MaxInspectedBody int64

	// InspectUpstreamTLS overrides the TLS config used for the proxy's
	// connection to the REAL upstream in inspection mode (tests inject a
	// RootCAs trusting a local httptest server). Nil uses the system
	// trust store with proper verification.
	InspectUpstreamTLS *tls.Config

	// PinnedHosts names hosts the proxy pins: it resolves such a host itself,
	// through the shared SSRF guard (pkg/secure/httpdial), and dials only the
	// address the guard validated — so a DNS answer the guard refuses meets
	// it where the connection is made, whatever a client's own check saw
	// before (rebinding). Every way out pins alike — a CONNECT tunnel, a
	// plain-HTTP forward, an inspected upstream — and a request to a pinned
	// host never rides a pooled connection. A refusal answers 502 and is
	// reported through OnBlocked. A host is pinned in the form the policy
	// reads a request's host in, so every spelling of it is; the policy still
	// matches it by name, and the host.docker.internal alias, never resolved,
	// keeps its own road. An entry is matched as written, never as a
	// pattern; one with a port, a path, userinfo or whitespace fails New.
	PinnedHosts []string

	// PinAllowPrivate lifts the guard's public-unicast rule for the pinned
	// hosts: the first answer is dialed whatever it is, the proxy's own
	// dialer still refusing a loopback, link-local or unspecified
	// destination (a Dial set in Options owns that refusal). The zero value
	// requires every answer to be public unicast.
	PinAllowPrivate bool

	// PinSource, when set, is read when New builds the proxy and again
	// whenever the proxy reaches an upstream — every dial, every forwarded
	// or inspected request — for a pin whose configuration can change while
	// the proxy runs: the host it names joins the pinned hosts for the
	// proxy's life, and its allowPrivate replaces PinAllowPrivate. An empty
	// host adds none.
	PinSource func() (host string, allowPrivate bool)

	// PinResolver answers the pin's lookups; nil is net.DefaultResolver. A
	// test injects one so a pinned name resolves without live DNS.
	PinResolver httpdial.HostResolver
}

// dialTimeout bounds an upstream dial — the name's resolution and the
// connect together.
const dialTimeout = 10 * time.Second

// errLocalDestination is the proxy's refusal to dial its own host: the
// egress allowlist governs destinations, and the proxy host's local services
// are not among them.
var errLocalDestination = errors.New("netproxy: dial refused: a loopback, link-local or unspecified destination is the proxy host's own, not an egress destination")

// localDestination reports whether a connect to ip lands on the proxy host
// itself: its loopback, the unspecified address (a connect to 0.0.0.0 or ::
// lands on the loopback), or a link-local address (the cloud metadata
// endpoints among them). An IPv4-mapped IPv6 address is judged as IPv4.
func localDestination(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()
}

// refuseLocalDestination is the control hook of the proxy's own dialer. It
// runs on the address the socket is about to connect to, once the name is
// resolved — so a destination is judged whatever spelling led there (an IP
// literal, `localhost.`, a hosts-file alias, a DNS answer), with no window
// between the check and the connect.
func refuseLocalDestination(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("netproxy: dial refused: the connect address %q is not an IP and port: %w", address, err)
	}
	if localDestination(ap.Addr()) {
		return errLocalDestination
	}
	return nil
}

// New constructs a Proxy. The proxy is not yet listening — call
// [Proxy.Start] when ready to accept clients.
func New(opts Options) (*Proxy, error) {
	if opts.Policy == nil {
		return nil, errors.New("netproxy: New: Policy is required")
	}
	pinned, err := pinnedHostSet(opts.PinnedHosts)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		policy:    opts.Policy,
		token:     opts.Token,
		onBlocked: opts.OnBlocked,
	}
	dial, hostDial := opts.Dial, opts.Dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout, Control: refuseLocalDestination}).DialContext
		hostDial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	if len(pinned) > 0 || opts.PinSource != nil {
		p.pins = newPinSet(pinned, opts.PinAllowPrivate, opts.PinSource)
		dial = p.pinDial(dial, p.pins, opts.PinResolver)
	}
	// host.docker.internal is the container-side alias for the host
	// gateway. When a sandboxed client routes a call to an iterion host
	// service (the per-run board MCP listener — C082) THROUGH this proxy
	// (some MCP clients ignore NO_PROXY), the proxy — which runs ON the
	// host — must dial the host's own loopback, not literally resolve
	// "host.docker.internal" (which has no host-side DNS entry, so the dial
	// would fail). Rewrite it to 127.0.0.1 so host-gateway services are
	// reachable whether the client honours NO_PROXY (direct) or not
	// (proxied). External egress is unaffected — it uses real hostnames.
	// The alias is the one sanctioned way to the host's loopback, so it
	// dials on a dialer of its own: the proxy's dialer refuses that address.
	// It is matched as the policy reads it, every spelling alike, and never
	// resolved, so no pin applies to it.
	baseDial := dial
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && canonicalHost(host) == "host.docker.internal" {
			return hostDial(ctx, network, net.JoinHostPort("127.0.0.1", port))
		}
		return baseDial(ctx, network, addr)
	}
	p.dial = dial
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
	if p.pins != nil {
		p.pinnedForward = unpooled(p.forwardTransport)
	}
	// The operator's model hosts are validated whether or not inspection
	// runs: an invalid entry fails the run's start either way.
	modelHosts, err := modelHostPolicy(opts.ModelHosts)
	if err != nil {
		return nil, err
	}
	if opts.MaxInspectedBody < 0 {
		return nil, fmt.Errorf("netproxy: New: MaxInspectedBody %d: want a positive bound, or 0 for the default", opts.MaxInspectedBody)
	}
	p.maxBody = opts.MaxInspectedBody
	if opts.InspectCA != nil {
		upstreamTLS := opts.InspectUpstreamTLS
		if upstreamTLS == nil {
			upstreamTLS = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		upstream := &http.Transport{
			DialContext:         dial,
			TLSClientConfig:     upstreamTLS,
			ForceAttemptHTTP2:   false, // stay HTTP/1.1 end-to-end
			DisableCompression:  true,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
		p.inspect = &inspectConfig{
			ca:         opts.InspectCA,
			rewriter:   opts.Rewriter,
			modelHosts: modelHosts,
			upstream:   upstream,
		}
		if p.pins != nil {
			p.inspect.pinnedUpstream = unpooled(upstream)
		}
	}
	return p, nil
}

// unpooled is a copy of t that keeps no connection alive.
func unpooled(t *http.Transport) *http.Transport {
	t = t.Clone()
	t.DisableKeepAlives = true
	return t
}

// isPinned reports whether host — the target of a forward or an inspected
// tunnel, bare or with its port — is pinned, the pin source read anew.
func (p *Proxy) isPinned(host string) bool {
	if p.pins == nil {
		return false
	}
	pinned, _ := p.pins.lookup(host)
	return pinned
}

// dialAddr is the address a forwarded or inspected request is dialed on:
// its host as the policy read it (canonicalHost), its port kept. Handed the
// host as the client wrote it, net/http would fold a non-ASCII one with its
// own copy of the IDN tables and dial a name the policy, the pin and the
// secret substitution never read.
func dialAddr(hostport string) string {
	host := canonicalHost(hostport)
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
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
			iterlog.NewFromEnv(os.Stderr).Error("netproxy: serve stopped with error: %v", err)
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
	// Every request reads the pin source, one to the host alias — which no
	// pin dials — included.
	if p.pins != nil {
		p.pins.sync()
	}
	host := canonicalHost(r.Host)
	if host == "" && r.Method != http.MethodConnect {
		host = canonicalHost(r.URL.Host)
	}
	if host == "" {
		http.Error(w, "no valid host", http.StatusBadRequest)
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
	p.reportBlocked(host, "policy denial")
}

// reportBlocked hands a refusal to the OnBlocked hook, a panicking hook
// never taking the proxy down.
func (p *Proxy) reportBlocked(host, reason string) {
	if p.onBlocked != nil {
		defer func() { _ = recover() }()
		p.onBlocked(host, reason)
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

	// TLS-inspection mode (Layer 2): terminate TLS on the client side and
	// rewrite the plaintext request instead of tunnelling opaque bytes.
	// Policy was already enforced in [Proxy.handle] before dispatch here.
	if p.inspect != nil {
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
		if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			clientConn.Close()
			return
		}
		if err := bufrw.Flush(); err != nil {
			clientConn.Close()
			return
		}
		p.handleConnectInspect(host, clientConn, bufrw)
		return
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
	outReq.URL.Host = dialAddr(target.Host)
	outReq.Host = target.Host
	outReq.RequestURI = ""
	stripHopByHop(outReq.Header)

	// Content DLP applies to a plain-HTTP request like to an inspected one:
	// the two schemes share this proxy, and a gate on one only is a gate the
	// caller walks around. Its placeholders are never substituted: a value
	// never goes out over clear text.
	if p.inspect != nil {
		host := canonicalHost(target.Host)
		body, refusal := p.inspectRequest(outReq, host, false)
		if refusal != nil {
			p.reportBlocked(host, refusal.reason)
			w.Header().Set("X-Iterion-Reason", "sandbox secret policy")
			http.Error(w, refusal.message, refusal.status)
			return
		}
		setBody(outReq, body)
	}

	transport := p.forwardTransport
	if p.isPinned(target.Host) {
		transport = p.pinnedForward
	}
	resp, err := transport.RoundTrip(outReq)
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
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	// First direction finished → tear the tunnel down. Closing both ends
	// unblocks the peer io.Copy in the common case; SetDeadline in the
	// past additionally forces a Read/Write parked in a pathological TCP
	// state (half-closed, RST swallowed by netfilter, etc.) to return
	// immediately — Close alone does not always wake such a parked
	// syscall. Together they make the second goroutine's exit reliable
	// rather than something we wait on and then leak.
	for _, c := range []net.Conn{a, b} {
		_ = c.SetDeadline(time.Now())
		_ = c.Close()
	}
	// The deadline+close above guarantees the second copy returns; the
	// bounded wait remains only as a last-ditch backstop so tunnel itself
	// can never block forever on an exotic Conn whose SetDeadline is a
	// no-op.
	backstop := time.NewTimer(5 * time.Second)
	defer backstop.Stop() // release immediately on the common <-done path; time.After would leak the timer for 5s per tunnel teardown
	select {
	case <-done:
	case <-backstop.C:
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
