// Package netproxy implements iterion's HTTP CONNECT proxy that
// enforces the workflow's sandbox network policy.
//
// The proxy runs as a goroutine on the host (engine side) and is
// joined by every sandboxed container via HTTPS_PROXY/HTTP_PROXY env
// vars. By default it does NOT terminate TLS — only the CONNECT
// host:port is inspected — so host filtering works without minting a
// CA or injecting trust into the container. This is a cost/simplicity
// choice, not a cert-pinning constraint: the clients iterion runs
// (Claude Code, the Anthropic/OpenAI SDKs) are standard trust-store
// clients with no pinning and work behind a TLS-inspecting proxy once
// its CA is trusted — which is how the opt-in inspection mode (Layer 2
// secret egress substitution) operates.
//
// Pattern semantics, copied from the design plan (.plans/...,§5):
//
//	*.example.com    — exactly one DNS label
//	**.example.com   — one or more labels (greedy, dots allowed)
//	**               — any host (the "open" sentinel)
//	literal          — exact case-insensitive host match
//	!pattern         — exclusion (negation)
//	1.2.3.4          — IPv4 literal exact match
//	10.0.0.0/8       — CIDR range (only for IP literal rules)
//
// Evaluation: rules walk top-to-bottom, last-match-wins. A host that
// matches no rule falls back to the configured Mode default
// (allowlist denies, denylist allows, open accepts everything).
//
// IP-literal hosts (4-tuple) and bare IPs are compared after a
// failed DNS-label match. They are refused by default unless a rule
// explicitly lists them — that closes the cloud-metadata exfiltration
// vector (169.254.169.254 etc.).
package netproxy

import (
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// Mode is the egress default for unmatched hosts.
type Mode string

const (
	ModeAllowlist Mode = "allowlist"
	ModeDenylist  Mode = "denylist"
	ModeOpen      Mode = "open"
)

// Policy is a compiled set of rules with a fallback Mode.
//
// Construct via [Compile]. Reuse the same Policy across many host
// checks — the rule list is read-only and cheap to evaluate.
type Policy struct {
	mode  Mode
	rules []rule
}

type rule struct {
	negate  bool
	literal string     // empty when ipNet != nil
	cidr    *net.IPNet // nil when literal != ""
	// glob fields (compiled from literal):
	suffix string // labels after the wildcard (e.g. "example.com")
	star   string // "*" (single label) or "**" (multi-label) or ""
}

// Compile parses the rule list into a [Policy]. An empty Mode defaults
// to [ModeAllowlist] (the safer choice). Invalid rules are reported
// as errors so callers can surface them at compile time rather than
// at proxy connect time.
func Compile(mode Mode, rules []string) (*Policy, error) {
	if mode == "" {
		mode = ModeAllowlist
	}
	switch mode {
	case ModeAllowlist, ModeDenylist, ModeOpen:
	default:
		return nil, &ErrInvalidMode{Mode: mode}
	}
	p := &Policy{mode: mode}
	for _, raw := range rules {
		r, err := compileRule(raw)
		if err != nil {
			return nil, err
		}
		p.rules = append(p.rules, r)
	}
	return p, nil
}

// Allow reports whether the host is permitted by the policy.
//
// host is the raw value from a CONNECT line ("api.anthropic.com:443")
// or HTTP request — port suffix and surrounding whitespace are
// stripped before matching. Empty input is denied by all modes
// except open.
func (p *Policy) Allow(host string) bool {
	host = canonicalHost(host)
	if host == "" {
		return p.mode == ModeOpen
	}
	matched := false
	allowed := p.mode != ModeAllowlist
	for _, r := range p.rules {
		if r.matches(host) {
			matched = true
			allowed = !r.negate
		}
	}
	if !matched {
		switch p.mode {
		case ModeAllowlist:
			// IP literals are refused by default in allowlist mode
			// (no implicit DNS match). Already false; keep it.
			return false
		case ModeDenylist:
			return true
		case ModeOpen:
			return true
		}
	}
	return allowed
}

// canonicalHost lowercases, trims whitespace, strips a trailing :port
// and a trailing root-zone dot, and folds IDN labels to their ASCII
// (Punycode) form so a rule of `*.example.com` matches both `example.com.`
// and the IDN equivalent `пример.example.com`. Returns "" for
// malformed inputs.
func canonicalHost(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	// IPv6 in CONNECT: [::1]:443
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return ""
		}
		s = s[1:end]
	} else if i := strings.LastIndex(s, ":"); i >= 0 {
		// Distinguish "host:port" from "v6literal" — the v6 case is
		// handled above; bare v6 without brackets isn't supported.
		if !strings.Contains(s[:i], ":") {
			s = s[:i]
		}
	}
	// Trailing-dot ("example.com.") is the absolute-FQDN form. A
	// resolver treats it identically to "example.com" but our pattern
	// matcher uses literal string suffix comparison, so without the
	// trim a `*.example.com` rule would silently miss this variant.
	s = strings.TrimSuffix(s, ".")
	// IDN fold: an attacker can otherwise bypass an ASCII allowlist by
	// supplying the Unicode form of an allowed host. idna.Lookup
	// follows the strict "lookup" profile (no mapping of disallowed
	// codepoints), which is the right side of the conservative/liberal
	// trade-off for a security filter. On error we fall back to the
	// pre-fold value so plain-ASCII hosts (the common case) keep
	// working even if idna's internal tables disagree at the margins.
	if a, err := idna.Lookup.ToASCII(s); err == nil && a != "" {
		s = a
	}
	return s
}

