package secrets

import (
	"encoding/json"
	"encoding/pem"
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
