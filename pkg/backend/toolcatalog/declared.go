package toolcatalog

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ToolsDeclared reports whether the author declared a tool surface on the
// node — the ONE definition of that fact, so no reader re-derives it.
//
// `tools: []` is a declared value: the author wrote "this node has no tools".
// A node with no `tools:` line at all leaves the surface UNDECLARED, which
// every CLI backend reads as "no restriction". The two are carried apart by
// nilness from the parser down, which is why the parse arm for `tools:`
// returns a non-nil empty slice for `[]` while every other bracket list
// returns nil.
//
// The distinction stops at this predicate: `delegate.Task` carries the answer
// as a bool, so no backend re-derives nilness from a slice that crossed a
// JSON seam.
func ToolsDeclared(tools []string) bool { return tools != nil }

// ReceivesToolList reports whether a node's `tools:` list reaches a backend
// at all — the precondition for the list to bound anything there.
//
//   - claw resolves its ToolDefs from the list (but re-populates an empty
//     one with `ask_user` when `interaction:` is set, and the appends below
//     it follow);
//   - claude_code turns it into --disallowedTools over `claudeNativeTools`;
//   - codex maps it onto a sandbox mode.
//
// pi, kimi, grok and opencode are driven through the CLI-agent seam, which
// never passes the list to the agent: there a declared bound is silently
// dropped, which is what C270 warns about for a declared-empty list (a
// non-empty one is dropped just as silently, with no diagnostic at all).
//
// The default answer for an unknown backend is therefore the SAFE one — a
// backend is assumed not to receive the list until someone proves it does and
// names it here. `opencode` (#1640) is the worked example: it drives
// `opencode --format json run` with the prompt on stdin and reads no tool
// list at all, so `tools: [Bash, Read]` on such a node used to validate with
// zero diagnostics.
//
// It says the list ARRIVES, never that the node ends up holding nothing —
// and it is deliberately NOT a workspace-safety predicate. The claude_code
// roster is `backendcaps.NativeTools(ClaudeCode)`, a declaration of a surface
// iterion does not own, held against iterion's own second enumeration of it
// by delegate's anchoring guard (#1651): every tool it renders a header for
// is on the roster or excluded with a reason. Sibling lists still name tools
// the roster does not — `orchestrationTools` carries `TaskOutput` and
// `Monitor`, `headlessWithheldTools` carries `Workflow`, the cron family and
// `RemoteTrigger` — and every MCP tool survives any list, being on no roster.
// Which roster names survive a `tools: []` is the CLI's call, and some names
// are withheld from every spawn whatever the list says. On claw the runtime's
// own interaction and auto-memory appends can put `write_file` back. Anything that must know what
// a node can DO reads the effective surface, not this.
//
// It is also NOT ConstrainsTools, which answers a third question (can claw's
// registry resolve these bare names) and stays claw-only: widening that one
// turns advisory CLI-backend names into blocking C135 errors on shipped bots.
func ReceivesToolList(backend string) bool {
	return backendcaps.Offers(strings.TrimSpace(backend), backendcaps.ToolList)
}

// DurableWriteSpellingsOn are the names ONE backend can send for a durable
// write when nothing bounds its tool surface — the question a reader asks
// about a node whose `tools:` declaration does not reach, or does not bound,
// that route, where the agent keeps its own roster.
//
// It answers in NAMES a gate can match, never in canonical keys: a permission
// rule is evaluated against the name the backend's model emits, and a reader
// that handed the gate a bare concept key asked about a call no backend makes
// — which read `deny: ["Write*"]` as leaving a writer it removes
// (iterion#2832 round 3). Two sources, in this order:
//
//   - the backend's own roster where the registry declares one
//     (backendcaps.NativeTools — claude_code's twenty names, the seed
//     delegate's `--disallowedTools` projection is built from): those ARE the
//     names that route sends;
//   - otherwise every SPELLING of the shared table that folds onto a durable
//     write. A roster iterion does not declare is one it cannot name, so the
//     honest candidate set is the names the catalogue knows for the concept,
//     and a reader asking "does the gate leave a writer" takes them
//     existentially: every known spelling denied is a writer gone, one
//     surviving is a writer this reading cannot call bounded.
//
// The store's own builtin (`memory_write`) is in NEITHER: it has no spelling
// row and is on no roster, which is exactly what makes it claw's and the
// `memory:` block's — a reader asking about a block asks about that name
// directly.
func DurableWriteSpellingsOn(backend string) []string {
	writers := DurableWriteCanonicals()
	var out []string
	for _, native := range backendcaps.NativeTools(strings.TrimSpace(backend)) {
		if slices.Contains(writers, CanonicalToolName(native)) {
			out = append(out, native)
		}
	}
	if len(out) > 0 {
		return out
	}
	for spelling, canonical := range canonicalToolNames {
		if slices.Contains(writers, canonical) {
			out = append(out, spelling)
		}
	}
	slices.Sort(out)
	return out
}
