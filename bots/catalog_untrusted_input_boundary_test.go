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
//
// There is no second allowlist. #1494 drained the 104 acting prompts that
// shipped without the paragraph, so the class is closed by the predicate
// alone: a node that gains a writing tool, a board capability, or an omitted
// `tools:` list on a CLI backend reddens here until its prompt says what it
// reads is data.
func TestCatalogUntrustedInputBoundaryOnActingPrompts(t *testing.T) {
	deferred := map[string]string{
		"adr-cartograph/campaign":     "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"adr-cartograph/survey_code":  "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"adr-cartograph/verify_build": "DSL v2 session owns bots/adr-cartograph — fleet-contract § 1",
		"docs-refresh/campaign":       "DSL v2 session owns bots/docs-refresh — fleet-contract § 1",
		"docs-refresh/finalize_mr":    "DSL v2 session owns bots/docs-refresh — fleet-contract § 1",
		"modernize/upgrade_campaign":  "modernization campaign owns bots/modernize — fleet-contract § 1",
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
	for key := range deferred {
		if !seen[key] {
			stale = append(stale, key+" is not an acting prompt lacking the paragraph: drain its entry")
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

// TestUntrustedInputBoundaryParagraphInterpolatesNothing keeps the paragraph
// a LITERAL region of the system prompt.
//
// The paragraph's job is to NAME the fields whose values are data. Naming one
// with a template reference instead renders the value itself — the untrusted
// text — inside the authoritative half of the prompt, which is the exact
// attack the paragraph exists to refuse. It is not theoretical: a system
// prompt goes through the same resolver as a user prompt
// (pkg/backend/model.resolveSystemPrompt), so `{{input.issues}}` written
// inside the paragraph delivered a board issue body — titles and bodies built
// from the audited repository — into the instructions of a node holding bash
// and board.create.
//
// The rule is structural, not a list of forbidden field names: NO reference of
// any namespace between the marker line and the blank line that closes the
// paragraph. A path is as refused as a payload, because "which fields are
// untrusted" is not a property this guard can compute, and a predicate that
// tried to enumerate them would be widened by the next field. Write the field
// NAME in backticks; put anything that must render outside the paragraph.
func TestUntrustedInputBoundaryParagraphInterpolatesNothing(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read bots dir: %v", err)
	}
	var offenders []string
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bot := e.Name()
		if _, err := os.Stat(filepath.Join(bot, "main.bot")); err != nil {
			continue
		}
		wf := compilePlanPhaseBot(t, bot)
		names := make([]string, 0, len(wf.Prompts))
		for name := range wf.Prompts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p := wf.Prompts[name]
			if p == nil {
				continue
			}
			lines := strings.Split(p.Body, "\n")
			for i := 0; i < len(lines); i++ {
				if !strings.Contains(lines[i], untrustedInputBoundaryMarker) {
					continue
				}
				checked++
				for j := i + 1; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
					if strings.Contains(lines[j], "{{") {
						offenders = append(offenders, fmt.Sprintf("%s/%s:+%d %s", bot, name, j-i, strings.TrimSpace(lines[j])))
					}
					i = j
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no UNTRUSTED INPUT BOUNDARY paragraph found in the catalogue: this guard stopped seeing its own subject")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d line(s) interpolate a value inside an UNTRUSTED INPUT BOUNDARY paragraph — "+
			"the value renders into the authoritative half of the system prompt. Write the field NAME "+
			"in backticks (`issues`, `vars.scratch_dir`), or move the sentence out of the paragraph:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("checked %d boundary paragraphs", checked)
}
