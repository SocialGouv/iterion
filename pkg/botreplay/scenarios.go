package botreplay

import "path/filepath"

// Scenario links a golden fixture to the bot/node it captures and the
// invariants the replay test enforces on it. It is the single source of
// truth shared by record mode (which inputs to feed the node) and replay
// mode (which checks to run on the recorded output).
type Scenario struct {
	Bot  string
	Name string
	Node string

	// RequiredNonEmpty names output fields that must be present AND
	// non-empty — the semantic-presence check that schema validation
	// (which accepts an empty json array/object) cannot express.
	RequiredNonEmpty []string

	// CheckAssignees enables the no-hallucinated-assignee scan over the
	// recorded output.
	CheckAssignees bool

	// Record-only fields (ignored by replay). Input is the per-node
	// input map handed to NodeExecutor.Execute.
	Vars  map[string]string
	Input map[string]any
}

// FixturePath returns the testdata path for a scenario's fixture,
// relative to the package directory (where `go test` runs).
func (s Scenario) FixturePath() string {
	return filepath.Join("testdata", "bot-goldens", s.Bot, s.Name+".json")
}

// Scenarios returns the wired golden scenarios for this iteration:
// feature_dev + docs-refresh (the ADR-058 v2 campaign termination
// contracts — their claude_code whole-session `campaign` nodes are
// impractical to record live (cost, side-effects, interaction: human),
// so those fixtures are hand-authored seeds frozen against the
// termination schema; see the _note field) and whats-next (v2 — one
// conversational nexie turn, recorded live: claude_code + board MCP).
func Scenarios() []Scenario {
	return []Scenario{
		{
			Bot:              "feature-dev",
			Name:             "campaign_feature_complete",
			Node:             "campaign",
			RequiredNonEmpty: []string{"summary"},
			CheckAssignees:   true, // campaign_output carries no assignees; scan must stay clean
			Vars: map[string]string{
				"feature_prompt": "add Answer() int returning 42 in answer.go",
			},
			Input: map[string]any{
				"fail_log": "",
			},
		},
		{
			// whats-next v2: ONE conversational agent. The golden freezes a
			// full nexie turn (reply + close + quick_replies + dispatched_ids)
			// against the nexie_turn schema; the assignee scan keeps any bot
			// names it mentions honest (it routes work via set_bot, so a
			// hallucinated bot name is the expensive failure).
			Bot:              "whats-next",
			Name:             "nexie_turn_basic",
			Node:             "nexie",
			RequiredNonEmpty: []string{"reply"},
			CheckAssignees:   true,
			Vars: map[string]string{
				"scope_notes":     "",
				"initial_message": "Quels sont les quick wins sur ce board ? Recommande-m'en un.",
			},
			Input: map[string]any{
				"operator_message": "Quels sont les quick wins sur ce board ? Recommande-m'en un.",
			},
		},
		{
			Bot:              "docs-refresh",
			Name:             "campaign_docs_aligned",
			Node:             "campaign",
			RequiredNonEmpty: []string{"summary"},
			CheckAssignees:   true, // docs-refresh routes no work to bots; scan must stay clean
			Input: map[string]any{
				"total_docs":                  float64(3),
				"total_anchors":               float64(12),
				"verified_anchors":            float64(11),
				"drifted_anchors":             float64(1),
				"unverifiable_anchors":        float64(0),
				"manifest_coverage_pct":       float64(91),
				"coverage_target_pct":         float64(80),
				"drift_candidates":            []any{},
				"docs_with_drift_count":       float64(1),
				"chunked":                     false,
				"chunk_doc_count":             float64(1),
				"max_review_chunk_docs":       float64(30),
				"recently_changed_code_files": []any{},
				"fail_log":                    "",
			},
		},
	}
}
