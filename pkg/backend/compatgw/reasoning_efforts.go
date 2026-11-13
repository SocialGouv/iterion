package compatgw

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// ReasoningEffortsEnv is the runner's env table of accepted efforts, the
// fallback rung of AcceptedEfforts. It is additive: old runners ignore it,
// and malformed entries never invalidate the independent, strictly decoded
// pricing table.
const ReasoningEffortsEnv = "OPENAI_COMPATIBLE_REASONING_EFFORTS"

// SourceReasoningEffortsEnv is AcceptedEfforts' provenance when the
// runner's env table stated the id's efforts.
const SourceReasoningEffortsEnv = "env:" + ReasoningEffortsEnv

// AcceptedEfforts answers the reasoning_effort wire values a gateway id
// accepts and the source that stated them. It is the one ladder every
// effort writer reads:
//
//  1. the run's feed table (FeedTableEnv): its efforts map under the
//     normalized id, read through the memoised, planted-refusing reader
//     the price rung uses → `feed:<name>`. An entry the effort vocabulary
//     refuses warns REASONING_EFFORTS_INVALID and steps down;
//  2. OPENAI_COMPATIBLE_REASONING_EFFORTS, exact id (ReasoningEfforts) →
//     SourceReasoningEffortsEnv;
//  3. nothing: values nil and source "" — the requested effort passes.
func AcceptedEfforts(modelID string, get func(string) string, warn func(string, ...any)) (values []string, source string) {
	if values, feed, ok := feedEfforts(modelID, get, warn); ok {
		return values, SourceFeed(feed)
	}
	if values := ReasoningEfforts(modelID, get, warn); len(values) > 0 {
		return values, SourceReasoningEffortsEnv
	}
	return nil, ""
}

// ReasoningEfforts reads the env table's accepted wire values for one
// exact id. Each entry parses through the shared effort vocabulary
// (modelspecs.ParseEffortLevels); an invalid entry is dropped with a named
// warning. A malformed JSON document drops only this optional mapping;
// the gateway catalog still resolves normally.
func ReasoningEfforts(modelID string, get func(string) string, warn func(string, ...any)) []string {
	return envEffortsTable(get, warn)[modelID]
}

// envEffortsTable is the env table's valid entries, keyed as written.
func envEffortsTable(get func(string) string, warn func(string, ...any)) map[string][]string {
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
	table := make(map[string][]string, len(entries))
	for _, id := range keys {
		values, err := modelspecs.ParseEffortLevels(entries[id])
		if strings.TrimSpace(id) == "" {
			err = fmt.Errorf("empty model id")
		}
		if err != nil {
			warn("REASONING_EFFORTS_INVALID: %s[%q]: %v; ignoring entry", ReasoningEffortsEnv, id, err)
			continue
		}
		table[id] = values
	}
	return table
}

// feedEfforts answers one id's efforts from the run's feed table, with the
// feed that stated them; ok is false when the table states none for the
// id, or states a list the vocabulary refuses (warned by name).
func feedEfforts(modelID string, get func(string) string, warn func(string, ...any)) (values []string, feed string, ok bool) {
	table := feedTable(get)
	if table == nil {
		return nil, "", false
	}
	id := pricefeed.NormalizeID(modelID)
	entry, here := table.Efforts[id]
	if !here {
		return nil, "", false
	}
	values, err := feedEffortValues(entry)
	if err != nil {
		warn("REASONING_EFFORTS_INVALID: %s efforts[%q] (feed %s): %v; stepping down to %s", FeedTableEnv, id, entry.Feed, err, ReasoningEffortsEnv)
		return nil, "", false
	}
	return values, entry.Feed, true
}

// feedEffortValues reads a run-table efforts entry through the shared
// vocabulary — the server's strict parser wrote it, and the runner still
// refuses a level it does not know (a newer server's, a damaged value) or
// an empty list rather than map an effort by it.
func feedEffortValues(entry pricefeed.EffortsEntry) ([]string, error) {
	raw, err := json.Marshal(entry.Values)
	if err != nil {
		return nil, err
	}
	values, err := modelspecs.ParseEffortLevels(raw)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("the list is empty")
	}
	return values, nil
}

// CrossingReasoningEfforts renders the value the sandbox crossing writes
// under ReasoningEffortsEnv for a node on gateway id wire: the env table's
// valid entries, overlaid by the run table's valid efforts — each feed id
// written under its normalized spelling, over every env spelling of the
// same id, and under the node's own wire spelling — so an older sandbox
// image, whose route reads only this name by the exact wire id, maps an
// effort by the answer AcceptedEfforts gives the host. "" when neither
// table states anything: the crossing's explicit empty.
func CrossingReasoningEfforts(wire string, get func(string) string, warn func(string, ...any)) string {
	merged := map[string][]string{}
	for id, values := range envEffortsTable(get, warn) {
		merged[id] = values
	}
	if table := feedTable(get); table != nil {
		feed := make(map[string][]string, len(table.Efforts))
		for id, entry := range table.Efforts {
			id = pricefeed.NormalizeID(id)
			values, err := feedEffortValues(entry)
			if err != nil {
				warn("REASONING_EFFORTS_INVALID: %s efforts[%q] (feed %s): %v; not crossed", FeedTableEnv, id, entry.Feed, err)
				continue
			}
			feed[id] = values
		}
		for id := range merged {
			if values, ok := feed[pricefeed.NormalizeID(id)]; ok {
				merged[id] = values
			}
		}
		for id, values := range feed {
			merged[id] = values
		}
		if values, ok := feed[pricefeed.NormalizeID(wire)]; ok {
			merged[wire] = values
		}
	}
	if len(merged) == 0 {
		return ""
	}
	raw, _ := json.Marshal(merged) // a map of string lists always encodes
	return string(raw)
}
