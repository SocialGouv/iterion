package secrets

import (
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// ---------------------------------------------------------------------------
// Endpoint identity: which spellings of a base URL name the same destination.
//
// Every credential-routing verdict in the tree asks that one question — the
// facade-substitution claim against FacadeVendorEndpoints
// (delegate.OperatorEndpointClaim, and the registry's twin), the Claude
// forfait's wire pin (AnthropicForfaitWireOK) — so they ask it HERE. A fold
// of its own per verdict drifts: a spelling one reads as the vendor and the
// other as the operator's destination is exactly the mislabel the
// substitution refusal exists to stop.
// ---------------------------------------------------------------------------

// SameEndpoint reports whether two base-URL spellings name the same
// destination.
//
// The routing identity of an endpoint is its scheme, its authority and its
// request path. What carries no routing meaning folds away:
//
//   - surrounding whitespace, and a trailing slash;
//   - userinfo — it AUTHENTICATES the call, the host is what routes it.
//     Read through net/url, never by hand: in
//     "https://api.z.ai@evil.example/" the vendor's name is the USERINFO and
//     evil.example is the host, so dropping it leaves evil.example;
//   - a fragment, which never leaves the client, and a query, which no
//     vendor publishes in a base URL;
//   - the host's case and its internationalised spelling (folded to the
//     punycode net/http dials), and a trailing root-zone dot — "api.z.ai."
//     is the absolute form of the same name;
//   - the port the scheme implies, read as the number a dial resolves it to
//     (":443" and ":0443" on https, ":80" on http);
//   - the path's case, its percent-escapes and its empty and dot segments
//     ("/api//anthropic", "/api/./anthropic", "/api/anthropi%63").
//
// What never folds is the authority: another host, a non-default port,
// another scheme, or a host that merely CONTAINS the other's name
// ("api.z.ai.evil.example") is another destination and compares unequal. So
// the fold is safe in one direction by construction — it can only change the
// verdict of a comparison whose two sides already share one canonical
// authority. Widening it never turns a destination of the operator's into
// the vendor's unless the operator named the vendor's own authority.
//
// A string that does not parse as an absolute URL keeps the plain
// comparison — case, spacing, trailing slash — which is all a value with no
// structure to read offers.
func SameEndpoint(a, b string) bool {
	return CanonicalEndpoint(a) == CanonicalEndpoint(b)
}

// CanonicalEndpoint returns the routing identity SameEndpoint compares: the
// one spelling every spelling of the same destination folds to. Its own
// output is such a spelling, so the fold is idempotent.
func CanonicalEndpoint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	// A value with no URL structure: case and a trailing slash are the only
	// respellings there are to fold.
	plain := strings.TrimRight(strings.ToLower(trimmed), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return plain
	}
	host := canonicalEndpointHost(parsed.Hostname())
	if host == "" {
		return plain
	}
	if strings.Contains(host, ":") && net.ParseIP(host) != nil {
		host = "[" + host + "]" // an IPv6 literal keeps its brackets in an authority
	}
	if port := canonicalEndpointPort(parsed.Port(), parsed.Scheme); port != "" {
		host += ":" + port
	}
	// Scheme and path survive — the first decides the transport and the
	// implied port, the second names the resource at the destination. Only
	// their SPELLING folds: RawPath is left unset so String() re-escapes the
	// decoded path minimally, and cleanEndpointPath collapses the segments a
	// server resolves itself.
	folded := url.URL{
		Scheme: strings.ToLower(parsed.Scheme),
		Host:   host,
		Path:   cleanEndpointPath(parsed.Path),
	}
	return strings.TrimRight(folded.String(), "/")
}

