package secretguard

import "strings"

// MaterializeForHost swaps secret placeholders for their real values,
// but ONLY for secrets whose Hosts permit `host` (empty Hosts = any
// host). This is the egress-substitution half of Layer 2 (Deno-style
// host scoping): a placeholder destined for a host the secret isn't
// scoped to is left untouched, so it never resolves there. Nil-safe.
func (g *Guard) MaterializeForHost(s, host string) string {
	if g == nil || s == "" {
		return s
	}
	host = canonicalHostname(host)
	for _, sec := range g.secrets {
		if sec.RedactOnly || !hostAllowed(sec.Hosts, host) {
			continue
		}
		if strings.Contains(s, sec.Placeholder) {
			s = strings.ReplaceAll(s, sec.Placeholder, sec.Value)
		}
	}
	return s
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
