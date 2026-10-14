package bots

import (
	"fmt"
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
// withholds the file-writing TOOLS the node did not declare, not the write
// capability, so a node that keeps a shell writes any file it can reach
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

// readonlyWritingCanonicals are the canonical concepts a `tools:` entry has
// to fold onto for the node to hold a writer the flag leaves in place. They
// are the delegate's own list (pkg/backend/delegate, claudeWritingCanonicals),
// read through the shared spelling table, so `write_file`, `apply_patch` and
// `str_replace` classify like `write` and `edit`.
var readonlyWritingCanonicals = []string{"write", "edit", "notebookedit"}

// declaredWriters names the writing tools a node's own `tools:` list grants —
// the ones `readonly:` does NOT remove on claude_code, because deleting an
// explicit declaration in silence is what philosophy rule 1 forbids.
func declaredWriters(tools []string) []string {
	var kept []string
	for _, name := range tools {
		for _, canonical := range readonlyWritingCanonicals {
			if toolcatalog.CanonicalToolName(name) == canonical {
				kept = append(kept, name)
			}
		}
	}
	return kept
}

// withholdingScopes are the clauses that bound a statement of the flag to
// what it really removes: the writers the declaration did NOT name, or the
// kept one named outright. A statement carrying one is true on a node that
// declares a writer; an unscoped one is the opposite of what the backend logs
// on the same run ("readonly: true is NOT enforced for Write").
var withholdingScopes = []string{"did not declare", "did not name", "does not name", "is kept"}

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
		t.Errorf("%d prompt line(s) tell a readonly claude_code node that keeps a shell it holds no write path, and no line of the same prompt names that shell — on this backend `readonly:` removes the writing TOOLS the node did not declare, so the shell writes any file it can reach; state the shell, or drop it from the node's tools: list:\n  %s",
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

// TestNoReadonlyWriterIsToldTheFlagTookItsWritingTools is the other half of
// the class: a `readonly: true` claude_code node whose OWN `tools:` list
// names a writer keeps it (TestAReadonlyDeclarationThatNamesAWriterKeepsItAndSaysSo
// in pkg/backend/delegate asserts the argv, and the backend logs "readonly:
// true is NOT enforced for Write" on the same run), so a prompt telling that
// node the flag withholds its writing tools states the opposite of its own
// surface.
//
// Why it matters more than a wording: the model reads the prompt as its
// surface. Told its declared `write_file` was removed, it produces the report
// the same prompt mandates through `bash` redirection instead — and an author
// reading the same line believes those nodes hold no writer.
//
// Value-based, not a needle list: the offence is a statement of the flag's
// withholding with no clause bounding it to the undeclared writers, so a
// reworded claim is caught too. The escape hatch is greppable —
// withholdingScopes — and saying "your write_file is kept" discharges it.
func TestNoReadonlyWriterIsToldTheFlagTookItsWritingTools(t *testing.T) {
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
			writers := declaredWriters(llm.GetTools())
			if len(writers) == 0 {
				continue
			}
			checked++
			for _, name := range []string{f.SystemPrompt, f.UserPrompt} {
				p := sw.wf.Prompts[name]
				if p == nil {
					continue
				}
				// Collapsed the way the model reads it: these statements
				// wrap, and a per-line probe would call a scoped sentence
				// unscoped whenever the scope sat one wrap away.
				prose := strings.ToLower(strings.Join(strings.Fields(p.Body), " "))
				if !strings.Contains(prose, "readonly") || !strings.Contains(prose, "withhold") {
					continue
				}
				scoped := false
				for _, scope := range withholdingScopes {
					if strings.Contains(prose, scope) {
						scoped = true
						break
					}
				}
				if !scoped {
					offenders = append(offenders, fmt.Sprintf("%s%s (writers %v): prompt %q states the withholding with no scope",
						sw.prefix, id, writers, name))
				}
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d prompt(s) tell a readonly claude_code node that the flag withholds its writing tools, while its own tools: list declares one and the backend keeps it — scope the sentence to the writers the node did NOT declare, and name the kept one:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the discovery: this guard selects on `readonly:`, the
	// backend AND a declared writer at once, so a narrowed reading of any of
	// the three would make it pass over nothing. 3 is under the 4 shipped
	// today (sec-audit-deps, supply-shield, supply-shield-cve `llm_review`,
	// ultra11y `adjudicate`).
	if checked < 3 {
		t.Fatalf("the walk reached %d readonly claude_code node(s) declaring a writer, want at least 3: the discovery broke, and this guard is now green over an unexamined class", checked)
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
