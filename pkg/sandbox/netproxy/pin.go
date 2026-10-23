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

// pinEntry is one pinned host: how strictly its answers are judged, and
// whether the pin serves the run's own credential — a strictness the
// operator's escape hatch does not lift.
type pinEntry struct {
	strict  bool
	runHeld bool
}

// pinSet is what the pin holds: the pinned hosts, keyed by pinKey, each with
// its own strictness. Options.PinSource and Options.PinRunSource grow it
// while the proxy runs, so it is read and written under its lock.
type pinSet struct {
	mu        sync.Mutex
	hosts     map[string]pinEntry
	source    func() (host string, allowPrivate bool)
	runSource func() (host string, allowPrivate bool)
}

// newPinSet builds the pin over hosts (each at the given default
// strictness) and reads its sources once, so the hosts they name when the
// proxy is built are pinned from the start.
func newPinSet(hosts map[string]bool, allowPrivate bool, source func() (string, bool), runSource func() (string, bool)) *pinSet {
	entries := make(map[string]pinEntry, len(hosts))
	for key := range hosts {
		entries[key] = pinEntry{strict: !allowPrivate}
	}
	s := &pinSet{hosts: entries, source: source, runSource: runSource}
	s.refresh()
	return s
}

// refresh takes the hosts the sources name, each at ITS source's
// strictness: the environment gateway's entry tracks the hatch answer it
// was named with — the source re-names it at every connection, so the entry
// follows the hatch while the environment keeps serving it, and freezes at
// its last value when the source names nothing; a run-held credential's
// entry follows its source's answer, live — the operator's inherited lift
// admits and withdraws it — and freezes at its last admission when the
// source names nothing. The caller holds the lock, or has not shared the
// set yet.
func (s *pinSet) refresh() {
	if s.source != nil {
		named, allowPrivate := s.source()
		if key, ok := pinEntryKey(named); ok {
			s.hosts[key] = pinEntry{strict: !allowPrivate}
		}
	}
	if s.runSource != nil {
		if named, allowPrivate := s.runSource(); named != "" {
			if key, ok := pinEntryKey(named); ok {
				s.hosts[key] = pinEntry{strict: !allowPrivate, runHeld: true}
			}
		}
	}
}

// sync refreshes the set from its source.
func (s *pinSet) sync() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
}

// lookup refreshes the set from its sources, then reports whether host —
// bare or with its port — is pinned, and its own entry. A host a source has
// named once stays pinned.
func (s *pinSet) lookup(host string) (pinEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	entry, pinned := s.hosts[pinKey(host)]
	return entry, pinned
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
		entry, pinned := pins.lookup(host)
		if !pinned {
			return dial(ctx, network, addr)
		}
		bounded, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		ip, err := httpdial.ResolvePublicHostWith(bounded, res, host, entry.strict)
		if err != nil {
			if ctx.Err() == nil {
				kind := "pinned host"
				if entry.runHeld {
					kind = "pinned host (the run's gateway credential)"
				}
				p.reportBlocked(pinKey(host), kind+": "+err.Error())
			}
			if entry.runHeld {
				return nil, fmt.Errorf("netproxy: pinned host %q serves the run's gateway credential — its pin answers the operator's inherited lift, never a planted one: %w", host, err)
			}
			return nil, fmt.Errorf("netproxy: pinned host %q: %w", host, err)
		}
		return dial(bounded, network, net.JoinHostPort(ip.String(), port))
	}
}

// CanonicalHost is the canonical form of a host name — the form the pin,
// the policy and the dial all match in (lowercase, no root dot, IDN folded,
// an IP literal in its standard spelling). A guard that judges a host in
// any other form is judged by the spelling its caller happened to write.
func CanonicalHost(host string) string {
	return canonicalHost(host)
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
