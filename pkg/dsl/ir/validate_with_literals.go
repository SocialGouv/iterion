package ir

import (
	"encoding/json"
	"strings"
)

// validateWithMappingLiterals warns when an edge `with:` value is a
// pure literal (no `{{…}}`) reaching a typed input field. A `with:`
// value is a template, so a ref-less value ALWAYS travels verbatim as
// a STRING through `resolveMapping` (which returns `dm.Raw`) — the
// plausibility of the text (`"true"`, `"42"`) does not change that
// fact, the runtime never re-decodes. The trigger is therefore
// unconditional per target type: `bool`/`int`/`float` fires on every
// ref-less literal (whatever the text); `json`/`string[]` fires on
// every ref-less literal that is either JSON-shaped or not valid
// JSON (only bare primitives — `true`/`false`/numeric — stay silent
// on those two types).
//
// The check runs on edge with-mappings only — a subbot's target is a
// child bot loaded separately (its input schema is unknown here); an
// emit has no typed destination.
//
// The diagnostic is a **warning** at every consumer, whatever the
// destination kind. The check cannot separate the two runtime shapes
// that actually collide: a compute expression that PASSES the incoming
// string through to a typed OUTPUT field would fail SCHEMA_VALIDATION
// (measured on the wave-4 PR of #1344: `implementation_active: "false"`
// reached a compute whose output is `bool` and died with `expected
// bool, got string`), whereas a compute expression that NORMALIZES the
// value (`if(input.f, 1, 0)` — the copilot bot's pattern) survives
// because the expr's `truthy()` handler treats "0" / "false" / "no"
// / "" as falsy on purpose. Static analysis cannot tell the two
// shapes apart, and the runtime tolerates one while breaking the
// other; a warning surfaces the intent-vs-rendering divergence
// without turning a shipped bot red on a normalization that works
// today. The catalogue's copilot / sec-audit-source patterns are
// examples of the safe form.
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
			if dm == nil || len(dm.Refs) > 0 {
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

// checkWithLiteral emits C152 (warning) for a ref-less mapping whose
// text cannot be the target field's type. The runtime returns `dm.Raw`
// verbatim for a ref-less mapping — a STRING at every consumer — but
// the observed failure mode varies by consumer:
//
//   - An **agent** / **judge** / **human** renders the value into a
//     prompt or a UI (the LLM reads "false" interchangeably with the
//     decoded value); a **tool** shell-escapes it (a string is fine
//     there).
//   - A **compute** reads it through an expr; the expr's `truthy()`
//     handler already turns `"0"` / `"false"` / `"no"` / `""` into
//     falsy — deliberately, "LLM tool outputs and JSON-decoded env
//     vars commonly carry boolean-shaped data as strings" — so a bare
//     `if(input.f, ...)` behaves like an author would expect. Compute
//     inputs are NOT conformed to the input schema at run time; only
//     `ConformComputeOutput` is strict, on the compute's own output
//     map.
//
// The runtime-observable failure is confined to the sliver where a
// compute expr passes an incoming string through, verbatim, to a
// typed output field — a case this static check cannot separate from
// the many safe copilot-shaped normalisations already shipping. The
// diagnostic is a **warning** everywhere: the value is legibly not a
// bool/int/float, and the mapping is not going to reinterpret it, so
// the author's intent is visibly divergent from the runtime's
// rendering — but no compile-time refusal turns a shipped bot red on
// a pattern that is working today.
func (c *compiler) checkWithLiteral(e *Edge, dm *DataMapping, f *SchemaField, inSchema string, dst Node) {
	raw := dm.Raw
	kind := dst.NodeKind().String()
	switch f.Type {
	case FieldTypeBool:
		c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
			"edge %s -> %s, with %q: literal %q reaches a `bool` field %q of input schema %q on a %s node as a string (the runtime returns a mapping's ref-less text verbatim); the LLM / expr `truthy()` handler tolerates the pattern, but a compute expression that passes it through to a typed output would fail SCHEMA_VALIDATION — emit the constant from a compute's `expr:` (`%s: true`) and reference `{{outputs.<compute>.%s}}`, or bind the value from a producer's output",
			e.From, e.To, dm.Key, raw, dm.Key, inSchema, kind, dm.Key, dm.Key)
	case FieldTypeInt:
		c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
			"edge %s -> %s, with %q: literal %q reaches an `int` field %q of input schema %q on a %s node as a string (the runtime returns a mapping's ref-less text verbatim); a compute expression that passes it through to a typed output would fail SCHEMA_VALIDATION — emit the constant from a compute's `expr:` (`%s: 42`) and reference `{{outputs.<compute>.%s}}`",
			e.From, e.To, dm.Key, raw, dm.Key, inSchema, kind, dm.Key, dm.Key)
	case FieldTypeFloat:
		c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
			"edge %s -> %s, with %q: literal %q reaches a `float` field %q of input schema %q on a %s node as a string (the runtime returns a mapping's ref-less text verbatim); a compute expression that passes it through to a typed output would fail SCHEMA_VALIDATION — emit the constant from a compute's `expr:` (`%s: 3.14`) and reference `{{outputs.<compute>.%s}}`",
			e.From, e.To, dm.Key, raw, dm.Key, inSchema, kind, dm.Key, dm.Key)
	case FieldTypeJSON:
		if isJSONShapedText(raw) {
			c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
				"edge %s -> %s, with %q: literal %q reaches a `json` field %q of input schema %q on a %s node as a %d-character string, not the encoded value (a permissive consumer may tolerate the text; a strict one gets `expected object/array, got string`); emit the value from a compute's `expr:` (`%s: %s`) and reference `{{outputs.<compute>.%s}}`",
				e.From, e.To, dm.Key, raw, dm.Key, inSchema, kind, len(raw), dm.Key, jsonExprSuggestion(raw), dm.Key)
		}
	case FieldTypeStringArray:
		if isJSONArrayShapedText(raw) {
			c.warnfAt(DiagWithLiteralTypeMismatch, e.From, edgeID(e.From, e.To),
				"edge %s -> %s, with %q: literal %q reaches a `string[]` field %q of input schema %q on a %s node as one string (the runtime does not JSON-parse a mapping's text); emit the array from a compute's `expr:` (`%s: %s`) and reference `{{outputs.<compute>.%s}}`",
				e.From, e.To, dm.Key, raw, dm.Key, inSchema, kind, dm.Key, raw, dm.Key)
		}
	}
}

