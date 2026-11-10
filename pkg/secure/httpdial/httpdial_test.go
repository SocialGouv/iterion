package httpdial

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"10.0.0.5", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"169.254.169.254", false}, // AWS/Azure metadata
		{"169.254.1.2", false},     // link-local
		{"100.100.100.200", false}, // Alibaba metadata
		{"0.0.0.0", false},         // unspecified
		{"224.0.0.1", false},       // multicast
		{"::1", false},
		{"fe80::1", false},
		{"fd00::1", false}, // ULA
		{"2606:4700:4700::1111", true},
		{"2001:4860:4860::8888", true},

		// CGNAT, the shared address space 100.64.0.0/10 (RFC 6598): carrier
		// NAT, overlay networks, cluster pod ranges — the Alibaba metadata
		// alias above sits inside it. Both edges are refused, the public
		// neighbours on either side are not, and the IPv4-mapped spelling is
		// judged as the IPv4 address it maps.
		{"100.64.0.0", false},
		{"100.64.0.1", false},
		{"100.127.255.255", false},
		{"::ffff:100.64.0.1", false},
		{"100.63.255.255", true},
		{"100.128.0.1", true},

		// IPv4-compatible ::/96 (deprecated, RFC 4291 §2.5.5.1): a reader
		// that maps it to IPv4 reaches the address it embeds — ::127.0.0.1
		// is 127.0.0.1's loopback — and no public host is written this way,
		// so the whole block is refused. The IPv4-mapped block is judged as
		// the IPv4 address it maps, a public one staying public.
		{"::127.0.0.1", false},
		{"::10.0.0.1", false},
		{"::8.8.8.8", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:8.8.8.8", true},

		// The NAT64 well-known prefix 64:ff9b::/96 (RFC 6052): a translator
		// delivers to the IPv4 address in bits 96-127, so the address is as
		// public as the one it carries. An IPv6-only network with DNS64
		// reaches an IPv4-only public host through it (IANA marks the prefix
		// globally reachable); the same prefix carrying loopback, private,
		// CGNAT or metadata reaches the translator's own network. The row
		// past the /96 is no NAT64 address and is judged as itself: it pins
		// the mask.
		{"64:ff9b::7f00:1", false},    // 127.0.0.1
		{"64:ff9b::a00:1", false},     // 10.0.0.1
		{"64:ff9b::6440:1", false},    // 100.64.0.1
		{"64:ff9b::a9fe:a9fe", false}, // 169.254.169.254
		{"64:ff9b::808:808", true},    // 8.8.8.8
		{"64:ff9b::8c52:7003", true},  // 140.82.112.3
		{"64:ff9b::1:0:0", true},
		// The NAT64 local-use prefix 64:ff9b:1::/48 (RFC 8215) is refused
		// whole: its operator picks the prefix length, so where the IPv4
		// address sits is written nowhere in the address. A network-specific
		// prefix may sit anywhere inside the /48, so the rows past its first
		// /64 pin the mask's width: 169.254.169.254 under a /64 of its own,
		// the same address in the RFC 6052 /48 format, the block's last
		// address.
		{"64:ff9b:1::a00:1", false},
		{"64:ff9b:1:1::a9fe:a9fe", false},
		{"64:ff9b:1:a9fe:a9:fe00::", false},
		{"64:ff9b:1:ffff:ffff:ffff:ffff:ffff", false},

		// 6to4 2002::/16 (RFC 3056, deprecated by RFC 7526): a relay
		// delivers to the IPv4 address in bits 16-47, so the address is as
		// public as the one it carries. The next /16 is allocated public
		// space (the /16 below is the 2001:4860:: row above).
		{"2002:7f00:1::", false},     // 127.0.0.1
		{"2002:a00:1::", false},      // 10.0.0.1
		{"2002:a9fe:a9fe::1", false}, // 169.254.169.254
		{"2002:808:808::1", true},    // 8.8.8.8
		{"2003::1", true},

		// IPv4 blocks no Internet host sits in (RFC 6890), refused whole as
		// the address a host IS: the reserved Class E 240.0.0.0/4 (the
		// limited broadcast at its top) and benchmarking 198.18.0.0/15
		// (RFC 2544) — one /16 of each admitted as a name's answer, where
		// Istio's DNS proxy and Clash's fake-IP DNS answer from them
		// (TestReservedBlocks_RefusedLiteralAdmittedAsAnAnswer) — and the
		// IETF protocol assignments 192.0.0.0/24 (192.0.0.192 is Oracle
		// Cloud Classic's metadata). Both edges refused, the public
		// neighbours on either side not.
		{"240.0.0.0", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},
		{"::ffff:240.0.0.1", false},
		{"198.18.0.0", false},
		{"198.19.255.255", false},
		{"198.17.255.255", true},
		{"198.20.0.0", true},
		{"192.0.0.0", false},
		{"192.0.0.192", false},
		{"192.0.0.255", false},
		{"191.255.255.255", true},
		{"192.0.1.0", true},
		// Azure's WireServer: public space, reached only from inside Azure
		// as the platform's metadata and agent endpoint.
		{"168.63.129.16", false},
		{"168.63.129.17", true},
		// SIIT IPv4-translated ::ffff:0:0:0/96 (RFC 2765 §2.1): a
		// translator delivers to the IPv4 address in bits 96-127, so the
		// address is as public as the one it carries. The rows beside it pin
		// the mask: the IPv4-mapped ::ffff:0:0/96 is judged as its IPv4
		// address (above), and the next /96 is no translated address.
		{"::ffff:0:7f00:1", false},    // 127.0.0.1
		{"::ffff:0:a00:1", false},     // 10.0.0.1
		{"::ffff:0:a9fe:a9fe", false}, // 169.254.169.254
		{"::ffff:0:808:808", true},    // 8.8.8.8
		{"::ffff:1:7f00:1", true},
		// Site-local fec0::/10 (deprecated by RFC 3879): a site's own space,
		// both edges.
		{"fec0::1", false},
		{"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", tc.ip)
		}
		if got := IsPublicUnicast(ip); got != tc.want {
			t.Errorf("IsPublicUnicast(%s)=%v want %v", tc.ip, got, tc.want)
		}
	}
	// A slice of no IP address length is no destination.
	if IsPublicUnicast(net.IP{8, 8, 8}) {
		t.Error("IsPublicUnicast(a 3-byte slice) = true; want false — a slice that is no address is not public")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, b := range []string{"", "127.0.0.1", "localhost", "::1", "[::1]"} {
		if !IsLoopbackBind(b) {
			t.Errorf("IsLoopbackBind(%q) should be true", b)
		}
	}
	for _, b := range []string{"0.0.0.0", "10.0.0.1", "example.com"} {
		if IsLoopbackBind(b) {
			t.Errorf("IsLoopbackBind(%q) should be false", b)
		}
	}
}

