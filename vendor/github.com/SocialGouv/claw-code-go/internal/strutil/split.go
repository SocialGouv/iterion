package strutil

import "strings"

// SplitComma splits a comma-separated value into trimmed non-empty items.
// Returns nil for an empty input.
func SplitComma(value string) []string {
	if value == "" {
		return nil
	}
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
