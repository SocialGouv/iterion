package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// untrustedInputBoundaryMarker is the phrase every acting prompt carries. It
// is a posture cue, not a directive list: the paragraph it heads tells the
// LLM that what it reads out of the target repository is data, and a guard
// that enumerated forbidden spellings instead would be widened by the next
// payload (docs/agents/bot-authoring.md, "Prompts that can act carry the
// UNTRUSTED INPUT BOUNDARY").
const untrustedInputBoundaryMarker = "UNTRUSTED INPUT BOUNDARY"

// readCapabilities are the capabilities that only read. Every other one
// (board.create, board.comment, board.label, board.assign, board.move,
// board.close, and whatever the engine adds next) writes to a durable store
// off text the node was handed, so it puts the node in the class.
var readCapabilities = map[string]bool{
	boardops.CapBoardRead: true,
	runops.CapRunsRead:    true,
}

// authoredDefaults resolves a backend at its authored default (`${VAR:-x}`
// reads x) so the class does not depend on which dials the host running the
// test happens to set.
func authoredDefaults(string) string { return "" }

// actingPrompt is one agent/judge that can act on what it reads: a tool
// surface the engine classifies as able to write (pkg/runtime.ToolSurfaceCanWrite)
// or a capability that writes to the board.
type actingPrompt struct {
	node         string
	systemPrompt string
	reasons      []string
}

// actingPrompts classifies every agent and judge of a compiled bot.
// Capabilities follow the executor's inheritance rule: a node that declares
// none runs with the workflow's list (pkg/backend/model/executor_build_task.go,
// effectiveCaps).
func actingPrompts(wf *ir.Workflow) []actingPrompt {
	var out []actingPrompt
	for id, node := range wf.Nodes {
		llm, ok := node.(ir.LLMNode)
		if !ok {
			continue
		}
		var reasons []string
		if runtime.ToolSurfaceCanWrite(node, wf.DefaultBackend, authoredDefaults) {
			reasons = append(reasons, toolSurfaceReason(llm, wf.DefaultBackend))
		}
		caps := llm.GetCapabilities()
		if caps == nil {
			caps = wf.Capabilities
		}
		for _, c := range caps {
			if !readCapabilities[c] {
				reasons = append(reasons, "capability "+c)
			}
		}
		if len(reasons) == 0 {
			continue
		}
		out = append(out, actingPrompt{node: id, systemPrompt: llm.GetLLMFields().SystemPrompt, reasons: reasons})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].node < out[j].node })
	return out
}

// toolSurfaceReason names, for the failure message, what makes the node's
// tool surface a writing one.
func toolSurfaceReason(llm ir.LLMNode, defaultBackend string) string {
	f := llm.GetLLMFields()
	if f.FullAccess {
		return "full_access"
	}
	tools := llm.GetTools()
	if len(tools) == 0 {
		backend := strings.TrimSpace(ir.ExpandWithDefault(f.Backend, authoredDefaults))
		if backend == "" {
			backend = strings.TrimSpace(ir.ExpandWithDefault(defaultBackend, authoredDefaults))
		}
		if backend == "claw" {
			return "no tools: declared, and a fallbacks: route onto a CLI backend runs the full native toolset"
		}
		return "no tools: declared on " + backend + ", the full native toolset"
	}
	var acting []string
	for _, t := range tools {
		if !runtime.IsReadOnlyTool(t) {
			acting = append(acting, t)
		}
	}
	return "tools: " + strings.Join(acting, ", ")
}

