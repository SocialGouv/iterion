package parser

import "fmt"

// DiagCode identifies the kind of parse diagnostic.
type DiagCode string

const (
	// Structural errors
	DiagUnexpectedToken DiagCode = "E001" // unexpected token
	DiagExpectedToken   DiagCode = "E002" // expected specific token
	DiagBadIndentation  DiagCode = "E003" // indentation mismatch (tabs, misaligned dedent, nesting depth)
	DiagUnterminatedStr DiagCode = "E004" // unterminated string literal
	DiagBadEscape       DiagCode = "E005" // unknown escape sequence in a quoted string (strict-escape mode)

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

	// Header errors (the `dsl: N` syntax profile, ADR-098)
	DiagUnknownProfile     DiagCode = "E040" // the header names a profile this build does not read, or is not a positive integer
	DiagMisplacedHeader    DiagCode = "E041" // the header is not the first declaration, or appears twice
	DiagDirectiveInProfile DiagCode = "E042" // the profile-1 strict-escape directive in a file of profile 2 or later
	DiagRemovedInProfile   DiagCode = "E043" // a property the file's profile removed (`project_root:` from profile 2)
)

// hints is the one-line remedy each parse code arrives with. A parse error
// is positioned to the token, so the hint names the shape the parser wanted
// there rather than repeating the message.
var hints = map[DiagCode]string{
	DiagUnexpectedToken:     "Inside a block every line is `key: value` (or `src -> dst` inside `workflow:`); check this line's indentation and that its block is still open.",
	DiagExpectedToken:       "Give this position the shape the parser wanted: a bare name for a prompt/schema/node reference, a quoted string for a value, an indented block under a header, an inline `[a, b]` list.",
	DiagBadIndentation:      "Indent with spaces only, by the same width at every level, and align the line with an enclosing block.",
	DiagUnterminatedStr:     "Close the quote, or use a backtick raw string / a `|` block scalar for multi-line content.",
	DiagBadEscape:           "In strict-escape mode a backslash only escapes `\\\"`, `\\\\`, `\\n`, `\\t`, `\\r` and `\\0`: double the backslash for a literal one, or move the text to a backtick raw string / a `|` block scalar.",
	DiagDuplicateDecl:       "Rename one of the two declarations.",
	DiagReservedName:        "`done` and `fail` are the reserved terminal targets; pick another name.",
	DiagUnknownProperty:     "Check the property table for this node kind in docs/references/dsl-grammar.md — a property another kind accepts is not accepted here.",
	DiagMissingProperty:     "Add the property the message names.",
	DiagDuplicateBlock:      "Keep one `vars:` / `presets:` / `attachments:` / `secrets:` block per file and merge the entries into it.",
	DiagInvalidValue:        "Use one of the accepted values the message lists.",
	DiagInvalidType:         "Types are `string`, `bool`, `int`, `float`, `json` and `string[]` (a schema field may also be `file`).",
	DiagDuplicateEdgeClause: "Each of `when`/`else`, `as` and `with` may appear once per edge.",
	DiagElseWithWhen:        "An edge is either guarded (`when`) or the fallback (`else`), never both.",
	DiagUnknownProfile:      "Write `dsl: 2`, or omit the header for profile 1. A file written for a newer profile needs a newer engine: keep it off older builds with `requires: { iterion: \">= <version>\" }` in the bundle manifest.",
	DiagMisplacedHeader:     "Move the `dsl:` line above every declaration — after the leading comments, before the first block or node — and keep a single one.",
	DiagDirectiveInProfile:  "Profile 2 reads standard escapes in every quoted string by default: delete the `strict-escape` directive (a backslash that must stay literal is written `\\\\`).",
	DiagRemovedInProfile:    "Keep the file in profile 1 (drop the `dsl: 2` header), or redesign the memory scope: `visibility:` is a different axis (C171), not a drop-in replacement for `project_root:`.",
}

// HintFor returns the one-line remedy for a parse code, or "" when none is
// catalogued.
func HintFor(code DiagCode) string {
	return hints[code]
}

// expectedTokenHint names the shape the parser wanted at a token, so an
// "expected X, got Y" arrives with the remedy for THAT shape rather than the
// generic E002 line. The most common miss is the inverse of the generic
// advice: a quoted string where a bare reference name belongs
// (`system: "Review the diff"` — the author meant to declare a prompt).
func expectedTokenHint(want, got TokenType) string {
	switch want {
	case TokenIdent:
		if got == TokenString {
			return "This property takes a bare name — a declared `prompt`, `schema` or node — not a quoted string. For `system:`/`user:` declare the text as a prompt (`prompt my_prompt:` with the text below it, then `system: my_prompt`); for `entry:`/`input:`/`output:` just remove the quotes (`entry: a`)."
		}
		return "This property takes a bare name (letters, digits, `_`), such as a declared prompt, schema or node."
	case TokenString:
		if got == TokenIdent || got == TokenInt || got == TokenFloat {
			return "Quote this value (`backend: \"claw\"`, `timeout: \"20m\"`) — an unquoted word is read as an identifier."
		}
		return "This property takes a quoted string (or a backtick raw string, or a `|` block scalar)."
	case TokenInt:
		return "This property takes an unquoted integer literal."
	case TokenIndent:
		return "Open an indented block under this header: its properties (or edges) go on the following lines, indented by two spaces."
	case TokenColon:
		return "Write `key: value` — a colon right after the property name — or `src -> dst` for an edge."
	case TokenLBrack:
		return "This property takes an inline list on one line: `[a, b]`."
	case TokenRBrack:
		return "Close the list with `]`: comma-separated elements on ONE line, no `- item` lines."
	case TokenArrow:
		return "An edge is `src -> dst`, optionally followed by `when …`, `else`, `as name(N)` or `with { … }`."
	case TokenNewline:
		return "One property or edge per line; nothing may follow the value on the same line except a comment."
	}
	return ""
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
