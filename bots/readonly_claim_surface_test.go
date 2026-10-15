package bots

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// readonlyIncapacityClaims are the phrasings that tell a node what its SURFACE
// is — that it holds no write path at all. They are matched case-insensitively
// against every line of a node's resolved system prompt.
//
// The distinction this list draws, and the reason it is this short: a MANDATE
// says what the node must do ("never edit code", "change nothing", "Do NOT
// write any file"), and stays true whatever the argv is. A CLAIM states a fact
// about the argv — and on `claude_code` the argv contradicts it: `readonly:`
// withholds the file-writing TOOLS, not the write capability, so a node that
// keeps a shell writes any file it can reach
// (docs/dsl.md, delegate.Task.Readonly). A mandate is left alone on purpose;
// widening this list to catch one would force a rewrite on every read-only
// instruction in the catalogue and prove nothing.
var readonlyIncapacityClaims = []string{
	"no write tool",
	"purely read-only",
	"read-only on the workspace",
	"read-only on workspace",
}

// namesAShell reports whether a prompt body tells the node about the shell it
// keeps. It is the greppable escape hatch of the guard below: a claim read
// beside the shell that contradicts it is not a node being told it cannot
// write — it is the shape the catalogue's 20 injection-boundary paragraphs
// already carry ("`readonly: true` withholds the file-writing tools and keeps
// Bash").
//
// Read over the WHOLE body, not the claim's own line: bots/rgaa-audit states
// the mandate in its first paragraph and "You hold bash" in the next one, and
// a line-local check would call that prompt dishonest.
func namesAShell(body string) bool {
	low := strings.ToLower(body)
	return strings.Contains(low, "bash") || strings.Contains(low, "shell")
}

// declaredWriters names the writing tools a node's own `tools:` list asks for
// — the ones `readonly:` removes anyway on every backend that reads the flag.
//
// The concepts are toolcatalog.WritingCanonicals(), the ONE set the delegate's
// own withholding reads (claudeReadonlyWithheld) — not a literal of this
// file's: a corpus rule narrower than the production list it enforces passes
// a node the flag would strip, in silence. The fold through
// CanonicalToolName is what makes `write_file`, `apply_patch` and
// `str_replace` classify like `write` and `edit`.
func declaredWriters(tools []string) []string {
	var declared []string
	for _, name := range tools {
		for _, canonical := range toolcatalog.WritingCanonicals() {
			if toolcatalog.CanonicalToolName(name) == canonical {
				declared = append(declared, name)
			}
		}
	}
	return declared
}

// readonlySpawnKeepsAShell reports whether a claude_code spawn of this node
// still offers the model a shell, which is what makes an incapacity claim
// false.
//
// It reads the same key the delegate reads: `toolcatalog.CanonicalToolName`,
// whose `bash` row is what `delegate.claudeNativeForCanonical["bash"]`
// projects onto the native `Bash`, plus `diagnostic_shell`, which the executor
// maps onto the same native (pkg/backend/model/executor_build_task.go,
// `DiagnosticShell`). An UNDECLARED node keeps the full native roster, shell
// included — that is the surface `readonly:` used to mean nothing on.
func readonlySpawnKeepsAShell(tools []string) bool {
	if len(tools) == 0 {
		return true
	}
	for _, name := range tools {
		if toolcatalog.CanonicalToolName(name) == "bash" || strings.TrimSpace(name) == "diagnostic_shell" {
			return true
		}
	}
	return false
}

