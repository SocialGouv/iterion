// Package httpdial is the single source of truth for iterion's SSRF guard:
// resolving an operator/admin-supplied host to a safe IP and dialing only that
// pinned IP (DNS-rebinding-proof). It backs the studio preview proxy
// (pkg/server), completion webhooks (pkg/notify), and the per-org OIDC SSO
// connectors (pkg/auth/oidc), which fetch discovery/token/userinfo/JWKS
// endpoints derived from an org-admin-supplied issuer URL.
//
// The guard blocks the conventional SSRF categories — loopback, private
// (RFC1918 / ULA), link-local, multicast, unspecified — plus the cloud
// metadata endpoints, and refuses conventional cluster-internal hostname
// aliases that service meshes re-route even with no DNS record.
package httpdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// IsPublicUnicast reports whether ip is safe to dial from a cloud pod context.
// The blocked set matches the typical SSRF blocklist plus AWS/GCP/Azure/Alibaba
// metadata endpoints.
func IsPublicUnicast(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// Cloud metadata services sit on top of the link-local range (already
	// covered above); belt-and-suspenders string check for readability and to
	// cover the IPv6 metadata aliases.
	switch ip.String() {
	case "169.254.169.254", "100.100.100.200", "fd00:ec2::254", "fe80::a9fe:a9fe":
		return false
	}
	return true
}

// IsLoopbackBind returns true when bind is one of the conventional loopback
// identifiers. Callers use it to decide whether the permissive "let the user
// embed/reach their own dev servers" mode is safe (loopback-bound = safe).
func IsLoopbackBind(bind string) bool {
	switch strings.ToLower(strings.TrimSpace(bind)) {
	case "", "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(bind); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// ResolvePublicHost resolves host and returns a single IP safe to dial. When
// strict, every resolved IP must be public unicast — any private/link-local/
// loopback/multicast/metadata hit (or a reserved cluster-internal alias)
// refuses. When non-strict, the first resolved address is returned regardless
// (loopback-bound local mode embedding the user's own dev servers). Resolution
// always fails closed.
func ResolvePublicHost(ctx context.Context, host string, strict bool) (net.IP, error) {
	if host == "" {
		return nil, errors.New("httpdial: empty host")
	}

	// Already a numeric address: validate and return.
	if ip := net.ParseIP(host); ip != nil {
		if strict && !IsPublicUnicast(ip) {
			return nil, fmt.Errorf("httpdial: address %s is not a public unicast IP", ip)
		}
		return ip, nil
	}

	// Refuse cluster-internal aliases outright in strict mode regardless of DNS
	// — service meshes re-route these even when they don't resolve.
	if strict {
		lower := strings.ToLower(host)
		if strings.HasSuffix(lower, ".svc.cluster.local") ||
			strings.HasSuffix(lower, ".svc") ||
			lower == "kubernetes.default" ||
			lower == "metadata.google.internal" {
			return nil, fmt.Errorf("httpdial: hostname %q is reserved for cluster-internal services", host)
		}
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("httpdial: resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("httpdial: resolve %q: no addresses", host)
	}
	for _, a := range addrs {
		if strict && !IsPublicUnicast(a.IP) {
			return nil, fmt.Errorf("httpdial: resolved address %s is not a public unicast IP", a.IP)
		}
	}
	return addrs[0].IP, nil
}

// SafeTransport returns an *http.Transport whose DialContext resolves the
// target host through ResolvePublicHost (strict→public-unicast) and pins the
// dial to that validated IP. Because the guard runs on *every* new connection,
// endpoints discovered at runtime (an OIDC token/userinfo/JWKS URL read from a
// discovery doc) are re-validated too — closing second-order SSRF. The
// original host travels in the Host header / TLS SNI so virtual-hosted and
// TLS-terminated upstreams behave correctly.
func SafeTransport(strict bool) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ip, err := ResolvePublicHost(ctx, host, strict)
			if err != nil {
				return nil, err
			}
			return PlainDialer().DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// PlainDialer returns the dialer SafeTransport connects through, WITHOUT the
// guard: it bounds the connect and keeps the connection alive, and dials
// whatever address it is handed. It serves a transport that keeps
// SafeTransport's bounds while connecting where the guard refuses by design
// — a proxy's own private address — its caller validating the request's
// host before any byte reaches that proxy.
func PlainDialer() *net.Dialer {
	return &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
}

// SafeClient wraps SafeTransport in an *http.Client that does NOT auto-follow
// redirects (each hop would re-target an unvalidated host; the caller decides
// whether to chase a 3xx, re-entering the guard).
func SafeClient(strict bool, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransport(strict),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
