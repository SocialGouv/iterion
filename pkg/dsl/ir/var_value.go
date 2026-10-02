package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CoerceVarValue narrows a var's text — an override from `--var` or a
// launch payload, or a default the compiler kept as text — to the type
// the var declares: a bool by its spellings, an int, a float, a `json`
// var's text as JSON (or the text itself when it is not), a `string[]`
// var's text as a JSON list or as comma-separated values. A value that
// is not a string is typed already and passes through.
//
// It is the NARROWING half only. Every host reads a var through
// ResolveVarText, which expands the env forms first and calls this; a
// caller that reaches here directly judges the text as written, which is
// what a compile-time view of a default must do when it has no
// environment to speak for.
func CoerceVarValue(v any, vt VarType) (any, error) {
	s, isStr := v.(string)
	if !isStr {
		return v, nil
	}
	switch vt {
	case VarString:
		return s, nil
	case VarBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid bool %q", s)
		}
	case VarInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int %q: %w", s, err)
		}
		return n, nil
	case VarFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q: %w", s, err)
		}
		return n, nil
	case VarJSON:
		// Parse JSON; if the user gave us non-JSON text, leave it
		// as a string — JSON expressions accept either.
		var out any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return s, nil
		}
		return out, nil
	case VarStringArray:
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return []any{}, nil
		}
		// Accept either JSON array form (["a","b"]) or
		// comma-separated (a,b).
		if strings.HasPrefix(trimmed, "[") {
			var arr []any
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr, nil
			}
		}
		parts := strings.Split(trimmed, ",")
		out := make([]any, len(parts))
		for i, p := range parts {
			out[i] = strings.TrimSpace(p)
		}
		return out, nil
	default:
		return s, nil
	}
}

// ResolveVarText is the ONE reading of a var's TEXT — the default the
// compiler kept from the `.bot`, or an override from `--var` / a launch
// payload / a preset. Both take this path, so `tags: string[] = "a,b"` and
// `--var tags=a,b` start the run with the same value (#1285): before, the
// default seeded the raw string and the override seeded ["a","b"], and a
// template or a fan-out reading {{vars.tags}} saw a string in one launch
// and a list in the other.
//
// The order is EXPAND then COERCE, because the env forms the DSL honours
// everywhere else decide what there is to read: `${LIST:-a,b}` is one
// `string[]` of two elements, and coercing first split the unexpanded text
// on its comma into ["${LIST:-a", "b}"].
//
// `json` is read as the DOCUMENT it is: parsed first, then each string
// LEAF expanded through the braced-only reading. A document's `$` is data
// — `{"cost":"$5"}` is five dollars, `{"awk":"{print $1}"}` is a program —
// while `${PROJECT_DIR}` inside a leaf still resolves, and an expansion
// carrying a `"` can no longer break the syntax around it.
//
// lookup is the host's environment reading — nil means the process env.
func ResolveVarText(v any, vt VarType, lookup func(string) string) (any, error) {
	return resolveVarText(v, vt, varExpander(vt, lookup, false))
}

// resolveVarTextAsWritten is ResolveVarText for a COMPILE-TIME view of a
// default: the `${VAR:-default}` forms resolve (they have an answer with
// no environment) and every other reference is kept AS WRITTEN. A
// published contract must say the same thing on every machine, and
// resolving `${PROJECT_DIR}/audits` against an empty environment would
// advertise `/audits` — neither the source text nor any run's value.
func resolveVarTextAsWritten(v any, vt VarType) (any, error) {
	return resolveVarText(v, vt, varExpander(vt, noEnvLookup, true))
}

// varExpander is the env reading a var of this type gets: braced-only for
// a `json` document, the full reading for every other type, which is what
// a `vars:` default has always had.
func varExpander(vt VarType, lookup func(string) string, keepUnresolved bool) func(string) string {
	policy := expandPolicy{bracedOnly: vt == VarJSON, keepUnresolved: keepUnresolved}
	return func(s string) string {
		out, _ := expandWithDefault(s, lookup, policy)
		return out
	}
}

func resolveVarText(v any, vt VarType, expand func(string) string) (any, error) {
	s, isStr := v.(string)
	if !isStr {
		return v, nil
	}
	if vt == VarJSON {
		out, err := CoerceVarValue(s, vt)
		if err != nil {
			return nil, err
		}
		if text, notJSON := out.(string); notJSON {
			return expand(text), nil
		}
		return expandJSONLeaves(out, expand), nil
	}
	return CoerceVarValue(expand(s), vt)
}

// expandJSONLeaves expands the env forms in every STRING LEAF of a decoded
// document, never in its syntax. Splicing an expansion into the text
// before parsing let a value carrying a `"` break the document, and
// CoerceVarValue then returned the corrupt text as a plain string with no
// error — a `json` var silently arriving as a string, which is the silent
// type change this reading exists to remove. Object KEYS are names, not
// values, and are left as the author wrote them.
func expandJSONLeaves(v any, expand func(string) string) any {
	switch x := v.(type) {
	case string:
		return expand(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = expandJSONLeaves(e, expand)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = expandJSONLeaves(e, expand)
		}
		return out
	}
	return v
}
