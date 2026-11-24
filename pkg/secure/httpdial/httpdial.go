// Package httpdial is the single source of truth for iterion's SSRF guard:
// resolving an operator/admin-supplied host to a safe IP and dialing only that
// pinned IP (DNS-rebinding-proof). It backs the studio preview proxy
// (pkg/server), completion webhooks (pkg/notify), and the per-org OIDC SSO
// connectors (pkg/auth/oidc), which fetch discovery/token/userinfo/JWKS
// endpoints derived from an org-admin-supplied issuer URL.
//
// The guard blocks the conventional SSRF categories — loopback, private
// (RFC1918 / ULA), link-local, multicast, unspecified — plus the ranges it
// lists (the shared CGNAT space, the IETF protocol assignments, the
// deprecated IPv4-compatible and site-local blocks, the NAT64 local-use
// prefix), a NAT64 well-known, 6to4 or SIIT-translated address whose
// embedded IPv4 address it blocks, and the cloud metadata endpoints. Two
// more blocks — the reserved Class E block and benchmarking — are refused
// whole for a literal host and as a resolver's answer for a name, except one
// /16 of each a resolver may answer a name from (answerOnlyPrefixes): Istio's
// DNS auto-allocation pool and Clash's default fake-IP range. Under strict it
// refuses the names no public host
// carries (ReservedName: a single label, the names a cluster, a cloud, a
// container or a workstation reserves) before any lookup, and looks every
// other name up absolute. The address set is these lists, not every range
// IANA marks not globally reachable.
package httpdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// IsPublicUnicast reports whether ip is safe to dial from a cloud pod context
// as the address a host IS — a literal in a URL or a host field. It refuses
// what net.IP's own predicates name (loopback, unspecified, multicast,
// link-local, private), the ranges in nonPublicPrefixes and
// reservedPrefixes, an address of a v4Embeddings prefix whose embedded IPv4
// address it refuses, and the AWS/GCP/Azure/Alibaba metadata endpoints with
// Azure's WireServer. An IPv4-mapped address is judged as the IPv4 address it
// maps. The set is these lists, not every range IANA marks not globally
// reachable. A resolver's answer for a name is judged by ResolvePublicHost,
// which admits answerOnlyPrefixes.
func IsPublicUnicast(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if inNonPublicPrefix(ip) || inPrefixes(ip, reservedPrefixes) {
		return false
	}
	if v4, ok := embeddedIPv4(ip); ok && !IsPublicUnicast(v4) {
		return false
	}
	// Cloud metadata services sit on top of the link-local range and the
	// shared address space (already covered above); belt-and-suspenders
	// string check for readability and to cover the IPv6 metadata aliases.
	// Azure's WireServer is the exception in public space: 168.63.129.16 is
	// reached only from inside Azure, as its metadata and agent endpoint.
	switch ip.String() {
	case "169.254.169.254", "100.100.100.200", "fd00:ec2::254", "fe80::a9fe:a9fe", "168.63.129.16":
		return false
	}
	return true
}

// nonPublicPrefixes are refused whole, literal or answer, beside net.IP's own
// predicates. The shared address space serves carrier NAT, overlay networks
// and cluster pod ranges. The IETF protocol assignments hold no Internet host
// (RFC 6890), and a cloud has put its metadata there (192.0.0.192). The
// deprecated site-local block is a site's own space. The two IPv4-carrying
// IPv6 blocks are refused whatever address they carry: the deprecated
// IPv4-compatible form, which no host is written in and which a reader that
// maps it delivers to the IPv4 address it carries, and the NAT64 local-use
// prefix, whose operator picks the prefix length and so where the IPv4
// address sits.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // shared address space, CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments (RFC 6890 §2.2.2)
	netip.MustParsePrefix("fec0::/10"),      // site-local (RFC 3879, deprecated)
	ipv4CompatiblePrefix,                    // IPv4-compatible (RFC 4291 §2.5.5.1, deprecated)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use prefix (RFC 8215)
}

// reservedPrefixes hold no Internet host (RFC 6890): a literal host in them
// is refused whole, and so is a resolver's answer outside answerOnlyPrefixes.
// Class E is where GKE puts Pod and Service secondary ranges beside
// 100.64.0.0/10, and 198.19.0.0/16 is one of the two ranges EKS custom
// networking recommends for pod subnets (the other is 100.64.0.0/10): a name
// answering there reaches a cluster's own pods or Services. Refusing only the
// literal would cost an attacker nothing — a public wildcard DNS name (sslip.io,
// nip.io) answers any address its label spells.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, Class E (RFC 1112 §4)
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking (RFC 2544)
}

