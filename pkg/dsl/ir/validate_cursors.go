package ir

import (
	"strconv"
	"strings"
)

// validateCursorInvocations walks every agent/judge in the workflow
// and ensures each cursor invocation refers to a declared cursor and
// carries a value that resolves to a known enum entry or band.
//
// `${VAR}` invocations skip the static value check — runtime
// substitution handles the lookup and emits a fatal-error event if
// the resolved value is invalid.
//
// Unknown cursor names produce C083 (warning, since cursors are an
// open-namespace primitive an operator may add later). Invalid
// values produce C084 (error).
func (c *compiler) validateCursorInvocations(w *Workflow) {
	if w == nil {
		return
	}
	for _, n := range w.Nodes {
		var inv *CursorInvocation
		var kind, id string
		switch v := n.(type) {
		case *AgentNode:
			inv = v.Cursors
			kind = "agent"
			id = v.ID
		case *JudgeNode:
			inv = v.Cursors
			kind = "judge"
			id = v.ID
		default:
			continue
		}
		if inv == nil {
			continue
		}
		for _, s := range inv.Settings {
			def, ok := w.Cursors[s.Key]
			if !ok {
				c.warnfAt(DiagUnknownCursor, id, "",
					"%s %q references unknown cursor %q (declare it with `cursor %s:` at workflow scope)",
					kind, id, s.Key, s.Key)
				continue
			}
			if isEnvSubstitutionString(s.Value) {
				continue
			}
			if _, ok, reason := ResolveCursorValue(def, s.Value); !ok {
				c.errorfAt(DiagInvalidCursorVal, id, "",
					"%s %q: cursor %q value %q is invalid: %s",
					kind, id, s.Key, s.Value, reason)
			}
		}
	}
}

// ResolveCursorValue classifies raw against the cursor definition
// and returns the matching prompt fragment. Used by both the
// compile-time validator (to verify reachability — it ignores the
// prompt and only consults ok) and the runtime resolver (to obtain
// the fragment). reason is set when ok is false to drive precise
// C084 diagnostics; it is "" on success.
//
// Numeric values clamp to [0,1]. When the cursor declares only
// values:, numeric inputs snap to an enum position. When it
// declares only bands:, enum inputs are rejected.
func ResolveCursorValue(def *CursorDef, raw string) (prompt string, ok bool, reason string) {
	if def == nil {
		return "", false, "cursor not declared"
	}
	if v, parsed := tryParseFloat(raw); parsed {
		if v < 0 || v > 1 {
			return "", false, "numeric cursor values must lie in [0.0, 1.0]"
		}
		if len(def.Bands) > 0 {
			if b, found := lookupBand(def.Bands, v); found {
				return b.Prompt, true, ""
			}
			return "", false, "no band covers this value"
		}
		if len(def.Values) > 0 {
			idx := int(v * float64(len(def.Values)))
			if idx >= len(def.Values) {
				idx = len(def.Values) - 1
			}
			return def.Values[idx].Prompt, true, ""
		}
		return "", false, "cursor defines neither values nor bands"
	}
	for _, ev := range def.Values {
		if ev.Name == raw {
			return ev.Prompt, true, ""
		}
	}
	if len(def.Bands) > 0 && len(def.Values) == 0 {
		return "", false, "cursor expects a numeric value (it declares bands, not values)"
	}
	return "", false, "value is not one of the declared enum names"
}

// lookupBand returns the first band whose [Lo, Hi] (inclusive on
// both ends) contains v. Overlap is rejected by C085 at compile-time,
// so the first match here is also the only match.
func lookupBand(bands []CursorBandSpec, v float64) (CursorBandSpec, bool) {
	for _, b := range bands {
		if v >= b.Lo && v <= b.Hi {
			return b, true
		}
	}
	return CursorBandSpec{}, false
}

func tryParseFloat(raw string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// isEnvSubstitutionString returns true for values shaped like
// "${VAR}" or "${VAR:-default}". Stricter than IsEnvSubstitutedEffort
// (which only checks for `$`) — the cursor validator wants to defer
// only well-formed env refs and surface garbage like `$WHOOPS` at
// compile time.
func isEnvSubstitutionString(s string) bool {
	s = strings.TrimSpace(s)
	return strings.Contains(s, "${") && strings.Contains(s, "}")
}
