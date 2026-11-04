package compatgw

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ReasoningEffortsEnv is additive: old runners ignore it, and malformed
// entries never invalidate the independent, strictly decoded pricing table.
const ReasoningEffortsEnv = "OPENAI_COMPATIBLE_REASONING_EFFORTS"

// ReasoningEfforts reads the route's accepted wire values. Each invalid entry
// is dropped with a named warning. A malformed JSON document drops only this
// optional mapping; the gateway catalog still resolves normally.
func ReasoningEfforts(modelID string, get func(string) string, warn func(string, ...any)) []string {
	raw := catalogEnv(get, ReasoningEffortsEnv)
	if raw == "" {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil {
		warn("REASONING_EFFORTS_INVALID: %s must be a JSON object; ignoring effort mapping", ReasoningEffortsEnv)
		return nil
	}
	keys := make([]string, 0, len(entries))
	for id := range entries {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var result []string
	for _, id := range keys {
		values, err := parseReasoningEfforts(entries[id])
		if strings.TrimSpace(id) == "" {
			err = fmt.Errorf("empty model id")
		}
		if err != nil {
			warn("REASONING_EFFORTS_INVALID: %s[%q]: %v; ignoring entry", ReasoningEffortsEnv, id, err)
			continue
		}
		if id == modelID {
			result = values
		}
	}
	return result
}

func parseReasoningEfforts(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || string(raw) == "null" {
		return nil, fmt.Errorf("expected an array of effort levels or integers 1..100")
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		var value string
		if err := json.Unmarshal(entry, &value); err != nil {
			value = string(entry)
		}
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 {
				return nil, fmt.Errorf("unknown effort level (expected none, minimal, low, medium, high, xhigh, max or an integer 1..100)")
			}
			value = strconv.Itoa(n)
		}
		values = append(values, value)
	}
	return values, nil
}