func TestResolvePublicHost_Literals(t *testing.T) {
	ctx := context.Background()
	// Public literal passes strict.
	if _, err := ResolvePublicHost(ctx, "8.8.8.8", true); err != nil {
		t.Errorf("public literal rejected: %v", err)
	}
	// Private literal rejected strict, allowed non-strict.
	if _, err := ResolvePublicHost(ctx, "10.0.0.1", true); err == nil {
		t.Errorf("private literal should be rejected in strict mode")
	}
	if _, err := ResolvePublicHost(ctx, "10.0.0.1", false); err != nil {
		t.Errorf("private literal should pass in non-strict mode: %v", err)
	}
	// Cluster-internal alias refused in strict mode without DNS.
	for _, h := range []string{"foo.svc.cluster.local", "kubernetes.default", "metadata.google.internal"} {
		if _, err := ResolvePublicHost(ctx, h, true); err == nil {
			t.Errorf("alias %q should be refused in strict mode", h)
		}
	}
	// Empty host always errors.
	if _, err := ResolvePublicHost(ctx, "", false); err == nil {
		t.Errorf("empty host should error")
	}

	// The absolute spelling of a reserved name — one trailing dot — meets
	// the refusal of the name itself, before any lookup: the resolver below
	// answers every name with a public address, so only the guard's own
	// reading of the name can refuse it.
	for _, h := range []string{"metadata.google.internal.", "gw.svc.", "foo.svc.cluster.local.", "KUBERNETES.DEFAULT."} {
		res := answering("203.0.113.7")
		if ip, err := ResolvePublicHostWith(ctx, res, h, true); err == nil {
			t.Errorf("alias %q admitted in strict mode as %s — its trailing dot escaped the refusal", h, ip)
		}
		if len(res.asked) != 0 {
			t.Errorf("alias %q was looked up (%q) — the refusal must not wait on DNS", h, res.asked)
		}
	}
	// Non-strict lifts the alias refusal, the dotted spelling included.
	if _, err := ResolvePublicHostWith(ctx, answering("203.0.113.7"), "gw.svc.", false); err != nil {
		t.Errorf("a dotted alias refused in non-strict mode: %v", err)
	}
	// One dot is cut, never two: what is left with an empty label is no DNS
	// name, refused at every strictness before any lookup.
	for _, h := range []string{"metadata.google.internal..", "gw.svc..", "gw..example.test", ".gw.example.test", "."} {
		for _, strict := range []bool{true, false} {
			res := answering("203.0.113.7")
			if ip, err := ResolvePublicHostWith(ctx, res, h, strict); err == nil {
				t.Errorf("host %q with an empty label admitted (strict=%v) as %s", h, strict, ip)
			}
			if len(res.asked) != 0 {
				t.Errorf("host %q with an empty label was looked up (strict=%v): %q", h, strict, res.asked)
			}
		}
	}
	// The trailing dot itself is legal: an absolute public name resolves,
	// looked up as the caller spelled it — the dot keeps a search domain
	// from rewriting the name the resolver is asked.
	res := answering("203.0.113.7")
	if ip, err := ResolvePublicHostWith(ctx, res, "gw.example.test.", true); err != nil || !ip.Equal(net.ParseIP("203.0.113.7")) {
		t.Errorf("an absolute public name = %v, %v; want its public answer", ip, err)
	}
	if len(res.asked) != 1 || res.asked[0] != "gw.example.test." {
		t.Errorf("the resolver was asked %q; want the caller's spelling, once", res.asked)
	}
}

