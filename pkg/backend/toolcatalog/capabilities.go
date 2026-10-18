package toolcatalog

import (
	"fmt"
	"sort"
	"strings"
)

// Capability names one behaviour a registered built-in carries — the metadata
// a rule's tool class resolves from.
//
// The registry, not the permissions package, owns the enumeration: a class
// row in a rule (`class:shell`) must cover the members that register AFTER
// the rule was written, and only the registry sees those registrations. A
// class that lived in pkg/backend/permission would go stale at exactly the
// moment it matters — the next shell-shaped built-in.
type Capability string

// CapShellExec marks a built-in that executes code the model supplies,
// through a shell or a language runtime. These are the tools a
// `deny: ["Bash"]` row is READ as bounding and is not: each one is a second
// shell beside the one the row names.
//
// The tag is read from what the tool DOES, not from its name:
//
//   - bash runs the model's command line through a shell;
//   - repl evaluates the model's code (`bash -lc` / `python -c` /
//     `node -e`), so it is a shell even where no shell binary is named;
//   - diagnostic_shell is the bash executor under a second NAME (its own
//     approval contract) — a spelling row keeps it apart from `bash`, the
//     capability puts it back in the class.
//
// Deliberately absent: workspace_grep and screenshot (no process spawn),
// computer_use (spawns fixed xdotool/import binaries — the model supplies
// coordinates and keys, not code), skill_install (fixed git invocations on a
// model-supplied URL — an installer, not a command line). The last two are
// the boundary cases a reviewer argues from: the class answers "can the model
// make this tool run code it wrote", and only the three members above
// answer yes.
const CapShellExec Capability = "shell-exec"

// builtinCapabilities is the capability metadata itself, keyed by the
// catalog's canonical names (the same keys clawBuiltins holds). One row per
// tagged built-in: a built-in that starts executing code adds its row here
// and every `class:shell` rule — deny, ask and allow — covers it, with no
// edit to the permissions package and no change to any workflow.
//
// A name here that clawBuiltins does not hold is drift, and the catalog's
// own test fails on it; the registry conformance test in pkg/backend/tool
// fails on the wider half (a name nothing registers).
var builtinCapabilities = map[string]map[Capability]bool{
	"bash":             {CapShellExec: true},
	"repl":             {CapShellExec: true},
	"diagnostic_shell": {CapShellExec: true},
}

// ruleClasses names the classes a rule may reference — `class:<name>` in an
// allow:/ask:/deny: row — and the capability whose holders are its members.
// One row per class: a new class is a name here plus the capability it reads.
var ruleClasses = map[string]Capability{
	"shell": CapShellExec,
}

// ClassShell is the class of built-ins that execute model-supplied code
// through a shell or language runtime — the class a `deny: ["Bash"]` row is
// read as bounding. Spelled out for messages and docs, which must not
// hardcode a string the class table owns.
const ClassShell = "shell"

// IsKnownClass reports whether name is a class a rule may reference.
func IsKnownClass(name string) bool {
	_, ok := ruleClasses[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// KnownClasses returns the class names a rule may reference, sorted. For the
// authoring error that refuses an unknown class — it must name what IS
// known, or the author is guessing again.
func KnownClasses() []string {
	out := make([]string, 0, len(ruleClasses))
	for name := range ruleClasses {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ClassContains reports whether name — canonicalised first, so a caller may
// pass either a raw tool name or the key it already matched with — is a
// member of the named class. An unknown class contains nothing: the grammar
// refuses one at authoring, and a rule that somehow reached evaluation with
// an empty match would deny nothing rather than everything.
func ClassContains(class, name string) bool {
	cap, ok := ruleClasses[strings.ToLower(strings.TrimSpace(class))]
	if !ok {
		return false
	}
	return builtinCapabilities[CanonicalToolName(name)][cap]
}

// ClassMembers returns the class's members as a sorted slice of canonical
// names — for diagnostics, docs and the tests that pin the tagging. An
// unknown class is an error: a caller that asked for members by a typo'd
// name must not read an empty set as "the class is empty".
func ClassMembers(class string) ([]string, error) {
	cap, ok := ruleClasses[strings.ToLower(strings.TrimSpace(class))]
	if !ok {
		return nil, fmt.Errorf("unknown tool class %q (known classes: %s)", class, strings.Join(KnownClasses(), ", "))
	}
	var out []string
	for name, caps := range builtinCapabilities {
		if caps[cap] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// HasCapability reports whether name (canonicalised) carries the capability.
func HasCapability(name string, cap Capability) bool {
	return builtinCapabilities[CanonicalToolName(name)][cap]
}

// CapabilityTaggedBuiltins returns every name carrying at least one
// capability, sorted — for the conformance test that refuses a tag on a
// name nothing registers.
func CapabilityTaggedBuiltins() []string {
	out := make([]string, 0, len(builtinCapabilities))
	for name := range builtinCapabilities {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CapabilityClass returns the class name a capability backs, and whether one
// exists. The lookup behind ClassRuleFor, the helper diagnostics read.
func CapabilityClass(cap Capability) (string, bool) {
	for class, c := range ruleClasses {
		if c == cap {
			return class, true
		}
	}
	return "", false
}

// ClassRuleFor returns the `class:<name>` rule that names cap's class — the
// spelling a diagnostic may suggest as a remedy, read from the class table
// rather than hardcoded so a renamed class cannot leave the advice stale (an
// unknown class is refused at authoring, so stale advice would point at a
// rule the parser rejects). A capability no class backs falls back to
// ClassShell: the table is a registry literal that holds the class today,
// and the caller needs a total string. The same advice is stated hardcoded
// outside this helper — diag_catalog.go's DiagPermissionClassRisk fix text
// and the docs rows (docs/references/diagnostics.md's C167 row,
// docs/permissions.md) — so a class rename stales those alongside the table.
func ClassRuleFor(cap Capability) string {
	if class, ok := CapabilityClass(cap); ok {
		return "class:" + class
	}
	return "class:" + ClassShell
}
