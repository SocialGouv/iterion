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

// shippedWorkflow is one compiled workflow of the catalogue, with the prefix
// its nodes are keyed under.
type shippedWorkflow struct {
	prefix string
	wf     *ir.Workflow
}

// shippedWorkflows compiles EVERY workflow this repository ships, not just the
// `main.bot` of each bundle. A bundle may carry sibling entrypoints
// (golden-master ships extend.bot, reanchor.bot and sync-harness.bot) and a
// bundle may have no main.bot at all (smoke ships board_smoke.bot) — stat-ing
// main.bot made all of those invisible to the class, which is how three acting
// prompts shipped with no paragraph and no way to redden. The dispatcher's
// zero-config fallback is shipped too, compiled into every binary and run
// against a raw issue body, so it is walked from here rather than left
// outside every guard.
//
// Nodes of a `main.bot` keep the historical `bot/node` key; a sibling
// entrypoint is keyed `bot/file:node` so one file's node can never be
// mistaken for another's.
func shippedWorkflows(t *testing.T) []shippedWorkflow {
	t.Helper()
	var out []shippedWorkflow
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read bots dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "testdata" {
			continue
		}
		bot := e.Name()
		paths, err := filepath.Glob(filepath.Join(bot, "*.bot"))
		if err != nil {
			t.Fatalf("glob %s: %v", bot, err)
		}
		sort.Strings(paths)
		for _, path := range paths {
			prefix := bot + "/"
			if filepath.Base(path) != "main.bot" {
				prefix = bot + "/" + strings.TrimSuffix(filepath.Base(path), ".bot") + ":"
			}
			out = append(out, shippedWorkflow{prefix: prefix, wf: compileBotFile(t, path)})
		}
	}
	const dispatchFallback = "../pkg/cli/templates/dispatch_bots_default.bot"
	if _, err := os.Stat(dispatchFallback); err == nil {
		out = append(out, shippedWorkflow{prefix: "cli/dispatch_bots_default:", wf: compileBotFile(t, dispatchFallback)})
	} else {
		t.Fatalf("the dispatcher fallback template is no longer at %s: it is embedded in every binary and runs on a raw issue body, so it may not drop out of this walk silently", dispatchFallback)
	}
	return out
}

// compileBotFile compiles one workflow file with its imported fragments.
func compileBotFile(t *testing.T, path string) *ir.Workflow {
	t.Helper()
	cr := ir.Compile(parseBotUnit(path).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	return cr.Workflow
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

	var missing, stale []string
	seen := map[string]bool{}
	for _, sw := range shippedWorkflows(t) {
		for _, m := range actingPrompts(sw.wf) {
			key := sw.prefix + m.node
			body := ""
			if p := sw.wf.Prompts[m.systemPrompt]; p != nil {
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
	var offenders []string
	checked := 0
	for _, sw := range shippedWorkflows(t) {
		names := make([]string, 0, len(sw.wf.Prompts))
		for name := range sw.wf.Prompts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p := sw.wf.Prompts[name]
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
						offenders = append(offenders, fmt.Sprintf("%s%s:+%d %s", sw.prefix, name, j-i, strings.TrimSpace(lines[j])))
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