// Under strict the resolver is asked the absolute name, so no search domain
// rewrites the name the guard judged into one it never judged: in a pod, a
// relative "gw.ai" is Service gw of namespace ai, whose ClusterIP can sit in
// address space the guard calls public. A lifted caller keeps the relative
// name its search domains complete — a self-hosted Service named the
// cluster's way.
func TestResolvePublicHostWith_StrictAsksTheAbsoluteName(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ host, want string }{
		{"gw.ai", "gw.ai."},
		{"argocd-server.argocd", "argocd-server.argocd."},
		{"GW.Example.Test", "GW.Example.Test."},
		{"gw.example.test.", "gw.example.test."},
	} {
		res := answering("203.0.113.7")
		if _, err := ResolvePublicHostWith(ctx, res, c.host, true); err != nil {
			t.Errorf("strict %q = %v; want its public answer", c.host, err)
		}
		if len(res.asked) != 1 || res.asked[0] != c.want {
			t.Errorf("strict %q asked the resolver %q; want [%q], the absolute name", c.host, res.asked, c.want)
		}
	}
	res := answering("10.0.0.7")
	if _, err := ResolvePublicHostWith(ctx, res, "litellm.pliage", false); err != nil || len(res.asked) != 1 || res.asked[0] != "litellm.pliage" {
		t.Errorf("non-strict litellm.pliage asked %q (%v); want the relative name its search domains complete", res.asked, err)
	}
}

