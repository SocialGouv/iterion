package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// ---------------------------------------------------------------------------
// tool_search per-node honesty (#2717)
// ---------------------------------------------------------------------------
//
// claw's tool_search searches the registry's WHOLE catalogue and answers
// names and search metadata. A node declares a subset of that catalogue in
// its `tools:` list, and a call to a tool outside the subset fails as
// "unknown tool" — so a bare match told the model a tool exists without
// telling it THIS node cannot execute it ("select:bash" found bash for a
// reviewer that has no bash). The result now carries the node's verdict:
// every match is marked available or not, an available match carries the
// schema the "fetch exact tools" wording promises, and an unavailable one
// says why instead of handing out the invitation to call it.

// ctxKeyNodeExecutableTools carries the names of the tools the node under
// execution may actually run (its resolved ToolDefs, model-facing spelling).
// The dispatch paths that know the set — the in-process generation loop and
// the launcher's side of a sandboxed crossing — pin it on the context they
// hand to a tool's Execute.
type ctxKeyNodeExecutableTools struct{}

// WithNodeExecutableTools pins the executing node's executable tool names on
// the context a tool's Execute closure receives. Consumers (tool_search) read
// it to answer for THIS node rather than for the whole registry.
func WithNodeExecutableTools(ctx context.Context, names []string) context.Context {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return context.WithValue(ctx, ctxKeyNodeExecutableTools{}, set)
}

// NodeExecutableToolsFrom reads the pinned set. The second return is false
// when no dispatch path pinned one — a caller that cannot name the node's set
// must not guess it.
func NodeExecutableToolsFrom(ctx context.Context) (map[string]bool, bool) {
	set, ok := ctx.Value(ctxKeyNodeExecutableTools{}).(map[string]bool)
	return set, ok
}

// toolSearchResult mirrors claw's ToolSearchResult wire shape (the vendor's
// struct is internal); the enrichment parses it and re-emits it with the
// per-node detail beside the fields a reader already knows.
type toolSearchResult struct {
	Matches            []string `json:"matches"`
	Query              string   `json:"query"`
	NormalizedQuery    string   `json:"normalized_query"`
	TotalDeferredTools int      `json:"total_deferred_tools"`
}

// toolSearchMatch is one match with the node's verdict on it. InputSchema is
// the promise the tool's own description makes ("fetch exact tools") — kept
// for the tools the node may call, withheld from the ones it may not: handing
// out the schema of a tool the dispatch will refuse is the very lie the
// enrichment exists to remove.
type toolSearchMatch struct {
	Name        string          `json:"name"`
	Available   bool            `json:"available"`
	CallName    string          `json:"call_name,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Reason      string          `json:"unavailable_reason,omitempty"`
}

// toolSearchResultHonest is toolSearchResult plus the per-node detail.
type toolSearchResultHonest struct {
	toolSearchResult
	Tools []toolSearchMatch `json:"tools"`
	Note  string            `json:"note"`
}

// enrichToolSearchResult rewrites a tool_search answer for the node under
// execution: every match keeps its catalogue name and gains the node's
// verdict. haystack is the same snapshot the search ran against, so the
// descriptions and schemas it carries are the ones the match named.
func enrichToolSearchResult(raw string, haystack []api.Tool, executable map[string]bool) (string, error) {
	var res toolSearchResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		// The search's own output did not decode — hand it back untouched
		// rather than fail a lookup the model was told is free.
		return raw, nil
	}
	byName := make(map[string]api.Tool, len(haystack))
	for _, t := range haystack {
		byName[t.Name] = t
	}
	out := toolSearchResultHonest{
		toolSearchResult: res,
		Tools:            make([]toolSearchMatch, 0, len(res.Matches)),
		Note: "available reports whether THIS node may execute the tool: a match outside the node's tools: list " +
			"is part of the catalogue but cannot be called from here — selecting it does not grant it.",
	}
	for _, name := range res.Matches {
		match := toolSearchMatch{Name: name}
		// The catalogue keys tools by qualified name ("mcp.server.tool")
		// while the node calls them sanitized ("mcp_server_tool"): the
		// verdict compares the spelling the model must call with.
		callName := strings.ReplaceAll(name, ".", "_")
		if callName != name {
			match.CallName = callName
		}
		if t, ok := byName[name]; ok {
			match.Description = t.Description
		}
		if executable[callName] {
			match.Available = true
			if t, ok := byName[name]; ok {
				if schema, err := json.Marshal(t.InputSchema); err == nil {
					match.InputSchema = schema
				}
			}
		} else {
			match.Reason = "not in this node's tools: list — it exists in the catalogue, but a call from this node fails as unknown tool"
		}
		out.Tools = append(out.Tools, match)
	}
	enriched, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("tool_search: encode the per-node answer: %w", err)
	}
	return string(enriched), nil
}
