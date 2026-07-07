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
	Input map[string]interface{}
}

// FixturePath returns the testdata path for a scenario's fixture,
// relative to the package directory (where `go test` runs).
func (s Scenario) FixturePath() string {
	return filepath.Join("testdata", "bot-goldens", s.Bot, s.Name+".json")
}

// Scenarios returns the wired golden scenarios for this iteration:
// feature_dev (the v2 campaign termination contract), whats-next (the
// assignee-bearing bot, two scenarios), and docs-refresh. The ADR-058 v2
// bots' `campaign` nodes are claude_code whole-session agents — recording
// them live is impractical (cost, side-effects, interaction: human), so
// their fixtures are hand-authored seeds frozen against the termination
// schema (same provenance tier as the original seeds; see the _note field).
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
			Input: map[string]interface{}{
				"fail_log": "",
			},
		},
		{
			Bot:            "whats-next",
			Name:           "propose_roadmap_basic",
			Node:           "propose_roadmap",
			CheckAssignees: true, // roadmap_item.assignee must resolve to a real bot or be ""
			Vars: map[string]string{
				"scope_notes": "",
			},
			Input: map[string]interface{}{
				"exploration":     map[string]interface{}{"observations": []interface{}{}},
				"user_priorities": "improve test coverage and developer tooling",
				"workspace_dir":   "",
			},
		},
		{
			Bot:              "whats-next",
			Name:             "emit_action_basic",
			Node:             "emit_action",
			RequiredNonEmpty: []string{"created_issues"},
			CheckAssignees:   true,
			Input: map[string]interface{}{
				"roadmap":         map[string]interface{}{},
				"user_priorities": "improve test coverage and developer tooling",
				"workspace_dir":   "",
				"selected_titles": []interface{}{},
			},
		},
		{
			Bot:              "docs-refresh",
			Name:             "campaign_docs_aligned",
			Node:             "campaign",
			RequiredNonEmpty: []string{"summary"},
			CheckAssignees:   true, // docs-refresh routes no work to bots; scan must stay clean
			Input: map[string]interface{}{
				"total_docs":                  float64(3),
				"total_anchors":               float64(12),
				"verified_anchors":            float64(11),
				"drifted_anchors":             float64(1),
				"unverifiable_anchors":        float64(0),
				"manifest_coverage_pct":       float64(91),
				"coverage_target_pct":         float64(80),
				"drift_candidates":            []interface{}{},
				"docs_with_drift_count":       float64(1),
				"chunked":                     false,
				"chunk_doc_count":             float64(1),
				"max_review_chunk_docs":       float64(30),
				"recently_changed_code_files": []interface{}{},
				"fail_log":                    "",
			},
		},
	}
}
