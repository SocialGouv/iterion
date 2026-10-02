package secretguard

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"
)

// encodingsOf returns the distinct textual representations a secret
// value can take on the wire or in a log. This is the heart of the
// "detect base64 / other formats" requirement: rather than guessing
// whether an arbitrary high-entropy blob is a secret, we precompute
// the encodings of the secrets we KNOW and match those literally.
//
// The raw value is always first; the remaining forms are the common
// ways a value gets reshaped before it lands in a request body, a
// JSON event, or a URL:
//   - base64 std / url, with and without padding
//   - hex, lower and upper case
//   - URL query / path percent-encoding
//   - JSON string escaping (the body between the quotes)
//
// Duplicates (an encoding equal to the raw value, or to a prior
// encoding) are dropped so the matcher stays minimal.
func encodingsOf(v string) []string {
	if v == "" {
		return nil
	}
	out := make([]string, 0, 12)
	seen := make(map[string]struct{}, 12)
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	add(v) // raw

	b := []byte(v)
	add(base64.StdEncoding.EncodeToString(b))
	add(base64.RawStdEncoding.EncodeToString(b))
	add(base64.URLEncoding.EncodeToString(b))
	add(base64.RawURLEncoding.EncodeToString(b))

	hl := hex.EncodeToString(b)
	add(hl)
	add(strings.ToUpper(hl))

	add(url.QueryEscape(v))
	add(url.PathEscape(v))
	// A URL written back by a JS runtime (a fetched URL a tool reports):
	// query (special and not), path — with `^`, which Bun and Node encode
	// there too — fragment, userinfo, and encodeURIComponent's set.
	add(whatwgEscape(v, " \"#<>'"))
	add(whatwgEscape(v, " \"#<>"))
	add(whatwgEscape(v, " \"#<>?`{}"))
	add(whatwgEscape(v, " \"#<>?^`{}"))
	add(whatwgEscape(v, " \"<>`"))
	add(whatwgEscape(v, " \"#<>?^`{}/:;=@[\\]|"))
	add(whatwgEscape(v, " \"#<>?^`{}/:;=@[\\]|$%&+,"))
	// The WHATWG application/x-www-form-urlencoded serializer
	// (URLSearchParams, a form body; Java's URLEncoder): every byte but ASCII
	// alphanumerics and *-._ percent-encoded, a space as "+" — Go's
	// QueryEscape differs on "*" and "~".
	add(formURLEncode(v))
	// RFC 3986 strict (curl's escape, Python's quote(safe=""), PHP's rawurlencode):
	// Go's QueryEscape with a space as %20 — a literal "+" is already %2B —
	// and Python's quote() default, which keeps "/".
	strict := strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
	add(strict)
	add(strings.ReplaceAll(strict, "%2F", "/"))

	// JSON string escaping: the escaped body between the quotes (embedded
	// `\"` / `\n` / `\\`), as Go writes it and as JSON.stringify or jq do
	// (no HTML escaping of & < >).
	for _, body := range jsonBodies(v) {
		add(body)
	}

	return out
}

// tryDecode attempts to interpret tok as base64 (std/url, padded or
// not) or hex, returning the decoded string when it yields mostly
// printable text. It is used by the recursive-decode heuristic to
// peel one encoding layer off an UNKNOWN blob and re-scan the inner
// bytes for token shapes (e.g. an AKIA key wrapped in base64).
//
// An empty return means "not a useful decode" — the caller leaves the
// token untouched.
// decodeAttempts is the fixed set of decoders tryDecode walks, hoisted
// to package scope so it isn't reallocated on every base64-shaped token.
var decodeAttempts = []func(string) ([]byte, error){
	base64.RawStdEncoding.DecodeString,
	base64.StdEncoding.DecodeString,
	base64.RawURLEncoding.DecodeString,
	base64.URLEncoding.DecodeString,
	hex.DecodeString,
}

func tryDecode(tok string) string {
	if len(tok) < 12 {
		return ""
	}
	for _, dec := range decodeAttempts {
		raw, err := dec(tok)
		if err != nil || len(raw) < 8 {
			continue
		}
		if !mostlyPrintable(raw) {
			continue
		}
		return string(raw)
	}
	return ""
}

// mostlyPrintable reports whether b is valid UTF-8 made up mostly of
// printable characters — the signal that a decode produced text (a
// nested token) rather than binary garbage.
func mostlyPrintable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	printable, total := 0, 0
	for _, r := range string(b) {
		total++
		if r == '\t' || r == '\n' || r == '\r' || r >= 0x20 {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= 0.85
}

// jsonBodies returns the JSON string bodies of v: HTML-escaped (Go's
// default) and not.
func jsonBodies(v string) []string {
	var out []string
	if j, err := json.Marshal(v); err == nil && len(j) >= 2 {
		out = append(out, string(j[1:len(j)-1]))
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err == nil {
		if j := strings.TrimSuffix(b.String(), "\n"); len(j) >= 2 && (len(out) == 0 || j[1:len(j)-1] != out[0]) {
			out = append(out, j[1:len(j)-1])
		}
	}
	return out
}

// formURLEncode is the WHATWG application/x-www-form-urlencoded byte
// serializer.
func formURLEncode(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// whatwgEscape percent-encodes C0 controls, bytes >= 0x7F and the bytes of
// set, the way a WHATWG URL serialiser writes a query (set: space, `"`, `#`,
// `<`, `>`, `'`) or a path (space, `"`, `#`, `<`, `>`, `?`, backtick, `{`,
// `}`).
func whatwgEscape(v, set string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c >= 0x7F || strings.IndexByte(set, c) >= 0 {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
