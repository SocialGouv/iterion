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

// campaignScenario builds the shared shape of an ADR-058 v2 campaign
// termination-contract scenario: hand-authored fixture (recording a
// claude_code whole-session campaign node is impractical — cost, repo
// side-effects), summary required non-empty, assignee scan on (campaign
// outputs carry no assignees; the scan must stay clean).
func campaignScenario(bot, name string) Scenario {
	return Scenario{
		Bot:              bot,
		Name:             name,
		Node:             "campaign",
		RequiredNonEmpty: []string{"summary"},
		CheckAssignees:   true,
		Input:            map[string]any{"fail_log": ""},
	}
}

// Scenarios returns the wired golden scenarios: the ADR-058 v2 campaign
// termination contracts of the whole loop fleet (hand-authored seeds
// frozen against each bot's campaign_output schema — recording a
// claude_code whole-session campaign node live is impractical: cost,
// side-effects, interaction: human; see the fixtures' _note field),
// docs-refresh (same family, richer input), and whats-next (v2 — one
// conversational nexie turn, recorded live: claude_code + board MCP).
func Scenarios() []Scenario {
	campaigns := []Scenario{
		campaignScenario("whole-improve-loop", "campaign_axis_complete"),
		campaignScenario("branch-improve-loop", "campaign_branch_clean"),
		campaignScenario("feature-gap-fill", "campaign_gap_closed"),
		campaignScenario("test-coverage", "campaign_coverage_complete"),
		campaignScenario("adr-cartograph", "campaign_adrs_aligned"),
	}
	featureDev := campaignScenario("feature-dev", "campaign_feature_complete")
	featureDev.Vars = map[string]string{
		"feature_prompt": "add Answer() int returning 42 in answer.go",
	}
	campaigns = append(campaigns, featureDev)
	return append(campaigns,
		Scenario{
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
		Scenario{
			// whats-next v3: a roadmap-study synthesis turn — the reply
			// carries the chantiers/quick-wins/top-3/blind-spots markdown
			// and dispatch has NOT happened yet (dispatched_ids stays
			// empty until the operator arbitrates). Hand-authored seed:
			// a live study spawns 3 Task sub-agents over a real repo
			// (cost + side-effects), so the fixture freezes the envelope,
			// not the prose. The assignee scan stays on so any
			// assignee/bot key that ever enters the turn envelope is
			// checked against the real catalog.
			Bot:              "whats-next",
			Name:             "nexie_study_synthesis",
			Node:             "nexie",
			RequiredNonEmpty: []string{"reply"},
			CheckAssignees:   true,
			Vars: map[string]string{
				"scope_notes":     "",
				"initial_message": "Quels sont les prochains chantiers pour ce trimestre ?",
			},
			Input: map[string]any{
				"operator_message": "Quels sont les prochains chantiers pour ce trimestre ?",
			},
		},
		Scenario{
			// docs-refresh v3: the campaign's input is the ADVISORY hints
			// report (help, never obligations) — the termination-contract
			// OUTPUT schema is unchanged from v2.
			Bot:              "docs-refresh",
			Name:             "campaign_docs_aligned",
			Node:             "campaign",
			RequiredNonEmpty: []string{"summary"},
			CheckAssignees:   true, // docs-refresh routes no work to bots; scan must stay clean
			Input: map[string]any{
				"hints": []any{
					map[string]any{
						"doc": "README.md", "line": float64(3), "kind": "missing_path",
						"value": "cmd/app/old.go", "note": "path cited in the doc not found on disk (cmd/ exists)",
					},
				},
				"hint_count":                  float64(1),
				"hints_note":                  "1 missing path(s), 0 dead link(s)/anchor(s), 0 unmentioned area(s) — from 4 checkable path(s) + 2 internal link(s) across 3 doc(s); 0 ledger-dismissed excluded",
				"recently_changed_code_files": []any{},
				"fail_log":                    "",
			},
		},
	)
}