// A name no public host carries is refused under strict before any lookup,
// whatever it would resolve to — a cluster's Service range can be public
// address space (GKE's default is 34.118.224.0/20), a pod's or a VM's a
// range the guard calls public: a single label (an /etc/hosts alias, the
// pod's own name, a Service the search domains expand), and the names a
// cluster, a cloud, a container or a workstation reserves for itself — the
// set the gateway credential's write door refuses (ReservedName). The
// refusal wraps ErrNotPublic, and a lifted caller resolves the name.
func TestResolvePublicHostWith_StrictRefusesReservedNames(t *testing.T) {
	ctx := context.Background()
	for _, host := range []string{
		"kubernetes", "mongo", "localhost", "_gateway", "Metadata.",
		"240-0-0-5.default.pod.cluster.local", "gw.svc.cluster.local", "vm-1.c.proj.internal", "metadata.google.internal",
		"printer.local", "api.localhost", "localhost.localdomain", "db.localdomain", "gw.svc", "kubernetes.default",
	} {
		res := answering("34.118.224.1")
		if ip, err := ResolvePublicHostWith(ctx, res, host, true); !errors.Is(err, ErrNotPublic) {
			t.Errorf("strict %q = %v, %v; want a refusal wrapping ErrNotPublic", host, ip, err)
		}
		if len(res.asked) != 0 {
			t.Errorf("strict %q was looked up (%q) — the refusal must not wait on DNS", host, res.asked)
		}
		if _, err := ResolvePublicHostWith(ctx, answering("34.118.224.1"), host, false); err != nil {
			t.Errorf("non-strict %q = %v; want it resolved", host, err)
		}
	}
}

// An IPv6-only network with DNS64 answers an IPv4-only host with a
// 64:ff9b::/96 address synthesized beside its A record: the strict guard
// judges the synthesized address by the IPv4 address it carries, so a public
// host stays reachable and a private one stays refused.
func TestResolvePublicHostWith_DNS64Answers(t *testing.T) {
	ctx := context.Background()
	if ip, err := ResolvePublicHostWith(ctx, answering("140.82.112.3", "64:ff9b::8c52:7003"), "github.com", true); err != nil || !ip.Equal(net.ParseIP("140.82.112.3")) {
		t.Errorf("strict github.com answered with its DNS64 synthesis = %v, %v; want the public host admitted", ip, err)
	}
	if ip, err := ResolvePublicHostWith(ctx, answering("64:ff9b::8c52:7003"), "github.com", true); err != nil || !ip.Equal(net.ParseIP("64:ff9b::8c52:7003")) {
		t.Errorf("strict github.com answered by DNS64 alone = %v, %v; want the synthesized address admitted", ip, err)
	}
	if _, err := ResolvePublicHostWith(ctx, answering("64:ff9b::a00:5"), "gw.example.test", true); !errors.Is(err, ErrNotPublic) {
		t.Errorf("strict gw.example.test synthesized from 10.0.0.5 = %v; want a refusal wrapping ErrNotPublic", err)
	}
}

// An IPv4-compatible address (::a.b.c.d, deprecated by RFC 4291) is refused
// at every strictness: no host is written in that form, and a reader that
// maps it reaches the IPv4 address it carries — ::127.0.0.1 is 127.0.0.1's
// loopback. No hatch lifts it, so its refusal does not wrap ErrNotPublic. The
// unspecified :: and the loopback ::1 sit in ::/96 and are no such address:
// a lifted caller still reaches its own [::1].
func TestResolvePublicHostWith_IPv4CompatibleRefusedAtEveryStrictness(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, strict := range []bool{true, false} {
		for _, sample := range []string{"::127.0.0.1", "::10.0.0.1", "::8.8.8.8"} {
			lit, litErr := ResolvePublicHostWith(ctx, answering(), sample, strict)
			ans, ansErr := ResolvePublicHostWith(ctx, answering("203.0.113.7", sample), "gw.example.test", strict)
			_, dialErr := SafeTransport(strict).DialContext(canceled, "tcp", net.JoinHostPort(sample, "443"))
			for _, got := range []struct {
				form string
				ip   net.IP
				err  error
			}{{"literal", lit, litErr}, {"answer", ans, ansErr}, {"dial", nil, dialErr}} {
				if got.err == nil || !strings.Contains(got.err.Error(), "IPv4-compatible") {
					t.Errorf("strict=%v %s %s = %v, %v; want the IPv4-compatible refusal", strict, got.form, sample, got.ip, got.err)
				} else if errors.Is(got.err, ErrNotPublic) {
					t.Errorf("strict=%v %s %s = %v; no hatch lifts it, so it must not wrap ErrNotPublic", strict, got.form, sample, got.err)
				}
			}
		}
	}
	if ip, err := ResolvePublicHostWith(ctx, answering(), "::1", false); err != nil || !ip.Equal(net.IPv6loopback) {
		t.Errorf("non-strict ::1 = %v, %v; want the loopback a lifted caller reaches", ip, err)
	}
}

