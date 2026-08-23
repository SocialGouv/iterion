package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

// compileBot is the shared compile path the C034 tests in pkg/dsl/ir
// also use, so this file can prove the validator and the resolver agree
// on the same edge mapping.
func compileBot(t *testing.T, src string) *ir.CompileResult {
	t.Helper()
	res := parser.Parse("test.bot", src)
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	return ir.Compile(res.File)
}

func c034Messages(r *ir.CompileResult) []string {
	var out []string
	for _, d := range r.Diagnostics {
		if d.Code == ir.DiagInputFieldNotInSchema {
			out = append(out, d.Message)
		}
	}
	return out
}

func hasC034Field(msgs []string, field string) bool {
	needle := `field "` + field + `"`
	for _, m := range msgs {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// Three-field mapping on one edge. The source input schema, source
// output schema, and run-level vars are disjoint, so each {{input.x}}
// names exactly one of those namespaces.
const edgeInputRefBot = `
schema src_in:
  only_in: string

schema src_out:
  produced: string

schema dst_in:
  from_out: string
  from_in: string
  from_run: string

prompt sys:
  System.

prompt usr:
  User.

agent seed:
  model: "m"
  output: src_in
  system: sys
  user: usr

agent src:
  model: "m"
  input: src_in
  output: src_out
  system: sys
  user: usr

agent dst:
  model: "m"
  input: dst_in
  output: src_out
  system: sys
  user: usr

vars:
  reviewer: string

workflow test:
  entry: seed
  worktree: none
  sandbox: none
  seed -> src with { only_in: "{{outputs.seed.only_in}}" }
  src -> dst with {
    from_out: "{{input.produced}}",
    from_in: "{{input.only_in}}",
    from_run: "{{input.reviewer}}"
  }
  dst -> done
`

// TestEdgeInputRef_CompileAndRuntimeAgree is the #479 contract: the
// same with-mapping is compiled and executed, and C034 names the
// namespace the resolver actually reads.
//
//	produced  — only in the source output → compiles, resolves
//	only_in   — only in the source input  → C034, resolves to nil
//	reviewer  — only in run inputs / vars → C034, resolves to nil
func TestEdgeInputRef_CompileAndRuntimeAgree(t *testing.T) {
	cr := compileBot(t, edgeInputRefBot)
	if cr.Workflow == nil {
		t.Fatalf("compile returned nil workflow: %v", cr.Diagnostics)
	}
	msgs := c034Messages(cr)
	if hasC034Field(msgs, "produced") {
		t.Errorf("C034 on source-output field produced: %v", msgs)
	}
	if !hasC034Field(msgs, "only_in") {
		t.Errorf("missing C034 for source-input-only field only_in: %v", msgs)
	}
	if !hasC034Field(msgs, "reviewer") {
		t.Errorf("missing C034 for run-input-only field reviewer: %v", msgs)
	}

	var got map[string]any
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{"only_in": "from-seed"}, nil
	})
	exec.on("src", func(in map[string]any) (map[string]any, error) {
		if in["only_in"] != "from-seed" {
			t.Errorf("src input only_in = %#v, want from-seed", in["only_in"])
		}
		return map[string]any{"produced": "from-src"}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		got = in
		return map[string]any{"produced": "ok"}, nil
	})

	s := tmpStore(t)
	eng := New(cr.Workflow, s, exec)
	if err := eng.Run(context.Background(), "run-edge-input", map[string]any{"reviewer": "on"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-edge-input")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	if got == nil {
		t.Fatal("dst was not executed")
	}
	if got["from_out"] != "from-src" {
		t.Errorf("from_out (source output) = %#v, want from-src", got["from_out"])
	}
	if got["from_in"] != nil {
		t.Errorf("from_in (source input only) = %#v, want nil — compile rejected it and runtime must not resolve it from the source input", got["from_in"])
	}
	if got["from_run"] != nil {
		t.Errorf("from_run (run inputs only) = %#v, want nil — compile rejected it and runtime must not fall back to run inputs; use {{vars.reviewer}}", got["from_run"])
	}
}

const routerPassThroughBot = `
schema payload:
  data: string

prompt sys:
  System.

prompt usr:
  User.

router distribute:
  mode: fan_out_all

agent analyzer_a:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr
  readonly: true

agent analyzer_b:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr
  readonly: true

agent join:
  model: "m"
  output: payload
  system: sys
  user: usr
  await: wait_all

workflow test:
  entry: distribute
  worktree: none
  sandbox: none
  budget:
    max_parallel_branches: 2
  distribute -> analyzer_a with { data: "{{input.data}}" }
  distribute -> analyzer_b with { data: "{{input.data}}" }
  analyzer_a -> join
  analyzer_b -> join
  join -> done
`

// TestEdgeInputRef_RouterPassThrough preserves the documented pattern:
// an entry fan_out_all copies run-level inputs onto its output, so
// {{input.data}} on an outgoing with-mapping forwards that payload.
// This is source-output resolution, not a run-input fallback.
func TestEdgeInputRef_RouterPassThrough(t *testing.T) {
	cr := compileBot(t, routerPassThroughBot)
	if cr.Workflow == nil {
		t.Fatalf("compile returned nil workflow: %v", cr.Diagnostics)
	}
	if msgs := c034Messages(cr); len(msgs) > 0 {
		t.Fatalf("router pass-through must not trip C034: %v", msgs)
	}

	got := map[string]any{}
	exec := newStubExecutor()
	exec.on("analyzer_a", func(in map[string]any) (map[string]any, error) {
		got["a"] = in["data"]
		return map[string]any{"data": in["data"]}, nil
	})
	exec.on("analyzer_b", func(in map[string]any) (map[string]any, error) {
		got["b"] = in["data"]
		return map[string]any{"data": in["data"]}, nil
	})
	exec.on("join", func(map[string]any) (map[string]any, error) {
		return map[string]any{"data": "joined"}, nil
	})

	s := tmpStore(t)
	eng := New(cr.Workflow, s, exec)
	if err := eng.Run(context.Background(), "run-passthrough", map[string]any{"data": "payload"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-passthrough")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	if got["a"] != "payload" || got["b"] != "payload" {
		t.Errorf("router pass-through: analyzer inputs a=%#v b=%#v, want payload", got["a"], got["b"])
	}
}

const llmRouterPassThroughBot = `
schema payload:
  topic: string

prompt sys:
  System.

prompt usr:
  User.

router pick:
  mode: llm
  model: "m"
  system: sys
  user: usr

agent left:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr

agent right:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr

workflow test:
  entry: pick
  worktree: none
  sandbox: none
  pick -> left with { topic: "{{input.topic}}" }
  pick -> right with { topic: "{{input.topic}}" }
  left -> done
  right -> done
`

// TestEdgeInputRef_LLMRouterPassThrough is R3bc3ad: an llm router used
// to store only {selected_route, reasoning}, so {{input.topic}} on an
// outgoing with-mapping was nil. It now overlays the selection onto
// the payload it received, like every other router mode.
func TestEdgeInputRef_LLMRouterPassThrough(t *testing.T) {
	cr := compileBot(t, llmRouterPassThroughBot)
	if cr.Workflow == nil {
		t.Fatalf("compile returned nil workflow: %v", cr.Diagnostics)
	}
	if msgs := c034Messages(cr); len(msgs) > 0 {
		t.Fatalf("llm-router {{input.topic}} must not trip C034: %v", msgs)
	}

	var got any
	exec := newStubExecutor()
	exec.on("pick", func(in map[string]any) (map[string]any, error) {
		if _, ok := in["_route_candidates"]; !ok {
			t.Error("expected _route_candidates on the llm router input")
		}
		return map[string]any{"selected_route": "left", "reasoning": "go left"}, nil
	})
	exec.on("left", func(in map[string]any) (map[string]any, error) {
		got = in["topic"]
		return map[string]any{"topic": in["topic"]}, nil
	})
	exec.on("right", func(map[string]any) (map[string]any, error) {
		t.Error("right must not run")
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(cr.Workflow, s, exec)
	if err := eng.Run(context.Background(), "run-llm-pass", map[string]any{"topic": "hello"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "hello" {
		t.Errorf("left topic = %#v, want hello (llm router must pass its input through)", got)
	}
}

const schemalessSeedBot = `
schema dst_in:
  reviewer: string
  produced: string

prompt sys:
  System.

prompt usr:
  User.

agent seed:
  model: "m"
  system: sys
  user: usr

agent dst:
  model: "m"
  input: dst_in
  output: dst_in
  system: sys
  user: usr

vars:
  reviewer: string

workflow test:
  entry: seed
  worktree: none
  sandbox: none
  seed -> dst with {
    reviewer: "{{input.reviewer}}",
    produced: "{{input.produced}}"
  }
  dst -> done
`

// TestEdgeInputRef_SchemalessSourceWarnsAndDoesNotFallBack is R79a8cc:
// a schemaless agent used to leak --var reviewer=on into {{input.reviewer}}
// with no diagnostic. C032 now warns, and the resolver does not fall back.
func TestEdgeInputRef_SchemalessSourceWarnsAndDoesNotFallBack(t *testing.T) {
	cr := compileBot(t, schemalessSeedBot)
	if cr.Workflow == nil {
		t.Fatalf("compile returned nil workflow: %v", cr.Diagnostics)
	}
	if hasC034Field(c034Messages(cr), "reviewer") {
		t.Errorf("schemaless {{input.reviewer}} must warn C032, not error C034")
	}
	var warned bool
	for _, d := range cr.Diagnostics {
		if d.Code == ir.DiagRefNodeNoSchema && strings.Contains(d.Message, `field "reviewer"`) {
			warned = true
			if d.Severity != ir.SeverityWarning {
				t.Errorf("C032 severity = %s, want warning", d.Severity)
			}
		}
	}
	if !warned {
		t.Fatalf("missing C032 for schemaless {{input.reviewer}}: %v", cr.Diagnostics)
	}

	var got map[string]any
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{"produced": "from-seed"}, nil
	})
	exec.on("dst", func(in map[string]any) (map[string]any, error) {
		got = in
		return map[string]any{"reviewer": "", "produced": "ok"}, nil
	})

	s := tmpStore(t)
	eng := New(cr.Workflow, s, exec)
	if err := eng.Run(context.Background(), "run-schemaless", map[string]any{"reviewer": "on"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got == nil {
		t.Fatal("dst was not executed")
	}
	if got["produced"] != "from-seed" {
		t.Errorf("produced (source output) = %#v, want from-seed", got["produced"])
	}
	if got["reviewer"] != nil {
		t.Errorf("reviewer (run input only) = %#v, want nil — C032 warned and runtime must not fall back; use {{vars.reviewer}}", got["reviewer"])
	}
}