// compileRule turns a raw rule string into a [rule] struct.
func compileRule(raw string) (rule, error) {
	r := rule{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return r, &ErrInvalidRule{Raw: raw, Reason: "empty rule"}
	}
	if strings.HasPrefix(raw, "!") {
		r.negate = true
		raw = strings.TrimSpace(raw[1:])
		if raw == "" {
			return r, &ErrInvalidRule{Raw: "!", Reason: "negation must be followed by a pattern"}
		}
	}
	raw = strings.ToLower(raw)

	// CIDR range
	if strings.Contains(raw, "/") {
		_, ipnet, err := net.ParseCIDR(raw)
		if err != nil {
			return r, &ErrInvalidRule{Raw: raw, Reason: "invalid CIDR: " + err.Error()}
		}
		r.cidr = ipnet
		return r, nil
	}

	// IP literal
	if ip := net.ParseIP(raw); ip != nil {
		r.literal = raw
		return r, nil
	}

	// Glob pattern: at most one * or ** segment.
	if !strings.ContainsAny(raw, "*") {
		r.literal = raw
		return r, nil
	}

	// Find the wildcard position. We accept exactly one wildcard
	// segment ("*.x.com", "**.x.com", "**") for simplicity. Embedded
	// wildcards inside a label ("a*b.com") are rejected — they have
	// no useful semantics for hostname filtering.
	if raw == "**" {
		r.star = "**"
		return r, nil
	}
	if raw == "*" {
		// `*` alone matches a single label — equivalent to a TLD-only
		// match, almost never useful but legal.
		r.star = "*"
		return r, nil
	}
	// Expect "*<sep>rest" or "**<sep>rest"; sep is "."
	switch {
	case strings.HasPrefix(raw, "**."):
		r.star = "**"
		r.suffix = raw[len("**."):]
	case strings.HasPrefix(raw, "*."):
		r.star = "*"
		r.suffix = raw[len("*."):]
	default:
		// Wildcard not in the leading position. Reject — supporting
		// arbitrary positions inflates the surface for little gain
		// (every meaningful hostname pattern leads with the wildcard).
		return r, &ErrInvalidRule{Raw: raw, Reason: "wildcard must be the leading label (e.g. *.example.com or **.example.com)"}
	}
	if r.suffix == "" {
		return r, &ErrInvalidRule{Raw: raw, Reason: "wildcard requires a suffix label"}
	}
	if strings.ContainsAny(r.suffix, "*") {
		return r, &ErrInvalidRule{Raw: raw, Reason: "only one wildcard segment supported"}
	}
	return r, nil
}

// matches reports whether the (already-canonicalised) host matches
// this rule.
func (r rule) matches(host string) bool {
	// CIDR
	if r.cidr != nil {
		ip := net.ParseIP(host)
		return ip != nil && r.cidr.Contains(ip)
	}
	// IP literal
	if r.literal != "" && r.star == "" {
		// Exact match (works for both DNS literals and IP literals).
		return host == r.literal
	}
	// Wildcard
	switch r.star {
	case "**":
		if r.suffix == "" {
			return true // `**` alone
		}
		// match if host equals suffix OR ends with "."+suffix
		return host == r.suffix || strings.HasSuffix(host, "."+r.suffix)
	case "*":
		if r.suffix == "" {
			// "*" alone → exactly one label, no dot
			return !strings.Contains(host, ".")
		}
		// match if host = <label>.<suffix> (exactly one extra label)
		if !strings.HasSuffix(host, "."+r.suffix) {
			return false
		}
		head := strings.TrimSuffix(host, "."+r.suffix)
		return head != "" && !strings.Contains(head, ".")
	}
	return false
}