// A caller's escape hatch is named on the refusals it lifts, so the fact
// travels in the error: every strict refusal — a literal, a reserved name in
// either spelling, a resolved answer — wraps ErrNotPublic, and a refusal no
// hatch fixes (a malformed name, a failed or empty lookup, an
// IPv4-compatible address) does not.
func TestResolvePublicHostWith_OnlyALiftableRefusalCarriesErrNotPublic(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		host string
		res  HostResolver
	}{
		{"10.0.0.1", answering()},
		{"100.64.0.1", answering()},
		{"64:ff9b::7f00:1", answering()},
		{"metadata.google.internal", answering("203.0.113.7")},
		{"gw.svc.", answering("203.0.113.7")},
		{"gw.example.test", answering("203.0.113.7", "2002:7f00:1::")},
	} {
		if _, err := ResolvePublicHostWith(ctx, c.res, c.host, true); !errors.Is(err, ErrNotPublic) {
			t.Errorf("strict %q = %v; want a refusal wrapping ErrNotPublic", c.host, err)
		}
	}
	for _, c := range []struct {
		host string
		res  HostResolver
	}{
		{"gw..example.test", answering("203.0.113.7")},
		{"gw.svc..", answering("203.0.113.7")},
		{"gw.example.test", answering()},
		{"gw.example.test", failing{}},
		{"::127.0.0.1", answering()},
		{"gw.example.test", answering("203.0.113.7", "::127.0.0.1")},
	} {
		_, err := ResolvePublicHostWith(ctx, c.res, c.host, true)
		if err == nil || errors.Is(err, ErrNotPublic) {
			t.Errorf("strict %q = %v; want a refusal that does not wrap ErrNotPublic", c.host, err)
		}
	}
}

// failing is a HostResolver whose every lookup fails.
type failing struct{}

// LookupIPAddr implements HostResolver.
func (failing) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// answering is a HostResolver that answers every name with ips and records
// the names it was asked.
func answering(ips ...string) *recordingResolver {
	return &recordingResolver{answer: ips}
}

type recordingResolver struct {
	answer []string
	asked  []string
}

