package secrets

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/secure/httpdial"
	"golang.org/x/net/idna"
)

// ShapeError is a credential refused at ingestion because its SHAPE could
// not authenticate — never a store or sealing failure. Field names what
// was rejected, Reason why; neither carries the value. Handlers map it to
// 400 and audit it; anything else stays a 500.
type ShapeError struct {
	Field  string
	Reason string
}

func (e *ShapeError) Error() string { return e.Field + " " + e.Reason }

// ValidateTokenShape rejects credential material that could not possibly
// authenticate: a bearer token or api-key value is one run of visible
// characters — never a terminal transcript, a credentials.json paste, a
// CLI banner, or a value with an invisible character glued on. The check
// is format-agnostic on purpose (no vendor prefix pin, which changes over
// time) and covers the paid ingestion failures: an accessToken with
// embedded newlines/ANSI escapes rendering every LLM call
// `Bearer <transcript>` -> 401, and a key with a leading tab/space or a
// no-break space that fools string-equality auth on the server side.
//
// The ASCII cases carry their own wording; every other white-space
// (U+00A0, U+0085, U+2028, U+2029, U+3000, ...) and every non-printing
// rune (U+200B, U+FEFF, controls, format characters) is refused by the
// unicode catch-all, naming the rune and its position. Empty, NUL and
// invalid UTF-8 are refused too. field only phrases the error so an
// operator sees WHAT was rejected (accessToken vs api-key secret).
func ValidateTokenShape(field, value string) error {
	if value == "" {
		return &ShapeError{Field: field, Reason: "is empty"}
	}
	if !utf8.ValidString(value) {
		return &ShapeError{Field: field, Reason: "is not valid UTF-8 — this looks like binary data, not a token"}
	}
	for i, r := range value {
		switch {
		case r == 0x00:
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a NUL byte at position %d — this looks like binary data, not a token", i)}
		case r == '\n' || r == '\r':
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a newline at position %d — this looks like a terminal transcript or credentials.json paste, not a bare token", i)}
		case r == '\t':
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a tab at position %d — strip leading/trailing whitespace before pasting", i)}
		case r == ' ':
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a space at position %d — a bearer token has none", i)}
		case r < 0x20:
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a control character (U+%04X) at position %d — this looks like a terminal transcript, not a bare token", r, i)}
		case r == 0x7f:
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a DEL byte at position %d — strip control characters before pasting", i)}
		case unicode.IsSpace(r):
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a non-ASCII white-space character (U+%04X) at position %d — a bearer token has none; retype it rather than pasting from a rendered page", r, i)}
		case !unicode.IsPrint(r):
			return &ShapeError{Field: field, Reason: fmt.Sprintf("contains a non-printing character (U+%04X) at position %d — an invisible character glued to the token; retype it rather than pasting from a rendered page", r, i)}
		}
	}
	return nil
}

// ValidateAPIKeyShape is the BYOK ingestion gate: a bearer-token provider
// gets ValidateTokenShape; a provider whose credential is a JSON document
// (Provider.CredentialIsJSON — Bedrock, Vertex) must carry a JSON OBJECT,
// which legitimately contains the newlines and spaces the token rule
// refuses.
func ValidateAPIKeyShape(provider Provider, value string) error {
	if provider == ProviderOpenAICompatible {
		if err := ValidateTokenShape("api-key secret", value); err != nil {
			return err
		}
		// A gateway's virtual key is operator-minted and long; a placeholder
		// such as vLLM's "EMPTY" would share its fingerprint with every other
		// placeholder and its row would authenticate nothing worth metering.
		if len(value) < gatewaySecretMinLen {
			return &ShapeError{Field: "api-key secret", Reason: fmt.Sprintf("is too short for a gateway key (%d bytes, minimum %d) — a placeholder cannot be metered", len(value), gatewaySecretMinLen)}
		}
		return nil
	}
	if !provider.CredentialIsJSON() {
		return ValidateTokenShape("api-key secret", value)
	}
	if strings.TrimSpace(value) == "" {
		return &ShapeError{Field: "api-key secret", Reason: "is empty"}
	}
	fields, kind := jsonDocument(value)
	if kind != jsonObject {
		return &ShapeError{Field: "api-key secret", Reason: fmt.Sprintf("must be a JSON credential object for provider %s (an AWS-style credential document, a service-account file), not a bearer token", provider)}
	}
	// An EMPTY object parses and carries nothing: a credential that cannot
	// authenticate is refused at ingestion like any other, rather than
	// discovered at the first call.
	if fields == 0 {
		return &ShapeError{Field: "api-key secret", Reason: fmt.Sprintf("is an empty JSON object — a %s credential document must carry its fields", provider)}
	}
	return nil
}

