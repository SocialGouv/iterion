package author

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The bounds of a document the converter reads, the twins of the `.bot`
// lexer's own (10 MiB of source, 100 levels of nesting): a YAML file is not
// a way around them. The node count bounds the work a small file can ask
// for (a billion-laughs document has few bytes and many nodes) — anchors
// and aliases are refused outright, so the count is the honest one.
const (
	maxSourceSize   = 10 * 1024 * 1024
	maxNestingDepth = 100
	maxNodes        = 1 << 20
)

// yamlLineRe reads the line number yaml.v3 writes into its own errors
// ("yaml: line 3: mapping values are not allowed in this context").
var yamlLineRe = regexp.MustCompile(`line (\d+):`)

// yamlLineIn is the line number msg names in yaml.v3's own words, 0 where
// it names none — the one reading of that number, so that no refusal names
// one line in its text and another in its position (an editor squiggle the
// author's mistake is not under).
func yamlLineIn(msg string) int {
	m := yamlLineRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0
	}
	return n
}

// yamlErrorLine is the line yaml.v3 names in err's own words, 0 where it
// names none: a decode that failed leaves its node unfilled, so the error
// is the only thing that knows where the refusal belongs.
func yamlErrorLine(err error) int { return yamlLineIn(err.Error()) }

// yamlErrorAtLine is msg with the line number yaml.v3 writes into its own
// words set to line, msg itself where it names none: a refusal names one
// line, the one it lands on, so the author reads the same number in the
// text and under the squiggle. Called where the refusal's position comes
// from the document's own node rather than from the error, the only
// reading that can disagree with yaml's number.
func yamlErrorAtLine(msg string, line int) string {
	loc := yamlLineRe.FindStringSubmatchIndex(msg)
	if loc == nil {
		return msg
	}
	return msg[:loc[2]] + strconv.Itoa(line) + msg[loc[3]:]
}

// yamlNoDocumentStart is the one yaml.v3 message whose remedy names the
// FIRST line of the file as a thing to edit — a line that is a whole value
// of its own is a whole document on its own. The entry of yamlRemedies that
// answers it and the position override of decode read the same constant, so
// neither can name the message the other does not.
const yamlNoDocumentStart = "did not find expected <document start>"

// documentEndIndicatorLine reports whether line n of src is a `...` closing
// the document — the first thing the remedy of yamlNoDocumentStart tells the
// author to remove, so the refusal belongs on it. yaml.v3's own reading: the
// three dots start the line and nothing but a blank follows them (its
// scanner asks is_blankz), so an indented `...` is text inside a value (the
// body of a `|` block) and `...x` is a plain value — neither is a line the
// author must take out.
func documentEndIndicatorLine(src []byte, n int) bool {
	lines := sourceLines(src)
	if n < 1 || n > len(lines) {
		return false
	}
	rest, cut := strings.CutPrefix(lines[n-1], "...")
	return cut && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// documentEndLine is the first line of src between from and to that closes
// the document, 0 where none does. The question is asked of the range and
// not of the one line yaml.v3 names: the line it names is the one before
// the second document's first token, so a comment or a blank line after the
// `...` moves it off the indicator, and the line the remedy names first
// would appear nowhere in the refusal. from and to need no bound of their
// own: documentEndIndicatorLine answers false for a line outside src.
func documentEndLine(src []byte, from, to int) int {
	for n := from; n <= to; n++ {
		if documentEndIndicatorLine(src, n) {
			return n
		}
	}
	return 0
}

// decode reads exactly one YAML document off src and returns its root
// node, or the diagnostics that refuse it: a source too large, text that
// is not YAML, no document at all, a second document (a `---` separator
// among them), and — on the tree — every construct guardTree refuses.
// When the document reads, the second return carries the guard's warnings,
// never refusals.
func decode(name string, src []byte) (*yaml.Node, []parser.Diagnostic) {
	if len(src) > maxSourceSize {
		return nil, []parser.Diagnostic{docDiag(name, 1, 1, fmt.Sprintf("the document exceeds the maximum size (%d bytes > %d)", len(src), maxSourceSize))}
	}
	// yaml.v3 also reads UTF-16; every reader of the source at a node's
	// position reads UTF-8 (sourceLines), as the .bot it stands for is.
	if at := invalidUTF8(src); at >= 0 {
		return nil, []parser.Diagnostic{docDiag(name, len(sourceLines(src[:at])), 1, fmt.Sprintf("the document is not UTF-8 text (byte %d): write it in UTF-8, as the .bot it stands for is", at))}
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}
		}
		line := 1
		if n := yamlErrorLine(err); n > 0 {
			line = n
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, "not YAML: "+err.Error()+YAMLRemedy(err.Error()))}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		line := second.Line
		if len(second.Content) > 0 && second.Content[0].Line > 0 {
			line = second.Content[0].Line
		}
		if err != nil {
			if n := yamlErrorLine(err); n > 0 {
				line = n
			}
			// yaml.v3 names the line BEFORE the token that opened the
			// second document (its fail() adds 1 to the 0-based mark for a
			// scanner error only), which a comment or a blank line between
			// the two makes a line the author must not touch. For
			// yamlNoDocumentStart alone the source holds the line its remedy
			// names, in the order the remedy names them: the `...` that
			// closed the document, read off the lines between the first
			// document and the line yaml named; then, where the first
			// document is a whole value of its own rather than the
			// `key: value` mapping an author document is — a plain or quoted
			// value, a `|`/`>` block, a `[…]` or a `{…}` — the line of that
			// value, which the node decode filled holds. Any other message
			// is the second document's own syntax, whose remedy names that
			// document's spelling: yaml's line stands.
			if root := doc.Content[0]; root.Line > 0 && strings.Contains(err.Error(), yamlNoDocumentStart) {
				valueRoot := root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0
				switch end := documentEndLine(src, root.Line, line); {
				case end > 0:
					line = end
				case valueRoot:
					line = root.Line
				}
			}
		}
		if line == 0 {
			line = 1
		}
		msg := "one document per file: a second document follows"
		if err != nil {
			msg += ", and it is not YAML: " + yamlErrorAtLine(err.Error(), line) + YAMLRemedy(err.Error())
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, msg)}
	}
	root := doc.Content[0]
	errs, warns := guardTree(name, root, src)
	if len(errs) > 0 {
		return nil, errs
	}
	// The document reads: the guard's warnings ride back beside the
	// converter's and the parser's (a warning never refuses), and the
	// invisible line separators it holds get their one document-level
	// notice (#1663).
	if n := separatorNotice(name, src); n != nil {
		warns = append(warns, *n)
	}
	return root, warns
}

