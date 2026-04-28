package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"regexp"
	"sort"
	"strings"
)

func ToolSearchTool() api.Tool {
	return api.Tool{
		Name:        "tool_search",
		Description: "Search for available tools by name or description. Supports exact selection (select:name1,name2) and keyword search.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"query":       {Type: "string", Description: "Search query. Use 'select:tool1,tool2' for exact matches, or keywords for fuzzy search."},
				"max_results": {Type: "integer", Description: "Maximum number of results to return (default 5)."},
			},
			Required: []string{"query"},
		},
	}
}

// ToolSearchResult holds search results.
type ToolSearchResult struct {
	Matches            []string `json:"matches"`
	Query              string   `json:"query"`
	NormalizedQuery    string   `json:"normalized_query"`
	TotalDeferredTools int      `json:"total_deferred_tools"`
}

// ExecuteToolSearch searches the provided tool list by name/description.
func ExecuteToolSearch(input map[string]any, allTools []api.Tool) (string, error) {
	query, ok := input["query"].(string)
	if !ok || query == "" {
		return "", fmt.Errorf("tool_search: 'query' is required")
	}

	maxResults := 5
	if raw, ok := input["max_results"]; ok {
		if n, ok := toInt64(raw); ok && n > 0 {
			maxResults = int(n)
		}
	}

	var matches []string

	// Handle "select:" syntax
	if strings.HasPrefix(query, "select:") {
		names := strings.Split(strings.TrimPrefix(query, "select:"), ",")
		toolMap := make(map[string]bool, len(allTools))
		for _, t := range allTools {
			toolMap[t.Name] = true
		}
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" && toolMap[name] {
				matches = append(matches, name)
			}
		}
	} else {
		// Keyword search with scoring
		type scored struct {
			name  string
			score int
		}

		normalized := normalizeToolSearchQuery(query)
		terms := strings.Fields(normalized)
		if len(terms) == 0 {
			// Empty normalized query: return all tool names
			for _, t := range allTools {
				matches = append(matches, t.Name)
			}
		} else {
			var required []string
			var optional []string
			for _, term := range terms {
				if strings.HasPrefix(term, "+") {
					required = append(required, strings.TrimPrefix(term, "+"))
				} else {
					optional = append(optional, term)
				}
			}
			allTerms := make([]string, 0, len(required)+len(optional))
			allTerms = append(allTerms, required...)
			allTerms = append(allTerms, optional...)

			var results []scored
			for _, t := range allTools {
				nameLower := strings.ToLower(t.Name)
				descLower := strings.ToLower(t.Description)
				nameCanonical := canonicalize(t.Name)
				descCanonical := canonicalize(t.Description)

				// Check required terms
				allRequired := true
				for _, req := range required {
					if !strings.Contains(nameLower, req) && !strings.Contains(descLower, req) {
						allRequired = false
						break
					}
				}
				if !allRequired {
					continue
				}

				score := 0
				for _, term := range allTerms {
					termCanonical := canonicalize(term)
					if nameLower == term {
						score += 8
					}
					if nameCanonical == termCanonical {
						score += 12
					}
					if strings.Contains(nameLower, term) {
						score += 4
					}
					if strings.Contains(descLower, term) {
						score += 2
					}
					if strings.Contains(descCanonical, termCanonical) {
						score += 3
					}
				}
				if score > 0 {
					results = append(results, scored{name: t.Name, score: score})
				}
			}

			sort.Slice(results, func(i, j int) bool {
				if results[i].score != results[j].score {
					return results[i].score > results[j].score
				}
				return results[i].name < results[j].name
			})

			for _, r := range results {
				matches = append(matches, r.name)
			}
		}
	}

	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	result := ToolSearchResult{
		Matches:            matches,
		Query:              query,
		NormalizedQuery:    normalizeToolSearchQuery(query),
		TotalDeferredTools: len(allTools),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

var nonAlphanumericRe = regexp.MustCompile(`[^a-z0-9\s+]`)

func normalizeToolSearchQuery(q string) string {
	q = strings.ToLower(q)
	q = nonAlphanumericRe.ReplaceAllString(q, "")
	// Remove "tool" suffix from tokens
	tokens := strings.Fields(q)
	var filtered []string
	for _, t := range tokens {
		t = strings.TrimSuffix(t, "tool")
		if t != "" {
			filtered = append(filtered, t)
		}
	}
	return strings.Join(filtered, " ")
}

var canonicalizeRe = regexp.MustCompile(`[^a-z0-9]`)

func canonicalize(s string) string {
	return canonicalizeRe.ReplaceAllString(strings.ToLower(s), "")
}
