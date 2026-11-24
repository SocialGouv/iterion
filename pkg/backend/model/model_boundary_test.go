package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The spine of ADR-146's axis 1a: a sentinel node A printed re-enters node
// B's context through `{{outputs.a.stdout}}` and MUST reach the model as the
// placeholder — in the user prompt, in the system prompt — on `claude_code`
// and on `claw` alike, because one executor builds both tasks and one seam
// (executeDelegate) hands them over. The crossing itself keeps the value:
// the template snapshot B rendered from is untouched, so a tool node of B
// could still spend what A obtained.
func TestASentinelFromNodeAReachesNodeBsModelAsThePlaceholder(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-4f9c2e7a1b3d-sentinel"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	for _, backendName := range []string{delegate.BackendClaudeCode, delegate.BackendClaw} {
		t.Run(backendName, func(t *testing.T) {
			t.Parallel()
			backend := &capturingBackend{results: []delegate.Result{{Output: map[string]any{"ok": true}, BackendName: backendName}}}
			reg := delegate.NewRegistry()
			reg.Register(backendName, backend)
			wf := &ir.Workflow{
				Prompts: map[string]*ir.Prompt{
					"sys":  {Name: "sys", Body: "Remember what A said: {{outputs.a.stdout}}"},
					"user": {Name: "user", Body: "Node A printed: {{outputs.a.stdout}} — use it."},
				},
				Schemas: map[string]*ir.Schema{},
			}
			exec := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg), WithSecretGuard(guard))
			node := &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "b"},
				LLMFields: ir.LLMFields{Backend: backendName, Model: "anthropic/m", SystemPrompt: "sys", UserPrompt: "user"},
			}
			td := &TemplateData{
				Outputs: map[string]map[string]any{"a": {"stdout": "token is " + sentinel + "\n"}},
				Nodes:   map[string]ir.Node{"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}}},
			}
			if _, err := exec.executeBackend(WithTemplateData(context.Background(), td), node, map[string]any{}); err != nil {
				t.Fatalf("executeBackend: %v", err)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if len(backend.tasks) != 1 {
				t.Fatalf("backend calls = %d, want 1", len(backend.tasks))
			}
			got := backend.tasks[0]
			for name, text := range map[string]string{"user prompt": got.UserPrompt, "system prompt": got.SystemPrompt} {
				if strings.Contains(text, sentinel) {
					t.Errorf("%s's %s carries the value node A printed: %q", backendName, name, text)
				}
				if !strings.Contains(text, placeholder) {
					t.Errorf("%s's %s does not carry the placeholder where the value was: %q", backendName, name, text)
				}
			}
			// The crossing keeps the real value: what B rendered from is what
			// the nodes downstream and the checkpoint read.
			if !strings.Contains(td.Outputs["a"]["stdout"].(string), sentinel) {
				t.Errorf("the live output of node A lost its value: %q", td.Outputs["a"]["stdout"])
			}
		})
	}
}

// The boundary follows the Layer 1 switch: with placeholders off the
// operator asked for values in prompts, and the boundary renders them too.
func TestTheModelBoundaryFollowsThePlaceholderSwitch(t *testing.T) {
	t.Parallel()
	const sentinel = "tok-7a1b3d4f9c2e-sentinel"
	cfg := secretguard.DefaultConfig()
	cfg.Placeholders = false
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, cfg)
	e := &ClawExecutor{secretGuard: guard}
	task := e.taskForModel(delegate.Task{UserPrompt: "A said " + sentinel, SystemPrompt: sentinel, UserContent: []delegate.ContentBlock{{Type: "text", Text: sentinel}}})
	if !strings.Contains(task.UserPrompt, sentinel) || task.SystemPrompt != sentinel || task.UserContent[0].Text != sentinel {
		t.Fatalf("placeholders off, yet the boundary redacted: %+v", task)
	}

	on := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())
	e = &ClawExecutor{secretGuard: on}
	in := delegate.Task{UserPrompt: "A said " + sentinel, PresetFragment: "focus " + sentinel, UserContent: []delegate.ContentBlock{{Type: "text", Text: "see " + sentinel}, {Type: "image", Text: ""}}}
	out := e.taskForModel(in)
	for name, text := range map[string]string{"user prompt": out.UserPrompt, "preset fragment": out.PresetFragment, "content block": out.UserContent[0].Text} {
		if strings.Contains(text, sentinel) {
			t.Errorf("%s still carries the value: %q", name, text)
		}
	}
	// The caller's task is a value and its content blocks are copied: the
	// executor's own copy keeps what it rendered.
	if in.UserContent[0].Text != "see "+sentinel {
		t.Errorf("the boundary mutated the caller's content blocks: %q", in.UserContent[0].Text)
	}
	var none *ClawExecutor
	if got := none.taskForModel(delegate.Task{UserPrompt: sentinel}); got.UserPrompt != sentinel {
		t.Errorf("a nil executor redacted: %q", got.UserPrompt)
	}
}

// Every task crosses into a backend through executeDelegate, whichever path
// built it: a `backend.Execute(…)` written anywhere else in the package would
// hand a model the values this boundary keeps out, so the package is read
// and the only function allowed to call Execute on a backend handle is the
// seam. A backend handle is an identifier whose name says so (`backend`,
// `baseBackend`, …); a ToolDef's Execute (`resolved.Execute`, `gt.Execute`)
// is a tool, not a model, and is not a backend handle.
func TestEveryBackendExecuteCrossesTheModelBoundary(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, callers := 0, map[string]bool{}
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
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Execute" {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok || !strings.Contains(strings.ToLower(recv.Name), "backend") {
					return true
				}
				callers[fn.Name.Name] = true
				return true
			})
		}
	}
	if files < 40 {
		t.Fatalf("parsed %d non-test files — the sweep is not reading the package", files)
	}
	if !callers["executeDelegate"] {
		t.Fatal("executeDelegate no longer calls Execute on a backend handle: the seam is gone and this sweep proves nothing")
	}
	delete(callers, "executeDelegate")
	for fn := range callers {
		t.Errorf("%s calls Execute on a backend handle directly — route it through executeDelegate, the model boundary", fn)
	}
}
