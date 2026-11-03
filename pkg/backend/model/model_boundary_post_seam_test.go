package model

import (
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
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// The other half of ADR-146 axis 1a: beside the texts a delegate.Task
// FIELD holds, claw hands the model system blocks it composed for ITSELF —
// the ambient instruction files it reads under the task's work dir
// (clawAmbientSystemContext) and the workspace-memory index and autoload a
// store hands it (installWorkspaceMemory). No field holds those, so
// taskForModel's field pass cannot reach them; they take the same pass at
// the request, the last point before the wire.

// The window: node A's agent prints a declared secret and records the
// lesson in the repository's CLAUDE.md (this tree instructs its agents to
// pay discoveries back into committed .md), then node B reads that file
// into its system blocks. `workspace` is the DEFAULT policy, so the walk is
// on unless the author turned it off.
func TestEverySystemTextTheRequestCarriesCrossesTheBoundary(t *testing.T) {
	t.Parallel()
	const sentinel = "ghp-postseam-7a1b3d4f9c2e"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"),
		[]byte("# rules\n\ndeploy with "+sentinel+"\n"), 0o600); err != nil {
		t.Fatalf("workspace CLAUDE.md: %v", err)
	}

	e := &ClawExecutor{secretGuard: guard}
	out := e.taskForModel(delegate.Task{SystemPrompt: "base, and the key is " + sentinel, WorkDir: dir})
	// Premise: the field pass ran, so what is left is what it cannot reach.
	if strings.Contains(out.SystemPrompt, sentinel) {
		t.Fatalf("the boundary never ran on the task, so this test proves nothing: %q", out.SystemPrompt)
	}
	if out.AmbientContext.String() != "workspace" {
		t.Fatalf("a task built without a policy no longer defaults to workspace (%s): the default-on premise moved", out.AmbientContext)
	}

	// The two real producers, called as claw's Execute calls them.
	ambientBlock := clawAmbientSystemContext(out)
	if !strings.Contains(ambientBlock, sentinel) {
		t.Fatalf("the ambient walk read no CLAUDE.md, so this test proves nothing: %q", ambientBlock)
	}
	memoryBlock := renderAutoloadBlock("/memory", []knowledge.AutoloadEntry{
		{Path: "lessons.md", Content: []byte("the deploy token is " + sentinel)},
	})
	if !strings.Contains(memoryBlock, sentinel) {
		t.Fatalf("the memory autoload rendered nothing, so this test proves nothing: %q", memoryBlock)
	}

	opts := GenerationOptions{
		Model: "anthropic/m",
		SystemBlocks: []api.ContentBlock{
			{Type: "text", Text: out.BuildSystemPrompt()},
			{Type: "text", Text: ambientBlock},
			{Type: "text", Text: memoryBlock},
		},
		ScreenModelText: out.ScreenModelText,
	}
	req, err := buildRequest(opts, []api.Message{{
		Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}},
	}}, nil, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}

	for i, block := range req.SystemBlocks {
		if strings.Contains(block.Text, sentinel) {
			t.Errorf("system block %d reaches the model carrying the value: %q", i, block.Text)
		}
	}
	if strings.Contains(req.System, sentinel) {
		t.Errorf("the mirrored system string reaches the model carrying the value: %q", req.System)
	}
	// One for the author's prompt, one for the CLAUDE.md, one for the
	// memory entry: a pass that covered only the prompt would count 1.
	if n := strings.Count(req.System, placeholder); n != 3 {
		t.Errorf("the request carries %d placeholders, want 3 (the prompt, the ambient file, the memory entry): %q", n, req.System)
	}
	// The record keeps what the run composed: the boundary copies rather
	// than rewriting the options the tool loop reuses across turns.
	if !strings.Contains(opts.SystemBlocks[1].Text, sentinel) {
		t.Error("the request builder rewrote the caller's own blocks in place")
	}
}

// The twin of the task path: the executor's DIRECT generations reach the
// same request builder, so they hand it the same pass — a block one of them
// grows later is screened by the chokepoint instead of needing a guard of
// its own.
func TestADirectGenerationHandsTheRequestBuilderTheBoundaryPass(t *testing.T) {
	t.Parallel()
	const sentinel = "ghp-direct-7a1b3d4f9c2e"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	opts := (&ClawExecutor{secretGuard: guard}).generationForModel(GenerationOptions{
		Model:        "anthropic/m",
		SystemBlocks: []api.ContentBlock{{Type: "text", Text: "a file says " + sentinel}},
	})
	req, err := buildRequest(opts, []api.Message{{
		Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}},
	}}, nil, nil)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if strings.Contains(req.System, sentinel) || !strings.Contains(req.System, placeholder) {
		t.Errorf("a direct generation's system block reaches the model unscreened: %q", req.System)
	}
}

// The other half of pi's witness, which can only use a stand-in pass (the
// delegate package is decoupled from the guard by construction): the task a
// backend receives carries the GUARD's pass, so a composer that routes
// through it screens against the run's real table.
func TestTheBoundaryHandsTheTaskItsOwnPass(t *testing.T) {
	t.Parallel()
	const sentinel = "ghp-carried-7a1b3d4f9c2e"
	placeholder := secretguard.PlaceholderForName("api_token")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_token", Value: sentinel}}, secretguard.DefaultConfig())

	out := (&ClawExecutor{secretGuard: guard}).taskForModel(delegate.Task{})
	if out.ScreenModelText == nil {
		t.Fatal("the boundary hands the task no pass: every text a backend composes for itself reaches the model unscreened")
	}
	if got := out.ScreenedModelText("a file says " + sentinel); !strings.Contains(got, placeholder) || strings.Contains(got, sentinel) {
		t.Errorf("the pass the task carries is not the guard's model-boundary pass: %q", got)
	}
	// Redaction off is the identity, the contract the nil case states.
	bare := (&ClawExecutor{}).taskForModel(delegate.Task{})
	if bare.ScreenedModelText(sentinel) != sentinel {
		t.Error("a task built with no guard rewrote its own text")
	}
}

// The wiring the witness above assumes, read from the source: claw's
// Execute hands the request builder the pass the task carries. An options
// literal that dropped the key would leave every block it composed
// unscreened while a test that builds its own options stayed green.
func TestClawExecuteHandsTheRequestBuilderTheBoundaryPass(t *testing.T) {
	t.Parallel()
	const field = "ScreenModelText"
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Clean("claw_backend.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse claw_backend.go: %v", err)
	}
	var execute *ast.FuncDecl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Execute" && fn.Recv != nil {
			execute = fn
		}
	}
	if execute == nil {
		t.Fatal("claw_backend.go declares no Execute with a receiver: the wiring moved")
	}
	seen := false
	ast.Inspect(execute.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "GenerationOptions" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != field {
				continue
			}
			sel, ok := kv.Value.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field {
				t.Errorf("Execute sets %s from something other than the task's own pass", field)
				continue
			}
			seen = true
		}
		return true
	})
	if !seen {
		t.Errorf("Execute's GenerationOptions literal carries no %s: every system block it composes for itself reaches the model unscreened", field)
	}
}
