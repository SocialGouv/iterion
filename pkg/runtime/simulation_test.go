package runtime

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Every arm of a simulation is off on an engine nobody asked to simulate,
// and on when asked.
func TestAProductionEngineSimulatesNothing(t *testing.T) {
	if New(&ir.Workflow{}, nil, nil).Simulating() {
		t.Fatal("an engine built without WithSimulation simulates")
	}
	for name, s := range map[string]Simulation{
		"humans":   {AnswerHumans: true},
		"events":   {EventsArrive: true},
		"answers":  {AnswersArrive: true},
		"branches": {BranchesRunToTheirEnd: true},
		"invented": {Invented: &inventedStub{}},
	} {
		if !New(&ir.Workflow{}, nil, nil, WithSimulation(s)).Simulating() {
			t.Fatalf("the %s arm does not read as simulating", name)
		}
	}
	if New(&ir.Workflow{}, nil, nil, WithSimulation(Simulation{})).Simulating() {
		t.Fatal("the zero simulation reads as simulating")
	}
}

// No production launch passes WithSimulation: the option is pkg/dryrun's
// alone. The sweep reads the source of every package that builds an
// engine — a launch path that gained the option would simulate a real run.
func TestNoProductionPackagePassesWithSimulation(t *testing.T) {
	root := filepath.Join("..", "..")
	// Dependency and tool trees: no first-party source under any of them.
	skipDirs := map[string]bool{"vendor": true, "node_modules": true, "web": true}
	var launchers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			// Dot-directories belong to git, to a tool or to a run — never to
			// this repo's source, so none is named here one at a time.
			// `.iterion/worktrees/<run-id>/` matters most: it holds whole
			// COPIES of the tree, which made this verdict depend on how many
			// runs the operator happened to keep. `rel == "."` is the root
			// itself, whose Name() is "..".
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if skipDirs[d.Name()] || rel == filepath.Join("pkg", "runtime") || rel == filepath.Join("pkg", "dryrun") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(src, []byte("runtime.New(")) {
			launchers = append(launchers, rel)
		}
		if bytes.Contains(src, []byte("WithSimulation(")) {
			t.Errorf("%s passes WithSimulation: a production launch would simulate", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The sweep must have read the launchers, or it proves nothing: the
	// CLI, the runner, the dispatcher and the run view all build an engine.
	if len(launchers) < 4 {
		t.Fatalf("the sweep read %d engine launchers (%v): it did not cover the tree", len(launchers), launchers)
	}
}

// inventedStub answers the Invented seam for a test: it says every
// failure rests on an invented value, or none does, and returns the
// stand-in it was built with.
type inventedStub struct {
	ok      bool
	standIn any
	asked   []ExpressionFailure
}

func (s *inventedStub) Inconclusive(f ExpressionFailure) (any, bool) {
	s.asked = append(s.asked, f)
	return s.standIn, s.ok
}

func computeFailingWorkflow(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	ast, err := expr.Parse(src)
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	return &ir.Workflow{
		Name:  "compute_invented",
		Entry: "derive",
		Nodes: map[string]ir.Node{
			"derive": &ir.ComputeNode{
				BaseNode: ir.BaseNode{ID: "derive"},
				Exprs:    []*ir.ComputeExpr{{Key: "total", AST: ast, Raw: src}},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "derive", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// The Invented arm is read at one site, and only when a simulation is on:
// a production engine turns a compute failure into EXPRESSION_FAILED
// whatever the stand-in would have been; a simulation whose seam answers
// "the program's own values" gets the same death; a simulation whose seam
// answers "invented" sees the field take the stand-in and the run reach
// done, and the seam is handed the node, the field, the source and the
// references the expression reads.
func TestAnInventedValueSeamDecidesAComputeFailure(t *testing.T) {
	src := `sum("x") + outputs.plan.scores`
	for name, tc := range map[string]struct {
		sim    *Simulation
		stub   *inventedStub
		wantOK bool
	}{
		"production":   {sim: nil, stub: &inventedStub{ok: true, standIn: "stand-in"}},
		"the programs": {sim: &Simulation{}, stub: &inventedStub{ok: false, standIn: "stand-in"}},
		"invented":     {sim: &Simulation{}, stub: &inventedStub{ok: true, standIn: "stand-in"}, wantOK: true},
	} {
		t.Run(name, func(t *testing.T) {
			wf := computeFailingWorkflow(t, src)
			s := tmpStore(t)
			var opts []EngineOption
			if tc.sim != nil {
				tc.sim.Invented = tc.stub
				opts = append(opts, WithSimulation(*tc.sim))
			}
			err := New(wf, s, newStubExecutor(), opts...).Run(context.Background(), "run-invented", nil)
			run, lerr := s.LoadRun(context.Background(), "run-invented")
			if lerr != nil {
				t.Fatalf("load run: %v", lerr)
			}
			if !tc.wantOK {
				if err == nil || run.FailureCode != store.FailureExpressionFailed {
					t.Fatalf("the failure should be the program's: err=%v code=%q", err, run.FailureCode)
				}
				if tc.sim == nil && len(tc.stub.asked) != 0 {
					t.Fatalf("a production engine asked the seam: %+v", tc.stub.asked)
				}
				return
			}
			if err != nil {
				t.Fatalf("the run should reach done on the stand-in: %v", err)
			}
			if run.Status != store.RunStatusFinished {
				t.Fatalf("status = %q, want finished", run.Status)
			}
			if len(tc.stub.asked) != 1 {
				t.Fatalf("the seam was asked %d times, want once: %+v", len(tc.stub.asked), tc.stub.asked)
			}
			f := tc.stub.asked[0]
			if f.NodeID != "derive" || f.Field != "total" || f.Source != src || f.EdgeTo != "" || f.Err == nil {
				t.Fatalf("the seam was handed %+v", f)
			}
			if len(f.Refs) != 1 || f.Refs[0].Namespace != "outputs" || strings.Join(f.Refs[0].Path, ".") != "plan.scores" {
				t.Fatalf("the seam was handed the refs %+v", f.Refs)
			}
			if got := run.Checkpoint.Outputs["derive"]["total"]; got != "stand-in" {
				t.Fatalf("the field took %#v, want the stand-in", got)
			}
		})
	}
}

// An edge `when` that fails is skipped by a production engine; under a
// simulation whose seam answers "invented", the stand-in decides it — true
// takes the edge, false leaves it to the fallback.
func TestAnInventedValueSeamDecidesAnEdgeWhen(t *testing.T) {
	when, err := expr.Parse(`sum("x") + outputs.plan.scores > 0`)
	if err != nil {
		t.Fatal(err)
	}
	build := func() *ir.Workflow {
		return &ir.Workflow{
			Name:  "edge_invented",
			Entry: "plan",
			Nodes: map[string]ir.Node{
				"plan": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "plan"}},
				"yes":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "yes"}},
				"no":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "no"}},
				"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			},
			Edges: []*ir.Edge{
				{From: "plan", To: "yes", Expression: when, ExpressionSrc: `sum("x") + outputs.plan.scores > 0`},
				{From: "plan", To: "no", IsElse: true},
				{From: "yes", To: "done"},
				{From: "no", To: "done"},
			},
			Schemas: map[string]*ir.Schema{},
			Prompts: map[string]*ir.Prompt{},
			Vars:    map[string]*ir.Var{},
			Loops:   map[string]*ir.Loop{},
		}
	}
	for name, tc := range map[string]struct {
		stub *inventedStub
		want string
	}{
		"production":     {stub: nil, want: "no"},
		"stand-in true":  {stub: &inventedStub{ok: true, standIn: true}, want: "yes"},
		"stand-in false": {stub: &inventedStub{ok: true, standIn: false}, want: "no"},
	} {
		t.Run(name, func(t *testing.T) {
			s := tmpStore(t)
			var opts []EngineOption
			if tc.stub != nil {
				opts = append(opts, WithSimulation(Simulation{Invented: tc.stub}))
			}
			if err := New(build(), s, newStubExecutor(), opts...).Run(context.Background(), "run-edge", nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			run, err := s.LoadRun(context.Background(), "run-edge")
			if err != nil {
				t.Fatal(err)
			}
			_, tookYes := run.Checkpoint.Outputs["yes"]
			_, tookNo := run.Checkpoint.Outputs["no"]
			if (tc.want == "yes") != tookYes || (tc.want == "no") != tookNo {
				t.Fatalf("took yes=%v no=%v, want %s", tookYes, tookNo, tc.want)
			}
			if tc.stub != nil && len(tc.stub.asked) == 0 {
				t.Fatal("the seam was never asked")
			}
		})
	}
}
