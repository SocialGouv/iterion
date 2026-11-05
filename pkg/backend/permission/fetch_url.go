package permission

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// fetchURL is THE reading of a WebFetch URL — every candidate the gate
// matches for a fetched URL, on either side, is derived from it. It is the
// place a fetcher reaches, read the way the WHATWG URL standard reads it:
// claude_code's WebFetch parses its URL with Node's `new URL()`, and claw's Go
// fetcher dials that same host whenever net/url parses the URL at all, so a
// rule judged against this reading is judged against the host and path the
// call reaches on both backends. net/url alone is not that reading: it reads
// `https:e.com/x` as an opaque URL with no host, `https://e.com\x` as no URL
// at all, and `https://E.com:443/x` as a host `E.com:443` — every one of them
// a fetch of https://e.com/x.
type fetchURL struct {
	scheme   string // http or https, lower-cased
	hostname string // lower-cased ASCII; IPv4 in dotted decimal, IPv6 bracketed
	port     string // decimal, "" when absent or the scheme's default
	path     string // percent-decoded, `\` read as `/`, dot segments as sent
	query    string // as sent, without its `?`
	hasQuery bool
}

// readFetchURL reads a URL the way a WHATWG fetcher does, reporting false
// when it cannot: a scheme other than http(s), or an authority with no host
// the standard accepts. What it reads:
//
//   - leading and trailing C0 controls and spaces are dropped, and every tab
//     and newline inside the URL is removed (`/.\t./` is `/../`);
//   - the scheme is case-folded, and any run of `/` or `\` after it opens the
//     authority (`https:e.com`, `https:/e.com`, `https:///e.com`);
//   - `\` is a separator wherever `/` is, the query and fragment excepted;
//   - the userinfo is dropped (the host follows the last `@`), the host is
//     percent-decoded, mapped through IDNA, case-folded, and read as an IPv4
//     address when its last label is a number (`0xa9fea9fe` is
//     169.254.169.254); an IPv6 host is serialised canonically;
//   - the port is read as a number and dropped when it is the scheme's
//     default (`:443`, `:0443` on https);
//   - the path is percent-decoded, `%2e%2e` and `%2f` included, the way a
//     server reads it.
//
// A reading this lenient may accept a URL the standard refuses; that URL is
// one no WHATWG fetcher fetches, so no verdict over it reaches anything. The
// converse — a URL the standard reads and this refuses — is the one that
// matters, and the gate fails closed on it (candidates.unread).
func readFetchURL(raw string) (fetchURL, bool) {
	s := strings.TrimFunc(raw, func(r rune) bool { return r <= ' ' })
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || !validURLScheme(s[:colon]) {
		return fetchURL{}, false
	}
	f := fetchURL{scheme: strings.ToLower(s[:colon])}
	if fetchDefaultPort(f.scheme) == "" {
		return fetchURL{}, false
	}
	rest := s[colon+1:]
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		tail := rest[i:]
		rest = rest[:i]
		if tail[0] == '?' {
			q, _, _ := strings.Cut(tail[1:], "#")
			f.query, f.hasQuery = q, true
		}
	}
	rest = strings.TrimLeft(strings.ReplaceAll(rest, `\`, "/"), "/")
	authority, p := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, p = rest[:i], rest[i:]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = authority[at+1:]
	}
	host, port := splitFetchAuthority(authority)
	hostname, ok := readFetchHost(host)
	if !ok {
		return fetchURL{}, false
	}
	f.hostname = hostname
	if f.port, ok = readFetchPort(port, f.scheme); !ok {
		return fetchURL{}, false
	}
	f.path = strings.ReplaceAll(percentDecode(p), `\`, "/")
	return f, true
}

// host is the URL's authority as a rule names it: the hostname, and the port
// when it is not the scheme's default.
func (f fetchURL) host() string {
	if f.port == "" {
		return f.hostname
	}
	return f.hostname + ":" + f.port
}

// climbs reports whether the decoded path carries a `..` segment: a fetcher
// resolves it before the server sees the request, so the URL's text names a
// directory the fetch is not under.
func (f fetchURL) climbs() bool { return pathClimbs(f.path) }

// resolved renders the location the fetch reaches once its dot segments and
// repeated separators are resolved — the spelling a closing rule names
// (`deny: ["WebFetch(https://e.com/admin/**)"]`) that `https://e.com/./%61dmin/x`
// reaches without carrying. The lexical cleanup is one reading among the
// fetchers' and servers', which is why only a closing verdict reads it.
func (f fetchURL) resolved() string {
	clean := path.Clean("/" + f.path)
	last := f.path[strings.LastIndex(f.path, "/")+1:]
	if clean != "/" && (strings.HasSuffix(f.path, "/") || last == "." || last == "..") {
		clean += "/"
	}
	u := url.URL{Scheme: f.scheme, Host: f.host(), Path: clean, RawQuery: f.query, ForceQuery: f.hasQuery && f.query == ""}
	return u.String()
}

// upgraded is the URL a fetcher that upgrades http to https reaches — what
// claude_code's WebFetch does with every http URL. A port that becomes the
// new scheme's default is dropped, as the WHATWG scheme setter drops it.
func (f fetchURL) upgraded() (fetchURL, bool) {
	if f.scheme != "http" {
		return fetchURL{}, false
	}
	f.scheme = "https"
	if f.port == fetchDefaultPort(f.scheme) {
		f.port = ""
	}
	return f, true
}

// unmapped is the fetch an IPv4-mapped IPv6 host reaches: the WHATWG reading
// keeps `[::ffff:169.254.169.254]` an IPv6 host (`[::ffff:a9fe:a9fe]`), and
// both backends' fetchers — Node's and Go's — dial the IPv4 address it maps,
// 169.254.169.254. Only the mapped prefix (::ffff:0:0/96) is read that way:
// it is the one a socket connects to over IPv4 whatever the network.
func (f fetchURL) unmapped() (fetchURL, bool) {
	inner, ok := strings.CutPrefix(f.hostname, "[")
	if !ok {
		return fetchURL{}, false
	}
	a, err := netip.ParseAddr(strings.TrimSuffix(inner, "]"))
	if err != nil || !a.Is4In6() {
		return fetchURL{}, false
	}
	f.hostname = a.Unmap().String()
	return f, true
}

// validURLScheme reports a WHATWG scheme: an ASCII letter, then letters,
// digits, `+`, `-` and `.`.
func validURLScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// fetchDefaultPort is the port a fetched scheme implies, "" for a scheme no
// WebFetch backend fetches.
func fetchDefaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// splitFetchAuthority splits a host from its port at the first `:` outside
// an IPv6 literal's brackets.
func splitFetchAuthority(authority string) (host, port string) {
	inBrackets := false
	for i := 0; i < len(authority); i++ {
		switch authority[i] {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				return authority[:i], authority[i+1:]
			}
		}
	}
	return authority, ""
}

// readFetchPort reads a port as the number a fetcher dials: digits only, at
// most 65535, "" when absent or the scheme's default.
func readFetchPort(port, scheme string) (string, bool) {
	if port == "" {
		return "", true
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return "", false
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", false
	}
	if dialled := strconv.FormatUint(n, 10); dialled != fetchDefaultPort(scheme) {
		return dialled, true
	}
	return "", true
}

// whatwgIDNA is UTS #46 ToASCII with the flags the WHATWG URL standard's
// "domain to ASCII" sets: non-transitional, CheckBidi and CheckJoiners on,
// CheckHyphens and UseSTD3ASCIIRules off, no DNS length check.
var whatwgIDNA = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.VerifyDNSLength(false),
)

