package pricefeed

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// StaticSource is a source whose answer is its params: nothing to fetch,
// nothing to age. The fold reads a static feed's entries from its config
// at every fold (Fold), and the reconciler never refreshes one (Due).
// Fetch answers the same entries, so a manual sync through Refresh reads
// what the fold reads.
type StaticSource interface {
	Source
	// Entries answers the feed's entries from its params alone, under
	// the same rules ValidateParams enforces on a write.
	Entries(params map[string]string) (map[string]modelspecs.Spec, error)
}

// IsStatic reports whether a source type answers from its params.
func IsStatic(typ string) bool {
	src, ok := SourceFor(typ)
	if !ok {
		return false
	}
	_, static := src.(StaticSource)
	return static
}

// declaredParamEntries is the declared feed's one param: the entry
// document, in ParseFeedEntries' format.
const declaredParamEntries = "entries"

// declaredSource is the operator's own table, stored in the feed config:
// `params.entries` holds the operator-table JSON document plus
// `reasoning_efforts` (ParseFeedEntries). It is the seat for what no
// tariff source publishes — a gateway's accepted efforts, a window or a
// price an operator states by hand.
type declaredSource struct{}

func (declaredSource) ValidateParams(params map[string]string) error {
	_, err := declaredEntries(params)
	return err
}

func (declaredSource) Fetch(_ context.Context, params map[string]string) (map[string]modelspecs.Spec, error) {
	entries, err := declaredEntries(params)
	if err != nil {
		return nil, fmt.Errorf("pricefeed: %w", err)
	}
	return entries, nil
}

func (declaredSource) Entries(params map[string]string) (map[string]modelspecs.Spec, error) {
	return declaredEntries(params)
}

// declaredEntries parses and judges a declaration. Beyond the feed
// format's own rules it refuses an empty document, an entry that states
// nothing, and an entry that states a window, an output cap, a cache
// price or a capability flag without the price pair: an unpriced entry
// never answers (ADR-163 — the fold skips it), so such a declaration
// would be silently inert. An entry stating only reasoning_efforts is
// the one unpriced shape that answers: the efforts fold per field.
func declaredEntries(params map[string]string) (map[string]modelspecs.Spec, error) {
	what := "params." + declaredParamEntries
	raw := strings.TrimSpace(params[declaredParamEntries])
	if raw == "" {
		return nil, fmt.Errorf("%s is required: the declared table, e.g. {\"<gateway id>\": {\"input_usd_per_mtok\": F, \"output_usd_per_mtok\": F, \"reasoning_efforts\": [\"low\", \"high\"]}}", what)
	}
	entries, err := ParseFeedEntries(raw, what)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s declares no entry", what)
	}
	for _, id := range sortedIDs(entries) {
		spec := entries[id]
		if spec.InputCostPerM > 0 && spec.OutputCostPerM > 0 {
			continue
		}
		stated := unpricedFields(spec)
		if len(stated) == 0 {
			if len(spec.ReasoningEfforts) > 0 {
				continue
			}
			return nil, fmt.Errorf("%s[%q] states nothing: declare the price pair (input_usd_per_mtok, output_usd_per_mtok), reasoning_efforts, or both", what, id)
		}
		return nil, fmt.Errorf("%s[%q] states %s without the price pair — an unpriced entry never answers (ADR-163): state input_usd_per_mtok and output_usd_per_mtok beside it, or state only reasoning_efforts", what, id, strings.Join(stated, ", "))
	}
	return entries, nil
}

// unpricedFields names the fields an entry states beyond its prices and
// its efforts, in the format's own spelling.
func unpricedFields(spec modelspecs.Spec) []string {
	var stated []string
	if spec.ContextWindow > 0 {
		stated = append(stated, "context_window")
	}
	if spec.MaxOutputTokens > 0 {
		stated = append(stated, "max_output_tokens")
	}
	if spec.CacheReadCostPerM > 0 {
		stated = append(stated, "cache_read_usd_per_mtok")
	}
	if spec.Reasoning != nil {
		stated = append(stated, "reasoning")
	}
	if spec.ToolCall != nil {
		stated = append(stated, "tool_call")
	}
	if spec.Temperature != nil {
		stated = append(stated, "temperature")
	}
	return stated
}

// sortedIDs lists a parsed table's ids, sorted: a refusal names the same
// id whatever map order ranged.
func sortedIDs(entries map[string]modelspecs.Spec) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}
