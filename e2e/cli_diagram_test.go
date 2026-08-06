package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// `iterion diagram` is how an operator reads a `.bot`'s control flow
// without executing it. Its observable contract is the Mermaid text: a
// declaration for EVERY node the workflow compiled (including the
// implicit terminals) and an arrow for EVERY edge, carrying the
// condition / loop / mapping annotations that make the graph
// trustworthy. The renderer is unit-covered in pkg/dsl/ir; the command
// was only covered on its file-not-found path — so nothing asserted
// that a real `.bot` reaches the operator's screen at all.
//
// Mutation check: drop the edge loop from the renderer and the arrow
// assertions fail; ignore --view and the compact/detailed/full
// distinctions collapse; stop compiling and every assertion fails;
// silently coerce a typo'd --view and the refusal assertion fails.

const diagramFixture = "testdata/diagram_mini.bot"

// diagramOut runs the command and returns everything it printed.
func diagramOut(t *testing.T, opts cli.DiagramOptions, format cli.OutputFormat) string {
	t.Helper()
	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: format}
	if err := cli.RunDiagram(opts, p); err != nil {
		t.Fatalf("diagram %+v: %v", opts, err)
	}
	return buf.String()
}

func TestDiagramRendersEveryNodeAndEdgeOfTheWorkflow(t *testing.T) {
	out := diagramOut(t, cli.DiagramOptions{File: diagramFixture}, cli.OutputHuman)

	if !strings.Contains(out, "flowchart TD") {
		t.Fatalf("output is not a Mermaid flowchart:\n%s", out)
	}

	// Every declared node, plus the terminals the compiler adds.
	for _, id := range []string{"survey", "split", "build", "assess", "sign_off", "done", "fail"} {
		if !strings.Contains(out, "    "+id) {
			t.Errorf("node %q has no declaration in the diagram:\n%s", id, out)
		}
	}

	// Every edge, in the direction the workflow declares it. These are
	// the wires an operator reads the graph for.
	for _, arrow := range []string{
		"survey --> split",
		"split --> build",
		"split --> assess",
		"build --> assess",
		"sign_off --> done",
	} {
		if !strings.Contains(out, arrow) {
			t.Errorf("edge %q missing from the diagram:\n%s", arrow, out)
		}
	}

	// The conditional + bounded-loop back-edge carries its annotation:
	// an unlabelled arrow here would misrepresent the graph as an
	// unconditional infinite loop.
	if !strings.Contains(out, `assess -->|"NOT ok / loop:retry(3)"| survey`) {
		t.Errorf("the negated + bounded-loop back-edge lost its label:\n%s", out)
	}
	if !strings.Contains(out, `assess -->|"ok"| sign_off`) {
		t.Errorf("the positive conditional edge lost its label:\n%s", out)
	}

	// Node kinds are visually distinguished — the router is a rhombus,
	// the human an asymmetric flag, the terminals stadiums, and the
	// convergence judge a doubled rectangle.
	for _, shape := range []string{
		`split{"`,    // router
		`sign_off>"`, // human
		`done(["`,    // terminal
		`assess[["`,  // await: wait_all convergence
	} {
		if !strings.Contains(out, shape) {
			t.Errorf("shape %q missing — node kinds are not distinguished:\n%s", shape, out)
		}
	}
}

func TestDiagramViewsDifferAndUnknownViewIsRefused(t *testing.T) {
	compact := diagramOut(t, cli.DiagramOptions{File: diagramFixture, View: "compact"}, cli.OutputHuman)
	detailed := diagramOut(t, cli.DiagramOptions{File: diagramFixture, View: "detailed"}, cli.OutputHuman)
	full := diagramOut(t, cli.DiagramOptions{File: diagramFixture, View: "full"}, cli.OutputHuman)

	// detailed carries per-node metadata compact deliberately omits.
	if !strings.Contains(detailed, "claude-opus-4-7") {
		t.Errorf("--view detailed omitted the node model:\n%s", detailed)
	}
	if strings.Contains(compact, "claude-opus-4-7") {
		t.Errorf("--view compact leaked detailed metadata:\n%s", compact)
	}
	// and the edge data mapping, which compact also omits.
	if !strings.Contains(detailed, "with: note=") {
		t.Errorf("--view detailed omitted the edge data mapping:\n%s", detailed)
	}
	if strings.Contains(compact, "with: note=") {
		t.Errorf("--view compact leaked the edge data mapping:\n%s", compact)
	}

	// full adds the workflow-metadata subgraph (vars + budget) that
	// neither of the other two views renders.
	if !strings.Contains(full, "Variables") || !strings.Contains(full, "attempts") {
		t.Errorf("--view full omitted the vars metadata:\n%s", full)
	}
	if !strings.Contains(full, "Budget") {
		t.Errorf("--view full omitted the budget metadata:\n%s", full)
	}
	if strings.Contains(detailed, "<b>Variables</b>") {
		t.Errorf("--view detailed leaked the full-view metadata subgraph:\n%s", detailed)
	}

	// A typo must be refused, not silently coerced to compact — that
	// coercion is exactly how an operator reads the wrong graph.
	var buf bytes.Buffer
	p := &cli.Printer{W: &buf, Format: cli.OutputHuman}
	err := cli.RunDiagram(cli.DiagramOptions{File: diagramFixture, View: "detaild"}, p)
	if err == nil {
		t.Fatalf("--view detaild was accepted; output:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "detaild") {
		t.Errorf("error = %v, want it to quote the rejected view", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused view still printed a diagram:\n%s", buf.String())
	}
}

func TestDiagramJSONCarriesTheRenderedGraph(t *testing.T) {
	out := diagramOut(t, cli.DiagramOptions{File: diagramFixture, View: "detailed"}, cli.OutputJSON)

	var got cli.DiagramResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode --json diagram from %q: %v", out, err)
	}
	if got.WorkflowName != "diagram_mini" {
		t.Errorf("workflow_name = %q, want diagram_mini", got.WorkflowName)
	}
	if got.View != "detailed" {
		t.Errorf("view = %q, want detailed", got.View)
	}
	if got.File != diagramFixture {
		t.Errorf("file = %q, want %q", got.File, diagramFixture)
	}
	if !strings.Contains(got.Mermaid, "flowchart TD") || !strings.Contains(got.Mermaid, "survey --> split") {
		t.Errorf("mermaid field does not carry the rendered graph:\n%s", got.Mermaid)
	}
}