// jsonDocumentKind classifies what a value parses as at the top level.
type jsonDocumentKind int

const (
	jsonNotADocument jsonDocumentKind = iota
	jsonObject
	jsonArray
)

// jsonDocument parses value as a top-level JSON object or array and
// returns how many members it carries. Shared by every ingestion gate so
// "is this a JSON credential document" has one answer: a scalar, a
// truncated paste or trailing garbage is not a document.
func jsonDocument(value string) (members int, kind jsonDocumentKind) {
	trimmed := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var doc map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &doc) != nil {
			return 0, jsonNotADocument
		}
		return len(doc), jsonObject
	case strings.HasPrefix(trimmed, "["):
		var doc []json.RawMessage
		if json.Unmarshal([]byte(trimmed), &doc) != nil {
			return 0, jsonNotADocument
		}
		return len(doc), jsonArray
	}
	return 0, jsonNotADocument
}

// SecretShapeKind names the shape a stored secret's value must have. The
// generic secret store legitimately holds a PEM key or a JSON document
// as well as bearer tokens, so the bare-token rule cannot be the only
// one — the kind selects which gate applies, and SecretShapeRaw opts out
// explicitly for a value that is none of the three (a passphrase, a
// connection string, a blob).
type SecretShapeKind string

const (
	SecretShapeToken SecretShapeKind = "token"
	SecretShapeJSON  SecretShapeKind = "json"
	SecretShapePEM   SecretShapeKind = "pem"
	SecretShapeRaw   SecretShapeKind = "raw"
)

// SecretShapeKinds lists the accepted kinds, in the order an operator
// meets them in the help text.
var SecretShapeKinds = []SecretShapeKind{SecretShapeToken, SecretShapeJSON, SecretShapePEM, SecretShapeRaw}

// ParseSecretShapeKind resolves an operator-supplied kind. An empty
// string means "infer from the value" and is the caller's business
// (InferSecretShapeKind); an unrecognised one is an error, never a
// silent pass-through to no checking.
func ParseSecretShapeKind(s string) (SecretShapeKind, error) {
	k := SecretShapeKind(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range SecretShapeKinds {
		if k == known {
			return k, nil
		}
	}
	names := make([]string, 0, len(SecretShapeKinds))
	for _, known := range SecretShapeKinds {
		names = append(names, string(known))
	}
	return "", fmt.Errorf("unknown secret kind %q (want one of: %s)", s, strings.Join(names, "|"))
}

// InferSecretShapeKind reads the shape off the value itself: a PEM
// header, a JSON document opener, else a bare token. It never returns
// SecretShapeRaw — opting out of the check is a decision an operator
// takes, not one a heuristic takes for them.
func InferSecretShapeKind(value string) SecretShapeKind {
	trimmed := strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(trimmed, "-----BEGIN "):
		return SecretShapePEM
	case strings.HasPrefix(trimmed, "{"), strings.HasPrefix(trimmed, "["):
		return SecretShapeJSON
	}
	return SecretShapeToken
}

// ResolveSecretShape decides WHICH gate applies to a stored secret: the kind
// the operator named, or — when they named none — the one the value itself
// says (InferSecretShapeKind). An unrecognised kind is an error, never a
// silent pass-through to no checking.
//
// It is shared by every door into the generic secret store (`iterion secret
// set`, the studio's local Secrets view) so the two cannot disagree on what a
// value is: the reason a check-free door existed at all is that the rule lived
// at one of them. Each caller phrases its own remedy — the CLI names
// `--kind raw`, an API names the field — from the kind returned here.
func ResolveSecretShape(kind, value string) (SecretShapeKind, error) {
	if strings.TrimSpace(kind) == "" {
		return InferSecretShapeKind(value), nil
	}
	return ParseSecretShapeKind(kind)
}

