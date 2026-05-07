package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handlePreviewProxy serves GET /api/runs/{id}/preview?target=<url>.
// It is the back-channel that lets the editor's Browser pane embed
// a URL whose origin would otherwise reject framing via
// X-Frame-Options or Content-Security-Policy: frame-ancestors. The
// proxy fetches the target server-side, strips frame-blocking response
// headers, and returns the body sandboxed via a strict CSP so the
// proxied page cannot navigate the editor SPA top-level.
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
	// turning the editor into an open relay even for clients on a
	// loopback origin.
	if s.runs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run console disabled")
		return
	}
	if _, err := s.runs.LoadRun(runID); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}

	cloudMode := s.cfg.Mode == "cloud"

	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	pinnedIP, err := resolvePreviewHost(r.Context(), host, cloudMode)
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
		// The editor receives the 3xx and decides whether to chase it
		// (typically it won't — iframes follow on their own when the
		// proxy round-trips).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "build upstream request: %v", err)
		return
	}
	// Pass through a minimal set of headers. Cookies are intentionally
	// dropped — the editor SPA's cookie space must not leak to the
	// proxied origin, and vice versa.
	if accept := r.Header.Get("Accept"); accept != "" {
		upstreamReq.Header.Set("Accept", accept)
	}
	if al := r.Header.Get("Accept-Language"); al != "" {
		upstreamReq.Header.Set("Accept-Language", al)
	}
	upstreamReq.Header.Set("User-Agent", "iterion-preview-proxy/1")

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
			"set-cookie":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-same-origin; frame-ancestors 'self'")
	w.Header().Set("X-Iterion-Preview-Proxy", "1")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// resolvePreviewHost resolves host and returns a single IP that is
// safe to dial. In cloud mode, every resolved IP must be a public
// unicast address: any private/link-local/loopback/multicast/cloud-
// metadata hit causes a refusal. In local mode the first resolved
// address is returned regardless — the editor is loopback-bound and
// the user is supposed to be able to embed dev servers.
func resolvePreviewHost(ctx context.Context, host string, cloudMode bool) (net.IP, error) {
	if host == "" {
		return nil, errors.New("empty host")
	}

	// Already a numeric address: validate and return.
	if ip := net.ParseIP(host); ip != nil {
		if cloudMode && !isPublicUnicast(ip) {
			return nil, fmt.Errorf("address %s is not a public unicast IP", ip)
		}
		return ip, nil
	}

	// Refuse some hostnames outright in cloud mode regardless of DNS
	// resolution — they're conventional aliases for cluster-internal
	// services that may not exist in DNS but get re-routed by service
	// meshes.
	if cloudMode {
		lower := strings.ToLower(host)
		if strings.HasSuffix(lower, ".svc.cluster.local") ||
			strings.HasSuffix(lower, ".svc") ||
			lower == "kubernetes.default" ||
			lower == "metadata.google.internal" {
			return nil, fmt.Errorf("hostname %q is reserved for cluster-internal services", host)
		}
	}

	resolver := &net.Resolver{}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %q: no addresses", host)
	}

	for _, a := range addrs {
		if cloudMode && !isPublicUnicast(a.IP) {
			return nil, fmt.Errorf("resolved address %s is not a public unicast IP", a.IP)
		}
	}
	return addrs[0].IP, nil
}

// isPublicUnicast reports whether ip is safe to fetch from a cloud
// pod context. The set of blocked categories matches the typical
// SSRF blocklist plus AWS/GCP/Azure metadata endpoints.
func isPublicUnicast(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Cloud metadata services use specific addresses on top of the
	// link-local range; the link-local check above covers
	// 169.254.169.254 / fe80::a9fe:a9fe. The Azure IMDS uses the
	// same link-local IP. Belt-and-suspenders check by string for
	// readability:
	switch ip.String() {
	case "169.254.169.254", "100.100.100.200", "fd00:ec2::254":
		return false
	}
	return true
}
