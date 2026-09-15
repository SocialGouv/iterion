package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

func TestLiteralTemplateRefsPreserveFences(t *testing.T) {
	for _, raw := range []string{`{{"{{"}}`, `{{ "{{" }}`} {
		refs, err := ParseRefs(raw + `vars.missing}} / {{vars.real}} / ` + raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 3 || refs[0].Kind != RefLiteralOpen || refs[1].Kind != RefVars || refs[2].Kind != RefLiteralOpen || refs[0].Raw != raw {
			t.Fatalf("refs=%+v", refs)
		}
	}
	for _, bad := range []string{`{{outer{{inner}}}}`, `{{"arbitrary"}}`, `{{!"{{"}}`, `{{"{{"`, `{{"{{" + vars.x}}`} {
		if _, err := ParseRefs(bad); err == nil {
			t.Fatalf("malformed/noncanonical %q accepted", bad)
		}
	}
}

func TestLiteralTemplateSurvivesIncludesAndTransport(t *testing.T) {
	for _, profile := range []string{"", "dsl: 2\n"} {
		dir := t.TempDir()
		// This escaped include must never try to read the missing file.
		const included = `Show {{"{{"}}include "missing.md"}} and {{"{{"}}vars.missing}}.`
		if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte(included), 0600); err != nil {
			t.Fatal(err)
		}
		src := profile + botWithInclude("rules.md")
		first := compileAt(t, filepath.Join(dir, "main.bot"), src)
		if first.HasErrors() {
			t.Fatal(first.Diagnostics)
		}
		raw, err := ast.MarshalFile(parseFile(t, strings.Replace(src, `{{include "rules.md"}}`, included, 1)))
		if err != nil {
			t.Fatal(err)
		}
		restored, err := ast.UnmarshalFile(raw)
		if err != nil {
			t.Fatal(err)
		}
		result := Compile(restored)
		if result.HasErrors() {
			t.Fatal(result.Diagnostics)
		}
		for _, wf := range []*Workflow{first.Workflow, result.Workflow} {
			if !strings.Contains(wf.Prompts["sys"].Body, included) {
				t.Fatalf("literal lost before rendering: %q", wf.Prompts["sys"].Body)
			}
			if len(wf.Prompts["sys"].TemplateRefs) != 2 {
				t.Fatalf("refs=%+v", wf.Prompts["sys"].TemplateRefs)
			}
		}
	}
}

func TestLiteralTemplateGroupParametersRemainOpaque(t *testing.T) {
	src := `dsl: 2
schema out:
  result: string
group g(value):
  tool work:
    command: "printf '%s' '{{\"{{\"}}params.value}} / {{params.value}}'"
    output: out
use g as a with { value: "ONE" }
use g as b with { value: "TWO" }
workflow w:
  entry: a.work
  a.work -> b.work
  b.work -> done
`
	wf := mustCompile(t, src)
	for _, id := range []string{"a.work", "b.work"} {
		node := wf.Nodes[id].(*ToolNode)
		if !strings.Contains(node.Command, `{{"{{"}}params.value}}`) || len(node.CommandRefs) != 1 || node.CommandRefs[0].Kind != RefLiteralOpen {
			t.Fatalf("%s: %q refs=%+v", id, node.Command, node.CommandRefs)
		}
	}
}

func TestLiteralTemplateLoopCapHasStringType(t *testing.T) {
	src := "dsl: 2\n" + strings.Replace(loopCapExpressionSource, `"vars.max_passes - 1"`, `"{{\"{{\"}}"`, 1)
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError && strings.Contains(d.Message, "cap has type string; expected an integer") {
			return
		}
	}
	t.Fatalf("missing string-cap diagnostic: %+v", r.Diagnostics)
}

func TestLiteralTemplateQuotedDelimiterDoesNotHideDynamicWarning(t *testing.T) {
	got := refInQuotes(`printf '%s' '{{"{{"}}vars.x}} / {{vars.real}}'`)
	if len(got) != 1 || got[0] != `{{vars.real}}` {
		t.Fatalf("quoted dynamic refs=%v", got)
	}
}

func TestLiteralTemplateCompilesEveryStringSite(t *testing.T) {
	const src = `dsl: 2
schema out:
  text: string
tool start:
  command: "printf '%s' '{{\"{{\"}}'"
  postcondition: "test '{{\"{{\"}}' = '{{\"{{\"}}'"
  output: out
tool script_node:
  language: js
  script: |
    console.log(JSON.stringify({text:"{{"{{"}}vars.missing}}"}))
  output: out
agent describe:
  model: "test-model"
  images: ["/tmp/{{\"{{\"}}literal.png"]
  output: out
fail stop:
  code: TEST_LITERAL
  message: "{{\"{{\"}}vars.missing}}"
workflow w:
  worktree: none
  entry: start
  start -> script_node with { text: "{{\"{{\"}}vars.missing}}" }
  script_node -> script_node as foreach scan(item in "{{\"{{\"}}")
  script_node -> describe
  describe -> stop
`
	result := compileFile(t, src)
	if result.HasErrors() {
		t.Fatal(result.Diagnostics)
	}
	for _, d := range result.Diagnostics {
		if d.Code == DiagQuotedCommandRef {
			t.Fatalf("literal incorrectly diagnosed as dynamic: %v", d)
		}
	}
	wf := result.Workflow
	for _, refs := range [][]*Ref{wf.Nodes["start"].(*ToolNode).CommandRefs, wf.Nodes["start"].(*ToolNode).PostcondRefs, wf.Nodes["script_node"].(*ToolNode).ScriptRefs, wf.Nodes["stop"].(*FailNode).Message.Refs, wf.Foreaches["scan"].CollectionRefs, wf.Edges[0].With[0].Refs} {
		if len(refs) == 0 {
			t.Fatal("literal was dropped before rendering")
		}
		for _, r := range refs {
			if r.Kind != RefLiteralOpen {
				t.Fatalf("unexpected ref=%+v", r)
			}
		}
	}
	if got := wf.Nodes["describe"].(*AgentNode).Images[0]; got != `/tmp/{{"{{"}}literal.png` {
		t.Fatalf("image=%q", got)
	}
}
