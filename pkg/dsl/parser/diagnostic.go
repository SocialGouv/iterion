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
	Line     int // 1-based
	Column   int // 1-based
}

func (d Diagnostic) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s [%s]: %s", d.File, d.Line, d.Column, d.Severity, d.Code, d.Message)
}
