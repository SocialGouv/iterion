package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// validateWithMappingLiterals warns (C152) when an edge `with:` value
// reaches a typed input field of the destination as TEXT. A `with:`
// value is a template: the runtime hands the destination a string for
// every mapping except one that is exactly a single `{{…}}` block
// covering the whole text — that one passes the referenced value
// through with its type (resolveMapping, engine_resolve.go), except
// the literal-open form `{{"{{"}}`, whose value IS the literal text.
// So a
// ref-less literal (`v: "42"`) AND a template that interpolates a
// reference into prose (`v: "{{vars.n}} "`) both arrive as a string,
// whatever they spell — `"true"` and `"42"` arrive exactly like
// `"yes"` does. A mapping whose template failed to parse (an
// unterminated `{{`) is left to C004.
//
// The trigger follows what a string can be on each field type:
//
//   - `bool` / `int` / `float` / `string[]`: never a string — every
//     text-arriving value fires. A text can spell a bool or a JSON
//     array; it cannot BE one.
//   - `json`: a string IS a JSON value, so a plain word (`mode: "fast"`)
//     arrives exactly as written and stays silent; a ref-less literal
//     that visibly attempts an encoding — bracketed, braced, quoted, or
//     a JSON keyword — fires, because the author meant the decoded value
//     and the runtime hands over the text. A bare number stays silent
//     as a deliberate exception, locked by the tests: it reads as the
//     JSON string the author wrote, though a consumer doing arithmetic
//     on it would see the difference.
//   - `string` / `file`: a string is the legal value; silent.
//
// Edge with-mappings only: a subbot's target is a child bot loaded
// separately (its input schema is unknown here); an emit has no typed
// destination.
//
// A warning at every consumer, whatever the destination kind. Some
// sinks tolerate the text (an LLM reads "false" for what it is; the
// expr `truthy()` handler folds "0" / "false" / ""; a tool `command:`
// renders the list `[]` and the text `[]` the same), others break: a
// compute that passes the value through to a typed output fails
// SCHEMA_VALIDATION, and `length()` counts the BYTES of `"[]"`. Static
// analysis cannot tell the two consumers apart, and a refusal would
// turn shipped bots red on a pattern that runs today.
func (c *compiler) validateWithMappingLiterals(w *Workflow) {
	if w == nil {
		return
	}
	for _, e := range w.Edges {
		if e == nil || len(e.With) == 0 {
			continue
		}
		dst := w.Nodes[e.To]
		if dst == nil {
			continue // C001 handles the missing destination
		}
		inSchema := NodeInputSchema(dst)
		if inSchema == "" {
			continue
		}
		schema, ok := w.Schemas[inSchema]
		if !ok || schema == nil {
			continue // C002 handles the missing schema
		}
		for _, dm := range e.With {
			if dm == nil || !mappingArrivesAsText(dm) {
				continue
			}
			f := findField(schema, dm.Key)
			if f == nil {
				continue // C028/C034 flag the mismatch on the key
			}
			c.checkWithLiteral(e, dm, f, inSchema, dst)
		}
	}
}

// MappingArrivesAsText is mappingArrivesAsText for a host outside this
// package that reads what a node's input holds: whether a `with` mapping
// reaches its destination as a string, or passes a value through with its
// type.
func MappingArrivesAsText(dm *DataMapping) bool {
	return mappingArrivesAsText(dm)
}

// mappingArrivesAsText mirrors the runtime's resolveMapping decision:
// the destination receives a STRING for every mapping except one whose
// raw text is exactly a single `{{…}}` block — that one resolves the
// reference and passes its value through with its type. The one
// exception is the literal-open form: its "value" IS the literal text
// `{{`, so it arrives a string whatever shape the mapping takes. A
// ref-less raw that still holds `{{` is an unparseable template; C004
// owns it and C152 stays out of the way (a genuine ref-less literal
// never contains an opening brace — the literal-open form parses as a
// reference).
func mappingArrivesAsText(dm *DataMapping) bool {
	switch len(dm.Refs) {
	case 0:
		return !strings.Contains(dm.Raw, "{{")
	case 1:
		if dm.Refs[0].Kind == RefLiteralOpen {
			return true
		}
		spans := mappingTemplateSpans(dm.Raw)
		if len(spans) != 1 {
			// Unreachable for compiler-built mappings — ParseRefs
			// yields exactly one reference per block, or none on a
			// broken template. The runtime's mismatch fallback
			// resolves a lone reference whatever the text holds
			// (hand-built IR with an empty raw included), so the
			// value arrives typed.
			return false
		}
		return spans[0].start != 0 || spans[0].end != len(dm.Raw)
	default:
		return true
	}
}

// mappingTemplateSpan marks one `{{…}}` block, end-exclusive.
type mappingTemplateSpan struct{ start, end int }

