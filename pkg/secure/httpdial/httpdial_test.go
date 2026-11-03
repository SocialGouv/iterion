package httpdial

import (
	"context"
	"errors"
	"net"
	"net/http"
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

// A caller's escape hatch is named on the refusals it lifts, so the fact
// travels in the error: every strict refusal — a literal, a reserved name in
// either spelling, a resolved answer — wraps ErrNotPublic, and a refusal no
// hatch fixes (a malformed name, a failed or empty lookup) does not.
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

// The CGNAT, IPv4-compatible and NAT64 local-use classes, and a NAT64 or 6to4
// address carrying a non-public IPv4 address (the samples carry loopback and
// private ones), are refused under strict and admitted when a caller lifts
// it — the private classes' treatment, so
// each caller's own escape hatch is the one that lifts them. One row per
// caller class and arm, with what sets the arm: refusing the classes is a
// decision read caller by caller. Each arm drives the three ways a caller
// meets the guard: a literal host, a name whose answer holds the class
// beside a public address (every answer is judged), and a dial through
// SafeTransport. The gateway credential's write door judges IsPublicUnicast
// alone, with no lift: its decision is TestIsPublicUnicast's rows.
func TestNewClasses_EachCallersArmDecides(t *testing.T) {
	samples := []string{"100.64.0.1", "::127.0.0.1", "64:ff9b::7f00:1", "64:ff9b:1::a00:1", "2002:7f00:1::"}
	arms := []struct {
		caller string
		strict bool
	}{
		{"LLM gateway endpoint — compatgw Validate, dial and sandbox proxy, model dispatch, the run proxy's pin — ITERION_LLM_ENDPOINT_ALLOW_PRIVATE unset", true},
		{"LLM gateway endpoint — ITERION_LLM_ENDPOINT_ALLOW_PRIVATE=1", false},
		{"studio preview proxy, per-org OIDC connectors, forge clients — cloud, or local on a non-loopback bind", true},
		{"studio preview proxy, per-org OIDC connectors, forge clients — local, loopback-bound", false},
		{"completion webhooks", true},
		{"completion webhooks — WithAllowPrivate", false},
		{"runner clone guard — gitws pre-check and clone-guard proxy", true},
		{"runner clone guard — ITERION_RUNNER_CLONE_ALLOW_PRIVATE=1", false},
		{"connector calls, local tier", true},
		{"connector calls, local tier — ITERION_CONNECTOR_ALLOW_PRIVATE=1", false},
		{"connector calls, cloud tier — always strict, a tenant-supplied base URL", true},
		{"connectors CLI description fetch — always strict", true},
		{"alert webhook sink — never strict, an operator-pinned receiver", false},
	}
	ctx := context.Background()
	// A canceled context stops the dial before any packet: an error that is
	// not the guard's refusal is the dialer's cancellation.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, arm := range arms {
		for _, sample := range samples {
			lit, litErr := ResolvePublicHostWith(ctx, answering(), sample, arm.strict)
			_, ansErr := ResolvePublicHostWith(ctx, answering("203.0.113.7", sample), "gw.example.test", arm.strict)
			_, dialErr := SafeTransport(arm.strict).DialContext(canceled, "tcp", net.JoinHostPort(sample, "443"))
			if arm.strict {
				for _, got := range []struct {
					form string
					err  error
				}{{"literal", litErr}, {"answer", ansErr}, {"dial", dialErr}} {
					if got.err == nil || !strings.Contains(got.err.Error(), "not a public unicast IP") {
						t.Errorf("%s: %s %s = %v, want refused as not public", arm.caller, got.form, sample, got.err)
					}
				}
				continue
			}
			if litErr != nil || !lit.Equal(net.ParseIP(sample)) {
				t.Errorf("%s: literal %s = %v, %v; want it admitted", arm.caller, sample, lit, litErr)
			}
			if ansErr != nil {
				t.Errorf("%s: an answer holding %s refused: %v", arm.caller, sample, ansErr)
			}
			if !errors.Is(dialErr, context.Canceled) {
				t.Errorf("%s: dial %s = %v; want the dialer's cancellation, past the guard", arm.caller, sample, dialErr)
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