// yamlContextProblems are the problem strings a YAML parser raises with a
// context mark — the mark of the value it had begun reading — beside the
// mark of the token that broke (yaml.v3 parserc.go:538, 678, 733, 873, 949,
// 1086). Neither mark is the line to edit: both are 0-based and only a
// scanner error is incremented, so the number written is at best the line
// above the token that broke. Which mark wins is the reader's own choice
// and the two vendored readers disagree — yaml.v3's fail() prefers the
// context mark (decode.go:112-126), yaml.v2's the problem's
// (go.yaml.in/yaml/v2/decode.go:111-122) — so the gap is one line for one
// of them and the whole header for the other: measured on yaml.v3, a
// document opening with a comment moves the root mapping off line 1 and the
// refusal lands four lines above the token, on the comment. Every author
// document of the conformance corpus opens with a comment. Neither
// direction holds on its own: a context mark on the file's first line is 0,
// which sends yaml.v3 to the problem mark too, and the construct then
// starts ABOVE the line named — `- a` / `- b` / `dsl: 2` is refused on line
// 2 while the list the remedy says to delete whole starts on line 1. All
// the remedy claims is the construct's reach: it can start above the line
// named and end below it.
var yamlContextProblems = []string{
	"did not find expected key",
	"did not find expected '-' indicator",
	"did not find expected ',' or ']'",
	"did not find expected ',' or '}'",
	"did not find expected node content",
	"found undefined tag handle",
}

// yamlReadsFromAbove heads the remedy of a yamlContextProblems message:
// without it the clauses that follow — each telling the author to delete or
// move "that line" — name the line of the squiggle, which for these
// messages is a line the mistake need not be on. It names the construct's
// reach rather than a direction: both are measured, the value starting
// above the line named and ending below it.
const yamlReadsFromAbove = "the line this refusal names is not always the line to edit: the value YAML was reading can start above it and end below it — read the whole construct, not this line alone"

// strayLineSpans tails every clause that tells the author to delete or move
// a line standing where a `key: value` pair belongs: a value is not always
// one line, and a line of one left behind is refused again — measured, a
// two-line title, a two-line quoted value, a `[…]` broken over two lines, a
// `|`/`>` block and a `- item` list each cost a second refusal when one
// line of them goes. The move is prescribed with its indentation for the
// same reason: the lines a key takes are the ones indented under it, so the
// flat spelling of a moved two-line value is refused with the message the
// author just read.
const strayLineSpans = " — a value can span several lines: a plain or quoted value runs on into the lines under it, a `|`/`>` block carries the lines indented under it, a `[…]` or `{…}` runs to its closing bracket, a list carries one line per `- item` and an item runs on into the lines indented under it: delete them all, or move them all under one key, with the lines below that key indented under it"

