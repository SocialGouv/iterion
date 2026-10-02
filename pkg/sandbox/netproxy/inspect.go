package netproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// SecretRewriter is the proxy's view of the secret guard, used by the
// TLS-inspection mode to rewrite plaintext requests (Layer 2). It is a
// structural interface so netproxy stays decoupled from
// pkg/backend/secretguard (which implements it).
type SecretRewriter interface {
	// MaterializeForHostWithin swaps secret placeholders for real values,
	// but only for secrets scoped to (or unrestricted toward) host, and
	// refuses (false) a result longer than limit bytes before it allocates
	// one: a placeholder expands to its value.
	MaterializeForHostWithin(s, host string, limit int) (string, bool)
	// ExfiltratesTo reports whether s carries a real secret value bound
	// for a host that secret is NOT scoped to (a blockable exfiltration).
	ExfiltratesTo(s, host string) bool
}

// inspectConfig holds the per-proxy TLS-inspection state. Nil disables
// inspection (the proxy stays a transparent CONNECT tunnel).
type inspectConfig struct {
	ca       *EphemeralCA
	rewriter SecretRewriter
	upstream http.RoundTripper
	// modelHosts matches the operator's own model hosts, beside the built-in
	// ones (see modelRequest); nil when there are none.
	modelHosts *Policy
}