// readFetchHost reads a special URL's host: an IPv6 literal, or a domain —
// percent-decoded, IDNA-mapped when it is not ASCII (mapped first and
// case-folded after, the order a resolver's name comes from), refused when it
// holds a code point no domain may — that is read as an IPv4 address when
// its last label is a number.
func readFetchHost(h string) (string, bool) {
	if h == "" {
		return "", false
	}
	if strings.HasPrefix(h, "[") {
		if !strings.HasSuffix(h, "]") {
			return "", false
		}
		a, err := netip.ParseAddr(h[1 : len(h)-1])
		if err != nil || !a.Is6() || a.Zone() != "" {
			return "", false
		}
		return "[" + ipv6String(a) + "]", true
	}
	domain := percentDecode(h)
	if !utf8.ValidString(domain) {
		return "", false
	}
	for i := 0; i < len(domain); i++ {
		if domain[i] >= utf8.RuneSelf {
			ascii, err := whatwgIDNA.ToASCII(domain)
			if err != nil {
				return "", false
			}
			domain = ascii
			break
		}
	}
	domain = strings.ToLower(domain)
	if domain == "" || strings.ContainsFunc(domain, forbiddenDomainRune) {
		return "", false
	}
	if hostEndsInANumber(domain) {
		return ipv4Host(domain)
	}
	return domain, true
}