// answerOnlyPrefixes are the two /16s of reservedPrefixes a resolver's answer
// for a name is admitted from, because a resolver inside a cluster or on a
// workstation answers names there: Istio's DNS proxy answers a ServiceEntry host declared without
// addresses (and not a wildcard) with a VIP auto-allocated from 240.240.0.0/16,
// and a fake-IP DNS proxy answers every name from its fake-IP range, Clash's
// default being 198.18.0.0/16 — the dial then reaches the mesh's or the proxy's
// forwarder. The cost is a cluster whose own pod or Service ranges sit in one
// of these two /16s: a name answering there reaches them from every strict
// dial, the read primitives included (the studio preview proxy and the cloud
// connector client hand the response back). A literal host in them stays
// refused, and so does a transition address carrying one (v4Embeddings, judged
// as a literal: no mesh or fake-IP proxy answers with one).
var answerOnlyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("240.240.0.0/16"), // Istio DNS auto-allocation pool
	netip.MustParsePrefix("198.18.0.0/16"),  // Clash's default fake-IP range
}

// inPrefixes reports whether ip — an IPv4-mapped address judged as the IPv4
// address it maps — falls in one of prefixes.
func inPrefixes(ip net.IP, prefixes []netip.Prefix) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// isPublicAnswer is IsPublicUnicast for a resolver's answer: an address of
// answerOnlyPrefixes is admitted.
func isPublicAnswer(ip net.IP) bool {
	return inPrefixes(ip, answerOnlyPrefixes) || IsPublicUnicast(ip)
}

// ipv4CompatiblePrefix is the deprecated IPv4-compatible block (RFC 4291
// §2.5.5.1); ResolvePublicHost refuses its addresses at every strictness
// (checkAddress).
var ipv4CompatiblePrefix = netip.MustParsePrefix("::/96")

// v4Embeddings are the IPv6 prefixes whose every address carries, at a fixed
// place, the IPv4 address a translator or a relay delivers to: such an
// address is exactly as public as the IPv4 address it carries. An IPv6-only
// network with DNS64 reaches an IPv4-only public host through the NAT64
// well-known prefix (IANA marks it globally reachable), while the same
// prefix carrying 10.0.0.5 or 169.254.169.254 reaches the translator's own
// network.
var v4Embeddings = []struct {
	prefix netip.Prefix
	at     int // byte offset of the IPv4 address in the IPv6 one
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},    // NAT64 well-known prefix (RFC 6052 §2.2): bits 96-127
	{netip.MustParsePrefix("2002::/16"), 2},        // 6to4 (RFC 3056 §2, deprecated by RFC 7526): bits 16-47
	{netip.MustParsePrefix("::ffff:0:0:0/96"), 12}, // SIIT IPv4-translated (RFC 2765 §2.1): bits 96-127
}

// embeddedIPv4 returns the IPv4 address ip carries when ip falls in a
// v4Embeddings prefix.
func embeddedIPv4(ip net.IP) (net.IP, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return nil, false
	}
	addr = addr.Unmap()
	for _, e := range v4Embeddings {
		if e.prefix.Contains(addr) {
			b := addr.As16()
			return net.IPv4(b[e.at], b[e.at+1], b[e.at+2], b[e.at+3]), true
		}
	}
	return nil, false
}

