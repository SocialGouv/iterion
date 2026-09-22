package parser

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ParseEdgeLine reads one edge line — `src -> dst [when …|else] [as …]
// [with {…}]` — as the grammar of that profile reads it inside a workflow
// or a group body, and reports the edges it names. Quoted strings on the
// line are read with the standard escapes whatever the profile: the form
// the author document writes them in (pkg/dsl/author), where an edge
// travels as one string of the .bot's own grammar.
//
// The whole line must be the edge: a line that does not open with a node
// reference and an arrow, a clause the grammar refuses, or text after the
// last clause reports a diagnostic, positioned on the line (line 1) at the
// column of the token. Nothing is read past the line — a second line is
// refused by name, never read as a second edge.
func ParseEdgeLine(profile int, line string) ([]*ast.Edge, []Diagnostic) {
	const name = "edge"
	if strings.ContainsAny(line, "\n\r") {
		return nil, []Diagnostic{{
			Code: DiagExpectedToken, Severity: SeverityError, File: name, Line: 1, Column: 1,
			Message: "an edge is one line: `src -> dst`, then its clauses",
			Hint:    HintFor(DiagExpectedToken),
		}}
	}
	if strings.TrimSpace(line) == "" {
		return nil, []Diagnostic{{
			Code: DiagExpectedToken, Severity: SeverityError, File: name, Line: 1, Column: 1,
			Message: "expected an edge line `src -> dst …`, got an empty line",
			Hint:    HintFor(DiagExpectedToken),
		}}
	}
	if profile < 1 {
		profile = ast.DefaultProfile
	}
	p := &parser{lex: newLexer(name, line+"\n", profile, true), file: name}
	if t := p.peek(); t.Type == TokenIndent {
		p.addError(DiagExpectedToken, t, "an edge line starts with its source node, not with indentation")
		return nil, p.diags
	}
	if !p.edgeAhead() {
		p.addError(DiagExpectedToken, p.peek(), "expected an edge line `src -> dst …` — a node reference, dotted for a group instance's node, then `->`")
		return nil, p.diags
	}
	edges := p.parseEdge()
	for {
		t := p.peek()
		switch t.Type {
		case TokenEOF:
			return edges, p.diags
		case TokenNewline, TokenComment, TokenDedent:
			p.next()
		default:
			p.addError(DiagExpectedToken, t, "an edge line ends after its clauses, got '"+t.Value+"'")
			return edges, p.diags
		}
	}
}