// mappingTemplateSpans locates every `{{…}}` block with the same
// left-to-right fence walk the runtime runs before deciding to
// interpolate (templateSpans in pkg/runtime/engine_resolve.go). If the
// two scans ever drift, this warning drifts with them — the
// interpolated-template fixtures pin the mixed shape end to end.
func mappingTemplateSpans(s string) []mappingTemplateSpan {
	var spans []mappingTemplateSpan
	for i := 0; ; {
		start := strings.Index(s[i:], "{{")
		if start == -1 {
			return spans
		}
		start += i
		end := strings.Index(s[start:], "}}")
		if end == -1 {
			return spans
		}
		end += start + 2
		spans = append(spans, mappingTemplateSpan{start: start, end: end})
		i = end
	}
}

// checkWithLiteral emits C152 for a value arriving as text on a field a
// string cannot satisfy — or, on `json`, one the text was visibly
// meant to be decoded into. Every arm phrases its message through
// withLiteralArrival and withLiteralRemedy; the literal is echoed into
// the remedy at ONE place, withLiteralSuggestion, which sanitises it —
// no arm interpolates the raw text into a suggestion.
func (c *compiler) checkWithLiteral(e *Edge, dm *DataMapping, f *SchemaField, inSchema string, dst Node) {
	switch f.Type {
	case FieldTypeBool, FieldTypeInt, FieldTypeFloat, FieldTypeStringArray:
		// A string can never be one of these: unconditional.
	case FieldTypeJSON:
		if !isAttemptedJSONEncoding(dm.Raw) {
			return
		}
	default:
		return
	}
	c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
		"edge %s -> %s, with %q: value %q reaches the `%s` field %q of input schema %q on %s %s node as %s; %s",
		e.From, e.To, dm.Key, dm.Raw, f.Type, dm.Key, inSchema, aAn(dst.NodeKind().String()), dst.NodeKind(),
		withLiteralArrival(f.Type, dm.Raw, len(dm.Refs) == 0), withLiteralRemedy(f.Type, dm.Key, dm.Raw))
}

// aAn is the indefinite article for s, by its initial letter — the
// node kinds and type names this package spells into messages
// (an agent, an emit, an int; a tool, a judge, a compute).
func aAn(s string) string {
	if s == "" {
		return "a"
	}
	switch s[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an"
	}
	return "a"
}

// isAttemptedJSONEncoding is true when a ref-less literal reaching a
// `json` field visibly spells an encoded value rather than a plain
// string: it opens or closes with a bracket, a brace or a quote, or is
// a JSON keyword (a closing apostrophe does not count — a plural
// possessive is prose, not an attempted encoding; the two-apostrophe
// literal still fires through the opening set). A plain word stays
// silent — the string is the value, and nothing is echoed back. A bare
// number stays silent too, a
// decision this file's tests lock, although a consumer doing
// arithmetic on it reads the string: the diagnostic claims the
// literals that visibly carry structure and the JSON keywords, not
// every scalar the destination might have wanted typed. A literal of
// two apostrophes fires: an attempted (and wrong) spelling of the
// empty string, and the catalogue example of #1420.
func isAttemptedJSONEncoding(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	switch t {
	case "null", "true", "false":
		return true
	}
	return strings.ContainsRune(`[{"'`, rune(t[0])) || strings.ContainsRune(`]}"`, rune(t[len(t)-1]))
}

// withLiteralArrival names what the destination receives for a
// text-arriving value on a field of type t, and the consumer that
// breaks on it. Each claim is measured on the engine: a compute
// passing the value through to a typed output fails SCHEMA_VALIDATION
// with the quoted wording, `length()` on the text counts its BYTES.
// The byte counts are quoted only for a ref-less literal, whose raw
// text IS the delivered string; an interpolated or literal-open
// mapping delivers the RENDERED template, whose shape only the run
// knows.
func withLiteralArrival(t FieldType, raw string, refLess bool) string {
	switch t {
	case FieldTypeBool, FieldTypeInt, FieldTypeFloat:
		return fmt.Sprintf("a string (the runtime hands a mapping's text over verbatim unless it is exactly one value reference — the literal-open form renders the literal text): an LLM or the expr `truthy()` handler reads the text tolerantly, but a compute that passes it through to a typed output fails SCHEMA_VALIDATION (`expected %s, got string`)", conformExpectation(t))
	case FieldTypeStringArray:
		if refLess {
			return fmt.Sprintf("one string of %d bytes, not a list (the runtime never JSON-decodes a mapping's text): a compute's `length()` counts those bytes, and a pass-through to a `string[]` output fails SCHEMA_VALIDATION (`expected string array, got string`)", len(raw))
		}
		return "one string (the interpolated template renders its reference as text), not a list (the runtime never JSON-decodes a mapping's text): a pass-through to a `string[]` output fails SCHEMA_VALIDATION (`expected string array, got string`)"
	case FieldTypeJSON:
		if refLess {
			return fmt.Sprintf("the %d-byte string %q, not a decoded value (the runtime never JSON-decodes a mapping's text): a compute's `length()` counts those bytes and a prompt renders the text", len(raw), raw)
		}
		return "the rendered template text, not a decoded value (the runtime never JSON-decodes a mapping's text): a prompt renders the text"
	}
	return "a string"
}

