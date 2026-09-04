package secrets

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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
	if !provider.CredentialIsJSON() {
		return ValidateTokenShape("api-key secret", value)
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return &ShapeError{Field: "api-key secret", Reason: "is empty"}
	}
	var doc map[string]json.RawMessage
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &doc) != nil {
		return &ShapeError{Field: "api-key secret", Reason: fmt.Sprintf("must be a JSON credential object for provider %s (an AWS-style credential document, a service-account file), not a bearer token", provider)}
	}
	// An EMPTY object parses and carries nothing: a credential that cannot
	// authenticate is refused at ingestion like any other, rather than
	// discovered at the first call.
	if len(doc) == 0 {
		return &ShapeError{Field: "api-key secret", Reason: fmt.Sprintf("is an empty JSON object — a %s credential document must carry its fields", provider)}
	}
	return nil
}