// LookupIPAddr implements HostResolver.
func (r *recordingResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.asked = append(r.asked, host)
	addrs := make([]net.IPAddr, 0, len(r.answer))
	for _, ip := range r.answer {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return addrs, nil
}

// The CGNAT and NAT64 local-use classes, and a NAT64 or 6to4 address carrying
// a non-public IPv4 address (the samples carry loopback and private ones),
// are the private classes' case: refused under strict, admitted lifted. This
// is the guard's own verdict per strictness, through the three ways a caller
// meets it: a literal host, a name whose answer holds the class beside a
// public address (every answer is judged), and a dial through SafeTransport.
// Which strictness each caller passes is witnessed in the caller's own
// package, through its production constructor; the gateway credential's
// write door judges IsPublicUnicast alone, with no lift. One /16 of the
// reserved Class E and benchmarking blocks each is admitted as a name's answer
// (TestReservedBlocks_RefusedLiteralAdmittedAsAnAnswer).
func TestNewClasses_RefusedStrictAdmittedLifted(t *testing.T) {
	// The IPv4-compatible class is no sample: no strictness admits it
	// (TestResolvePublicHostWith_IPv4CompatibleRefusedAtEveryStrictness).
	samples := []string{"100.64.0.1", "64:ff9b::7f00:1", "64:ff9b:1::a00:1", "2002:7f00:1::",
		"192.0.0.192", "168.63.129.16", "::ffff:0:7f00:1", "fec0::1"}
	ctx := context.Background()
	// A canceled context stops the dial before any packet: an error that is
	// not the guard's refusal is the dialer's cancellation.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, strict := range []bool{true, false} {
		for _, sample := range samples {
			lit, litErr := ResolvePublicHostWith(ctx, answering(), sample, strict)
			_, ansErr := ResolvePublicHostWith(ctx, answering("203.0.113.7", sample), "gw.example.test", strict)
			_, dialErr := SafeTransport(strict).DialContext(canceled, "tcp", net.JoinHostPort(sample, "443"))
			if strict {
				for _, got := range []struct {
					form string
					err  error
				}{{"literal", litErr}, {"answer", ansErr}, {"dial", dialErr}} {
					if !errors.Is(got.err, ErrNotPublic) || !strings.Contains(got.err.Error(), "not a public unicast IP") {
						t.Errorf("strict: %s %s = %v, want refused as not public", got.form, sample, got.err)
					}
				}
				continue
			}
			if litErr != nil || !lit.Equal(net.ParseIP(sample)) {
				t.Errorf("lifted: literal %s = %v, %v; want it admitted", sample, lit, litErr)
			}
			if ansErr != nil {
				t.Errorf("lifted: an answer holding %s refused: %v", sample, ansErr)
			}
			if !errors.Is(dialErr, context.Canceled) {
				t.Errorf("lifted: dial %s = %v; want the dialer's cancellation, past the guard", sample, dialErr)
			}
		}
	}
}

func TestSafeClient_RejectsPrivateDial(t *testing.T) {
	client := SafeClient(true, 0)
	// Dialing a private literal must be refused by the guarded transport.
	_, err := client.Get("http://10.1.2.3/")
	if err == nil {
		t.Fatalf("expected dial to private IP to be refused")
	}
	if !strings.Contains(err.Error(), "public unicast") {
		t.Errorf("expected public-unicast refusal, got %v", err)
	}
}

// PlainDialer is SafeTransport's dialer without the guard: its promise to a
// caller dialing a proxy is SafeTransport's bounds — a connect timeout and
// a TCP keep-alive.
func TestPlainDialer_KeepsSafeTransportsBounds(t *testing.T) {
	d := PlainDialer()
	if d.Timeout <= 0 {
		t.Errorf("PlainDialer().Timeout = %v, want a connect bound", d.Timeout)
	}
	if d.KeepAlive <= 0 {
		t.Errorf("PlainDialer().KeepAlive = %v, want SafeTransport's keep-alive period", d.KeepAlive)
	}
}

func TestSafeClient_NoRedirectFollow(t *testing.T) {
	c := SafeClient(true, 0)
	if c.CheckRedirect == nil {
		t.Fatal("SafeClient must set CheckRedirect")
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Errorf("expected ErrUseLastResponse, got %v", err)
	}
}

// #2338: the guard decides egress, never an ambient proxy. SafeTransport pins
// Proxy nil — restoring http.ProxyFromEnvironment (the void the ticket names:
// the guard validates the PROXY's public address while the proxy reaches a
// private target the guard never saw, and a private proxy is refused
// outright) turns this red.
func TestSafeTransport_PinsNoProxy(t *testing.T) {
	for _, strict := range []bool{true, false} {
		if tr := SafeTransport(strict); tr.Proxy != nil {
			t.Fatalf("SafeTransport(%v).Proxy is set — an ambient proxy would decide where guarded egress goes", strict)
		}
	}
	c := SafeClient(true, 0)
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("SafeClient's transport must pin Proxy nil like SafeTransport")
	}
}

