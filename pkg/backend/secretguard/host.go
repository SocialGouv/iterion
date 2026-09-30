package secretguard

import (
	"math"
	"strings"
)

// MaterializeForHost is MaterializeForHostWithin unbounded: the
// convenience for callers that do not hold the result in memory. The proxy,
// which holds every substitution, uses the bounded call.
func (g *Guard) MaterializeForHost(s, host string) string {
	out, _ := g.MaterializeForHostWithin(s, host, math.MaxInt)
	return out
}

// MaterializeForHostWithin swaps secret placeholders for their real values,
// but ONLY for secrets whose Hosts permit `host` (empty Hosts = any host).
// This is the egress-substitution half of Layer 2 (Deno-style host scoping):
// a placeholder destined for a host the secret isn't scoped to is left
// untouched, so it never resolves there.
//
// The caller holds the result in memory, so it is bounded: a result longer
// than limit bytes is refused (s returned as given, false). A placeholder
// expands to its value, so a short input can stand for a long result — the
// length is computed before each substitution allocates it. Nil-safe.
func (g *Guard) MaterializeForHostWithin(s, host string, limit int) (string, bool) {
	if len(s) > limit {
		return s, false
	}
	if g == nil || s == "" {
		return s, true
	}
	in := s
	host = canonicalHostname(host)
	// The secrets that shrink go first, so the room they free is available
	// to the ones that grow: the verdict is the result's length, never the
	// order two substitutions happen to be declared in.
	for _, grows := range [2]bool{false, true} {
		for _, sec := range g.secrets {
			if sec.RedactOnly || grows != (len(sec.Value) > len(sec.Placeholder)) || !hostAllowed(sec.Hosts, host) {
				continue
			}
			// A value may carry another secret's placeholder, so the count
			// is taken on the current text, right before its substitution.
			n := strings.Count(s, sec.Placeholder)
			if n == 0 {
				continue
			}
			if grow := len(sec.Value) - len(sec.Placeholder); grow > 0 && n > (limit-len(s))/grow {
				return in, false
			}
			s = strings.ReplaceAll(s, sec.Placeholder, sec.Value)
		}
	}
	return s, true
}

// ExfiltratesTo reports whether s carries a real secret value (in any
// encoding) whose Hosts do NOT permit `host` — a secret leaving toward
// an unapproved destination. This is the deterministic egress DLP gate
// (Layer 2): it fires only on values we are certain about and only when
// the destination is out of scope, so legitimate use toward an approved
// host is never blocked. Nil-safe.
func (g *Guard) ExfiltratesTo(s, host string) bool {
	if g == nil || s == "" {
		return false
	}
	// Fast path: the precompiled matcher spans every encoding of every
	// known secret. No match → no secret value is present at all, so
	// nothing can be exfiltrating — skip the per-secret/per-encoding scan
	// (this is the common case on the proxy hot path).
	if g.matcher == nil || !g.matcher.MatchString(s) {
		return false
	}
	host = canonicalHostname(host)
	for i, sec := range g.secrets {
		if hostAllowed(sec.Hosts, host) {
			continue // this destination is approved for this secret
		}
		for _, enc := range g.encodings[i] {
			if strings.Contains(s, enc) {
				return true
			}
		}
	}
	return false
}

// hostAllowed reports whether host is permitted by a secret's Hosts
// list. An empty list means "no restriction" (allowed everywhere). A
// pattern matches the host exactly or as a parent domain (so
// "github.com" permits "api.github.com").
func hostAllowed(hosts []string, host string) bool {
	if len(hosts) == 0 {
		return true
	}
	for _, h := range hosts {
		if hostMatch(canonicalHostname(h), host) {
			return true
		}
	}
	return false
}

func hostMatch(pattern, host string) bool {
	if pattern == "" {
		return false
	}
	if pattern == host {
		return true
	}
	// Parent-domain match: pattern "github.com" permits "api.github.com".
	return strings.HasSuffix(host, "."+pattern)
}

// canonicalHostname lowercases and strips a trailing :port (and IPv6
// brackets) so policy comparisons are stable.
func canonicalHostname(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	// Strip IPv6 brackets: [::1]:443 → ::1
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i >= 0 {
			return h[1:i]
		}
	}
	// Strip :port (only when the colon isn't part of a bare IPv6).
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[:i], ":") {
		return h[:i]
	}
	return h
}
