package netproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
)

// dialFunc is the shape of the proxy's upstream dialer.
type dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// pinSet is what the pin holds: the pinned hosts, keyed by pinKey, and
// whether a private answer is admitted. Options.PinSource grows it while the
// proxy runs, so it is read and written under its lock.
type pinSet struct {
	mu           sync.Mutex
	hosts        map[string]bool
	allowPrivate bool
	source       func() (host string, allowPrivate bool)
}

// newPinSet builds the pin over hosts and reads its source once, so the host
// the source names when the proxy is built is pinned from the start.
func newPinSet(hosts map[string]bool, allowPrivate bool, source func() (string, bool)) *pinSet {
	s := &pinSet{hosts: hosts, allowPrivate: allowPrivate, source: source}
	s.refresh()
	return s
}

// refresh adds the host the source names to the set and takes the source's
// strictness. The caller holds the lock, or has not shared the set yet.
func (s *pinSet) refresh() {
	if s.source == nil {
		return
	}
	named, allowPrivate := s.source()
	if key, ok := pinEntryKey(named); ok {
		s.hosts[key] = true
	}
	s.allowPrivate = allowPrivate
}

// sync refreshes the set from its source.
func (s *pinSet) sync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
}

// lookup refreshes the set from its source, then reports whether host — bare
// or with its port — is pinned and how strictly. A host the source has named
// once stays pinned.
func (s *pinSet) lookup(host string) (pinned, strict bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	return s.hosts[pinKey(host)], !s.allowPrivate
}

// pinDial wraps dial with the pin (Options.PinnedHosts, Options.PinSource):
// a pinned host is resolved here, through the shared SSRF guard, and dial is
// handed the address the guard validated — never the name, which it would
// resolve again — so the answer that is judged is the answer that is
// connected to. The lookup and the connect share the bound an unpinned dial
// puts on its resolution and connect. A refusal is returned and reported;
// a lookup cut short because its request went away refused nothing and is
// not reported. Only a CONNECT's lookup ends with its request: net/http
// dials a forwarded or inspected request apart from it, to its bound.
// Another host is dialed as it is.
func (p *Proxy) pinDial(dial dialFunc, pins *pinSet, res httpdial.HostResolver) dialFunc {
	if res == nil {
		res = net.DefaultResolver
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return dial(ctx, network, addr)
		}
		pinned, strict := pins.lookup(host)
		if !pinned {
			return dial(ctx, network, addr)
		}
		bounded, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		ip, err := httpdial.ResolvePublicHostWith(bounded, res, host, strict)
		if err != nil {
			if ctx.Err() == nil {
				p.reportBlocked(pinKey(host), "pinned host: "+err.Error())
			}
			return nil, fmt.Errorf("netproxy: pinned host %q: %w", host, err)
		}
		return dial(bounded, network, net.JoinHostPort(ip.String(), port))
	}
}

// pinKey is the form a host is pinned and matched in: the policy's canonical
// form (lowercase, no root dot, IDN folded), an IP address in its standard
// spelling — an IPv4 address written as IPv6 is the IPv4 one, a zone is
// dropped because the pin's dial never carries one, and an IPv4 notation
// only a system resolver reads becomes the dotted quad the dial reaches.
func pinKey(host string) string {
	c := canonicalHost(host)
	if a, err := netip.ParseAddr(c); err == nil {
		return unzoned(a).String()
	}
	if a, ok := numericIPv4(c); ok {
		return a.String()
	}
	return c
}

// unzoned strips an IPv6 zone: a zone names an interface of the machine
// that parsed the literal, and the proxy dials from the host side, so a
// pin and a CONNECT meet only when both key the bare address.
func unzoned(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}

// numericIPv4 parses the IPv4 notations a system resolver reads that netip
// refuses — inet_aton's forms: one to four dot-separated parts, each
// decimal, octal (a leading 0) or hex (0x), the last field widening over
// the bytes that remain. The dial's resolver (cgo getaddrinfo) reads such a
// host as that address; keyed as the name it spells, the host would slip
// past a pin on the address itself.
func numericIPv4(s string) (netip.Addr, bool) {
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	var b [4]byte
	for i, p := range parts {
		n, ok := numericField(p)
		if !ok {
			return netip.Addr{}, false
		}
		if i < len(parts)-1 {
			if n > 255 {
				return netip.Addr{}, false
			}
			b[i] = byte(n)
			continue
		}
		// The last field fills the bytes that remain: 32 bits for one
		// part, 24 for two, 16 for three, one byte for four.
		width := uint(8 * (5 - len(parts)))
		if n >= 1<<width {
			return netip.Addr{}, false
		}
		for shift := 0; shift < int(width)/8; shift++ {
			b[4-int(width)/8+shift] = byte(n >> (width - 8 - uint(shift)*8))
		}
	}
	return netip.AddrFrom4(b), true
}

// numericField reads one inet_aton field: decimal, octal (a leading 0) or
// hex (0x), refusing empty and overflowing. The octal reading is the one
// the resolver the dial goes through gives a leading-0 part.
func numericField(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	base := uint64(10)
	digits := p
	switch {
	case strings.HasPrefix(p, "0x"), strings.HasPrefix(p, "0X"):
		base, digits = 16, p[2:]
	case len(p) > 1 && p[0] == '0':
		base, digits = 8, p[1:]
	}
	if digits == "" {
		return 0, false
	}
	var n uint64
	for _, r := range digits {
		var d uint64
		switch {
		case r >= '0' && r <= '9':
			d = uint64(r - '0')
		case base == 16 && r >= 'a' && r <= 'f':
			d = uint64(r-'a') + 10
		case base == 16 && r >= 'A' && r <= 'F':
			d = uint64(r-'A') + 10
		default:
			return 0, false
		}
		if n > (^uint64(0)-d)/base || n*base > 1<<32-1-d {
			return 0, false
		}
		n = n*base + d
	}
	return n, true
}

// pinnedHostSet keys Options.PinnedHosts by pinKey. An entry that cannot be
// a host fails New rather than leave the host it meant unpinned; the error
// names it by its position.
func pinnedHostSet(hosts []string) (map[string]bool, error) {
	set := make(map[string]bool, len(hosts))
	for i, raw := range hosts {
		key, ok := pinEntryKey(raw)
		if !ok {
			return nil, fmt.Errorf("netproxy: pinned host #%d: want a host name or an IP literal, with no port, path or userinfo", i+1)
		}
		set[key] = true
	}
	return set, nil
}

// pinEntryKey keys one pinned host: an IP literal (an IPv6 one bracketed or
// not) or a name, matched as written once canonical — whatever a URL's host
// can hold, never a pattern. A host with a port, a path, userinfo or
// whitespace is refused.
func pinEntryKey(raw string) (string, bool) {
	h := strings.TrimSpace(raw)
	if inner, bracketed := strings.CutPrefix(h, "["); bracketed {
		inner, closed := strings.CutSuffix(inner, "]")
		if a, err := netip.ParseAddr(inner); closed && err == nil && a.Is6() {
			return pinKey(inner), true
		}
		return "", false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return pinKey(h), true
	}
	if strings.ContainsAny(h, ":/@[] \t\r\n") {
		return "", false
	}
	key := pinKey(h)
	return key, key != ""
}
