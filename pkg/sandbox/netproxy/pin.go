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
// spelling — an IPv4 address written as IPv6 is the IPv4 one.
func pinKey(host string) string {
	c := canonicalHost(host)
	if a, err := netip.ParseAddr(c); err == nil {
		return a.Unmap().String()
	}
	return c
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
