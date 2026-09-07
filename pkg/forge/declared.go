package forge

import "encoding/json"

// UnmarshalDeclaring decodes b into v and reports whether the JSON object
// carried key at all — true even when its value is null. v must be a type
// with no UnmarshalJSON of its own (the callers pass a `type plain T` alias),
// or this recurses.
//
// `null` and an absent key both decode to the zero value, and for a pull
// request's head repository the two are different facts: a forge that models
// forks sends `"repo": null` once the head repo is DELETED or blocked, while
// an answer that omits the key never had one. Collapsing them leaves a
// refusal that cannot name its cause — and an empty name one careless step
// from "therefore the base repo". prforge.Ref draws the same line on the
// webhook side; this is the shared form for the API adapters, so the two
// sides cannot drift on what an absence means.
func UnmarshalDeclaring(b []byte, v any, key string) (bool, error) {
	if err := json.Unmarshal(b, v); err != nil {
		return false, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return false, err
	}
	_, declared := keys[key]
	return declared, nil
}
