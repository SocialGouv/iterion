package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// outputsRef matches an {{outputs.<node>.<field>}} reference.
var outputsRef = regexp.MustCompile(`\{\{\s*!?\s*outputs\.[A-Za-z0-9_.]+\s*\}\}`)

// TestCatalogToolCommandsResolveTheirRefs pins a seam that fails SILENTLY.
//
// A tool node's `command:` is resolved by resolveCommandTemplate
// (pkg/backend/model/executor_tool.go), which substitutes {{input.X}} and
// {{vars.X}} — and nothing else. An {{outputs.<node>.<field>}} ref belongs on
// an EDGE mapping, where it does resolve; written inside a command body it
// survives as literal text, and the shell happily passes that text on.
//
// Nothing catches it: the bot parses, compiles, validates and runs. The
// failure only appears in production, as a value that is a template string
// pretending to be data.
//
// It has already shipped once. bots/review-pr's stale-anchor guard compared
// REVIEWED_SHA={{outputs.diff_precheck.reviewed_sha}} against the real HEAD;
// the literal never equals a sha, so every review skipped publishing — and
// since that publish carries the merge-gate status, and revi/review is a
// required check, every PR on the repo became unmergeable with no error
// anywhere. Found by reading a run log, not by any test.
func TestCatalogToolCommandsResolveTheirRefs(t *testing.T) {
	teamBots, _ := filepath.Glob("*/main.bot")
	demoMain, _ := filepath.Glob("../examples/*/main.bot")
	demoLoose, _ := filepath.Glob("../examples/*.bot")
	targets := append(append(teamBots, demoMain...), demoLoose...)
	if len(targets) == 0 {
		t.Fatal("no catalog workflows found — discovery glob likely broke")
	}

	checked := 0
	for _, path := range targets {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}
		pr := parser.Parse(path, string(src))
		if pr.File == nil {
			continue // the parse/compile test owns that failure
		}
		cr := ir.Compile(pr.File)
		if cr.Workflow == nil {
			continue
		}
		for _, n := range cr.Workflow.Nodes {
			tool, ok := n.(*ir.ToolNode)
			if !ok {
				continue
			}
			for _, body := range []string{tool.Command, tool.Script} {
				if body == "" {
					continue
				}
				checked++
				if m := outputsRef.FindString(body); m != "" {
					t.Errorf("%s: node %q passes %s inside its command/script body; "+
						"only {{input.X}} and {{vars.X}} are substituted there, so this "+
						"reaches the shell as literal text. Map it on the edge into this "+
						"node and read it as {{input.<field>}}.", path, tool.ID, m)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no tool bodies inspected — the IR shape likely changed")
	}
}