// yamlRemedies pairs the problem strings yaml.v3's scanner, parser, reader
// and composer build with the spelling that avoids them: yaml.v3's own
// message says what it saw, never what to write. Measured on the authoring
// probe: a shell command holding a `: ` (a jq filter) written unquoted cost
// a mid-size model four validation rounds, the message repeating itself. A
// message given for several mistakes names each: the line it reports does
// not tell them apart (a quoted value that ends early is reported at the
// start of its mapping).
var yamlRemedies = []struct {
	// messages are the yaml.v3 problem strings this remedy answers.
	messages []string
	remedy   string
}{
	{
		messages: []string{"mapping values are not allowed"},
		remedy:   "a plain value holding a `:` followed by a space or by the end of the line reads as a nested mapping: quote the whole value (a command with a jq filter, an expression, an edge whose with map has a `: `, a text ending in `:`), a `'` inside single quotes written twice (`''`); or a key is indented deeper than the keys beside it: dedent every key that is, to their column — one left behind is refused again; or a line above this one is a plain value where a `key: value` pair was expected (a title, a note): a plain value runs on into the lines below it, so the first `: ` it meets is refused here — delete that line, or write it as `key: value`" + strayLineSpans + "; or a `%YAML`/`%TAG` directive line heads the document, where the author document takes none",
	},
	{
		messages: []string{"cannot start any token"},
		remedy:   "the value starts with a character YAML reserves (`%`, `@`, a backtick): quote the whole value; or a tab indents the line, where YAML takes spaces only",
	},
	{
		messages: []string{"found a tab character"},
		remedy:   "YAML indents with spaces only: replace the tab",
	},
	{
		messages: []string{"did not find expected key", "did not find expected '-' indicator"},
		remedy:   "a value that starts with `{` or `[` (a `{{…}}` template, a `[WIP]`) is read by YAML as a mapping or a list, and a value with quotes of its own, written plain or inside double quotes, ends where YAML reads its quote as closed (an edge whose with map holds a quoted value): write either whole in single quotes, its double quotes as they are and a `'` inside written twice (`''`); or indentation: the keys of one mapping start at one column, a value's lines are indented under its key, a list's items under theirs; or a `|N`/`>N` indicator claims more spaces than the body has — the digit counts from the key's own indentation, so YAML reads none of the body: write `|` alone and let the body's indentation say it; or a line is a whole value of its own where a `key: value` pair was expected — a `- item` line reads as an item of the document itself, a `|`/`>` block stands where a key belongs: delete that line, or indent it under the key it belongs to" + strayLineSpans,
	},
	{
		// yaml.v3's scanner raises this message for a required simple key
		// more than 1024 characters long as well. That cause is left
		// unnamed: no key of the author schema is that long, and quoting
		// the key does not avoid it — the bound counts characters, quoted
		// or not, so the only spelling that avoids it is a shorter key.
		messages: []string{"could not find expected ':'"},
		remedy:   "a key is followed by `: ` (colon, space) and its value; or a `|` or `>` block's body is not indented under its key, which leaves its first line read as a key of its own; or a list's item is written without its `- `, under a key whose other items have one; or this line is a whole value of its own where a `key: value` pair was expected (a title, a note, a line pasted in): delete it, or write it as `key: value`" + strayLineSpans,
	},
	{
		messages: []string{"found unexpected end of stream", "found unexpected document indicator"},
		remedy:   "a quoted value or a block was left open, or a `---` or `...` line sits inside the document: one document, every quote closed — a `---` and a `...` are read as document indicators where they start a line, so indent it where it belongs to a value",
	},
	{
		// Both readings reach these two: the scanner raises the first, and
		// an alias naming no anchor is refused while the reader looks the
		// name up — yaml.v3 does that filling the document's node tree as
		// yaml.v2 does filling the catalog identity's Go fields (the
		// witnesses "an alias with no anchor" and "an alias naming no
		// anchor"). What the document's reader does NOT do is expand the
		// alias it found, which is the entry below.
		messages: []string{"did not find expected alphabetic or numeric character", "unknown anchor"},
		remedy:   "`&` opens a YAML anchor and `*` an alias, each named right after its sigil: where the sigil is part of the text (a `[ -f x ] && …` command, a `rm *.tmp`), the value goes whole in single quotes and reads as that text; where an alias was meant, it names an anchor written above it — the author document reads neither, and refuses an anchor and an alias with its own words: a value is written where it is used; a `.bot` frontmatter block resolves both",
	},
	{
		// The two refusals of the alias GRAPH, which the frontmatter
		// block's reading alone can reach: it expands the aliases into the
		// catalog identity's Go fields, where the author document's reader
		// copies the node tree and expands none. Their cause is neither a
		// sigil written in text nor an alias naming no anchor — the entry
		// above answers those — so a remedy prescribing single quotes or
		// an anchor written above says nothing to their author: both
		// documents name their anchor correctly, and the refusal is what
		// the resolving did with it.
		messages: []string{"document contains excessive aliasing", "value contains itself"},
		remedy:   "a `.bot` frontmatter block resolves its anchors and aliases, and this graph it cannot: an anchor whose value holds its own alias (`x: &a {b: *a}`) is a cycle the resolving has no end to, and a chain of aliases each naming the one above it expands into far more values than the block's own lines — write the keys out where they are used; the author document reads no anchor and no alias, and refuses each with its own words",
	},
	{
		messages: []string{"did not find expected comment or line break"},
		remedy:   "`|` and `>` take nothing but a comment after them on the key's line: write `|` alone and the text on the lines under it, indented — or, where the `|` is part of the value (a shell pipe), quote the whole value; a `%YAML`/`%TAG` directive line carrying trailing text gives the same message",
	},
	{
		messages: []string{"found an indentation indicator equal to 0"},
		remedy:   "the digit after `|` or `>` is how many spaces the body is indented by, and YAML counts it from 1: write `|` alone and let the indentation of the lines under the key say it",
	},
	{
		messages: []string{"block sequence entries are not allowed in this context"},
		remedy:   "a list's items each start a line of their own, indented under their key (`imports:`, then `  - lib/x.bot`): a `- ` on the key's line or following a value is not an item — write the list as `[a, b]` to keep it on one line, or quote the whole value where the `- ` is part of it",
	},
	{
		// `invalid map key: ` is yaml.v2's: a frontmatter block's keys
		// beyond the four are read into a Go map, where a key that is
		// itself a list or a mapping has no place.
		messages: []string{"mapping keys are not allowed in this context", "invalid map key: "},
		remedy:   "a value starting with `? ` (a question, a `?` flag) reads as YAML's explicit-key indicator: write the whole value in single quotes; and a `? ` key whose text is a list or a mapping is no key at all — write the key as plain text",
	},
	{
		// yaml.v2 resolves a merge key while reading a frontmatter block
		// into the catalog identity's Go fields. The author document's own
		// reader never resolves one — guardTree refuses `<<` with its own
		// words — so no author document reaches this message.
		messages: []string{"map merge requires map or sequence of maps as the value"},
		remedy:   "`<<` is YAML's merge key, which takes a mapping or a list of mappings: write the keys out, or quote the whole value where the `<<` is part of it — the author document takes none, and refuses a merge key with its own words; a `.bot` frontmatter block merges the keys in, as keys of its own",
	},
	{
		// The shapes the frontmatter block's reading reads, raised by that
		// reading alone: it decodes into workflowfile.Frontmatter, where
		// `name` and `description` are strings and `triggers` and
		// `capabilities` slices, while the author document's reader fills a
		// *yaml.Node and converts nothing. ONE message string, four causes:
		// they differ by the Go type the reading was decoding into — the
		// whole block where its own top level is not `key: value` lines, a
		// `string` for a key's name, for a one-line value and for a list's
		// item, a `[]string` for a list key — and the remedy names all four
		// rather than keying on that type, which stands behind a value the
		// reader quotes and which the quoted-value spans can eat. yaml.v2
		// builds the message inline (decode.go:265), not through an error
		// constructor, so the problem-string scan cannot read it:
		// frontmatterWitnesses is what holds this family, one witness per
		// Go type the reading can be asked to decode a node into.
		messages: []string{"cannot unmarshal "},
		remedy:   "a `.bot` frontmatter block is `key: value` lines, and the four keys it carries each take one shape: `name` and `description` one line of text, `triggers` and `capabilities` a list — `[a, b]` on one line, or one `- item` per line under the key, every item one line of text; a value of another shape is not read — a block whose own top level is not `key: value` lines (a line of prose, a `- item` list, a `[…]`, a `|` or `>` block, a bare number, `true`) is one whole value and no key at all: write it as `key: value` (`description: a bot that reviews pull requests`); and a key is plain text, so a `? ` key whose text is a list or a mapping is no key at all — write the key as plain text",
	},
	{
		messages: []string{"found unknown escape character", "did not find expected hexdecimal number", "found invalid Unicode character escape code"},
		remedy:   "inside double quotes a `\\` opens an escape, and few characters may follow it (`\\n`, `\\t`, `\\\\`, `\\xNN`, `\\uNNNN`): write the value in single quotes, where a `\\` stands for itself (a regex `\\d`, a Windows path `C:\\temp`), or double every `\\`",
	},
	{
		messages: []string{"did not find expected ',' or ']'", "did not find expected ',' or '}'"},
		remedy:   "inside `[…]` and `{…}` a `,` separates the items, and an item holding a `,`, a `]`, a `}`, a `:` or a `?` of its own goes in single quotes: close the `[` or `{` the value opened, add the missing `,`, or drop the brackets — one `- item` per line for a list, one `key: value` line per entry for a mapping",
	},
	{
		messages: []string{"did not find expected node content"},
		remedy:   "a `- item` line inside a `[…]` is not a list item: either close the `[…]` and separate its items with `,`, or drop the brackets — one `- item` per line for a list, one `key: value` line per entry for a mapping; a `,` with no item before it has no value either; a closing `]` or `}` that opens nothing is no value: delete it and write one, or quote the value it is part of; and a `---` or `...` line ends the document where a value was expected: remove it, or indent it under its key where it belongs to a value",
	},
	{
		messages: []string{"did not find expected whitespace or line break", "did not find expected '!'"},
		remedy:   "a value starting with `!` reads as a YAML tag and a line starting with `%` as a `%TAG` directive: quote the whole value, or remove the directive line — the author document carries neither",
	},
	{
		// The clause about the block's reading is scoped by the KEY, as
		// the shape entry is: the reading resolves the tag, and the value
		// it then holds is taken where the key's own shape fits it —
		// measured, `name: !!int 3` reads as the name `3` and
		// `triggers: !!int 3` is refused again, into []string. A clause
		// telling the author of a list key to write the value the tag
		// names sent them to that second refusal.
		//
		// The last two are yaml.v2's, raised while resolving the tag of a
		// frontmatter block's value into a Go field. " as a " is the part
		// of `cannot decode %s `%s` as a %s` that stands in both the
		// vendored format string and the message it writes, so one entry
		// answers the literal the problem-string scan reads and the words
		// an author is shown — the value the reader quotes between the two
		// is the author's own text, which YAMLRemedy takes out of the match.
		messages: []string{"did not find the expected '>'", "did not find expected tag URI", "did not find URI escaped octet", "found an incorrect leading UTF-8 octet", "found an incorrect trailing UTF-8 octet", "found undefined tag handle", "!!binary value contains invalid base64 data", " as a "},
		remedy:   "a value starting with `!` reads as a YAML tag (`!x`, `!!str`, `!<…>`): write it whole in single quotes — a `! grep -q x f` command, a `!` negation — a value's shape decides its type; the author document carries none, and refuses an explicit tag with its own words, while a `.bot` frontmatter block reads the tag: `name` and `description` then take the value the tag names (a `!!binary` value base64, a `!!int` value a number), and so does an item of `triggers` or `capabilities`, whose key itself still takes a list",
	},
	{
		messages: []string{"found unknown directive name", "could not find expected directive name", "found unexpected non-alphabetical character", "did not find expected digit or '.' character", "found extremely long version number", "did not find expected version number", "did not find expected whitespace", "found duplicate %YAML directive", "found duplicate %TAG directive", "found incompatible YAML document"},
		remedy:   "a line starting with `%` is read as a YAML directive (`%YAML`, `%TAG`), which the author document never needs: remove the line, or — where the text belongs to a value — indent it under its key and quote the value",
	},
	{
		messages: []string{yamlNoDocumentStart},
		remedy:   "a document closed before this line and what follows it reads as a second one, which would need its own `---`, where the author document is one: a `...` line closes the document — remove it, or indent it under its key where it belongs to a value; a `%YAML`/`%TAG` directive line with no `---` after it reads the same way, and the author document takes no directive — remove it; a first line that is a whole value of its own rather than a `key: value` pair — a plain value, a line in quotes, a `[…]` or a `{…}`, a `|` or `>` block — is a whole document on its own: delete that line, write it as `key: value`, or unquote it; and the document opens with an indented block of `key: value` lines, which is a document of its own that the first line below it at a shallower column closes: give the whole document one indentation — every key of its top level at one column, each key's own lines moving with it: dedent them all to the shallowest column the document holds, or indent them all to the block's" + strayLineSpans,
	},
	{
		messages: []string{"exceeded max depth of"},
		remedy:   "the document nests deeper than YAML itself reads (10000 levels of `[`, `{` or indentation): flatten it",
	},
	{
		messages: []string{"control characters are not allowed"},
		remedy:   "the document holds a control character (an ESC from a captured terminal output, a NUL byte): remove it, or write it inside double quotes as an escape (`\\u001b`)",
	},
}

