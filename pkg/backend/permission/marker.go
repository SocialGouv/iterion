package permission

// This file defines the structured "permission request" marker that
// rides the existing ask_user pause/resume plumbing. When the gate
// suspends a run for human approval (ModeAsk), it stashes the marker in
// the interaction's questions map under InteractionMarkerKey. The marker
// serves three consumers, all keyed off the same data:
//
//   - the executor, to recognise a permission pause and convert it even
//     when the node didn't opt into `interaction:`;
//   - the runtime resume path, to compute the grant rule from the
//     operator's allow/deny answer and inject it back into the policy
//     (so the agent's re-issued call passes the gate on BOTH backends);
//   - the studio, to render an approval card (tool + input + allow/deny
//     buttons) instead of a free-text question.

const (
	// InteractionMarkerKey is the questions-map key carrying the Marker.
	InteractionMarkerKey = "_permission"
	// GrantInputKey is the reserved node-input key the runtime sets on
	// resume with the computed grant rule; the executor reads it and adds
	// it to the resolved policy.
	GrantInputKey = "_permission_grant"
)

// Marker builds the structured permission-request payload stored in the
// interaction questions map. Values are plain JSON types so the marker
// round-trips through the interaction record + checkpoint unchanged.
func Marker(tool string, input map[string]any, rule string) map[string]any {
	return map[string]any{
		"tool":  tool,
		"input": input,
		"rule":  rule,
	}
}

// ParseMarker extracts (tool, input, rule) from a value previously built
// by Marker (after a JSON round-trip it is a map[string]any). ok is false
// when v is not a permission marker.
func ParseMarker(v any) (tool string, input map[string]any, rule string, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return "", nil, "", false
	}
	tool, _ = m["tool"].(string)
	rule, _ = m["rule"].(string)
	input, _ = m["input"].(map[string]any)
	if tool == "" {
		return "", nil, "", false
	}
	return tool, input, rule, true
}

// GrantFromAnswer maps an operator's free-text answer to an approval grant
// rule for the marked call. Returns ("", false) on denial (the run stays
// blocked). On approval it returns the allow-rule to add to the policy
// (whole-tool for "allow always", argument-scoped for "allow once").
func GrantFromAnswer(answer, tool string, input map[string]any) (rule string, approved bool) {
	allow, always := ParseAnswer(answer)
	if !allow {
		return "", false
	}
	return GrantRuleFor(tool, input, always), true
}

// GrantsFrom normalizes whatever the runtime put under GrantInputKey
// into the list of allow rules it stands for.
//
// Three shapes reach here. A []string is the current form: every grant
// the run has earned, so an `allow always` answered at one pause still
// holds at the next. A bare string is the older single-grant form, kept
// so a checkpoint written by a previous build resumes with its grant
// intact. A []any is the same slice after a JSON round-trip through the
// checkpoint. Anything else, including a nil entry, yields no rules.
func GrantsFrom(v any) []string {
	switch grant := v.(type) {
	case string:
		if grant == "" {
			return nil
		}
		return []string{grant}
	case []string:
		out := make([]string, 0, len(grant))
		for _, rule := range grant {
			if rule != "" {
				out = append(out, rule)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(grant))
		for _, raw := range grant {
			if rule, ok := raw.(string); ok && rule != "" {
				out = append(out, rule)
			}
		}
		return out
	default:
		return nil
	}
}
