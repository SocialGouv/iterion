package model

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/secretsink"
)

const directSentinel = "tok-direct-9f8e7d6c5b4a-sentinel"

func directGuard() *secretguard.Guard {
	return secretguard.New([]secretguard.Secret{{Name: "api_token", Value: directSentinel}}, secretguard.DefaultConfig())
}

func directWire(req api.CreateMessageRequest) string {
	b, _ := json.Marshal(req)
	return string(b)
}

// The executor's DIRECT model calls — the human stand-in, the interaction
// stand-in, the review companion — hand GenerateObjectDirect their options
// instead of crossing a backend: they take the same boundary pass as a
// task's prompts (generationForModel), so a sentinel node A printed reaches
// none of them in clear.
func TestEveryStandInModelCallCrossesTheModelBoundary(t *testing.T) {
	t.Parallel()
	td := func() *TemplateData {
		return &TemplateData{
			Outputs: map[string]map[string]any{"a": {"stdout": "token is " + directSentinel}},
			Nodes:   map[string]ir.Node{"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}}},
		}
	}
	t.Run("human stand-in", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry()
		mock := &captureRequestClient{stream: mockStreamEvents(`{"approve":true}`, "end_turn")}
		reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
		node := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM}, Model: "anthropic/claude-sonnet-4-6", SystemPrompt: "gate_sys"}
		node.OutputSchema = "decision"
		wf := &ir.Workflow{
			Name:    "direct_human",
			Nodes:   map[string]ir.Node{"gate": node},
			Prompts: map[string]*ir.Prompt{"gate_sys": {Name: "gate_sys", Body: "Node A printed: {{outputs.a.stdout}}"}},
			Schemas: map[string]*ir.Schema{"decision": {Name: "decision", Fields: []*ir.SchemaField{{Name: "approve", Type: ir.FieldTypeBool}}}},
			Vars:    map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		}
		e := NewClawExecutor(reg, wf, WithSecretGuard(directGuard()))
		_, _ = e.executeHumanLLM(WithTemplateData(context.Background(), td()), node, map[string]any{"ctx": "edge-mapped " + directSentinel}, nil)
		if len(mock.reqs) == 0 {
			t.Fatal("no request reached the client")
		}
		if wire := directWire(mock.last()); strings.Contains(wire, directSentinel) || !strings.Contains(wire, secretguard.PlaceholderForName("api_token")) {
			t.Fatalf("the human stand-in's request carries the value, or not the placeholder: %s", wire)
		}
	})
	t.Run("interaction stand-in", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry()
		mock := &captureRequestClient{stream: mockStreamEvents(`{"q1":"4"}`, "end_turn")}
		reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
		wf := &ir.Workflow{
			Name: "direct_interaction", Nodes: map[string]ir.Node{},
			Prompts: map[string]*ir.Prompt{"ask": {Name: "ask", Body: "Context from A: {{outputs.a.stdout}} Question: {{input.q1}}"}},
			Schemas: map[string]*ir.Schema{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		}
		e := NewClawExecutor(reg, wf, WithSecretGuard(directGuard()))
		_, _, _, _ = e.ExecuteHumanLLMForInteraction(WithTemplateData(context.Background(), td()), "worker",
			&ErrNeedsInteraction{NodeID: "worker", Questions: map[string]any{"q1": "Is " + directSentinel + " valid?"}},
			ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionPrompt: "ask", InteractionModel: "anthropic/claude-sonnet-4-6"})
		if len(mock.reqs) == 0 {
			t.Fatal("no request reached the client")
		}
		if wire := directWire(mock.last()); strings.Contains(wire, directSentinel) {
			t.Fatalf("the interaction stand-in's request carries the value: %s", wire)
		}
	})
	t.Run("review companion", func(t *testing.T) {
		t.Parallel()
		reg := NewRegistry()
		mock := &captureRequestClient{stream: mockStreamEvents(`{"message":"ok","needs_human_input":false,"decision":"approve"}`, "end_turn")}
		reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
		node := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "review"}, Model: "anthropic/claude-sonnet-4-6"}
		node.OutputSchema = "verdict"
		wf := &ir.Workflow{
			Name: "direct_review", Nodes: map[string]ir.Node{"review": node}, Prompts: map[string]*ir.Prompt{},
			Schemas: map[string]*ir.Schema{"verdict": {Name: "verdict", Fields: []*ir.SchemaField{{Name: "decision", Type: ir.FieldTypeString}}}},
			Vars:    map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		}
		e := NewClawExecutor(reg, wf, WithSecretGuard(directGuard()))
		_, _ = e.ExecuteReviewCompanion(context.Background(), node, "Authored contract; A said "+directSentinel, "# Change under review\n+ token="+directSentinel)
		if len(mock.reqs) == 0 {
			t.Fatal("no request reached the client")
		}
		if wire := directWire(mock.last()); strings.Contains(wire, directSentinel) {
			t.Fatalf("the review companion's request carries the value: %s", wire)
		}
	})
}