// yamlValueSlots are the three places a reader writes an author's own text
// into its own message, each bounded by the words the format itself writes
// and never by a delimiter that text can hold: `cannot unmarshal %s%s into
// %s` (go.yaml.in/yaml/v2/decode.go:265), `cannot decode %s `%s` as a %s`
// (resolve.go:109) and `invalid map key: %#v` (decode.go:673). Matched
// against the remedy table, an author's text answers for the message:
// measured, a `triggers` value spelling ` as a !!` took the tag remedy, a
// tagged value spelling `mapping values are not allowed` took the colon
// remedy of a message no reader had raised, and one backtick inside such a
// value split a span paired delimiter to delimiter — the tail then chose
// the remedy AND spelled a `line N:` no reader wrote.
//
// A slot runs from the EARLIEST opening left in the message: an author's
// text stands after an opening, never before one. `cannot unmarshal ` is
// the one format a refusal collects several of (one yaml.v2 TypeError
// element per value it cannot read), so its slot ends at the LAST ` into `
// before the next opening — that opening is 17 characters and the decoder
// renders at most 10 of a value (decode.go:253-265), so no value can spell
// a second element and move the bound. The other two are raised by panic,
// one message per refusal: the first ends at the LAST ` as a ` of the
// message, what follows it being the tag the reader names, and the second
// runs to the end, the format writing nothing after the key.
//
// The author's text no slot bounds is the name of an anchor
// (`unknown anchor '%s' referenced`, `anchor '%s' value contains itself`),
// which holds no space, where every message of the table does
// (TestNoRemedyMessageIsSpelledByTheTextNoSlotBounds).
var yamlValueSlots = []struct {
	// open and close are the reader's own words around the slot; close is
	// "" where the format writes nothing after the value.
	open, close string
	// repeat marks the format a refusal collects several of, whose slot
	// ends inside its own element.
	repeat bool
}{
	{open: "cannot unmarshal ", close: " into ", repeat: true},
	{open: "cannot decode ", close: " as a "},
	{open: "invalid map key: "},
}