// inNonPublicPrefix reports whether ip falls in nonPublicPrefixes, an
// IPv4-mapped address judged as the IPv4 address it maps. A slice of no IP
// address length is no destination, so it is not public either.
func inNonPublicPrefix(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, p := range nonPublicPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ErrNotPublic is wrapped by every refusal strict mode makes and non-strict
// mode lifts: an address that is not public unicast, a name no public host
// carries (ReservedName). A caller holding an escape hatch names it on
// this refusal alone — never on a failed lookup or a malformed name, which
// no hatch fixes.
var ErrNotPublic = errors.New("httpdial: not a public destination")

// notPublicError is a strict-mode refusal: its own message, ErrNotPublic in
// its chain.
type notPublicError struct{ msg string }

func (e *notPublicError) Error() string { return e.msg }

func (e *notPublicError) Unwrap() error { return ErrNotPublic }

func notPublic(format string, args ...any) error {
	return &notPublicError{msg: fmt.Sprintf(format, args...)}
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

// HostResolver is the guard's one DNS seam: the lookup a hostname goes
// through before validation. *net.Resolver satisfies it, and a test (or an
// embedded caller) can answer it without touching the network (#2080).
type HostResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ResolvePublicHost resolves host and returns a single IP safe to dial. When
// strict, every resolved IP must be public unicast — any private/shared/
// link-local/loopback/multicast/metadata hit (or a name no public host
// carries, ReservedName) refuses, wrapping ErrNotPublic — and the name is
// looked up absolute; a name's answer may sit in answerOnlyPrefixes, a
// literal host may not. When non-strict, the first resolved address is
// returned regardless (loopback-bound local mode embedding the user's own
// dev servers). At every strictness a name with an empty label and an
// IPv4-compatible address are refused (CheckHost). A name is judged without
// one trailing dot. Resolution always fails closed.
func ResolvePublicHost(ctx context.Context, host string, strict bool) (net.IP, error) {
	return ResolvePublicHostWith(ctx, net.DefaultResolver, host, strict)
}

// ResolvePublicHostWith is ResolvePublicHost through the caller's resolver:
// same guard, same fail-closed reading, one injected lookup. A nil res is a
// caller bug and panics — pass net.DefaultResolver for the system resolver.
func ResolvePublicHostWith(ctx context.Context, res HostResolver, host string, strict bool) (net.IP, error) {
	if err := CheckHost(host, strict); err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}

	// Under strict the lookup asks the absolute name — the caller's
	// spelling with its root dot — so no search domain rewrites the name
	// judged above into one never judged: in a pod, a relative "gw.ai" is
	// Service gw of namespace ai. A lifted caller's relative name keeps its
	// search domains, the way a self-hosted Service is named.
	lookup := host
	if strict && !strings.HasSuffix(host, ".") {
		lookup = host + "."
	}
	addrs, err := res.LookupIPAddr(ctx, lookup)
	if err != nil {
		return nil, fmt.Errorf("httpdial: resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("httpdial: resolve %q: no addresses", host)
	}
	for _, a := range addrs {
		if err := checkAddress(a.IP, strict, true); err != nil {
			return nil, err
		}
	}
	return addrs[0].IP, nil
}

// CheckHost is the guard's verdict on host as far as it reaches without a
// lookup — every refusal ResolvePublicHost makes before any DNS: an empty
// host, a name with an empty label and an IPv4-compatible literal at every
// strictness; under strict, a literal that is not public unicast and a name
// no public host carries (ReservedName), wrapping ErrNotPublic. nil leaves
// a name's answers to the lookup. A caller that vets an endpoint before it
// dials runs it, so the vetting refuses what the dial refuses on sight.
func CheckHost(host string, strict bool) error {
	if host == "" {
		return errors.New("httpdial: empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return checkAddress(ip, strict, false)
	}

	// A name is judged in one spelling: one trailing dot is the absolute
	// form of the same name, cut before the alias checks so that
	// "metadata.google.internal." meets the refusal of
	// "metadata.google.internal"; what is left with an empty label ("a..b",
	// a second trailing dot) is no DNS name, at any strictness.
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	if name == "" || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return fmt.Errorf("httpdial: hostname %q has an empty label", host)
	}

	// Refuse the names no public host carries outright in strict mode,
	// regardless of DNS — service meshes re-route the cluster's own even
	// when they don't resolve, and what they do resolve to can sit in
	// address space the guard calls public.
	if strict && ReservedName(name) {
		return notPublic("httpdial: hostname %q names no public host (a single label, or a name a cluster, a cloud, a container or a workstation reserves)", host)
	}
	return nil
}

// reservedNames and reservedSuffixes are the names a cluster, a cloud, a
// container or a workstation resolves to itself (single-label aliases such
// as localhost4 or ip6-localhost are refused as single labels).
var (
	reservedNames    = []string{"localhost.localdomain", "kubernetes.default"}
	reservedSuffixes = []string{".localhost", ".localdomain", ".localdomain4", ".localdomain6", ".local", ".internal", ".svc", ".cluster.local"}
)

// ReservedName reports whether name — lower-case, without its trailing dot —
// is one no public host carries: a single label (localhost and every
// /etc/hosts alias, _gateway, instance-data, the pod's own name, a Service
// the search domains expand) or a name a cluster, a cloud, a container or a
// workstation reserves for itself (ReservedNames). The strict guard refuses
// it before any lookup; the gateway credential's write door refuses it in a
// stored base URL — one set, so the two never disagree.
func ReservedName(name string) bool {
	if !strings.Contains(name, ".") {
		return true
	}
	for _, n := range reservedNames {
		if name == n {
			return true
		}
	}
	for _, suffix := range reservedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// ReservedNames lists the reserved multi-label names and suffixes as a
// refusal names them: "localhost.localdomain, kubernetes.default,
// *.localhost, …".
func ReservedNames() string {
	return strings.Join(reservedNames, ", ") + ", *" + strings.Join(reservedSuffixes, ", *")
}

// checkAddress is the guard's verdict on one address — a literal host, or a
// resolver's answer for a name (answer), which the refusal names "resolved
// address". An IPv4-compatible address is refused at every strictness, no
// hatch lifting it: no host is written in that deprecated form, and a reader
// that maps it reaches the IPv4 address it carries — ::127.0.0.1 is
// 127.0.0.1's loopback. Under strict, an address that is not public unicast
// is refused, wrapping ErrNotPublic: IsPublicUnicast for a literal,
// isPublicAnswer for an answer.
func checkAddress(ip net.IP, strict, answer bool) error {
	what, public := "address", IsPublicUnicast
	if answer {
		what, public = "resolved address", isPublicAnswer
	}
	if v4, ok := ipv4Compatible(ip); ok {
		return fmt.Errorf("httpdial: %s %s is IPv4-compatible (it carries %s), a deprecated form refused at every strictness", what, ip, v4)
	}
	if strict && !public(ip) {
		return notPublic("httpdial: %s %s is not a public unicast IP", what, ip)
	}
	return nil
}

// ipv4Compatible returns the IPv4 address an IPv4-compatible ip carries
// (::a.b.c.d, RFC 4291 §2.5.5.1). The unspecified :: and the loopback ::1
// sit in ::/96 and are no such address: they stay the IPv6 addresses they
// are.
func ipv4Compatible(ip net.IP) (net.IP, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || !ipv4CompatiblePrefix.Contains(addr) || addr.IsUnspecified() || addr.IsLoopback() {
		return nil, false
	}
	b := addr.As16()
	return net.IPv4(b[12], b[13], b[14], b[15]), true
}

// SafeTransport returns an *http.Transport whose DialContext resolves the
// target host through ResolvePublicHost (strict→public-unicast) and pins the
// dial to that validated IP. Because the guard runs on *every* new connection,
// endpoints discovered at runtime (an OIDC token/userinfo/JWKS URL read from a
// discovery doc) are re-validated too — closing second-order SSRF. The
// original host travels in the Host header / TLS SNI so virtual-hosted and
// TLS-terminated upstreams behave correctly.
//
// Proxy is pinned nil, never http.ProxyFromEnvironment (#2338): the guard
// must be the only voice on where a guarded byte goes. With an ambient
// proxy the guard would validate the PROXY's address while the proxy itself
// picks the target — a public proxy reaches a private target the guard never
// saw (the SSRF guard voided by its own transport), a private one is refused
// outright. A caller that legitimately crosses a proxy sets Proxy itself,
// validating the request's host before any byte reaches it — the shape
// pkg/backend/compatgw uses for the engine-provided sandbox proxy.
func SafeTransport(strict bool) *http.Transport {
	return &http.Transport{
		// Explicit, not the zero value left to reading: the guard decides
		// egress, an ambient proxy never does (#2338).
		Proxy: nil,
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

// PinnedTransport returns an *http.Transport that dials ONLY pinnedAddr — a
// host:port its caller resolved and validated through ResolvePublicHost —
// whatever host the request names; the original host still travels in the
// Host header and TLS SNI, so virtual-hosted and TLS-terminated upstreams
// behave. It is the transport for a delivery whose target was vetted once
// and must be connected exactly there (completion webhooks, the studio
// preview proxy), and it shares SafeTransport's bounds.
//
// Proxy is pinned nil, never http.ProxyFromEnvironment (#2337): under an
// ambient proxy, DialContext is called for the PROXY's address while this
// dialer connects to the pinned target — net/http then writes a proxy-form
// request, carrying the proxy's Proxy-Authorization, straight to the
// run-chosen receiver, which can read the operator's proxy credentials and
// replay them against the real proxy. The delivery is direct by
// construction, so no proxy credential can ride it.
func PinnedTransport(pinnedAddr string) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return PlainDialer().DialContext(ctx, network, pinnedAddr)
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
// whether to chase a 3xx, re-entering the guard) and never consults a proxy
// (SafeTransport pins Proxy nil).
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
