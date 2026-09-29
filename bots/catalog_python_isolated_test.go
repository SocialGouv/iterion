package bots

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// pythonCall matches a python invocation in a shell body whose mode puts the
// working directory first on sys.path (`-c`, `-m`, or `-` for stdin) and
// captures the flags written before the mode.
var pythonCall = regexp.MustCompile("(?:^|[\\s;&|(`\"'=])(python3?(?:\\.[0-9]+)?)((?:\\s+-[A-Za-z]+)*?)\\s+(-c|-m|-)(?:\\s|$)")

// TestCatalogPythonRunsIsolated: every python invocation a shell body of the
// catalog makes runs isolated (-I).
//
// A tool node runs with the workspace as its working directory. With -c, -m
// or stdin, python puts that directory first on sys.path, so a json.py,
// hashlib.py or subprocess.py at the root of the judged tree, git-ignored or
// not, replaces the standard module inside the node: a gate whose verdict
// the tree under judgement writes. -I drops that entry. A `script:` body in
// python is the engine's to isolate (scriptInterpreter); this guard reads
// the shell bodies, where the author writes the invocation.
func TestCatalogPythonRunsIsolated(t *testing.T) {
	found := 0
	for _, path := range catalogWorkflowFiles() {
		pr := parseBotUnit(path)
		if pr.File == nil {
			t.Logf("%s: not inspected (unparseable — the parse/compile test owns that)", path)
			continue
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			t.Logf("%s: not inspected (does not compile to a workflow)", path)
			continue
		}
		for _, n := range cr.Workflow.Nodes {
			tool, ok := n.(*ir.ToolNode)
			if !ok {
				continue
			}
			bodies := []string{tool.Command, tool.Postcondition}
			switch tool.Language {
			case "", "sh", "bash":
				bodies = append(bodies, tool.Script)
			}
			for _, body := range bodies {
				for _, m := range pythonCall.FindAllStringSubmatch(body, -1) {
					found++
					if !strings.Contains(m[2], "I") {
						t.Errorf("%s: node %q runs %q without -I: the workspace it runs in comes first on sys.path",
							path, tool.ID, strings.TrimSpace(m[0]))
					}
				}
			}
		}
	}
	if found < 100 {
		t.Fatalf("found %d python invocations in the catalog's shell bodies, want the catalog's ~150 — the scan no longer reads them", found)
	}
}