// yamlFirstValueOpening is the slot of yamlValueSlots whose opening stands
// first in msg at or after from, with the index of that opening; -1 where
// the rest of msg holds none.
func yamlFirstValueOpening(msg string, from int) (slot, at int) {
	slot = -1
	for i, s := range yamlValueSlots {
		j := strings.Index(msg[from:], s.open)
		if j < 0 {
			continue
		}
		if j += from; slot < 0 || j < at {
			slot, at = i, j
		}
	}
	return slot, at
}

// yamlWithoutQuotedValues is msg with every value a reader wrote back into
// its own message taken out: the ONE reading of a reader's own words, so
// that no author's text chooses a remedy at one site and a line clause at
// another. An opening the format's own closing word does not follow is
// text rather than one of these messages, and is read past, not cut.
func yamlWithoutQuotedValues(msg string) string {
	var b strings.Builder
	cursor := 0
	for cursor < len(msg) {
		slot, at := yamlFirstValueOpening(msg, cursor)
		if slot < 0 {
			break
		}
		s := yamlValueSlots[slot]
		from := at + len(s.open)
		to := len(msg)
		if s.close != "" {
			rest := msg[from:]
			if s.repeat {
				if next := strings.Index(rest, s.open); next >= 0 {
					rest = rest[:next]
				}
			}
			end := strings.LastIndex(rest, s.close)
			if end < 0 {
				b.WriteString(msg[cursor:from])
				cursor = from
				continue
			}
			to = from + end
		}
		b.WriteString(msg[cursor:from])
		b.WriteString("``")
		cursor = to
	}
	b.WriteString(msg[cursor:])
	return b.String()
}