// forbiddenDomainRune reports a WHATWG forbidden domain code point.
func forbiddenDomainRune(r rune) bool {
	return r <= ' ' || r == 0x7f || strings.ContainsRune("#%/:<>?@[\\]^|", r)
}

// hostEndsInANumber is the WHATWG "ends in a number" check: the host's last
// label (one trailing dot aside) is all digits or parses as an IPv4 number.
func hostEndsInANumber(host string) bool {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return false
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	_, ok := ipv4Number(last)
	return ok
}

// ipv4Number parses one part of an IPv4 host — decimal, `0x` hexadecimal or
// `0`-led octal. A value past uint64 reads as math.MaxUint64: too large for
// any part, which is the verdict the standard reaches on it.
func ipv4Number(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X"):
		s, base = s[2:], 16
	case len(s) >= 2 && s[0] == '0':
		s, base = s[1:], 8
	}
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return math.MaxUint64, true
		}
		return 0, false
	}
	return n, true
}

// ipv4Host parses a host that ends in a number as the WHATWG IPv4 parser
// does — one to four parts, the last filling the remaining bytes — and prints
// it in dotted decimal.
func ipv4Host(host string) (string, bool) {
	parts := strings.Split(host, ".")
	if parts[len(parts)-1] == "" && len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := ipv4Number(p)
		if !ok {
			return "", false
		}
		nums[i] = n
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-len(nums))) {
		return "", false
	}
	ip := last
	for i, n := range nums[:len(nums)-1] {
		if n > 255 {
			return "", false
		}
		ip += n << (8 * (3 - i))
	}
	return fmt.Sprintf("%d.%d.%d.%d", ip>>24, ip>>16&0xff, ip>>8&0xff, ip&0xff), true
}

// ipv6String serialises an IPv6 address as the WHATWG URL standard does:
// lower-case hexadecimal pieces, the first longest run of two or more zero
// pieces compressed — an IPv4-mapped address included, which netip would
// print dotted.
func ipv6String(a netip.Addr) string {
	b := a.As16()
	var pieces [8]uint16
	for i := range pieces {
		pieces[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	compress, longest := -1, 1
	for i := 0; i < len(pieces); {
		j := i
		for j < len(pieces) && pieces[j] == 0 {
			j++
		}
		if j-i > longest {
			compress, longest = i, j-i
		}
		i = max(j, i+1)
	}
	var sb strings.Builder
	for i := 0; i < len(pieces); i++ {
		if i == compress {
			if i == 0 {
				sb.WriteString("::")
			} else {
				sb.WriteString(":")
			}
			i += longest - 1
			continue
		}
		sb.WriteString(strconv.FormatUint(uint64(pieces[i]), 16))
		if i != len(pieces)-1 {
			sb.WriteString(":")
		}
	}
	return sb.String()
}

// percentDecode decodes every well-formed `%XX` escape and keeps a malformed
// one as written, as the WHATWG percent-decoder does.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			b.WriteByte(unhexDigit(s[i+1])<<4 | unhexDigit(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhexDigit(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
