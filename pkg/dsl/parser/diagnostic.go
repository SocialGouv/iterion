package parser

import "fmt"

// DiagCode identifies the kind of parse diagnostic.
type DiagCode string

const (
	// Structural errors
	DiagUnexpectedToken DiagCode = "E001" // unexpected token
	DiagExpectedToken   DiagCode = "E002" // expected specific token
	DiagBadIndentation  DiagCode = "E003" // indentation mismatch
	DiagUnterminatedStr DiagCode = "E004" // unterminated string literal

	// Declaration errors
	DiagDuplicateDecl   DiagCode = "E010" // duplicate declaration name
	DiagReservedName    DiagCode = "E011" // use of reserved name (done/fail) as declaration
	DiagUnknownProperty DiagCode = "E012" // unknown property in a node block
	DiagMissingProperty DiagCode = "E013" // required property missing
	DiagDuplicateBlock  DiagCode = "E014" // duplicate top-level block (vars/presets/attachments)

	// Value errors
	DiagInvalidValue DiagCode = "E020" // invalid value (e.g. bad session mode)
	DiagInvalidType  DiagCode = "E021" // invalid type expression

	// Edge clause errors
	DiagDuplicateEdgeClause DiagCode = "E030" // duplicate when/as/with clause on an edge
	DiagElseWithWhen        DiagCode = "E031" // an edge cannot carry both when and else
)

// hints is the one-line remedy each parse code arrives with. A parse error
// is positioned to the token, so the hint names the shape the parser wanted
// there rather than repeating the message.
var hints = map[DiagCode]string{
	DiagUnexpectedToken:     "Inside a block every line is `key: value` (or `src -> dst` inside `workflow:`); check this line's indentation and that its block is still open.",
	DiagExpectedToken:       "Quote string values (`backend: \"claw\"`), write lists inline as `[a, b]`, and open every declaration as `<kind> <name>:` above an indented block.",
	DiagBadIndentation:      "Indent with spaces only, by the same width at every level, and align the line with an enclosing block.",
	DiagUnterminatedStr:     "Close the quote, or use a backtick raw string / a `|` block scalar for multi-line content.",
	DiagDuplicateDecl:       "Rename one of the two declarations.",
	DiagReservedName:        "`done` and `fail` are the reserved terminal targets; pick another name.",
	DiagUnknownProperty:     "Check the property table for this node kind in docs/references/dsl-grammar.md — a property another kind accepts is not accepted here.",
	DiagMissingProperty:     "Add the property the message names.",
	DiagDuplicateBlock:      "Keep one `vars:` / `presets:` / `attachments:` / `secrets:` block per file and merge the entries into it.",
	DiagInvalidValue:        "Use one of the accepted values the message lists.",
	DiagInvalidType:         "Types are `string`, `bool`, `int`, `float`, `json` and `string[]` (a schema field may also be `file`).",
	DiagDuplicateEdgeClause: "Each of `when`/`else`, `as` and `with` may appear once per edge.",
	DiagElseWithWhen:        "An edge is either guarded (`when`) or the fallback (`else`), never both.",
}

// HintFor returns the one-line remedy for a parse code, or "" when none is
// catalogued.
func HintFor(code DiagCode) string {
	return hints[code]
}

// Severity indicates the severity of a diagnostic.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Diagnostic represents a positioned parse error or warning.
type Diagnostic struct {
	Code     DiagCode
	Severity Severity
	Message  string
	File     string
	Line     int    // 1-based
	Column   int    // 1-based
	Hint     string // one-line remedy, from HintFor unless the site set a more specific one
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s [%s]: %s", d.File, d.Line, d.Column, d.Severity, d.Code, d.Message)
}