// EndpointHasHost reports whether an endpoint spelling carries a host the
// routing verdicts can read: it parses as an absolute URL and its authority
// names a host whose canonical dial name is not empty.
//
// The spellings it refuses — a scheme-relative path ("https:/api.z.ai/…",
// which the WHATWG parser the claude CLI spawns reads as an authority), an
// empty authority ("https:///…"), backslashes in place of the double slash,
// a control character or a refused percent-escape in the host — have no
// dial name to reason about: a caller deciding whose endpoint a value is
// must treat them as no endpoint at all, never as "unparseable, therefore
// the operator's".
func EndpointHasHost(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return false
	}
	return canonicalEndpointHost(parsed.Hostname()) != ""
}

// cleanEndpointPath folds a decoded URL path to the target it names: case,
// repeated slashes and "."/".." segments, which every HTTP server resolves
// before it routes. A path that cleans to the root carries nothing.
//
// The decoded form is what is cleaned, so an encoded separator ("%2F") reads
// as the separator it decodes to. A base URL spelling one is already outside
// what any vendor publishes, and the conflation can only ever fold such a
// spelling ONTO the authority it already names.
func cleanEndpointPath(p string) string {
	if p == "" {
		return ""
	}
	cleaned := path.Clean(strings.ToLower(p))
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return cleaned
}

// canonicalEndpointHost folds a URL hostname to the name a dial resolves: an
// internationalised spelling mapped to punycode as net/http maps it before
// dialling, lower-cased AFTER that mapping, and trailing root-zone dots
// dropped.
//
// The order is load-bearing. Go's own lower-casing of U+0130 'İ' is an ASCII
// 'i' where the IDN mapping is 'i' + U+0307: fold first and the comparison
// reads the name the transport dials, lower-case first and it reads a name
// nothing dials. pkg/sandbox/netproxy's allowlist matcher folds in that same
// order, for that same reason.
//
// A plain-ASCII name the IDN profile refuses — an underscore, a literal
// address — is read as written, lower-cased: that is what gets dialled. A
// non-ASCII name it refuses has no readable dial name and folds to "", which
// leaves the caller comparing the raw spelling.
//
// Whitespace is not folded out of a name: net/url refuses an ASCII space in a
// host outright, and a non-ASCII space is a rune the IDN profile disallows, so
// a space-bearing spelling has no dial name to read — trimming one would hand
// the vendor's own identity to a value nothing resolves.
//
// canonicalGatewayHost is the VALIDATOR of this same shape, refusing a host a
// stored credential may not name. This one is total: a comparison has no
// refusal to return.
func canonicalEndpointHost(hostname string) string {
	h := hostname
	if h == "" {
		return ""
	}
	if ascii, err := idna.Lookup.ToASCII(h); err == nil && ascii != "" {
		h = ascii
	} else if !asciiOnly(h) {
		return ""
	}
	return strings.TrimRight(strings.ToLower(h), ".")
}

// canonicalEndpointPort folds an explicit port to the decimal the transport
// dials it as, and to "" when that is the port the scheme implies.
//
// net.LookupPort IS that rule: net.Dial resolves a port through it, so ":443"
// and ":0443" are one route on https. url.Parse has already refused every
// spelling but digits, which is the fast path of that lookup — arithmetic, no
// resolver and no /etc/services. ValidateGatewayBaseURL folds a stored
// gateway's port through the same number, which is where this reading of a
// port already lived.
//
// A number the dialler refuses — over 65535, which url.Parse accepts — is kept
// as written: nothing dials it, and keeping the spelling leaves it comparing
// equal to itself alone instead of collapsing two unroutable spellings into
// one endpoint.
func canonicalEndpointPort(port, scheme string) string {
	if port == "" {
		return ""
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil {
		return port
	}
	if dialled := strconv.Itoa(n); dialled != schemeDefaultPort(scheme) {
		return dialled
	}
	return ""
}

// schemeDefaultPort is the port a URL scheme implies — the one an explicit
// spelling of it does not have to carry to name the same endpoint.
func schemeDefaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// asciiOnly reports whether s holds no multi-byte rune.
func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
