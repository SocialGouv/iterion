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
// is not a string is typed already and passes through. One reading for
// every host: the engine's overrides and the public contract's view of a
// default (bindInput) read a var the same way.
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