// YAMLRemedy is the remedy of the first entry of yamlRemedies one of whose
// messages msg holds, ready to follow yaml.v3's own words; "" when no entry
// answers msg. Exported for the other author-facing readings of a YAML
// error: the frontmatter block of a `.bot`, which `fmt` reads with the
// catalog reader's own decoder (yaml.v2 builds the same problem strings).
//
// The words that choose the remedy are the reader's own: a value the reader
// quoted is taken out of the match, never out of what the author is shown.
func YAMLRemedy(msg string) string {
	read := yamlWithoutQuotedValues(msg)
	for _, r := range yamlRemedies {
		for _, m := range r.messages {
			if strings.Contains(read, m) {
				return " — " + yamlPositionNote(read) + r.remedy
			}
		}
	}
	return ""
}

// YAMLBlockLineNote is the clause a refusal of a `.bot` frontmatter block
// owes its reader where the reader named a line of lines, the block's own
// text as parser.Frontmatter hands it to the reading. That number counts
// the lines the reading was GIVEN, which are not the file's lines with an
// offset: a run of blank lines inside the block stands for ONE line of the
// reading, and a `dsl:` or `import` line written between two of its comment
// lines for none, so no count from the fence is true of every file — and
// the fence itself is written `# ---` as often as `## ---`. The note quotes
// the line instead, which the author reads off their own file; a message
// naming several quotes each.
//
// "" where the reader named no line of its own, or named one the block does
// not hold: there is then no line to quote, and a clause hung on a number
// an author's own text spells would send them to a line nobody named. The
// numbers are read off the reader's words alone — a value the reader quoted
// is taken out first, as YAMLRemedy takes it out of its match.
func YAMLBlockLineNote(msg string, lines []string) string {
	seen := map[int]bool{}
	var named []int
	for _, m := range yamlLineRe.FindAllStringSubmatch(yamlWithoutQuotedValues(msg), -1) {
		n, convErr := strconv.Atoi(m[1])
		if convErr != nil || n < 1 || n > len(lines) || seen[n] {
			continue
		}
		seen[n] = true
		named = append(named, n)
	}
	if len(named) == 0 {
		return ""
	}
	if len(named) == 1 {
		return ", where the line it names is the block's own, not the file's — `" + lines[named[0]-1] + "`"
	}
	var b strings.Builder
	b.WriteString(", where the lines it names are the block's own, not the file's —")
	for i, n := range named {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" line " + strconv.Itoa(n) + " `" + lines[n-1] + "`")
	}
	return b.String()
}

// yamlPositionNote heads the remedy of msg where the line msg names is not
// the line of the mistake, "" otherwise. Read per message rather than per
// remedy entry: two entries answer a yamlContextProblems message beside
// scanner-raised ones, which do name the line that broke, so a clause
// carried by the entry would be false for them. The note has a referent
// only where yaml.v3 wrote a number into msg — a value at the very first
// line leaves both marks at 0 and the message carries none.
func yamlPositionNote(msg string) string {
	if yamlLineIn(msg) == 0 {
		return ""
	}
	for _, p := range yamlContextProblems {
		if strings.Contains(msg, p) {
			return yamlReadsFromAbove + "; "
		}
	}
	return ""
}

