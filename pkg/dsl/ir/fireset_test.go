package ir_test

// The C167 fire set over the shipped corpus, as a CI fact. The docs say
// "measured 0 fires"; a number no test guards is a snapshot, and the walk
// that measured it compiled each file ALONE, which C030-aborts on the bots
// keeping their nodes in lib/ fragments — their nodes were invisible. This
// walk compiles each file AS A UNIT (unit.LoadDir merges the fragments,
// what `iterion validate` does), so the count covers every node, and it
// fails when a shipped workflow starts firing C167: the author either
// bounds the shell tools the cell names, or accepts the fire and states the
// new count here.
//
// C351 and C352 read the same walk: no shipped workflow declares a node's own
// permission rules on a claw route that serves the node no tool, and the
// rung-4 recovery agents the source routes to claw are counted with what
// each holds.

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// corpusCompile is one corpus file that parses, compiled as a unit.
type corpusCompile struct {
	path string
	res  *ir.CompileResult
}

// compileShippedCorpus compiles every corpus file that parses — bots/,
// examples/ and e2e/testdata — the way `iterion validate` reads it.
func compileShippedCorpus(t *testing.T) []corpusCompile {
	t.Helper()
	var out []corpusCompile
	for _, path := range dsltest.CorpusFiles(t, filepath.Join("..", "..", "..")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parsertest.Parse(path, string(src))
		// A deliberately broken fixture is someone else's diagnostic; the
		// fire set is a claim about workflows that compile.
		broken := pr.File == nil
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				broken = true
			}
		}
		if broken {
			continue
		}
		res := ir.Compile(pr.File)
		// Import-units compile AS A UNIT, or their fragment nodes are
		// invisible to every pass (C030 aborts the compile).
		unresolved := false
		for _, d := range res.Diagnostics {
			if d.Code == ir.DiagUnresolvedImports {
				unresolved = true
			}
		}
		if unresolved {
			u := unittest.Whole(unit.LoadDir(path))
			if u.Merged != nil {
				res = ir.Compile(u.Merged)
			}
		}
		out = append(out, corpusCompile{path: path, res: res})
	}
	return out
}

func TestShippedCorpusStaysSilentOnClassCoverage(t *testing.T) {
	corpus := compileShippedCorpus(t)
	fires := 0
	for _, c := range corpus {
		for _, d := range c.res.Diagnostics {
			if d.Code == ir.DiagPermissionClassRisk {
				fires++
				t.Errorf("%s: C167 fired on %q: %s", c.path, d.NodeID, strings.ToValidUTF8(d.Message, "?"))
			}
		}
	}
	if fires != 0 {
		t.Fatalf("%d C167 fires over %d corpus files — update the docs' measured count or bound the shells", fires, len(corpus))
	}
	t.Logf("%d corpus files walked, 0 C167 fires", len(corpus))
}

// TestShippedCorpusStaysSilentOnRulesWithoutTools: no shipped workflow keeps
// a node's own allow/ask/deny lists on a claw route that serves the node no
// tool (C351). A fire here is a bot whose rules bound nothing: give the node
// its `tools:`, or drop the lists.
//
// The walk has to have something to be silent ABOUT, so it counts what it
// read: the agent and judge nodes, and those the source routes to claw. A
// corpus that stopped compiling, or a walk that stopped reaching the nodes,
// fails here instead of reporting a silence nothing was asked for.
func TestShippedCorpusStaysSilentOnRulesWithoutTools(t *testing.T) {
	corpus := compileShippedCorpus(t)
	fires, llmNodes, clawNodes, ownRules := 0, 0, 0, 0
	for _, c := range corpus {
		for _, d := range c.res.Diagnostics {
			if d.Code == ir.DiagPermissionRulesNoTools {
				fires++
				t.Errorf("%s: C351 fired on %q: %s", c.path, d.NodeID, strings.ToValidUTF8(d.Message, "?"))
			}
		}
		w := c.res.Workflow
		if w == nil {
			continue
		}
		for _, n := range w.Nodes {
			nn, ok := n.(ir.LLMNode)
			if !ok {
				continue
			}
			llmNodes++
			if ir.SourceNodeBackendName(nn.GetLLMFields().Backend, w.DefaultBackend) == "claw" {
				clawNodes++
			}
			if len(nn.GetPermissionAllow())+len(nn.GetPermissionAsk())+len(nn.GetPermissionDeny()) > 0 {
				ownRules++
			}
		}
	}
	if llmNodes == 0 || clawNodes == 0 {
		t.Fatalf("the walk read %d agent/judge nodes, %d of them routed to claw, over %d corpus files — it cannot vouch for a silence", llmNodes, clawNodes, len(corpus))
	}
	if fires != 0 {
		t.Fatalf("%d C351 fires over %d corpus files", fires, len(corpus))
	}
	t.Logf("%d corpus files walked: %d agent/judge nodes, %d routed to claw by the source, %d declaring rules of their own, 0 C351 fires", len(corpus), llmNodes, clawNodes, ownRules)
}

// TestShippedCorpusRecoveryAgentsHoldATool is the C352 fire set over the
// shipped corpus: every tool node whose Verified Action reaches rung 4, with
// the route the source gives its agent and whether that agent declares a
// tool. A fire is a recovery agent dispatched on claw with nothing to call —
// give the node its `recovery.agent_tools:`, never an exemption here.
//
// The walk counts what it read, so a silence is a claim about rungs that
// exist: the undecided ones — no `default_backend:`, which is the only field
// that names a recovery agent's route — are the dispatch's to judge and are
// reported as their own number.
func TestShippedCorpusRecoveryAgentsHoldATool(t *testing.T) {
	corpus := compileShippedCorpus(t)
	fires, rungs, declaring, onClaw, undecided := 0, 0, 0, 0, 0
	for _, c := range corpus {
		for _, d := range c.res.Diagnostics {
			if d.Code == ir.DiagRecoveryAgentNoTools {
				fires++
				t.Errorf("%s: C352 fired on %q: %s", c.path, d.NodeID, strings.ToValidUTF8(d.Message, "?"))
			}
		}
		w := c.res.Workflow
		if w == nil {
			continue
		}
		for _, n := range w.Nodes {
			if !ir.ToolRecoveryAgentRung(n) {
				continue
			}
			rungs++
			if len(n.(*ir.ToolNode).Recovery.AgentTools) > 0 {
				declaring++
			}
			switch ir.SourceNodeBackendName("", w.DefaultBackend) {
			case "claw":
				onClaw++
			case "":
				undecided++
			}
		}
	}
	if rungs == 0 {
		t.Fatalf("the walk read no rung-4 recovery agent over %d corpus files — it cannot vouch for a silence", len(corpus))
	}
	if fires != 0 {
		t.Fatalf("%d C352 fires over %d corpus files", fires, len(corpus))
	}
	t.Logf("%d corpus files walked: %d rung-4 recovery agents, %d declaring agent_tools, %d routed to claw by the source, %d on a route the source does not decide, 0 C352 fires",
		len(corpus), rungs, declaring, onClaw, undecided)
}
