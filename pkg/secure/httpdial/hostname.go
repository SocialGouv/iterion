package httpdial

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// CanonicalHost folds a URL hostname to the name a dial resolves: an
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
// a value nothing resolves an identity it does not have.
//
// The fold is total: a comparison has no refusal to return. pkg/secrets'
// canonicalGatewayHost is the VALIDATOR of this same shape, refusing a host a
// stored credential may not name.
func CanonicalHost(hostname string) string {
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

// asciiOnly reports whether s holds no multi-byte rune.
func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