// The reserved Class E block and the benchmarking block are refused for a
// LITERAL host — no Internet host has such an address — and two /16s of them
// are admitted as a resolver's answer for a name: Istio's DNS auto-allocation
// pool (240.240.0.0/16), the VIPs it answers a ServiceEntry host with, and
// Clash's default fake-IP range (198.18.0.0/16), from which a fake-IP DNS
// proxy answers every name. The rest of the two blocks stays refused as an
// answer: Class E outside Istio's pool is where GKE puts Pod and Service
// ranges, 198.19.0.0/16 is where EKS custom networking puts pod subnets, and
// a public wildcard DNS name (sslip.io, nip.io) answers any address its label
// spells — the literal refusal alone would cost an attacker nothing. The
// other blocks the guard refuses stay refused as answers, and so do the
// limited broadcast and a transition address carrying one of the two blocks
// (judged as a literal: no mesh or fake-IP proxy answers with one). Lifted,
// every form is admitted.
func TestReservedBlocks_RefusedLiteralAdmittedAsAnAnswer(t *testing.T) {
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	admittedAnswers := []string{"240.240.0.1", "240.240.0.5", "240.240.255.254", "198.18.0.1", "198.18.0.5", "198.18.255.255"}
	refusedAnswers := []string{"240.0.0.1", "240.0.0.10", "240.239.255.255", "240.241.0.0", "255.255.255.254", "198.19.0.0", "198.19.0.10", "198.19.255.254"}
	for _, addr := range append(slices.Clone(admittedAnswers), refusedAnswers...) {
		admitted := slices.Contains(admittedAnswers, addr)
		// A public wildcard DNS name answers the address its label spells.
		name := addr + ".sslip.io"
		for _, strict := range []bool{true, false} {
			_, litErr := ResolvePublicHostWith(ctx, answering(), addr, strict)
			checkErr := CheckHost(addr, strict)
			_, dialErr := SafeTransport(strict).DialContext(canceled, "tcp", net.JoinHostPort(addr, "443"))
			ans, ansErr := ResolvePublicHostWith(ctx, answering(addr), name, strict)
			if !strict {
				if litErr != nil || checkErr != nil || !errors.Is(dialErr, context.Canceled) {
					t.Errorf("lifted: literal %s = %v / CheckHost %v / dial %v; want it admitted", addr, litErr, checkErr, dialErr)
				}
				if ansErr != nil || !ans.Equal(net.ParseIP(addr)) {
					t.Errorf("lifted: %s answered %s = %v, %v; want the answer admitted", name, addr, ans, ansErr)
				}
				continue
			}
			for _, got := range []struct {
				form string
				err  error
			}{{"literal", litErr}, {"CheckHost", checkErr}, {"dial", dialErr}} {
				if !errors.Is(got.err, ErrNotPublic) {
					t.Errorf("strict: %s %s = %v, want refused as not public", got.form, addr, got.err)
				}
			}
			switch {
			case admitted && (ansErr != nil || !ans.Equal(net.ParseIP(addr))):
				t.Errorf("strict: %s answered %s = %v, %v; want the answer admitted (a mesh VIP or a fake-IP proxy's answer)", name, addr, ans, ansErr)
			case !admitted && !errors.Is(ansErr, ErrNotPublic):
				t.Errorf("strict: %s answered %s = %v, %v; want the answer refused as not public", name, addr, ans, ansErr)
			}
		}
	}
	for _, addr := range []string{"192.0.0.170", "192.0.0.192", "fec0::1", "::ffff:0:7f00:1", "168.63.129.16",
		"255.255.255.255", "64:ff9b::f0f0:5", "2002:c612:5::", "::ffff:0:f0f0:5", "100.64.0.1", "10.0.0.1"} {
		if _, err := ResolvePublicHostWith(ctx, answering(addr), "api.openai.com", true); !errors.Is(err, ErrNotPublic) {
			t.Errorf("strict: api.openai.com answered %s = %v, want refused as not public", addr, err)
		}
	}
}
