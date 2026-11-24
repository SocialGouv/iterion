package identity

import (
	"fmt"
	"sort"
	"strings"
)

// BulkDecodeError reports individual unreadable documents after a bulk cursor
// completes successfully. The accompanying map contains every readable row;
// callers must explicitly handle this error before using those partial results.
// A query or cursor failure never returns this type or a partial map.
type BulkDecodeError struct {
	Operation string
	Failures  map[string]error
}

func (e *BulkDecodeError) Error() string {
	ids := make([]string, 0, len(e.Failures))
	for id := range e.Failures {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("document %q: %v", id, e.Failures[id]))
	}
	return e.Operation + ": " + strings.Join(parts, "; ")
}