// The schema re-ask is a second call built from the first task: it crosses
// the seam too, so the value a prompt reads through an upstream output never
// reaches the model on the retry either.
func TestTheSchemaReaskCrossesTheModelBoundary(t *testing.T) {
	t.Parallel()
	backend := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"other": "x"}, BackendName: "test_backend"},
		{Output: map[string]any{"answer": "blue"}, BackendName: "test_backend"},
	}}
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{"user": {Name: "user", Body: "A printed: {{outputs.a.stdout}}"}},
		Schemas: map[string]*ir.Schema{"out_schema": {Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}},
	}
	exec := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithSecretGuard(directGuard()))
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "answerer"}, LLMFields: ir.LLMFields{Backend: "test_backend", UserPrompt: "user"}, SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"}}
	td := &TemplateData{Outputs: map[string]map[string]any{"a": {"stdout": "token is " + directSentinel}}, Nodes: map[string]ir.Node{"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}}}}
	if _, err := exec.executeBackend(WithTemplateData(context.Background(), td), node, map[string]any{}); err != nil {
		t.Fatalf("executeBackend: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.tasks) != 2 {
		t.Fatalf("backend calls = %d, want the attempt and the re-ask", len(backend.tasks))
	}
	for i, task := range backend.tasks {
		if strings.Contains(task.UserPrompt, directSentinel) {
			t.Errorf("call %d carries the value: %q", i, task.UserPrompt)
		}
	}
}

// Two bounds of the boundary's matcher: a value registered on the NAME of
// a host variable (GIT_AUTHOR_EMAIL matches AUTH) is left alone — a
// placeholder there breaks the commit the agent was asked for — while a
// declared secret is turned back; and a declared secret kept off prompts
// (`sinks: [command, egress]`) maps to a token that resolves nowhere, not to
// the placeholder the proxy would spend on the wire.
func TestTheModelBoundaryLeavesAmbientNamesAloneAndWithholdsAPromptKeptOffSecret(t *testing.T) {
	t.Parallel()
	const email = "jo.dev42@example.org"
	guard := secretguard.New([]secretguard.Secret{
		{Name: "env_GIT_AUTHOR_EMAIL", Value: email, RedactOnly: true, Ambient: true},
		{Name: "api_token", Value: directSentinel},
		{Name: "gh_token", Value: "ghp-wire-only-4f9c2e7a1b3d", Sinks: []string{secretsink.Command, secretsink.Egress}},
	}, secretguard.DefaultConfig())
	e := &ClawExecutor{secretGuard: guard}
	got := e.taskForModel(delegate.Task{UserPrompt: "commit --author '" + email + "' token " + directSentinel + " header ghp-wire-only-4f9c2e7a1b3d"}).UserPrompt
	if !strings.Contains(got, email) {
		t.Errorf("the ambient author email was rewritten at the boundary: %q", got)
	}
	if strings.Contains(got, directSentinel) {
		t.Errorf("the declared secret reached the model: %q", got)
	}
	ph := secretguard.PlaceholderForName("gh_token")
	if strings.Contains(got, ph) || strings.Contains(got, "ghp-wire-only") || !strings.Contains(got, "__ITERION_SECRET_gh_token_redacted__") {
		t.Errorf("a secret kept off prompts reached the model as its value or its resolvable placeholder: %q", got)
	}
	if guard.MaterializeForHost("__ITERION_SECRET_gh_token_redacted__", "api.github.com") != "__ITERION_SECRET_gh_token_redacted__" {
		t.Error("the withheld token resolves on the wire")
	}
}

// Every GenerateObjectDirect the EXECUTOR makes — a direct model call that
// crosses no backend — hands its options through generationForModel: the
// package is read, and a call of a *ClawExecutor method whose options
// argument is not that call is refused. The backend's own calls
// (*ClawBackend, after the seam) are not the executor's.
func TestEveryDirectGenerationOfTheExecutorCrossesTheModelBoundary(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, seen := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		files++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				idx, ok := call.Fun.(*ast.IndexExpr)
				if !ok {
					return true
				}
				id, ok := idx.X.(*ast.Ident)
				if !ok || id.Name != "GenerateObjectDirect" {
					return true
				}
				if receiverType(fn) != "ClawExecutor" {
					return true
				}
				seen++
				if len(call.Args) != 3 || !isCallTo(call.Args[2], "generationForModel") {
					t.Errorf("%s:%s calls GenerateObjectDirect with options that did not come through generationForModel — the model boundary", name, fn.Name.Name)
				}
				return true
			})
		}
	}
	if files < 40 || seen < 3 {
		t.Fatalf("parsed %d files and saw %d direct generations of the executor — the sweep is not reading the package", files, seen)
	}
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func isCallTo(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name == name
	case *ast.Ident:
		return f.Name == name
	}
	return false
}
