package secretguard

import (
	"bytes"
	"encoding/json"
)

// MaterializeLeaves swaps placeholders in the DECODED strings of a value
// (a tool input: maps, slices, strings), so a secret goes in byte for byte.
// Swapping in the JSON text instead lets JSON decode the value: a backslash,
// a quote or a newline in the secret comes out altered, breaks the document,
// or — a quote followed by `","key":"…` — writes keys of its own into the
// tool call. Map keys are never materialised. It reports whether anything
// changed.
func MaterializeLeaves(v any, materialize func(string) string) (any, bool) {
	return mapLeaves(v, materialize)
}

// RedactLeaves applies a redactor to the decoded strings of a value (a tool
// input) — before anything cuts them for display: a secret cut at a display
// bound is no longer recognisable to the redactor. Map keys are left as they
// are.
func RedactLeaves(v any, redact func(string) string) any {
	out, _ := mapLeaves(v, redact)
	return out
}

// mapLeaves applies f to every string leaf of v (maps, slices, strings),
// returning a copy and whether any leaf changed.
func mapLeaves(v any, f func(string) string) (any, bool) {
	switch x := v.(type) {
	case string:
		m := f(x)
		return m, m != x
	case map[string]any:
		out := make(map[string]any, len(x))
		changed := false
		for k, e := range x {
			nv, c := mapLeaves(e, f)
			out[k] = nv
			changed = changed || c
		}
		return out, changed
	case []any:
		out := make([]any, len(x))
		changed := false
		for i, e := range x {
			nv, c := mapLeaves(e, f)
			out[i] = nv
			changed = changed || c
		}
		return out, changed
	default:
		return v, false
	}
}

// MaterializeJSON is MaterializeLeaves for a JSON document: decoded (numbers
// kept exact), materialised leaf by leaf, re-encoded. A document that does
// not decode is returned as is — never materialised as text: the tool
// reports it invalid, with the placeholder in it.
func MaterializeJSON(raw []byte, materialize func(string) string) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return raw
	}
	out, changed := MaterializeLeaves(v, materialize)
	if !changed {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return raw
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