// TestNoReadonlyClaudeCodeNodeIsToldItHoldsNoWritePath is the class contract
// behind #1816's docs half: a `readonly: true` node on `claude_code` that
// keeps a shell may not be told its surface holds no write path, unless the
// same prompt names the shell.
//
// Why a guard and not a sweep: the statement is shipped prompt text, and the
// class was corrected twice from a one-off grep — each pass fixed the sites
// the pattern of the day matched and left siblings behind. Keyed on the
// COMPILED corpus (`shippedWorkflows` + `wf.Prompts`), so a `- item` list, an
// inline `[a, b]` list and a missing `tools:` key classify the same way, and a
// bundle copying the pattern is checked the day it lands.
//
// What it does not prove: that a discharged prompt is well written. Naming the
// shell is the floor — the author is confronted with it in the same prompt —
// not a demonstration that the sentence beside it is true.
func TestNoReadonlyClaudeCodeNodeIsToldItHoldsNoWritePath(t *testing.T) {
	// The fold this guard depends on: a declaration spelled `shell`, `sh` or
	// `run_command` grants the same native Bash as `bash`, so counting only
	// the literal spelling would read a shell-holding node as shell-free.
	for _, spelling := range []string{"bash", "shell", "sh", "run_command"} {
		if got := toolcatalog.CanonicalToolName(spelling); got != "bash" {
			t.Fatalf("toolcatalog.CanonicalToolName(%q) = %q, want \"bash\": this guard counts a declared shell through that fold, and a row that moved makes it read a shell-holding node as shell-free", spelling, got)
		}
	}

	var offenders []string
	checked := 0
	for _, sw := range shippedWorkflows(t) {
		ids := make([]string, 0, len(sw.wf.Nodes))
		for id := range sw.wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			llm, ok := sw.wf.Nodes[id].(ir.LLMNode)
			if !ok {
				continue
			}
			f := llm.GetLLMFields()
			if !f.Readonly || !strings.Contains(resolvedBackend(llm, sw.wf.DefaultBackend), "claude_code") {
				continue
			}
			if !readonlySpawnKeepsAShell(llm.GetTools()) {
				continue
			}
			checked++
			body := ""
			if p := sw.wf.Prompts[f.SystemPrompt]; p != nil {
				body = p.Body
			}
			if namesAShell(body) {
				continue
			}
			for _, line := range strings.Split(body, "\n") {
				low := strings.ToLower(line)
				for _, claim := range readonlyIncapacityClaims {
					if strings.Contains(low, claim) {
						offenders = append(offenders, fmt.Sprintf("%s%s (tools %v): %s",
							sw.prefix, id, llm.GetTools(), strings.TrimSpace(line)))
						break
					}
				}
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d prompt line(s) tell a readonly claude_code node that keeps a shell it holds no write path, and no line of the same prompt names that shell — on this backend `readonly:` removes the writing TOOLS, not the write capability, so the shell writes any file it can reach; state the shell, or drop it from the node's tools: list:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the walk, because this guard selects on three node fields at
	// once: a rename, a backend default that stops expanding, or a `tools:`
	// reading that narrows would make it pass over nothing at all. 35 is under
	// the 45 shipped today, so bundles may be retired without touching this
	// line.
	if checked < 35 {
		t.Fatalf("the walk reached %d readonly claude_code node(s) keeping a shell, want at least 35: the discovery broke, and this guard is now green over an unexamined class", checked)
	}
}

// TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool is the corpus half of
// the rule: `readonly: true` and a `tools:` list naming a writer are two
// statements that contradict each other, and the flag wins wherever it is
// read, so the pair leaves a node whose prompt mandates a file it cannot
// write with the tool it names.
//
// Backend-independent, because every backend makes the pair a defect and not
// one of them keeps the writer:
//
//   - codex runs the node in a read-only OS sandbox (codex.go), pi narrows
//     its tool list to readers (pi.go), claude_code withholds the writing
//     natives from both of its spawns (claudeReadonlyWithheld) — the
//     declaration is dead on all three;
//   - `claw`, `opencode` and `cliagent` do not read the flag at all, so there
//     the writer DOES run, inside a workspace the engine admitted the node
//     into as non-mutating (pkg/runtime, isMutatingNodeIn) — a write beside a
//     parallel branch that was told nobody mutates.
//
// The way out is the declaration the node means: drop `readonly:` when the
// deliverable is a written file (its fan-out admission goes with it), or drop
// the writer when the node really only reads.
//
// The predicate is the DECLARED writer, and on the three backends that ignore
// the flag two other shapes hand the node the same writer with the same
// non-mutating admission, both outside it. An OMITTED `tools:` list is the
// full native roster there: 0 of the 10 corpus `readonly:` nodes that resolve
// to one of those backends omits it today, and the nearest miss is
// `bots/evolve` `review_gpt`, saved only by its declared-EMPTY `tools: []`.
// And `auto_memory` on claw appends `read_file, write_file, glob` to any
// NON-EMPTY list (pkg/backend/model/executor_build_task.go), switchable per
// node, per workflow or deployment-wide (automemory.ModeEnv) — a writer that
// never appears in `tools:` at all, and 9 of those 10 nodes have a non-empty
// list. Neither shape is visible here, nor to `isMutatingNodeIn`, which
// answers on `n.Readonly` before it consults the effective surface.
//
// Walked over catalogWorkflowFiles: every `.bot` of the catalogue bundles,
// the examples, the dispatcher template embedded in every binary and the
// operator scripts — so a bundle copying the shape is caught the day it
// lands. NOT the scaffold templates under pkg/botscaffold/templates: those
// are Go templates, unparseable as `.bot` until rendered, and the same
// predicate is asserted on the RENDERED gallery by
// pkg/botscaffold.TestNoScaffoldedNodeDeclaresReadonlyWithAWritingTool — the
// root `iterion bots create` copies from, and the one the sibling needle
// guard needed a floor of its own for.
//
// A router cannot be in the class: `readonly:` and `tools:` are not router
// properties — the parser answers E012 `unknown router property` for both —
// so `*ir.RouterNode` carries LLMFields whose Readonly no `.bot` can set, and
// a `case` for it here would be a branch nothing reaches.
func TestNoShippedWorkflowDeclaresReadonlyWithAWritingTool(t *testing.T) {
	paths := catalogWorkflowFiles(t)
	// A FLOOR on the discovery: this guard reads a glob, and a layout rename
	// that stopped matching would make it pass over nothing at all. 60 is
	// under the 70 files shipped today, so bundles may be retired without
	// touching this line.
	if len(paths) < 60 {
		t.Fatalf("the walk found %d workflow file(s), want at least 60: the discovery broke, and this guard is now green over an unexamined corpus", len(paths))
	}
	var offenders []string
	nodes, readonlyNodes, writerNodes := 0, 0, 0
	for _, path := range paths {
		wf := compileBotFile(t, path)
		ids := make([]string, 0, len(wf.Nodes))
		for id := range wf.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			llm, ok := wf.Nodes[id].(ir.LLMNode)
			if !ok {
				continue
			}
			nodes++
			writers := declaredWriters(llm.GetTools())
			if len(writers) > 0 {
				writerNodes++
			}
			if !llm.GetLLMFields().Readonly {
				continue
			}
			readonlyNodes++
			if len(writers) == 0 {
				continue
			}
			offenders = append(offenders, fmt.Sprintf("%s %s (backend %q, writers %v)",
				path, id, resolvedBackend(llm, wf.DefaultBackend), writers))
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d node(s) declare `readonly: true` together with a writing tool, which no backend keeps — codex runs the node in a read-only OS sandbox, pi pins its tool list to four readers, claude_code withholds the writing natives from both spawns, and the three that ignore the flag let the node write inside a workspace the engine admitted it into as non-mutating; drop `readonly:` where the deliverable is a written file, or drop the writer:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the node reading too: a `tools:` or `readonly:` accessor
	// that stopped resolving would leave the walk reading files and
	// classifying nothing. One count per SELECTION, not one for the walk: the
	// offender is the INTERSECTION of two predicates, so a total over every
	// LLM node stays green while either one answers nothing — measured, with
	// `declaredWriters` returning nil for every node and with the flag read
	// as always-false, and the whole package stayed green on both.
	if nodes < 140 {
		t.Fatalf("the walk reached %d agent/judge node(s), want at least 140: the IR reading broke, and this guard is now green over an unexamined corpus", nodes)
	}
	// 45 is under the 57 that carry the flag today, 12 under the 17 that
	// declare a writer: a bundle may be retired without touching these lines.
	if readonlyNodes < 45 {
		t.Fatalf("the walk read `readonly: true` on %d node(s), want at least 45: the flag reading broke, and this guard now intersects an empty set", readonlyNodes)
	}
	if writerNodes < 12 {
		t.Fatalf("the walk read a declared writer on %d node(s), want at least 12: declaredWriters (or the spelling table under it) broke, and this guard now intersects an empty set", writerNodes)
	}
}

// bundleReadonlyFlags reads, per bundle directory, whether each node id
// declares `readonly: true` — unioned over EVERY workflow of the bundle, not
// its main.bot alone: a sibling entrypoint declares nodes the prose names as
// freely, and a map built from main.bot would read one of those as absent and
// call a true sentence stale.
func bundleReadonlyFlags(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, sw := range shippedWorkflows(t) {
		bundle := sw.prefix
		if i := strings.IndexAny(bundle, "/:"); i >= 0 {
			bundle = bundle[:i]
		}
		if out[bundle] == nil {
			out[bundle] = map[string]bool{}
		}
		for id, n := range sw.wf.Nodes {
			llm, ok := n.(ir.LLMNode)
			if !ok {
				continue
			}
			out[bundle][id] = out[bundle][id] || llm.GetLLMFields().Readonly
		}
	}
	return out
}

// isIdentifierByte reports whether b can sit inside a node id, which is what
// makes the id match below a WHOLE word: `render` inside
// "attribute-rendering" is not the node `render`, and reading it as one is a
// false alarm this guard measured before the boundary was added.
func isIdentifierByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// readonlyAttributionOwner names the node a `(…)` attribute list at `open`
// describes: the nearest node id ENDING within 20 characters before the
// parenthesis, as a whole word. Empty when there is none.
//
// Nearest, and not "any id in the window", because these lists are written in
// sequence — `- gate` one line above `- review (adaptive claude_code,
// readonly)` put two candidates in front of one parenthesis, and the further
// one is the sibling bullet, not the subject. 20 characters is what the
// corpus's own longest connector needs (`triage agent (claw, readonly)`);
// a window wide enough to reach across a sentence is what read a list of HTML
// attributes as a node's tool surface.
func readonlyAttributionOwner(prose string, open int, flags map[string]bool) string {
	owner, ownerEnd := "", -1
	for id := range flags {
		for at := 0; at < open; {
			i := strings.Index(prose[at:open], id)
			if i < 0 {
				break
			}
			abs := at + i
			at = abs + 1
			end := abs + len(id)
			if abs > 0 && isIdentifierByte(prose[abs-1]) {
				continue
			}
			if end < len(prose) && isIdentifierByte(prose[end]) {
				continue
			}
			if open-end > 20 || end <= ownerEnd {
				continue
			}
			owner, ownerEnd = id, end
		}
	}
	return owner
}

// TestNoBundlePageAttributesReadonlyToANodeThatDoesNotDeclareIt is the class
// contract of a bundle's SHIPPED prose about its own nodes: a README or a
// skill that lists a node's attributes — `llm_review (claude_code, readonly,
// board.create + board.label)` — states what the `.bot` declares, and the
// word is a fact the run can be read against, not a label.
//
// Load-bearing because the skill is not author-only: sec-audit-deps' own
// `review_system` prompt lists `sec-audit-deps.md` under "Required reading
// (mirrored to .claude/skills/ at run time)", so the node reads its own phase
// description every run. A node told its flag withholds its only non-shell
// write path, by a prompt that also hands it `write_file`, is given two
// contradicting statements of its own surface — and the honest reading of the
// stale one produces the mandated report through a shell redirection instead.
//
// Scope, stated rather than implied: the ATTRIBUTE-LIST shape, a `(…)` group
// carrying the bare word `readonly` behind a node's name. A page quoting the
// declaration (`readonly: true`) or discussing the flag in prose is left
// alone — those say what the flag means, which the needle guard in
// pkg/backend/delegate owns, not which node carries it.
func TestNoBundlePageAttributesReadonlyToANodeThatDoesNotDeclareIt(t *testing.T) {
	flagsByBundle := bundleReadonlyFlags(t)
	var offenders []string
	attributions, pages := 0, 0
	bundles := make([]string, 0, len(flagsByBundle))
	for bundle := range flagsByBundle {
		bundles = append(bundles, bundle)
	}
	sort.Strings(bundles)
	for _, bundle := range bundles {
		flags := flagsByBundle[bundle]
		if st, err := os.Stat(bundle); err != nil || !st.IsDir() {
			continue
		}
		if err := filepath.WalkDir(bundle, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			pages++
			// Collapsed the way a reader reads it: these lists wrap, and the
			// name wears backticks or bold markers in half the corpus.
			prose := strings.ToLower(string(raw))
			for _, strip := range []string{"`", "*"} {
				prose = strings.ReplaceAll(prose, strip, " ")
			}
			prose = strings.Join(strings.Fields(prose), " ")
			for open := 0; open < len(prose); open++ {
				if prose[open] != '(' {
					continue
				}
				width := strings.Index(prose[open:], ")")
				if width < 0 {
					break
				}
				inner := prose[open+1 : open+width]
				at := strings.Index(inner, "readonly")
				if at < 0 {
					continue
				}
				if end := at + len("readonly"); end < len(inner) && inner[end] == ':' {
					continue // a quoted declaration, not an attribute list
				}
				owner := readonlyAttributionOwner(prose, open, flags)
				if owner == "" {
					continue
				}
				attributions++
				if !flags[owner] {
					offenders = append(offenders, fmt.Sprintf("%s: %s (%s)", path, owner, inner))
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", bundle, err)
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d shipped page(s) call a node `readonly` while its `.bot` declares no such flag — the bundles mirror their skills into .claude/skills/ at run time, so the node reads this about itself; state what the declaration says, or restore the flag:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// TWO floors, because this guard is an intersection as well: the pages it
	// reads, and the attributions it recognised in them. A collapse step or a
	// window that stopped matching would leave it reading every page and
	// judging nothing. 8 is under the 10 attributions shipped today, and 100
	// is under the 241 pages the walk reads.
	if pages < 100 {
		t.Fatalf("the walk read %d bundle page(s), want at least 100: the discovery broke, and this guard is now green over an unexamined corpus", pages)
	}
	if attributions < 8 {
		t.Fatalf("the walk recognised %d node attribute list(s) naming `readonly`, want at least 8: the shape reading broke, and this guard is now green over prose it no longer parses", attributions)
	}
}

// TestEveryDroppedReadonlyCommentStatesWhatWentWithTheFlag is the ratchet on
// the comment the escape hatch leaves behind. A node whose deliverable is a
// written file drops `readonly:` and says so on itself; the flag carries TWO
// meanings, and a comment that explains one leaves the next author to
// rediscover the other by a failed run.
//
// The tool half: on claude_code the flag withholds the file-writing tools
// whatever the `tools:` list says (claudeReadonlyWithheld). The scheduling
// half: the flag is also the node's non-mutating admission into a parallel
// branch (pkg/runtime.isMutatingNodeIn returns false on `n.Readonly` alone),
// so dropping it makes the node mutating — and a second mutating branch under
// one router is a hard ErrCodeWorkspaceSafety at validation, with nothing in
// the node to say the flag was deliberately given up.
func TestEveryDroppedReadonlyCommentStatesWhatWentWithTheFlag(t *testing.T) {
	const marker = "## no `readonly:`"
	var offenders []string
	sites := 0
	for _, path := range catalogWorkflowFiles(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if !strings.Contains(strings.ToLower(line), marker) {
				continue
			}
			sites++
			// The block is the run of `##` lines the marker opens.
			block := ""
			for j := i; j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "##"); j++ {
				block += " " + strings.TrimSpace(lines[j])
			}
			block = strings.ToLower(strings.Join(strings.Fields(block), " "))
			if !strings.Contains(block, "fan-out") && !strings.Contains(block, "fan out") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("%d comment(s) explain a dropped `readonly:` without naming what went with it — the node's non-mutating fan-out admission (pkg/runtime, isMutatingNodeIn), which makes a parallel branch around this node a workspace-safety refusal; say both halves:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the marker: a comment reworded to "No readonly flag" would
	// leave this guard reading every bot and finding nothing to judge. 3 is
	// under the 4 shipped today — three report writers plus
	// bots/dep-update-guard `verify_build`, the site this guard found — so a
	// bundle may stop writing a report without touching this line.
	if sites < 3 {
		t.Fatalf("the walk found %d dropped-`readonly:` comment(s), want at least 3: the marker was reworded, and this guard is now green over a class it no longer recognises", sites)
	}
}

// resolvedBackend reads a node's backend at its authored default, falling back
// to the workflow's, so the classification does not depend on the host.
func resolvedBackend(llm ir.LLMNode, defaultBackend string) string {
	b := strings.TrimSpace(ir.ExpandWithDefault(llm.GetLLMFields().Backend, authoredDefaults))
	if b == "" {
		b = strings.TrimSpace(ir.ExpandWithDefault(defaultBackend, authoredDefaults))
	}
	return b
}
