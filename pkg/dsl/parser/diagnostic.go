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
	DiagClauseBeforeArrow   DiagCode = "E032" // a clause in the middle of a chain `a -> b when x -> c`

	// Header errors (the `dsl: N` syntax profile, ADR-098)
	DiagUnknownProfile     DiagCode = "E040" // the header names a profile this build does not read, or is not a positive integer
	DiagMisplacedHeader    DiagCode = "E041" // the header is not the first declaration, or appears twice
	DiagDirectiveInProfile DiagCode = "E042" // the profile-1 strict-escape directive in a file of profile 2 or later
	DiagRemovedInProfile   DiagCode = "E043" // a property the file's profile removed (`project_root:` from profile 2)

	// Import errors (the multi-file unit, ADR-098 §3)
	DiagMisplacedImport  DiagCode = "E044" // an `import` after the file's first declaration
	DiagBadImportPath    DiagCode = "E045" // an import path that is not a quoted, relative, slash-separated `.bot` path into lib/
	DiagImportUnreadable DiagCode = "E046" // an imported fragment that cannot be read: missing, or beyond what the unit may read
	DiagImportCycle      DiagCode = "E047" // a fragment that imports itself, through however many files

	// Author-document errors (the YAML twin of a .bot, pkg/dsl/author)
	DiagAuthorDocument      DiagCode = "E050" // the YAML document itself is refused: not exactly one document, an anchor, an alias, a merge key or an explicit tag, a duplicate or non-string key, too deep or too large
	DiagAuthorValue         DiagCode = "E051" // a value that is not the shape its property or part takes, or one the .bot cannot write: a negative or non-finite number, a float where an integer, a word where a bool, a key the document's top level does not have
	DiagAuthorHeader        DiagCode = "E052" // no `dsl:` key — the author document names its syntax profile, always
	DiagAuthorPromptBody    DiagCode = "E053" // a text the .bot reads otherwise than the document wrote it — a prompt body the lexer settles, a block scalar holding a line separator: a warning names what changed, an error what has no written form
	DiagAuthorNoWrittenForm DiagCode = "E054" // the document reads, but the program it describes has no written .bot form: the text the writer produces reads back as another program (unparse.Verify names the cause)
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
	DiagClauseBeforeArrow:   "In a chain `a -> b -> c …` the clauses apply to the LAST segment only; to guard, loop or map an earlier one, write that segment as its own edge line.",
	DiagUnknownProfile:      "Write `dsl: 2`, or omit the header for profile 1. A file written for a newer profile needs a newer engine: keep it off older builds with `requires: { iterion: \">= <version>\" }` in the bundle manifest.",
	DiagMisplacedHeader:     "Move the `dsl:` line above every import and every declaration — after the leading comments, before the first `import`, block or node — and keep a single one.",
	DiagDirectiveInProfile:  "Profile 2 reads standard escapes in every quoted string by default: delete the `strict-escape` directive (a backslash that must stay literal is written `\\\\`).",
	DiagRemovedInProfile:    "Keep the file in profile 1 (drop the `dsl: 2` header), or redesign the memory scope: `visibility:` is a different axis (C171), not a drop-in replacement for `project_root:`.",
	DiagMisplacedImport:     "Move the `import` lines to the head of the file — after the `dsl:` header and the leading comments, before the first block or node.",
	DiagBadImportPath:       "Write `import \"lib/<name>.bot\"`: a quoted, relative, slash-separated path to a `.bot` fragment under the bot's `lib/` directory, one import per line.",
	DiagImportUnreadable:    "Create the fragment under the bot's `lib/` directory, or fix the path; a symlink, an absolute path or a path leaving the bot's directory is never read.",
	DiagImportCycle:         "A fragment may not import a file that imports it back: move the shared declarations into a third fragment both import.",
	DiagAuthorDocument:      "Write one plain YAML document: no `---` document separator, no anchor (`&a`) or alias (`*a`), no `<<` merge key, no explicit `!!tag`, every key once per mapping and written as a plain word.",
	DiagAuthorValue:         "Give the value the shape the property's form takes (docs/references/dsl-properties.md): a string, an integer as digits, `true`/`false`, a list as `[a, b]`, a block as an indented mapping. The .bot has no signed number, no exponent and no `.inf`, so neither has its YAML twin.",
	DiagAuthorHeader:        "Add `dsl: 2` (or `dsl: 1`) at the top of the document: the syntax profile the .bot is written in. The author document never guesses one.",
	DiagAuthorNoWrittenForm: "The .bot this document describes cannot be written so that it reads back as the same program; the message names the cause — a prompt body the .bot syntax cannot hold, a fallback route without a name, a profile-1 catalog long enough to push the strict-escape directive out of the lexer's window while a value needs it. Change what it names (`dsl: 2`, a shorter catalog, a named route, a reindented body): the written .bot would otherwise mean another program.",
	DiagAuthorPromptBody:    "A prompt's text is read as the .bot lexer reads a body: leading and trailing blank lines dropped, the first line's indentation taken off every line, interior blank lines dropped in profile 1. A block scalar's line separator (U+2028, U+2029) is read as the line break the scanner meant, in a literal block; a folded block cannot say it. Write the text as it will be read, or accept the reading.",
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
			return "This property takes a bare name — a declared `schema` or node — not a quoted string: remove the quotes (`entry: a`, `output: verdict`)."
		}
		return "This property takes a bare name (letters, digits, `_`), such as a declared prompt, schema or node."
	case TokenString:
		if got == TokenInt || got == TokenFloat {
			return "Quote this value (`timeout: \"20m\"`, `model: \"gpt-5.5\"`) — a bare value is only read when it is one plain word."
		}
		return "This property takes a string: quoted, a backtick raw string, a `|` block scalar, or one plain word (`backend: claw`)."
	case TokenInt:
		return "This property takes an unquoted integer literal."
	case TokenIndent:
		return "Open an indented block under this header: its properties (or edges) go on the following lines, indented by two spaces."
	case TokenColon:
		return "Write `key: value` — a colon right after the property name — or `src -> dst` for an edge."
	case TokenLBrack:
		return "This property takes a list: an inline list on the property's line (`[a, b]`), or one `- item` per line indented below it."
	case TokenRBrack:
		return "Close the inline list with `]` (comma-separated elements on ONE line), or write the list as `- item` lines indented below the property."
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