// ValidateSecretShape is the ingestion gate for a stored secret whose
// shape the operator named (or that InferSecretShapeKind read off the
// value). field phrases the refusal — pass the secret's name so an
// operator sees WHICH secret was rejected; the value never appears.
func ValidateSecretShape(kind SecretShapeKind, field, value string) error {
	switch kind {
	case SecretShapeRaw:
		return nil
	case SecretShapeToken:
		return ValidateTokenShape(field, value)
	case SecretShapeJSON:
		members, docKind := jsonDocument(value)
		if docKind == jsonNotADocument {
			return &ShapeError{Field: field, Reason: "is not a JSON document — it opens like one but does not parse, which is what a truncated or partially-copied paste looks like"}
		}
		if members == 0 {
			return &ShapeError{Field: field, Reason: "is an empty JSON document — it carries nothing to authenticate with"}
		}
		return nil
	case SecretShapePEM:
		if block, _ := pem.Decode([]byte(value)); block == nil {
			return &ShapeError{Field: field, Reason: "is not a PEM block — it opens with a PEM header but no complete -----BEGIN/-----END block decodes, which is what a truncated paste looks like"}
		}
		return nil
	}
	return fmt.Errorf("secrets: unknown secret shape kind %q", kind)
}

// gatewaySecretMinLen is the shortest gateway key the ingestion gate
// accepts: long enough to refuse a placeholder ("EMPTY", "none", "x") that
// a gateway with authentication off would take and that cannot be metered,
// short enough for a self-hosted server's documented example key
// (vLLM's "token-abc123"). LiteLLM virtual keys are far longer.
const gatewaySecretMinLen = 8

// maxGatewayBaseURLBytes bounds a gateway root. A real root is tens of
// bytes; the value is listed, audited and sealed into the AAD, and a
// pasted document must not ride it. Greppable, not operator-tunable.
const maxGatewayBaseURLBytes = 2048

