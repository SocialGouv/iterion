package botimport

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestImportDraftUsesProfileTwoAndKeepsParagraphs(t *testing.T) {
	res, err := Import("paragraphs.js", []byte("await agent(`First paragraph.\n\nSecond paragraph.`, {label: 'writer'})"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse("paragraphs.bot", res.BotSource)
	if pr.File == nil || pr.File.EffectiveProfile() != 2 {
		t.Fatalf("imported draft must declare profile 2: %s", res.BotSource)
	}
	if got, want := pr.File.Prompts[0].Body, "First paragraph.\n\nSecond paragraph."; got != want {
		t.Fatalf("imported prompt = %q, want %q", got, want)
	}
}

func TestEmitProfileTwoQuotedValuesRoundTrip(t *testing.T) {
	for _, value := range []string{
		`literal \n and a trailing backslash \`,
		"quotes \" and ' with `backticks`",
		"line one\nline two\r\tend",
		"control \x00\x07\x0b\x1b\x7f",
		"unicode é — \u2028 \U0001f916",
	} {
		t.Run(value, func(t *testing.T) {
			m := &model{
				WorkflowName: "quoted", Entry: "worker", Report: &Report{SourceFile: "quoted.js"},
				Schemas: []schemaOut{{name: "result", fields: []schemaField{{name: "value", typ: "string", enum: []string{value}}}}},
				Prompts: []promptOut{{name: "request", text: "Work."}},
				Nodes: []*nodeOut{
					{kind: "agent", id: "worker", model: value, userPrompt: "request", output: "result"},
					{kind: "router", id: "dispatch", routerMode: "fan_out_each", over: value, alias: "item"},
					{kind: "tool", id: "command", command: value},
				},
				Edges: []edgeOut{{src: "worker", dst: "done", when: value}},
			}
			text := emit(m)
			// Read as profile 2 independently of the header assertion so this
			// also catches a producer that changes the header but keeps Go %q.
			if parser.ReadPreamble(text).Profile != 2 {
				text = "dsl: 2\n" + text
			}
			pr := parser.Parse("quoted.bot", text)
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("emitted value %q does not parse under profile 2: %s", value, d.Error())
				}
			}
			if pr.File == nil {
				t.Fatal("no parsed draft")
			}
			f := pr.File
			for name, got := range map[string]string{
				"model": f.Agents[0].Model, "over": f.Routers[0].Over,
				"command": f.Tools[0].Command, "condition": f.Workflows[0].Edges[0].When.Expr,
				"enum": f.Schemas[0].Fields[0].EnumValues[0],
			} {
				if got != value {
					t.Errorf("%s = %q, want %q", name, got, value)
				}
			}
		})
	}
}