// TestCatalogUntrustedInputBoundaryOnActingPrompts is the catalog-wide guard
// for the class contract of #1324: every agent or judge that can act on what
// it reads carries the UNTRUSTED INPUT BOUNDARY paragraph in its system
// prompt. The predicate walks the COMPILED IR, so a `- item` list, an inline
// `[a, b]` list and a missing `tools:` field classify the same way, and it
// asks the engine what the surface can do rather than matching tool names.
//
// The only exemption is a `deferred` entry: a bot another agent session owns
// (fleet contract § 1), keyed per agent so a NEW acting prompt on that bot
// still reddens. An entry that no longer names an acting prompt lacking the
// marker is stale and reddens too — the allowlist cannot rot in either
// direction.
func TestCatalogUntrustedInputBoundaryOnActingPrompts(t *testing.T) {
	deferred := map[string]string{
		"adr-cartograph/campaign":     "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"adr-cartograph/survey_code":  "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"adr-cartograph/verify_build": "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"docs-refresh/campaign":       "DSL v2 session owns bots/docs-refresh — fleet-contract § 1",
		"docs-refresh/finalize_mr":    "DSL v2 session owns bots/docs-refresh — fleet-contract § 1",
		"modernize/upgrade_campaign":  "modernization campaign owns bots/modernize — fleet-contract § 1",
	}
	// Acting prompts that lack the paragraph today, one entry per agent,
	// tracked by #1494. Each entry is a defect, not a home.
	knownMissing := map[string]string{
		"adr-rechallenge/file_change_ticket":     "#1494",
		"adr-rechallenge/survey_code":            "#1494",
		"adr-rechallenge/write_addendum":         "#1494",
		"app-dev/campaign":                       "#1494",
		"app-dev/deploy":                         "#1494",
		"app-dev/finalize_mr":                    "#1494",
		"app-dev/interviewer":                    "#1494",
		"app-dev/plan":                           "#1494",
		"app-dev/plan_review":                    "#1494",
		"app-dev/plan_revise":                    "#1494",
		"app-dev/review":                         "#1494",
		"app-dev/verify_build":                   "#1494",
		"arbitrate/arbitrate_judge":              "#1494",
		"bmady/analyst":                          "#1494",
		"bmady/architect":                        "#1494",
		"bmady/dev":                              "#1494",
		"bmady/pm":                               "#1494",
		"bmady/qa":                               "#1494",
		"branch-improve-loop/campaign":           "#1494",
		"branch-improve-loop/finalize_mr":        "#1494",
		"branch-improve-loop/plan":               "#1494",
		"branch-improve-loop/plan_review":        "#1494",
		"branch-improve-loop/plan_revise":        "#1494",
		"branch-improve-loop/review":             "#1494",
		"branch-improve-loop/verify_build":       "#1494",
		"copilot/copi":                           "#1494",
		"copilot/reflect":                        "#1494",
		"dep-update-guard/align":                 "#1494",
		"dep-update-guard/commit":                "#1494",
		"dep-update-guard/security_audit":        "#1494",
		"dep-update-guard/verify_build":          "#1494",
		"devbox-setup/detect_stack":              "#1494",
		"devbox-setup/generate_devbox":           "#1494",
		"e2e-coverage/campaign":                  "#1494",
		"e2e-coverage/plan":                      "#1494",
		"e2e-coverage/plan_review":               "#1494",
		"e2e-coverage/plan_revise":               "#1494",
		"e2e-coverage/verify_build":              "#1494",
		"evolve/emit_backlog":                    "#1494",
		"evolve/investigate":                     "#1494",
		"evolve/load_nexie_handoff":              "#1494",
		"evolve/propose_evolutions":              "#1494",
		"evolve/review_claude":                   "#1494",
		"evolve/revise_vision":                   "#1494",
		"evolve/survey":                          "#1494",
		"evolve/synthesize_vision":               "#1494",
		"feature-dev/campaign":                   "#1494",
		"feature-dev/finalize_mr":                "#1494",
		"feature-dev/plan":                       "#1494",
		"feature-dev/plan_review":                "#1494",
		"feature-dev/plan_revise":                "#1494",
		"feature-dev/review":                     "#1494",
		"feature-dev/verify_build":               "#1494",
		"feature-gap-fill/campaign":              "#1494",
		"feature-gap-fill/plan":                  "#1494",
		"feature-gap-fill/plan_review":           "#1494",
		"feature-gap-fill/plan_revise":           "#1494",
		"feature-gap-fill/verify_build":          "#1494",
		"golden-master/mutants_adversary":        "#1494",
		"golden-master/oracle_campaign":          "#1494",
		"instrument/campaign":                    "#1494",
		"instrument/finalize_mr":                 "#1494",
		"instrument/review":                      "#1494",
		"instrument/verify_build":                "#1494",
		"issue-triage/triage":                    "#1494",
		"product-docs/campaign":                  "#1494",
		"product-docs/finalize_mr":               "#1494",
		"product-docs/publish":                   "#1494",
		"revi-converse/converse_agent":           "#1494",
		"review-env/deploy":                      "#1494",
		"review-pr/converge":                     "#1494",
		"review-pr/reviewer_claude":              "#1494",
		"review-pr/reviewer_claude_glance":       "#1494",
		"review-pr/reviewer_gpt":                 "#1494",
		"review-pr/reviewer_gpt_glance":          "#1494",
		"rgaa-audit/campaign":                    "#1494",
		"rgaa-audit/report_card":                 "#1494",
		"secured-renovacy/align_code":            "#1494",
		"secured-renovacy/batch_upgrade_patches": "#1494",
		"secured-renovacy/changelog_review":      "#1494",
		"secured-renovacy/detect_stack":          "#1494",
		"secured-renovacy/discover_outdated":     "#1494",
		"secured-renovacy/family_align_code":     "#1494",
		"secured-renovacy/fix_after_upgrade":     "#1494",
		"secured-renovacy/install":               "#1494",
		"secured-renovacy/p2_campaign":           "#1494",
		"secured-renovacy/p2_verify_build":       "#1494",
		"secured-renovacy/security_audit":        "#1494",
		"secured-renovacy/upgrade":               "#1494",
		"secured-renovacy/validate_upgrade":      "#1494",
		"test-coverage/campaign":                 "#1494",
		"test-coverage/plan":                     "#1494",
		"test-coverage/plan_review":              "#1494",
		"test-coverage/plan_revise":              "#1494",
		"test-coverage/verify_build":             "#1494",
		"ultra11y/adjudicate":                    "#1494",
		"ultra11y/publish":                       "#1494",
		"whole-improve-loop/campaign":            "#1494",
		"whole-improve-loop/finalize_mr":         "#1494",
		"whole-improve-loop/plan":                "#1494",
		"whole-improve-loop/plan_review":         "#1494",
		"whole-improve-loop/plan_revise":         "#1494",
		"whole-improve-loop/verify_build":        "#1494",
		"wiki-gen/author":                        "#1494",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read bots dir: %v", err)
	}
	var missing, stale []string
	seen := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bot := e.Name()
		if _, err := os.Stat(filepath.Join(bot, "main.bot")); err != nil {
			continue
		}
		wf := compilePlanPhaseBot(t, bot)
		for _, m := range actingPrompts(wf) {
			key := bot + "/" + m.node
			body := ""
			if p := wf.Prompts[m.systemPrompt]; p != nil {
				body = p.Body
			}
			has := strings.Contains(body, untrustedInputBoundaryMarker)
			_, exempt := deferred[key]
			if !exempt {
				_, exempt = knownMissing[key]
			}
			switch {
			case has && exempt:
				stale = append(stale, key+" carries the paragraph: drain its entry")
			case has:
			case exempt:
				seen[key] = true
			default:
				missing = append(missing, fmt.Sprintf("%s (system prompt %q; %s)", key, m.systemPrompt, strings.Join(m.reasons, "; ")))
			}
		}
	}
	for _, exemptions := range []map[string]string{deferred, knownMissing} {
		for key := range exemptions {
			if !seen[key] {
				stale = append(stale, key+" is not an acting prompt lacking the paragraph: drain its entry")
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("%d acting prompt(s) lack the %s paragraph — write it (docs/agents/bot-authoring.md, \"Prompts that can act carry the UNTRUSTED INPUT BOUNDARY\"):\n  %s",
			len(missing), untrustedInputBoundaryMarker, strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("%d stale exemption(s):\n  %s", len(stale), strings.Join(stale, "\n  "))
	}
}
