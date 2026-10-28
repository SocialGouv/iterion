package parser

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/readbudget"
)

// A loop cap past the read budget on its own is the compiler's to refuse
// (C320, on the bot's budget, at the edge): the parser keeps the cap's text
// and says nothing — a refusal here would drop the cap and the compiler
// would then say the loop has none.
func TestLoopCapExpressionPastTheBudgetIsLeftToTheCompiler(t *testing.T) {
	t.Setenv(readbudget.Env, "4096")
	cap := strings.Repeat("vars.n+", 2_500) + "vars.n" // 10 001 records on a budget of 4 096
	src := "workflow w:\n  entry: a\n  a -> b as l(\"" + cap + "\")\nagent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n"
	res := parseLenient("cap.bot", src)
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "read budget") || d.Line == 3 {
			t.Errorf("the parser said %s at %d:%d: %s — the cap past the budget is the compiler's to refuse", d.Code, d.Line, d.Column, d.Message[:min(len(d.Message), 120)])
		}
	}
	edges := res.File.Workflows[0].Edges
	if len(edges) != 1 || edges[0].Loop == nil || edges[0].Loop.MaxIterationsExpr != cap {
		t.Fatalf("the cap was not kept for the compiler: %d edges, loop %+v", len(edges), edges[0].Loop)
	}
	// A cap with a shape error is still refused here, its text quoted by
	// excerpt: a 10 000-rune cap draws a message under 500 bytes.
	res = parseLenient("bad.bot", "workflow w:\n  entry: a\n  a -> b as l(\"vars."+strings.Repeat("n", 10_000)+" +\")\nagent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n")
	var said bool
	for _, d := range res.Diagnostics {
		if d.Line == 3 && strings.Contains(d.Message, "invalid expression") {
			said = true
			if len(d.Message) > 500 || !strings.Contains(d.Message, "…") {
				t.Errorf("the E002 is %d bytes: %.160s — want the cap quoted by excerpt", len(d.Message), d.Message)
			}
		}
	}
	if !said {
		t.Errorf("a cap with a shape error drew no diagnostic at its line: %+v", res.Diagnostics)
	}
}

// Every diagnostic a source's readers record — the parser's, the YAML
// twin's, the unit loader's — passes through Report.Add, which holds its
// message and hint to expr.MaxDiagnosticText whatever the site composed.
func TestEveryRecordedDiagnosticTextIsBounded(t *testing.T) {
	huge := strings.Repeat("é", 1<<20)
	var r Report
	r.Add(Diagnostic{Code: DiagUnexpectedToken, Severity: SeverityError, Message: huge, Hint: huge, File: "b.bot", Line: 1, Column: 1})
	d := r.Diagnostics()[0]
	if len(d.Message) > expr.MaxDiagnosticText+len("…") || !strings.HasSuffix(d.Message, "…") || len(d.Hint) > expr.MaxDiagnosticText+len("…") {
		t.Errorf("a %d-byte message/hint was recorded as %d / %d bytes", len(huge), len(d.Message), len(d.Hint))
	}
	p := &parser{lex: lexSource("b.bot", "agent a:\n  model: \"m\"\n", sourceBudget()), file: "b.bot"}
	p.addErrorHint(DiagUnexpectedToken, Token{Type: TokenIdent, Value: "a", Line: 1, Column: 1}, huge, huge)
	if d := p.report.Diagnostics()[0]; len(d.Message) > expr.MaxDiagnosticText+len("…") {
		t.Errorf("through addErrorHint: %d bytes", len(d.Message))
	}
}
