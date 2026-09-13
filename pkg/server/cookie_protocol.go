package server

// The session cookie names are part of the wire protocol between this server
// and its OUT-OF-PROCESS clients: the desktop app reads them off Set-Cookie to
// harvest a rotated refresh token, and strips them so they never reach its
// webview. They are exported so the tree holds one spelling of each.
//
// It was two. The desktop kept private copies of the literals, so when the
// server began emitting the `__Host-` form its harvest matched nothing,
// silently kept the PREVIOUS refresh token, and replayed it on the next hop —
// which the server correctly reads as token reuse and answers by revoking
// every session that user holds, on every device. Green tests on both sides;
// the defect lived in the gap between them.
const (
	// AuthCookieName is the access-JWT cookie's base name.
	AuthCookieName = authCookieName
	// RefreshCookieName is the refresh-token cookie's base name.
	RefreshCookieName = refreshCookieName
	// HostCookiePrefix is the prefix a deployment adds when it can meet the
	// prefix's terms (Secure, Path=/, no Domain). See Server.usesHostPrefix.
	HostCookiePrefix = hostCookiePrefix
)

// SessionCookieMatches reports whether a cookie name is `base` in either
// spelling this server may emit. Clients that recognise a session cookie by
// name MUST go through this rather than comparing to a literal — during the
// migration a deployment emits one form and a browser may still hold the
// other.
func SessionCookieMatches(name, base string) bool {
	return name == base || name == HostCookiePrefix+base
}

// SessionCookieSpellings returns every name `base` can arrive under, the
// prefixed form first — the order a reader should prefer, since the prefixed
// cookie is the one no sibling host could have written.
func SessionCookieSpellings(base string) []string {
	return []string{HostCookiePrefix + base, base}
}