// ValidateGatewayBaseURL checks and canonicalises the root of an
// openai_compatible gateway. It refuses what can never serve a run: a
// scheme other than https (a tenant endpoint never rides plain http — the
// deployment's own gateway may, through the environment, not through a
// record), a URL without a host, userinfo, a query or a fragment, any
// whitespace, non-printable character or percent-escape (a pasted secret
// glued to the URL, an IPv6 zone), a value over maxGatewayBaseURLBytes, a
// port outside 1–65535, a path with an empty, "." or ".." segment, a host
// with an empty label, a literal address that is not public unicast
// (loopback, private, link-local, metadata — pkg/secure/httpdial's set),
// a single-label host (localhost and every /etc/hosts alias, _gateway,
// instance-data, a Service name — a public gateway has a dot), the names a
// cluster, a cloud or a container reserves for itself (reservedGatewayHosts
// and reservedGatewaySuffixes), and a host written as a number in any base
// (see canonicalGatewayHost). The path is ASCII letters, digits and
// "-._~:@/" only: a blank-looking printable rune must not glue a secret to
// it, and a backslash or a sub-delimiter is read as something else by a
// browser's parser. A name that RESOLVES to such an address is the
// dial-time guard's to refuse, once a run dials the gateway.
//
// The host is CONVERTED before it is judged: an internationalised name is
// mapped to punycode first (full-width digits become digits, invisible
// characters are dropped), and the literal-address, empty-label and
// reserved-name checks read the converted form — so no spelling of a
// refused host slips past as a "name". The canonical form lower-cases the
// host, prints a literal address as net.IP does (bracketed when IPv6),
// drops a trailing dot and the default port, writes the port as a number,
// and strips a trailing slash and every trailing "/v1" segment (the client
// appends /v1/chat/completions itself), so the same endpoint written
// several ways is one AAD, one fingerprint and one ceiling; the function
// is idempotent on its own output, and maxGatewayBaseURLBytes bounds the
// VALUE it returns, not only its input (TestValidateGatewayBaseURL_Guarantees
// fuzzes these properties). A refusal names a rule, never the value:
// url.Parse's own errors quote the input and are not repeated.
func ValidateGatewayBaseURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", &ShapeError{Field: "base_url", Reason: "is required for an openai_compatible key"}
	}
	if len(v) > maxGatewayBaseURLBytes {
		return "", &ShapeError{Field: "base_url", Reason: fmt.Sprintf("is longer than %d bytes", maxGatewayBaseURLBytes)}
	}
	for _, r := range v {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return "", &ShapeError{Field: "base_url", Reason: "contains whitespace or a non-printable character"}
		}
	}
	if strings.ContainsRune(v, '%') {
		return "", &ShapeError{Field: "base_url", Reason: "must not contain a percent-escape"}
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", &ShapeError{Field: "base_url", Reason: "is not a URL"}
	}
	if u.Scheme != "https" {
		return "", &ShapeError{Field: "base_url", Reason: "must use https"}
	}
	if u.User != nil {
		return "", &ShapeError{Field: "base_url", Reason: "must not carry credentials in the URL"}
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", &ShapeError{Field: "base_url", Reason: "must not carry a query or a fragment"}
	}
	host, ip, herr := canonicalGatewayHost(u.Hostname())
	if herr != nil {
		return "", herr
	}
	if ip != nil {
		if !httpdial.IsPublicUnicast(ip) {
			return "", &ShapeError{Field: "base_url", Reason: "names a loopback, private, link-local or metadata address — a gateway a run dials is a public host"}
		}
	} else if reservedGatewayHost(host) {
		return "", &ShapeError{Field: "base_url", Reason: "names a host the cluster, the cloud or the container reserves (" + strings.Join(reservedGatewayHosts, ", ") + ", *" + strings.Join(reservedGatewaySuffixes, ", *") + ")"}
	}
	hostport := host
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", &ShapeError{Field: "base_url", Reason: "has a port outside 1–65535"}
		}
		if n != 443 {
			hostport += ":" + strconv.Itoa(n)
		}
	}
	path := strings.TrimRight(u.Path, "/")
	if path != "" {
		for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			if seg == "" || seg == "." || seg == ".." {
				return "", &ShapeError{Field: "base_url", Reason: "has an empty, \".\" or \"..\" path segment"}
			}
		}
		for _, r := range path {
			if !pathRune(r) {
				return "", &ShapeError{Field: "base_url", Reason: "has a path character outside ASCII letters, digits, \"-._~:@/\" — a gateway root needs no more, and a blank-looking character must not glue a secret to it"}
			}
		}
	}
	for strings.HasSuffix(path, "/v1") {
		path = strings.TrimRight(strings.TrimSuffix(path, "/v1"), "/")
	}
	out := "https://" + hostport + path
	// The cap bounds the VALUE the record keeps, not only the input: a
	// name grows when mapped to punycode.
	if len(out) > maxGatewayBaseURLBytes {
		return "", &ShapeError{Field: "base_url", Reason: fmt.Sprintf("grows past %d bytes once its host is written in punycode", maxGatewayBaseURLBytes)}
	}
	return out, nil
}

// pathRune accepts the characters of a gateway root's path: ASCII
// unreserved characters, ":" and "@", and the "/" separator.
func pathRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~:@/", r)
}