// conformExpectation is the type name ConformComputeOutput prints for a
// scalar field the text fails to satisfy.
func conformExpectation(t FieldType) string {
	switch t {
	case FieldTypeInt:
		return "integer"
	case FieldTypeFloat:
		return "number"
	}
	return t.String()
}

// withLiteralRemedy names, per field type, the form that delivers a
// typed value — each proven on the engine: a compute `expr:` constant
// for a scalar (`ok: "false"` arrives as a bool), a producer's typed
// output for a list or an object (the expr language has no list or
// object literal, and an interpolated mapping is always one string).
func withLiteralRemedy(t FieldType, key, raw string) string {
	switch t {
	case FieldTypeBool, FieldTypeInt, FieldTypeFloat:
		if s := withLiteralSuggestion(t, raw); s != "" {
			return fmt.Sprintf("emit the constant from a compute's `expr:` (`%s: %q`) and reference `{{outputs.<compute>.%s}}`, bind a producer's typed output, or declare %s `%s` var and reference `{{vars.<name>}}`",
				key, s, key, aAn(t.String()), t)
		}
		return fmt.Sprintf("emit the constant from a compute's `expr:` — the text spells no value a `%s` can hold — bind a producer's typed output, or declare %s `%s` var and reference `{{vars.<name>}}`",
			t, aAn(t.String()), t)
	case FieldTypeStringArray:
		return fmt.Sprintf("bind the list from a producer whose `output:` schema declares `%s: string[]` — a tool that prints `{%q: %s}` — and reference `{{outputs.<node>.%s}}` (a compute `expr:` has no list literal, and an interpolated mapping is always one string); declare the field `string` if one string is what is meant",
			key, key, withLiteralSuggestion(t, raw), key)
	case FieldTypeJSON:
		if s := withLiteralSuggestion(t, raw); s != "" {
			return fmt.Sprintf("bind the value from a producer whose `output:` schema declares `%s: json` — a tool that prints `{%q: %s}` — and reference `{{outputs.<node>.%s}}` (a compute `expr:` has no list or object literal; a string value is one there: `%s: \"'text'\"`, the empty string `%s: \"''\"`); declare the field `string` if one string is what is meant",
				key, key, s, key, key, key)
		}
		return fmt.Sprintf("the text is not valid JSON, so no producer can type it as written: write the value you mean where a producer types it — a tool whose `output:` schema declares `%s: json`, or a compute `expr:` for a string (`%s: \"'text'\"`, the empty string `%s: \"''\"`) — and reference `{{outputs.<node>.%s}}`; declare the field `string` if one string is what is meant",
			key, key, key, key)
	}
	return ""
}

// withLiteralSuggestion spells the value as the typed constant or JSON
// value the remedy names — an expr literal for a scalar, a JSON list or
// value for a producer to print — always through the JSON encoder or a
// numeric re-format, so a quote, a backtick or a newline in the text
// can never leave the suggestion unpastable, and a float never
// re-formats into a digit-only literal the expr lexer would read as an
// overflowing int. It is the single place a literal is echoed into a
// remedy. A scalar the text does not spell returns "" — the remedy then
// says so instead of suggesting a value that would silently replace
// the author's (a text other than true/false spells no bool; a bare
// word spells no int; a fraction or an out-of-range magnitude spells
// no int64 float).
func withLiteralSuggestion(t FieldType, raw string) string {
	s := strings.TrimSpace(raw)
	switch t {
	case FieldTypeBool:
		if s == "true" || s == "false" {
			return s
		}
		return ""
	case FieldTypeInt:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		// A numeric spelling that fits an int64 exactly — `1e3` —
		// becomes that integer; anything else (a word, a fraction,
		// an out-of-range magnitude) is not spellable as an int.
		if f, ok := parseFiniteFloat(s); ok && f == math.Trunc(f) && f >= math.MinInt64 && f < 9223372036854775808.0 {
			return strconv.FormatInt(int64(f), 10)
		}
		return ""
	case FieldTypeFloat:
		if f, ok := parseFiniteFloat(s); ok {
			out := strconv.FormatFloat(f, 'f', -1, 64)
			if !strings.Contains(out, ".") {
				// Keep the spelling a FLOAT: a digit-only literal the
				// lexer cannot fit into an int64 fails to parse.
				out += ".0"
			}
			return out
		}
		return ""
	case FieldTypeStringArray:
		var xs []string
		if err := json.Unmarshal([]byte(s), &xs); err == nil && xs != nil {
			return jsonText(xs)
		}
		return jsonText([]string{raw})
	case FieldTypeJSON:
		if !json.Valid([]byte(s)) {
			return ""
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(s)); err != nil {
			return ""
		}
		return buf.String()
	}
	return ""
}

// parseFiniteFloat parses s as a float64 the expr language could hold:
// no error, no Inf, no NaN.
func parseFiniteFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// jsonText encodes v as one line of JSON without HTML escaping, so a
// suggestion reads as the author would write it.
func jsonText(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
