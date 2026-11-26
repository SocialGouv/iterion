package e2ematrix

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
)

// The contract slice runs read-only git in the checkout. The operator's
// global and system git config must not reach it: an alias, a pager or an
// fsmonitor configured there changes what git does and answers.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(m.Run())
}

// parseBotUnit parses a catalog bot as its UNIT — the file and the fragments
// its imports reach — and returns what parser.Parse returns for a single file.
func parseBotUnit(path string) *parser.ParseResult {
	u := unittest.Whole(unit.LoadDir(path))
	return &parser.ParseResult{File: u.Merged, Diagnostics: u.Diagnostics}
}

// toolCommand compiles the bot at path and returns the command of its tool
// node named node.
func toolCommand(t *testing.T, path, node string) string {
	t.Helper()
	pr := parseBotUnit(path)
	if pr.File == nil {
		t.Fatalf("parse of %s produced no File", path)
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatalf("compile of %s produced no Workflow", path)
	}
	raw, ok := cr.Workflow.Nodes[node]
	if !ok {
		t.Fatalf("no %s node in %s", node, path)
	}
	tn, ok := raw.(*ir.ToolNode)
	if !ok {
		t.Fatalf("%s is %T, want *ir.ToolNode", node, raw)
	}
	return tn.Command
}