// guardTree walks the tree once and refuses what the converter never
// reads: an alias or an anchor (a value is written where it is used), an
// explicit tag (the value's shape decides its type; a `!!str 3` would say
// otherwise) — the non-specific tag `!` of a scalar included, which yaml.v3
// drops without marking the node (read off src: a `!` where the scalar
// starts) — a merge key, a key that is not a plain string, a key
// repeated in one mapping (yaml.v3 keeps both; the .bot has one), a tree
// deeper or larger than the lexer's bounds. It also warns on a `#` that
// starts a comment at a plain text value's tail — the one placement where
// the comment may be a value cut short — and carries the warnings back
// apart from the refusals: a warning never refuses a document.
func guardTree(name string, root *yaml.Node, src []byte) (diags, warns []parser.Diagnostic) {
	g := &guard{name: name, lines: sourceLines(src), rawLines: strings.Split(strings.TrimPrefix(string(src), "\ufeff"), "\n")}
	g.walk(root, 0)
	return g.diags, g.warns
}

// sourceLines cuts src into the lines yaml.v3's positions count: a UTF-8
// BOM off, as the scanner skips it, and a line ended where the scanner ends
// one (yamlBreaks) — every reader of the source at a node's position reads
// it through here.
func sourceLines(src []byte) []string {
	return strings.Split(yamlBreaks.Replace(strings.TrimPrefix(string(src), "\ufeff")), "\n")
}

// invalidUTF8 is the offset of src's first byte that is not UTF-8, -1 when
// every byte is.
func invalidUTF8(src []byte) int {
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}

type guard struct {
	name  string
	lines []string
	// rawLines is the source split on "\n" only: a raw CR, NEL, LS or PS
	// stays in its line's bytes, where sourceLines had folded it away.
	rawLines []string
	count    int
	diags    []parser.Diagnostic
	warns    []parser.Diagnostic
}

// at is the source from node n's position to the end of its line, "" when
// the position is not on a line of the source.
func (g *guard) at(n *yaml.Node) string {
	if n.Line < 1 || n.Line > len(g.lines) {
		return ""
	}
	line := []rune(g.lines[n.Line-1])
	if col := n.Column - 1; col >= 0 && col < len(line) {
		return string(line[col:])
	}
	return ""
}

// droppedBang reports whether YAML read a `!` at scalar n's position as a
// tag and dropped it, without a mark on the node — yaml.v3 marks a named
// tag, never the non-specific `!`. A scalar of any style: a quoted value
// starts with its quote, a block with `|` or `>`, a plain value never with
// `!` — so a `!` where the node starts is its tag. `! grep -q x f` would
// otherwise reach the .bot as `grep -q x f`, `! 'test -f x'` as the command
// it negates. (A collection's `!` changes nothing: its kind is its type.)
func (g *guard) droppedBang(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Style&yaml.TaggedStyle == 0 && strings.HasPrefix(g.at(n), "!")
}

// excerpt is v cut short for a message: its first line, 60 characters at most.
func excerpt(v string) string {
	v, _, _ = strings.Cut(v, "\n")
	if r := []rune(v); len(r) > 60 {
		return string(r[:60]) + "\u2026"
	}
	return v
}

func (g *guard) refuse(n *yaml.Node, msg string) {
	g.diags = append(g.diags, docDiag(g.name, n.Line, n.Column, msg))
}

// warn says what the document reads otherwise without refusing it: the
// author may have meant the reading. The message carries its own remedy —
// the code's generic hint (a prompt body's) would not fit every case.
func (g *guard) warn(n *yaml.Node, msg string) {
	line, col := n.Line, n.Column
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	g.warns = append(g.warns, parser.Diagnostic{
		Code: parser.DiagAuthorPromptBody, Severity: parser.SeverityWarning,
		Message: msg, File: g.name, Line: line, Column: col,
	})
}

// rawFoldBreak refuses a quoted scalar whose SOURCE line carries a raw
// CR or NEL byte inside the quotes — the two breaks yaml.v3 folds to a
// space, so the .bot writes the fold in the value's place and nothing
// else says so (#1814). An LS or a PS is kept as written: it reads back
// byte for byte, and stays the author's.
func (g *guard) rawFoldBreak(n *yaml.Node) {
	if n.Line < 1 || n.Line > len(g.rawLines) {
		return
	}
	line := strings.TrimSuffix(g.rawLines[n.Line-1], "\r")
	runes := []rune(line)
	if n.Column < 1 || n.Column-1 >= len(runes) {
		return
	}
	tail := string(runes[n.Column-1:])
	// The scan stops at the value's closing quote: everything past it is
	// the rest of the document, and a CR or NEL there is a LINE TERMINATOR
	// yaml.v3 honours, not a fold inside the value.
	if quote := tail[0]; quote == '"' || quote == '\'' {
		end := 1
		for end < len(tail) {
			if quote == '"' && tail[end] == '\\' {
				end += 2
				continue
			}
			if tail[end] == quote {
				if quote == '\'' && end+1 < len(tail) && tail[end+1] == '\'' {
					end += 2
					continue
				}
				break
			}
			end++
		}
		if end >= len(tail) {
			return
		}
		tail = tail[:end+1]
	}
	if strings.ContainsAny(tail, "\r\u0085") {
		g.refuse(n, "a raw CR or NEL byte sits inside the quoted value: YAML folds it to a space and the .bot writes the fold in its place — replace it with a space or delete it")
	}
}