// canonicalGatewayHost converts a URL host to the one spelling the record
// keeps and reports whether it is a literal address. A name carrying
// non-ASCII characters is mapped to punycode FIRST (as the IDNA lookup
// profile maps it, before any case-folding of ours), then lower-cased and
// judged on that form, so that a full-width "127.0.0.1" is the literal it
// spells and an invisible character does not hide an empty host; one
// trailing dot is dropped; an empty label, a label with a character outside
// letters, digits, "_" and "-", a hyphen at a label's edge, a label over 63
// bytes or a name over 253 are refused, as is a name whose last label is a
// number ("127.1", "0x7f.0.0.1", "2130706433": the spellings a C resolver
// or a browser reads as an address and net.ParseIP does not — judged as a
// name here, they would reach the loopback or the metadata service from any
// non-Go reader of the stored URL). The returned ip is nil for a name.
func canonicalGatewayHost(raw string) (string, net.IP, error) {
	if raw == "" {
		return "", nil, &ShapeError{Field: "base_url", Reason: "has no host"}
	}
	if ip := net.ParseIP(raw); ip != nil {
		return literalHost(ip), ip, nil
	}
	host := raw
	for _, r := range raw {
		if r >= utf8.RuneSelf {
			ascii, err := idna.Lookup.ToASCII(raw)
			if err != nil {
				return "", nil, &ShapeError{Field: "base_url", Reason: "has a host name that is not a valid DNS name"}
			}
			host = ascii
			break
		}
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", nil, &ShapeError{Field: "base_url", Reason: "has no host"}
	}
	// The mapping may have produced a literal (full-width digits).
	if ip := net.ParseIP(host); ip != nil {
		return literalHost(ip), ip, nil
	}
	if len(host) > maxDNSNameBytes {
		return "", nil, &ShapeError{Field: "base_url", Reason: fmt.Sprintf("has a host name longer than %d bytes", maxDNSNameBytes)}
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", nil, &ShapeError{Field: "base_url", Reason: "has a host with an empty label or a hyphen at a label's edge"}
		}
		if len(label) > maxDNSLabelBytes {
			return "", nil, &ShapeError{Field: "base_url", Reason: fmt.Sprintf("has a host label longer than %d bytes", maxDNSLabelBytes)}
		}
		for _, r := range label {
			if !hostLabelRune(r) {
				return "", nil, &ShapeError{Field: "base_url", Reason: "has a host name that is not a valid DNS name"}
			}
		}
	}
	if endsInANumber(labels[len(labels)-1]) {
		return "", nil, &ShapeError{Field: "base_url", Reason: "has a host that ends in a number — a literal address is written as a dotted quad or a bracketed IPv6"}
	}
	// A public gateway always has a dot: a lone label resolves only through
	// /etc/hosts, a search domain or a synthetic record (_gateway,
	// instance-data, a Service name in the pod's namespace).
	if len(labels) < 2 {
		return "", nil, &ShapeError{Field: "base_url", Reason: "has a single-label host — a gateway a run dials has a public name with a dot"}
	}
	return host, nil, nil
}

// maxDNSLabelBytes and maxDNSNameBytes are the wire limits of a DNS name;
// a longer one resolves nowhere.
const (
	maxDNSLabelBytes = 63
	maxDNSNameBytes  = 253
)

// endsInANumber reports a last label that is decimal digits or "0x" plus
// hex digits — the WHATWG rule for "this host is an IPv4 address written
// some other way"; no public top-level domain is a number.
func endsInANumber(label string) bool {
	digits := label
	if strings.HasPrefix(label, "0x") {
		digits = label[2:]
		if digits == "" {
			return true
		}
		for _, r := range digits {
			if !hexRune(r) {
				return false
			}
		}
		return true
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hexRune accepts a lower-case hexadecimal digit.
func hexRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f'
}

// hostLabelRune accepts the characters of a lower-cased host label Go
// dials: letters, digits, "-" and "_".
func hostLabelRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

// literalHost prints an address as net.IP does, bracketed when IPv6.
func literalHost(ip net.IP) string {
	s := ip.String()
	if strings.Contains(s, ":") {
		return "[" + s + "]"
	}
	return s
}

// reservedGatewayHosts and reservedGatewaySuffixes name what no tenant
// gateway can legitimately be: the names a cluster, a cloud, a container or
// a workstation resolves to itself (single-label aliases such as
// localhost4 or ip6-localhost are already refused as single labels).
var (
	reservedGatewayHosts    = []string{"localhost.localdomain", "kubernetes.default"}
	reservedGatewaySuffixes = []string{".localhost", ".localdomain", ".localdomain4", ".localdomain6", ".local", ".internal", ".svc", ".cluster.local"}
)

// reservedGatewayHost reports a host name no tenant gateway can legitimately
// carry.
func reservedGatewayHost(host string) bool {
	for _, h := range reservedGatewayHosts {
		if host == h {
			return true
		}
	}
	for _, suffix := range reservedGatewaySuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