// handleConnectInspect terminates TLS on the hijacked client connection,
// rewrites each plaintext request (placeholder→secret for approved
// hosts; block on exfiltration to unapproved hosts), and forwards to the
// real upstream over a freshly-verified TLS connection. hostPort is the
// CONNECT target (host:port); the bare hostname drives leaf minting,
// substitution scoping, and DLP.
func (p *Proxy) handleConnectInspect(hostPort string, clientConn net.Conn, bufrw *bufio.ReadWriter) {
	defer clientConn.Close()

	// The policy's form of the target (lowercase, no trailing dot, IDN
	// folded): content DLP, substitution and the model match key on it too.
	hostname := canonicalHost(hostPort)

	// Read through the buffered reader (it already wraps clientConn and
	// may hold the ClientHello); write raw to clientConn.
	cc := &bufferedConn{Conn: clientConn, r: bufrw.Reader}
	tlsClient := tls.Server(cc, &tls.Config{
		GetCertificate: p.inspect.ca.GetCertificate,
		// Offer only HTTP/1.1 so we never have to demux HTTP/2 frames —
		// clients fall back transparently.
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		return
	}
	defer tlsClient.Close()

	// The request line and headers are read within the bound net/http's own
	// server applies (http.DefaultMaxHeaderBytes and its slack), as the
	// plain-HTTP path does; the body keeps its own bound (inspectRequest).
	// Without it one request's headers hold unbounded memory here.
	limited := &io.LimitedReader{R: tlsClient}
	reader := bufio.NewReader(limited)
	for {
		limited.N = http.DefaultMaxHeaderBytes + 4096
		req, err := http.ReadRequest(reader)
		if err != nil {
			if limited.N <= 0 {
				// Reported before it is answered: a client that has gone
				// away must not cost the refusal its event.
				p.reportBlocked(hostname, "request header over the inspection bound")
				writeSimpleResponse(tlsClient, http.StatusRequestHeaderFieldsTooLarge, "request header too large for secret inspection")
			}
			return // client closed, or malformed — done with this conn
		}
		limited.N = math.MaxInt64
		keepAlive := p.serveInspectedRequest(tlsClient, req, hostname, hostPort)
		if !keepAlive {
			return
		}
	}
}

// serveInspectedRequest rewrites and forwards one request, writing the
// upstream response back to the client. Returns whether the connection
// may be reused for another request.
func (p *Proxy) serveInspectedRequest(client net.Conn, req *http.Request, hostname, hostPort string) bool {
	// The request goes where its tunnel was opened: the policy, content DLP
	// and the substitution's scope all key on the CONNECT target. One naming
	// another host inside the tunnel is refused.
	if !requestTargetsTunnel(req, hostPort) {
		writeSimpleResponse(client, http.StatusMisdirectedRequest, "request host differs from the tunnel's")
		p.reportBlocked(hostname, "request host differs from the CONNECT target")
		return false
	}
	body, refusal := p.inspectRequest(req, hostname, true)
	if refusal != nil {
		writeSimpleResponse(client, refusal.status, refusal.message)
		p.reportBlocked(hostname, refusal.reason)
		return false
	}

	// Rebuild the request for the upstream RoundTrip, bound for the tunnel's
	// target.
	outURL := *req.URL
	outURL.Scheme = "https"
	outURL.Host = hostPort
	req.URL = &outURL
	req.RequestURI = ""
	setBody(req, body)

	resp, err := p.inspect.upstream.RoundTrip(req)
	if err != nil {
		writeSimpleResponse(client, http.StatusBadGateway, "upstream error: "+err.Error())
		return false
	}
	defer resp.Body.Close()

	// Force connection-close framing on the response we hand back to the
	// client. Streaming LLM/SSE endpoints (and any HTTP/1.1 response with
	// no Content-Length and no chunked transfer-encoding) are
	// *close-delimited*: the body ends when the server closes the socket.
	// If we keep the inspected client connection alive after such a
	// response, the in-container HTTP/1.1 client blocks forever waiting for
	// more bytes — observed as a hard hang on every sandboxed `claw` LLM
	// call once Layer-2 TLS inspection is active. Emitting `Connection:
	// close` and tearing the conn down after the body lets the client
	// detect EOF deterministically. resp.Write copies the body straight to
	// the raw *tls.Conn (no buffering), so streamed tokens still flush as
	// they arrive; we trade HTTP keep-alive reuse for correctness, and the
	// client simply opens a fresh connection for its next request.
	resp.Close = true
	if err := resp.Write(client); err != nil {
		return false
	}
	return false
}

// maxInspectedBody is the default bound of the request body the proxy holds
// to scan and substitute; a larger one is refused, never cut.
// Options.MaxInspectedBody (ITERION_SANDBOX_INSPECT_MAX_BODY) moves it. A
// variable for the tests.
var maxInspectedBody = 64 << 20

// refusal is why the proxy answers a request itself.
type refusal struct {
	status          int
	message, reason string
}

// inspectRequest applies Layer 2 to a request bound for host (in the
// policy's form): content DLP, then — when substitute, on the TLS path only: a
// value is never put on a clear-text link — placeholder substitution, in the
// headers always and in the body unless it goes to a model API. It returns
// the body to forward, or the refusal.
func (p *Proxy) inspectRequest(req *http.Request, host string, substitute bool) ([]byte, *refusal) {
	limit := int64(maxInspectedBody)
	if p.maxBody > 0 {
		limit = p.maxBody
	}
	var body []byte
	if req.Body != nil {
		// One byte past the bound tells a body over it from one at it; at
		// the largest bound there is no byte past it (limit+1 would wrap,
		// and a reader limited below zero reads nothing).
		read := limit
		if read < math.MaxInt64 {
			read++
		}
		b, err := io.ReadAll(io.LimitReader(req.Body, read))
		_ = req.Body.Close()
		if err != nil {
			return nil, &refusal{http.StatusBadRequest, "request body unreadable", "request body unreadable: " + err.Error()}
		}
		if int64(len(b)) > limit {
			return nil, &refusal{http.StatusRequestEntityTooLarge, "request body too large for secret inspection", "request body over the inspection bound"}
		}
		body = b
	}
	rw := p.inspect.rewriter
	if rw == nil {
		return body, nil
	}
	// DLP: a real secret value leaving toward an unapproved host is blocked
	// outright (defeats domain-fronting the allowlist can't see).
	if rw.ExfiltratesTo(inspectScanText(req, body), host) {
		return nil, &refusal{http.StatusForbidden, "blocked by sandbox secret policy", "secret exfiltration blocked"}
	}
	if !substitute {
		return body, nil
	}
	// Substitution is bounded too, as a placeholder expands to its value:
	// the body once substituted stays within the bound, and so does what
	// substitution adds to the header values, all of them together.
	bound := int(min(limit, int64(math.MaxInt)))
	if !modelRequest(host, req.URL.Path, p.inspect.modelHosts) {
		out, ok := rw.MaterializeForHostWithin(string(body), host, bound)
		if !ok {
			return nil, &refusal{http.StatusRequestEntityTooLarge, "request body too large for secret inspection", "request body over the inspection bound once its placeholders are substituted"}
		}
		body = []byte(out)
	}
	growth := bound
	// Sorted, so which header exhausts the shared budget is the request's
	// own doing and not a map's iteration order.
	for _, k := range slices.Sorted(maps.Keys(req.Header)) {
		for i, v := range req.Header[k] {
			within := math.MaxInt
			if growth <= math.MaxInt-len(v) {
				within = len(v) + growth
			}
			out, ok := rw.MaterializeForHostWithin(v, host, within)
			if !ok {
				return nil, &refusal{http.StatusRequestEntityTooLarge, "request headers too large for secret inspection", "request headers over the inspection bound once their placeholders are substituted"}
			}
			req.Header[k][i] = out
			if d := len(out) - len(v); d > 0 {
				growth -= d
			}
		}
	}
	return body, nil
}

// setBody makes body the request's, without the trailers it arrived with —
// header fields content DLP never scanned.
func setBody(req *http.Request, body []byte) {
	req.Trailer = nil
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if len(body) == 0 {
		req.Body = http.NoBody
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
}

// requestTargetsTunnel reports whether req names the tunnel's target: its
// host (http.ReadRequest takes an absolute URL's over the Host header; none:
// HTTP/1.0, the target), and its port when it names one.
func requestTargetsTunnel(req *http.Request, hostPort string) bool {
	authority := req.Host
	if authority == "" {
		return true
	}
	if canonicalHost(authority) != canonicalHost(hostPort) {
		return false
	}
	_, port, err := net.SplitHostPort(authority)
	if err != nil {
		return true
	}
	_, tunnelPort, _ := net.SplitHostPort(hostPort)
	return port == tunnelPort
}

// inspectScanText assembles the request surface a secret could leak
// through: the method, the URL, every header value, and the body.
func inspectScanText(req *http.Request, body []byte) string {
	var b strings.Builder
	b.Grow(len(body) + 256)
	b.WriteString(req.Method)
	b.WriteByte(' ')
	b.WriteString(req.URL.String())
	b.WriteByte('\n')
	for k, vals := range req.Header {
		// A field name reaches us canonicalised (each letter after a dash
		// upper-cased): its lower-case form too, so a lower-case value in a
		// name is seen as sent.
		lk := strings.ToLower(k)
		for _, v := range vals {
			b.WriteString(k)
			b.WriteString(" ")
			b.WriteString(lk)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}
	b.Write(body)
	return b.String()
}

// writeSimpleResponse writes a minimal HTTP/1.1 response to a raw conn.
func writeSimpleResponse(conn net.Conn, code int, msg string) {
	resp := &http.Response{
		StatusCode: code,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Content-Type":     {"text/plain; charset=utf-8"},
			"X-Iterion-Reason": {"sandbox secret policy"},
		},
		Body:          io.NopCloser(strings.NewReader(msg + "\n")),
		ContentLength: int64(len(msg) + 1),
	}
	_ = resp.Write(conn)
}

// bufferedConn is a net.Conn whose Read is served from a buffered reader
// (which already wraps the underlying conn), while Write/Close/etc.
// delegate to the underlying conn. Used so TLS termination consumes any
// bytes the HTTP server already buffered past the CONNECT line.
type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