// isJSONShapedText is true when the ref-less mapping literal reaching
// a `json` field is either JSON-shaped (an array, an object, `null`,
// a quoted string — the ticket #1420 shapes) OR not valid JSON at all
// (the copilot bot's `host_event: "”"` shape — single-quoted, not a
// JSON encoding of anything the runtime consumers accept). Only bare
// primitives that ARE valid JSON as-is (`true` / `false` / numerics,
// e.g. `42`, `3.14`, `-1`) stay silent: those round-trip through a
// permissive consumer without a rendering surprise. Every other
// ref-less literal on a `json` field is at least ambiguous — the
// runtime hands the destination a plain string — and the author's
// intent-vs-rendering divergence is worth surfacing so the fix
// (emit the typed value from a compute `expr:` and reference the
// output) is written where it is cheap.
func isJSONShapedText(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	// JSON-shaped structurally (the ticket's own examples). Fires even
	// when the text IS valid JSON, because the runtime hands the
	// destination the ENCODED text as a string, not the decoded value —
	// the intent-vs-rendering divergence is what C152 makes visible.
	switch t[0] {
	case '[', '{', '"':
		return true
	}
	if t == "null" || t == "true" || t == "false" {
		return true
	}
	// Bare primitives that ARE valid JSON stay silent: a numeric
	// literal round-trips through a permissive `json` consumer without
	// a rendering surprise.
	var any interface{}
	if err := json.Unmarshal([]byte(t), &any); err == nil {
		return false
	}
	// Not valid JSON either — a `''`, a bare word, punctuation. The
	// author probably meant an encoded value with slightly-wrong
	// syntax; the runtime hands the destination the raw text.
	return true
}

// isJSONArrayShapedText is true when the literal looks like an encoded
// JSON array. A string[] target field expects a list of strings — a
// literal `"[\"a\",\"b\"]"` reaches the field as ONE string, so the
// author's intent visibly diverges from the runtime's rendering.
func isJSONArrayShapedText(s string) bool {
	t := strings.TrimSpace(s)
	return len(t) >= 2 && t[0] == '[' && t[len(t)-1] == ']'
}

// jsonExprSuggestion echoes the literal back as an `expr:` value the
// author can paste — a JSON-shaped literal renders as itself, everything
// else falls back to a re-quoted string so the diagnostic never suggests
// invalid syntax.
func jsonExprSuggestion(raw string) string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return `""`
	}
	// Quick sanity check: only echo the literal when it parses as JSON,
	// so the hint stays valid. Otherwise re-quote it as a string.
	var any interface{}
	if err := json.Unmarshal([]byte(t), &any); err == nil {
		return t
	}
	b, _ := json.Marshal(raw)
	return string(b)
}
