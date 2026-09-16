package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestLiteralTemplateRenderingIsSinglePass(t *testing.T) {
	const source = `{{"{{"}}vars.x}} / {{vars.x}} / {{"{{"}}`
	const want = `{{vars.x}} / {{vars.y}} / {{`
	refs, err := ir.ParseRefs(source)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"x": "{{vars.y}}", "y": "CASCADE"}
	e := NewClawExecutor(NewRegistry(), &ir.Workflow{})
	e.vars = vars
	if got := e.resolveTemplate(source, nil, nil); got != want {
		t.Fatalf("prompt=%q", got)
	}
	for _, render := range []func(any) string{rawTemplateValue, shellEscapeValue, jsonLiteralValue} {
		// Literal source is inserted verbatim, dynamic values retain each site's encoding.
		expected := `{{vars.x}} / ` + render(vars["x"]) + ` / {{`
		if got := resolveTemplateWith(source, refs, nil, vars, nil, "", nil, render, true, nil); got != expected {
			t.Fatalf("render=%q want=%q", got, expected)
		}
	}
}

func TestLiteralTemplateRealShellCommandAndPostcondition(t *testing.T) {
	const command = `printf '%s' '{{"{{"}}vars.missing}}' | tee literal.txt`
	const post = `test -f literal.txt && test "$(cat literal.txt)" = '{{"{{"}}vars.missing}}'`
	refs, err := ir.ParseRefs(command)
	if err != nil {
		t.Fatal(err)
	}
	postRefs, err := ir.ParseRefs(post)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	dir := t.TempDir()
	e := newTestClawExecutor(NewRegistry(), wf, WithWorkDir(dir))
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "literal"}, Command: command, CommandRefs: refs, Postcondition: post, PostcondRefs: postRefs}
	out, err := e.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatal(err)
	}
	written, readErr := os.ReadFile(filepath.Join(dir, "literal.txt"))
	if readErr != nil || string(written) != `{{vars.missing}}` {
		t.Fatalf("written=%q err=%v result=%v", written, readErr, out)
	}
}

func TestLiteralTemplateMultimodalAndImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, []byte("image bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	const body = `Show {{"{{"}}attachments.pic}} then {{attachments.pic}} and {{"{{"}}`
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"p": {Name: "p", Body: body}}}
	e := NewClawExecutor(NewRegistry(), wf)
	td := &TemplateData{Attachments: map[string]AttachmentInfo{"pic": {Name: "pic", Path: path, HostPath: path, MIME: "image/png"}}}
	text, blocks := e.buildUserContent("p", nil, td, map[string]bool{"pic": true})
	if text != `Show {{attachments.pic}} then `+path+` and {{` {
		t.Fatalf("text=%q", text)
	}
	if len(blocks) != 3 || blocks[0].Text != `Show {{attachments.pic}} then ` || blocks[1].Type != "image" || blocks[2].Text != ` and {{` {
		t.Fatalf("blocks=%+v", blocks)
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}
	task, err := e.buildTask(context.Background(), node, backendFields{id: "a", images: []string{`/tmp/{{"{{"}}literal.png`}}, nil, delegate.BackendClaw, &nodeBuildSession{})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Images) != 1 || task.Images[0] != `/tmp/{{literal.png` {
		t.Fatalf("images=%v", task.Images)
	}
}

func TestLiteralTemplateRealScriptAndRecipe(t *testing.T) {
	const script = `console.log(JSON.stringify({text: "{{"{{"}}vars.missing}}", value: {{vars.x}}}))`
	refs, err := ir.ParseRefs(script)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Name: "w", Prompts: map[string]*ir.Prompt{"p": {Name: "p", Body: "old"}}, Vars: map[string]*ir.Var{"x": {Name: "x", Type: ir.VarString}}}
	spec := &recipe.RecipeSpec{Name: "literal", WorkflowRef: recipe.WorkflowRef{Name: "w"}, PresetVars: recipe.PresetVars{"x": "{{vars.missing}}"}, PromptPack: recipe.PromptPack{"p": `{{"{{"}}vars.missing}} / {{vars.x}}`}}
	configured, err := spec.Apply(wf)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestClawExecutor(NewRegistry(), configured, WithWorkDir(t.TempDir()))
	if got := e.resolveTemplate(configured.Prompts["p"].Body, nil, nil); got != `{{vars.missing}} / {{vars.missing}}` {
		t.Fatalf("recipe prompt=%q", got)
	}
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "literal-script"}, Script: script, ScriptRefs: refs, Language: "js"}
	out, err := e.Execute(context.Background(), node, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != `{{vars.missing}}` || out["value"] != `{{vars.missing}}` {
		t.Fatalf("script result=%v", out)
	}
	if wf.Prompts["p"].Body != "old" {
		t.Fatal("recipe mutated source")
	}
}
