// Package secretsink names the materialization sinks a declared secret may be
// granted to — the `sinks:` vocabulary of a `.bot` secret declaration
// (docs/adr/146-secret-boundary-v2-grants-and-sinks.md).
//
// It is a leaf on purpose: the DSL compiler refuses an ungranted reference
// with these names, the secret guard honours the grants the compiler cannot
// see (a placeholder a model emits, a request on the wire), and the runtime
// refuses a launch whose sandbox cannot serve a grant — three readers of one
// vocabulary, none of which may import the others.
package secretsink

import (
	"regexp"
	"strings"
)

const (
	// Prompt: a prompt body renders the reference as the placeholder, so the
	// model holds the placeholder from then on.
	Prompt = "prompt"
	// Agent: the placeholder a model emits in its own tool input is
	// materialised before the tool runs (claude_code's PreToolUse hook,
	// claw's in-process tool loop, a self-repair command a model proposed).
	Agent = "agent"
	// Command: a tool node's authored `command:`, `postcondition:` or
	// `script:` is materialised before exec.
	Command = "command"
	// Params: a tool action's `params:` value is materialised before the
	// vendor call.
	Params = "params"
	// File: the secret is written as a file the run mounts (`as: file`).
	File = "file"
	// Egress: the placeholder is substituted on the wire toward the
	// secret's `hosts:` by the sandbox's inspecting proxy (Layer 2).
	Egress = "egress"
)

// All lists every sink, in the order the documentation and the diagnostics
// spell them.
var All = []string{Prompt, Agent, Command, Params, File, Egress}

// Known reports whether name is one of the sinks.
func Known(name string) bool {
	for _, s := range All {
		if s == name {
			return true
		}
	}
	return false
}

// Grants reports whether a declaration granting `grants` reaches sink. A
// declaration that names no sink grants every sink: the default is ALL, and
// the restriction is opt-in (ADR-146, axis 2).
func Grants(grants []string, sink string) bool {
	if len(grants) == 0 {
		return true
	}
	for _, g := range grants {
		if g == sink {
			return true
		}
	}
	return false
}

// List spells the vocabulary for a diagnostic: "prompt, agent, …".
func List() string {
	return strings.Join(All, ", ")
}

// Materialising reports whether sink is one where a placeholder is swapped
// for its value by the guard — agent, command, params. `prompt` hands a
// model the placeholder, `file` mounts the value, `egress` substitutes it on
// the wire: none of them is a Materialize call.
func Materialising(sink string) bool {
	switch sink {
	case Agent, Command, Params:
		return true
	}
	return false
}

var placeholderSanitize = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// PlaceholderFor derives a secret's placeholder from its name — the
// deterministic, low-entropy, reversible token `__ITERION_SECRET_<name>__`
// the agent sees in place of the value. One derivation for the guard, which
// registers it, and the compiler, which refuses two declarations that would
// share one.
func PlaceholderFor(name string) string {
	clean := placeholderSanitize.ReplaceAllString(name, "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "value"
	}
	return "__ITERION_SECRET_" + clean + "__"
}
