package bots

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// readonlyLLMSurface is what this guard needs of an agent or judge: the flag,
// the backend it names, and its declaration. Any other node kind reports
// ok=false.
func readonlyLLMSurface(node ir.Node) (readonly bool, backend string, tools []string, ok bool) {
	switch n := node.(type) {
	case *ir.AgentNode:
		return n.Readonly, n.Backend, n.Tools, true
	case *ir.JudgeNode:
		return n.Readonly, n.Backend, n.Tools, true
	}
	return false, "", nil, false
}

// TestEveryVerifyBuildNodeKeepsTheWritingToolsItsContractNeeds is the class
// contract of the `verify_build` node every build-gate bundle ships: it
// WRITES the out-of-tree verify.sh that a deterministic downstream tool then
// executes, and that tool refuses a missing script
// (bots/dep-update-guard/main.bot `verify_run`: "NO SCRIPT: verify_build
// wrote no …").
//
// `readonly: true` on `claude_code` withholds `Write`/`Edit`/`MultiEdit`/
// `NotebookEdit` from BOTH spawns, whatever the node's own `tools:` list says
// (delegate.claudeReadonlyWithheld), so the flag leaves this node with its
// deliverable unreachable by the tools its prompt names — and with a shell as
// the only way out, inside a 30-turn budget. Declaring the writer buys
// nothing back: the flag beats the list, here as on codex and pi. Either
// statement is fine alone; together they are the contradiction this guard
// refuses, and dropping `readonly:` is the way out (its non-mutating
// admission goes with it, which is honest for a node that writes).
//
// Keyed on the node id, because that id IS the contract's name in each bundle
// that ships it: a fourth bundle copying the pattern is checked by this walk
// the day it lands.
func TestEveryVerifyBuildNodeKeepsTheWritingToolsItsContractNeeds(t *testing.T) {
	var offenders []string
	checked := 0
	for _, sw := range shippedWorkflows(t) {
		for id, node := range sw.wf.Nodes {
			if id != "verify_build" {
				continue
			}
			readonly, backend, tools, ok := readonlyLLMSurface(node)
			if !ok {
				continue
			}
			checked++
			if backend == "" {
				backend = sw.wf.DefaultBackend
			}
			// Only claude_code withholds under the flag; codex runs the node in a
			// read-only OS sandbox and pi pins its tool list to four readers,
			// neither of which this contract would survive either, and the
			// `${VAR:-claude_code}` form is the same backend.
			if readonly && strings.Contains(backend, "claude_code") {
				offenders = append(offenders, fmt.Sprintf(
					"%s%s (backend %q, tools %v)", sw.prefix, id, backend, tools))
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d verify_build node(s) carry `readonly: true` on claude_code, so both spawns reach the CLI without Write/Edit — whatever the tools: list says — while the node's deliverable is a written script; drop the flag:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the walk, just under the class: this guard reads a node id,
	// so a rename of the contract in some of the bundles — or a discovery
	// that narrows — would leave it green over the ones it no longer reaches.
	// 10 bundles ship the node today, every one of them on `claude_code`, so
	// 8 lets two be retired without touching this line while a rename across
	// the rest cannot pass. The shared walk's own floor (shippedWorkflows)
	// answers for a wholesale loss of bundles; it cannot see this id.
	if checked < 8 {
		t.Fatalf("the walk reached %d verify_build node(s), want at least 8 of the 10 bundles that ship the contract: the id was renamed or the discovery narrowed, and this guard is now green over an unexamined class", checked)
	}
}