// plainValueComment says a ` #` that starts a comment at a plain text
// VALUE's tail: the value ends there and the rest of the line is dropped —
// the one placement where a comment may be a value cut short. A number, a
// bool, a null or a quoted/block scalar cannot carry the `#`, so their
// trailing comment is the note it reads as. Called on value positions only:
// a comment after a mapping key hangs on the key, which is no value.
func (g *guard) plainValueComment(v *yaml.Node) {
	if v == nil || v.Kind != yaml.ScalarNode || v.Style != 0 || v.LineComment == "" || v.ShortTag() != "!!str" {
		return
	}
	g.warn(v, commentReading+strconv.Quote(v.Value)+", the rest of the line is dropped — quote the whole value if the `#` is part of it")
}

// commentReading opens the warning plainValueComment gives: IsCommentReading
// tells it from the other E053 readings by it.
const commentReading = "a ` #` starts a comment: the value reads as "

// IsCommentReading reports whether d is the warning on a ` #` that ends a
// plain text value in a comment. Unlike the other readings E053 names, a
// rewrite that keeps the comment on the same plain value keeps the author's
// text, and the warning with it: the reading is still the author's to
// settle, by quoting.
func IsCommentReading(d parser.Diagnostic) bool {
	return d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning && strings.HasPrefix(d.Message, commentReading)
}

func (g *guard) walk(n *yaml.Node, depth int) {
	if n == nil {
		return
	}
	g.count++
	if g.count > maxNodes {
		if g.count == maxNodes+1 {
			g.refuse(n, fmt.Sprintf("the document holds more than %d values", maxNodes))
		}
		return
	}
	if depth > maxNestingDepth {
		g.refuse(n, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth))
		return
	}
	if n.Anchor != "" {
		g.refuse(n, "an anchor (&"+n.Anchor+") is not read: write the value where it is used")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		g.refuse(n, "an explicit tag ("+n.Tag+") is not read: the value's shape decides its type")
	}
	if g.droppedBang(n) {
		g.refuse(n, "YAML reads a `!` before a value as a tag and drops it — `"+excerpt(g.at(n))+"` would be read without it: quote the whole value if the `!` is part of it")
	}
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		g.rawFoldBreak(n)
	}
	switch n.Kind {
	case yaml.AliasNode:
		g.refuse(n, "an alias (*"+n.Value+") is not read: write the value where it is used")
		return
	case yaml.MappingNode:
		seen := map[string]int{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.Kind == yaml.MappingNode && strings.HasPrefix(g.at(n), "{{"):
				// `user: {{input.task}}` reads as a mapping whose key is a
				// mapping: a template written unquoted.
				g.refuse(k, "a key is a plain word, not a mapping — a value that starts with `{{` (a template) is read by YAML as a mapping: quote the whole value")
			case k.Kind != yaml.ScalarNode:
				g.refuse(k, "a key is a plain word, not a "+kindWord(k))
			case k.Tag == "!!merge": // a plain `<<`; quoted, a key like any other
				g.refuse(k, "a merge key (<<) is not read: write the entries in place")
			case k.ShortTag() != "!!str":
				g.refuse(k, "a key is a plain word; `"+k.Value+"` reads as "+tagWord(k.ShortTag())+" — quote it if it is a name")
			default:
				if first, dup := seen[k.Value]; dup {
					g.refuse(k, fmt.Sprintf("the key `%s` appears twice in this mapping (first at line %d): a key names its value once", k.Value, first))
				} else {
					seen[k.Value] = k.Line
				}
			}
			g.walk(k, depth+1)
			g.plainValueComment(v)
			g.walk(v, depth+1)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			g.plainValueComment(c)
			g.walk(c, depth+1)
		}
	case yaml.DocumentNode:
		for _, c := range n.Content {
			g.walk(c, depth)
		}
	}
}

func docDiag(name string, line, col int, msg string) parser.Diagnostic {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	return parser.Diagnostic{
		Code: parser.DiagAuthorDocument, Severity: parser.SeverityError,
		Message: msg, File: name, Line: line, Column: col,
		Hint: parser.HintFor(parser.DiagAuthorDocument),
	}
}

// kindWord names a node's kind for a message.
func kindWord(n *yaml.Node) string {
	if n == nil {
		return "nothing"
	}
	switch n.Kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	case yaml.ScalarNode:
		return tagWord(n.ShortTag())
	}
	return "value"
}

// tagWord names a resolved scalar tag for a message: what the author's
// spelling READS as, which is what surprises them.
func tagWord(tag string) string {
	switch tag {
	case "!!str":
		return "a string"
	case "!!int":
		return "an integer"
	case "!!float":
		return "a float"
	case "!!bool":
		return "a bool"
	case "!!null":
		return "null (no value)"
	case "!!timestamp":
		return "a timestamp (a bare date) — quote it if the date is the text you mean"
	case "!!binary":
		return "binary data"
	case "!!merge":
		return "YAML's merge key"
	}
	return tag
}
