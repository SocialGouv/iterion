package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// previewProxyMaxBytes caps how much upstream body the proxy will
// stream to the studio before EOF. 32 MB covers any reasonable
// preview page (HTML + inline assets) while bounding the worst case
// when an operator points the proxy at a large download URL.
const previewProxyMaxBytes = 32 << 20

// handlePreviewProxy serves GET /api/runs/{id}/preview?target=<url>.
// It is the back-channel that lets the studio's Browser pane embed
// a URL whose origin would otherwise reject framing via
// X-Frame-Options or Content-Security-Policy: frame-ancestors. The
// proxy fetches the target server-side, strips frame-blocking response
// headers, and returns the body sandboxed via a strict CSP so the
// proxied page cannot navigate the studio SPA top-level.
//
// Cloud mode applies SSRF-grade input validation:
//   - target must be http(s)
//   - hostname must resolve to a public IP (no RFC1918, link-local,
//     loopback, IPv6 ULA, or cloud metadata 169.254.169.254)
//   - the dial is pinned to the resolved IP — Go won't re-resolve on
//     redirects or retries, defeating DNS rebinding
//
// Local mode allows loopback / private addresses (the whole point: a
// workflow's dev server runs on localhost). The cloud guard is opt-in
// per Server.cfg.Mode.
func (s *Server) handlePreviewProxy(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}

	runID := r.PathValue("id")
	if runID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "run id required")
		return
	}

	target := r.URL.Query().Get("target")
	if target == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "target query parameter required")
		return
	}

	parsed, err := url.Parse(target)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid target url: %v", err)
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "target must be http or https")
		return
	}
	if parsed.Host == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "target must include a host")
		return
	}

	// Confirm the run exists — keeps this endpoint scoped, and avoids
	// turning the studio into an open relay even for clients on a
	// loopback origin.
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run console disabled")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), runID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}

	// SSRF policy:
	//  - cloud mode (always strict)
	//  - local mode bound to a non-loopback address (multi-user dev
	//    box, devcontainer-shared host, LAN exposure): also strict.
	//    A logged-in user who can reach the studio on 0.0.0.0 should
	//    not be able to gateway requests to the host's loopback /
	//    private network through us. (F-S4)
	//  - local mode bound to 127.0.0.1 / ::1 / localhost: permissive
	//    so the studio can embed the user's own dev servers.
	strict := s.outboundStrict()

	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	pinnedIP, err := httpdial.ResolvePublicHost(r.Context(), host, strict)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusForbidden, "target rejected: %v", err)
		return
	}

	// Build a transport that always dials the pinned IP. The HTTP
	// request still sends the original Host header (and SNI for
	// https), so virtual-hosted upstreams behave correctly.
	pinnedAddr := net.JoinHostPort(pinnedIP.String(), port)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
			return d.DialContext(ctx, network, pinnedAddr)
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		// Refuse upgrades to plaintext on https targets — Go's default
		// already does that, but be explicit.
		ForceAttemptHTTP2: true,
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		// We don't auto-follow redirects: each hop must be re-validated.
		// The studio receives the 3xx and decides whether to chase it
		// (typically it won't — iframes follow on their own when the
		// proxy round-trips).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}

	// #nosec G107 G704 — SSRF-safe: host was validated by httpdial.ResolvePublicHost and the
	// transport below dials only the pinned resolved IP (DNS-rebinding-proof),
	// redirects are not auto-followed, and strict public-unicast validation is
	// enforced in cloud / non-loopback-bind modes. parsed.String() is reused
	// solely to preserve the original Host header / SNI for virtual hosts.
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "build upstream request: %v", err)
		return
	}
	// Pass through a minimal set of headers. Cookies are intentionally
	// dropped — the studio SPA's cookie space must not leak to the
	// proxied origin, and vice versa.
	if accept := r.Header.Get("Accept"); accept != "" {
		upstreamReq.Header.Set("Accept", accept)
	}
	if al := r.Header.Get("Accept-Language"); al != "" {
		upstreamReq.Header.Set("Accept-Language", al)
	}
	upstreamReq.Header.Set("User-Agent", "iterion-preview-proxy/1")

	// #nosec G107 G704 — SSRF-safe: see the httpdial.ResolvePublicHost guard above. The
	// client's transport is pinned to the validated IP and CheckRedirect
	// refuses to auto-follow, so no hop can re-target a private address.
	resp, err := client.Do(upstreamReq)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadGateway, "upstream fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "x-frame-options",
			"content-security-policy",
			"content-security-policy-report-only",
			"cross-origin-opener-policy",
			"cross-origin-embedder-policy",
			"cross-origin-resource-policy",
			"set-cookie",
			// The upstream is attacker-influenced (a run picks the target),
			// and these are Added, not Set — so a hostile value arrives
			// ALONGSIDE the one securityHeaders wrote, and the browser
			// resolves the pair in the sender's favour: a second
			// `Permissions-Policy: camera=*` defeats our `camera=()`, and a
			// second `Referrer-Policy: unsafe-url` leaks the full preview URL
			// to a third party. Drop them and keep ours.
			"referrer-policy",
			"permissions-policy",
			"x-content-type-options":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-same-origin; frame-ancestors 'self'")
	w.Header().Set("X-Iterion-Preview-Proxy", "1")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, previewProxyMaxBytes))
}
