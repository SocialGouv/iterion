package modelspecs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ParseEffortLevels reads one model's accepted reasoning efforts — a JSON
// array of named levels (none, minimal, low, medium, high, xhigh, max) or
// integers 1..100, as JSON numbers or strings — into their wire spelling:
// trimmed, lower-cased, integers in canonical decimal. It is the ONE
// vocabulary of accepted efforts: the runner's tolerant env reader
// (compatgw.ReasoningEfforts) and the price feed's strict entry parser
// (pricefeed.ParseFeedEntries) both read through it. It refuses what is
// not an array and any element outside the vocabulary; an empty array or
// a repeated level is the strict caller's business.
func ParseEffortLevels(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
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
				return nil, fmt.Errorf("unknown effort level %q (expected none, minimal, low, medium, high, xhigh, max or an integer 1..100)", value)
			}
			value = strconv.Itoa(n)
		}
		values = append(values, value)
	}
	return values, nil
}
