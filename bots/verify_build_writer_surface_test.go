package bots

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// declaresAWritingTool reports whether a node's `tools:` list names a tool
// that writes a file, read through the shared spelling table — the one the
// claude_code delegate reads a declaration with
// (delegate.claudeNativeToolsForAllowed), so `write_file`, `apply_patch` and
// `str_replace` count here exactly as they do there.
func declaresAWritingTool(tools []string) bool {
	for _, name := range tools {
		switch toolcatalog.CanonicalToolName(name) {
		case "write", "edit", "notebookedit":
			return true
		}
	}
	return false
}

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
// `NotebookEdit` from BOTH spawns of a node whose own `tools:` list names no
// writing tool (delegate.claudeReadonlyWithheld), so that shape leaves this
// node with its deliverable unreachable by the tools its prompt names — and
// with a shell as the only way out, inside a 30-turn budget. Either statement
// is fine alone; together they are the contradiction this guard refuses.
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
			// Only claude_code withholds under the flag; codex and pi lock the
			// node down entirely, which this contract would not survive either,
			// and the `${VAR:-claude_code}` form is the same backend.
			if readonly && strings.Contains(backend, "claude_code") && !declaresAWritingTool(tools) {
				offenders = append(offenders, fmt.Sprintf(
					"%s%s (backend %q, tools %v)", sw.prefix, id, backend, tools))
			}
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d verify_build node(s) carry `readonly: true` on claude_code with no writing tool declared, so both spawns reach the CLI without Write/Edit while the node's deliverable is a written script — declare the writer, or drop the flag:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	// A FLOOR on the walk: this guard reads a node id, so a bundle rename or a
	// discovery that narrows would make it pass over nothing at all.
	if checked < 3 {
		t.Fatalf("the walk reached %d verify_build node(s), want at least 3 (app-dev, adr-cartograph, dep-update-guard): the discovery broke, and this guard is now green over an unexamined class", checked)
	}
}
