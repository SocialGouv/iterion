package delegate

import (
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
)

// ClaudeToolWithholding is one tool a claude_code spawn removes, with the
// reason an author is owed. Always marks the withholdings no `tools:` list
// can lift (the headless surface, and `readonly:`'s); a declaration-driven
// withholding names the capability that would keep the tool. OnRoster marks
// membership of the pinned CLI's native roster (backendcaps.NativeTools) —
// a withheld name off the roster is defensive: the CLI this build pins does
// not register it, and the deny rule costs nothing.
type ClaudeToolWithholding struct {
	Name     string `json:"name"`
	Reason   string `json:"reason"`
	Always   bool   `json:"always,omitempty"`
	OnRoster bool   `json:"on_roster,omitempty"`
}

// ClaudeToolSurface is the claude_code native tool surface as ONE node meets
// it: the roster names the spawn keeps (Granted) and the ones it removes,
// with the reason per name (Withheld). It exists for the capability-drift
// disclosure (iterion#2815): on claude_code a `tools:` list becomes
// --disallowedTools over a roster iterion does not own, and what the list
// really grants — and what leaves the node whatever it says — was written
// down nowhere an author reads.
//
// The whole answer is READ OFF the projection the spawn's argv is built
// from — backendcaps.NativeTools for the roster, claudeNativeForCanonical
// for what a declaration grants, headlessWithheldTools and
// claudeReadonlyWithheld for what leaves anyway — never from a second list:
// a copy is how #1579 and #1651 both happened. A name added to the roster
// lands in Granted until a rule withholds it, and the witness
// (TestTheCapabilityDriftDisclosureCoversTheRoster) reddens when the
// disclosure no longer accounts for every roster name.
//
// It is the STATIC reading: the operator-local knobs
// (ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS, the background-tasks
// switch) are not reflected, and MCP tools survive any list, being on no
// roster.
func ClaudeCodeToolSurface(task Task) ClaudeToolSurface {
	roster := backendcaps.NativeTools(backendcaps.ClaudeCode)
	s := ClaudeToolSurface{Declared: toolBoundaryApplies(task)}
	byName := map[string]*ClaudeToolWithholding{}
	var order []string
	withhold := func(w ClaudeToolWithholding) {
		if _, dup := byName[w.Name]; dup {
			return
		}
		byName[w.Name] = &w
		order = append(order, w.Name)
	}
	for _, name := range headlessWithheldTools {
		withhold(ClaudeToolWithholding{
			Name:     name,
			Reason:   headlessWithheldReasons[name],
			Always:   true,
			OnRoster: slices.Contains(roster, name),
		})
	}
	if task.Readonly {
		for _, name := range claudeReadonlyWithheld() {
			withhold(ClaudeToolWithholding{
				Name:     name,
				Reason:   "readonly: true removes it whatever the tools: list says — the flag is the node's access intent (and what admits it into a shared workspace as non-mutating); dropping readonly: is the only way to keep it",
				Always:   true,
				OnRoster: slices.Contains(roster, name),
			})
		}
	}
	if toolBoundaryApplies(task) {
		granted := claudeNativeToolsForAllowed(task.AllowedTools, task.DiagnosticShell)
		for _, name := range roster {
			if granted[name] {
				continue
			}
			if _, already := byName[name]; already {
				continue
			}
			withhold(ClaudeToolWithholding{
				Name:     name,
				Reason:   declarationWithheldReason(name),
				OnRoster: true,
			})
		}
	}
	for _, name := range roster {
		if _, gone := byName[name]; !gone {
			s.Granted = append(s.Granted, name)
		}
	}
	for _, name := range order {
		s.Withheld = append(s.Withheld, *byName[name])
	}
	return s
}

// ClaudeToolSurface is the per-node reading `iterion validate --explain` and
// the Studio launch preview print.
type ClaudeToolSurface struct {
	// Declared reports whether the node's tool list is a visibility boundary
	// at all (toolBoundaryApplies): an UNDECLARED surface keeps the ambient
	// roster minus the unconditional withholdings.
	Declared bool `json:"declared"`
	// Granted are the native roster names the spawn keeps, roster order.
	Granted []string `json:"granted,omitempty"`
	// Withheld are the names the spawn removes, the reason per name: the
	// unconditional ones first (headless surface, then readonly's), then the
	// declaration's, roster order within each group.
	Withheld []ClaudeToolWithholding `json:"withheld,omitempty"`
}

// declarationWithheldReason is the reason a roster name leaves a node whose
// tools: list does not grant it, naming the declaration that would keep it —
// read off the SAME projection (claudeNativeForCanonical) by reversing it,
// so the offer cannot name a capability that grants nothing.
func declarationWithheldReason(name string) string {
	var canonicals []string
	for canonical, granted := range claudeNativeForCanonical {
		if slices.Contains(granted, name) {
			canonicals = append(canonicals, canonical)
		}
	}
	if len(canonicals) == 0 {
		return "the tools: list does not grant it, and no declaration can — it leaves only with the roster"
	}
	slices.Sort(canonicals)
	quoted := make([]string, len(canonicals))
	for i, c := range canonicals {
		quoted[i] = "`" + c + "`"
	}
	return "the tools: list does not grant it — declaring " + strings.Join(quoted, " or ") + " keeps it"
}
