package compatgw

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// ReasoningEffortsEnv is additive: old runners ignore it, and malformed
// entries never invalidate the independent, strictly decoded pricing table.
const ReasoningEffortsEnv = "OPENAI_COMPATIBLE_REASONING_EFFORTS"

// ReasoningEfforts reads the route's accepted wire values. Each entry parses
// through the shared effort vocabulary (modelspecs.ParseEffortLevels); an
// invalid entry is dropped with a named warning. A malformed JSON document
// drops only this optional mapping; the gateway catalog still resolves
// normally.
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
		values, err := modelspecs.ParseEffortLevels(entries[id])
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
