package author

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// The bounds of a document the converter reads, the twins of the `.bot`
// lexer's own (10 MiB of source, 100 levels of nesting): a YAML file is not
// a way around them. Its reading is held to the lexer's read budget
// (parser.ReadBudget, 2^20 unless the operator moved it) as the .bot's is —
// every value, every node of the tree, spends one record, every comment line
// two (pastReadBudget) — which bounds the work a small file can ask for (a
// billion-laughs document has few bytes and many nodes; anchors and aliases
// are refused outright, so a node is a value written once).
//
// yaml.v3 builds a document's whole tree before any caller sees a node and
// offers no hook to count while it builds, so before the decode runs its
// cost is bounded off the text (workflowfile.CheckYAMLDecode): the values
// the indicators outside the text of `|` and `>` blocks can open, at most
// twice the budget (decodeCeiling). The values themselves are counted on
// the tree, against the budget. Every decode of a document's YAML goes
// through both (readFirst), and the .bot the document spells is then read
// on the budget too (spell.go, author.Parse).
const (
	maxSourceSize   = 10 * 1024 * 1024
	maxNestingDepth = 100
)

// The remedies of a document past a bound. Such a document breaks no rule
// of YAML, so E050's catalogued hint — about separators, anchors and tags —
// would send its author to the wrong place.
const (
	docSizeHint  = "A document this large reads in parts: move declarations into .bot fragments under lib/, listed under `imports:`, each read on its own 10 MiB — the read budget stays the bot's."
	docDepthHint = "Nest a value at most 100 levels deep, the .bot's own bound: flatten it."
	// docDecodeHint is the remedy of a document refused before its decode
	// (decodeCeiling): what is counted, where text is not, and the hatch.
	docDecodeHint = "Before a document is decoded, the `:` `,` `?` `-` `[` `{` and `#` it holds — in its keys, its comments, its plain and quoted values, never in the text of a `|` or `>` block — are counted as the values and comments they can open, which may reach twice the read budget; its values and comment lines are then held to the budget itself. A long text written as a `|` block is never counted. The budget is " + parser.ReadBudgetEnv + " wherever the document is read: a larger count raises it, 0 removes it."
	// docReadBudgetHint is the remedy of a document whose .bot spelling is
	// past the read budget (spell.go, author.Parse): the reader's refusal,
	// with the document's remedy rather than the .bot's — the twin is read
	// by its author's iterion, and the fragments it imports share the
	// budget with it, so moving declarations there is no way past it.
	docReadBudgetHint = "The budget is " + parser.ReadBudgetEnv + " wherever the document is read, and the .bot fragments it imports share it with the document: a larger count raises it, 0 removes it."
)

// decodeCeiling is the most values a document's decode may build, counted
// before it runs (workflowfile.CheckYAMLDecode): twice the read budget. The
// count of a document's indicators reaches about one and a half times its
// values on a mapping of `key: value` lines, so a document within the
// budget is not refused before its values are counted. The decode costs
// about 200 bytes a value, and up to about 350 bytes a unit of the count
// when every value carries comments — each `#` weighed two. Measured at the
// ceiling of the default budget (2,097,152 units), the peak memory of one
// process each, over several runs of the same input: 396-423 MiB on
// a flow mapping of one-letter keys, the densest values; 535-692 MiB on
// the densest comments, a `- a #l` entry with a blank-line-wrapped `#f`
// after it (three comment readings a value), the worst shape found; and
// 481-676 MiB for those comments read through author.Parse, the CLI's door
// (the 676 in one of nine runs of the same input). No ceiling with no
// budget.
func decodeCeiling(budget int) int {
	if budget > math.MaxInt/2 {
		return math.MaxInt
	}
	return 2 * budget
}

// pastReadBudget is the node of the document — doc its DocumentNode — at
// which its reading passes the read budget: every value spends one record,
// every line of a comment yaml.v3 holds on a node two, as the .bot's own
// reading charges a comment (a comment yaml.v3 drops costs the tree
// nothing). nil within the budget.
func pastReadBudget(doc *yaml.Node, budget int) *yaml.Node {
	spent := 0
	var at *yaml.Node
	var walk func(n *yaml.Node) bool
	walk = func(n *yaml.Node) bool {
		if n.Kind != yaml.DocumentNode {
			spent++
		}
		spent += 2 * (commentLineCount(n.HeadComment) + commentLineCount(n.LineComment) + commentLineCount(n.FootComment))
		if spent > budget {
			at = n
			return false
		}
		for _, c := range n.Content {
			if !walk(c) {
				return false
			}
		}
		return true
	}
	walk(doc)
	return at
}

// commentLineCount counts the lines of a comment yaml.v3 holds on a node,
// the blank ones aside — what Comments lists of it (commentLines), with
// nothing allocated.
func commentLineCount(c string) int {
	n := 0
	for len(c) > 0 {
		line, rest, _ := strings.Cut(c, "\n")
		if strings.TrimSpace(line) != "" {
			n++
		}
		c = rest
	}
	return n
}

// errNotRead is the cause of a document readFirst does not decode: past a
// bound of its reader, or not UTF-8.
var errNotRead = errors.New("not read")

// readFirst decodes the first YAML document of src within its reader's
// bounds — the size cap, UTF-8, the decode's ceiling before it runs and the
// read budget after it — the one way this package reads a document's YAML
// besides decode, which says why it refuses one. nil and no error for a
// text that holds no document; the decoder, for a caller that asks what
// follows.
func readFirst(src []byte) (*yaml.Node, *yaml.Decoder, error) {
	if len(src) > maxSourceSize {
		return nil, nil, fmt.Errorf("%w: the text exceeds the maximum size (%d bytes > %d)", errNotRead, len(src), maxSourceSize)
	}
	if at := invalidUTF8(src); at >= 0 {
		return nil, nil, fmt.Errorf("%w: the text is not UTF-8 (byte %d)", errNotRead, at)
	}
	budget := parser.ReadBudget()
	if r := workflowfile.CheckYAMLDecode(src, decodeCeiling(budget), true); r != nil {
		return nil, nil, fmt.Errorf("%w: %v", errNotRead, r)
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, dec, nil
		}
		return nil, nil, err
	}
	if at := pastReadBudget(&doc, budget); at != nil {
		return nil, nil, fmt.Errorf("%w: line %d: the text is past the read budget of %d", errNotRead, at.Line, budget)
	}
	return &doc, dec, nil
}

// ReadOne decodes src as one YAML document, within the bounds every reading
// of a document is held to (readFirst): its DocumentNode — nil for a text
// that holds nothing but blanks — or an error for a text that is not one
// YAML document, comments alone included, or that passes a bound. The
// door for a reader of a document's YAML outside this package.
func ReadOne(src []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(bytes.TrimPrefix(src, []byte("\ufeff")))) == 0 {
		return nil, nil
	}
	doc, dec, err := readFirst(src)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("no YAML document")
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("a second YAML document follows")
	}
	return doc, nil
}

// yamlLineRe reads the line number a reader writes into its own errors
// ("yaml: line 3: mapping values are not allowed in this context"), where
// yamlErrorAtLine rewrites it. The numbers a remedy or a line note are
// keyed on are read where the reading runs (workflowfile.YAMLCause.Line),
// never back out of these words.
var yamlLineRe = regexp.MustCompile(`line (\d+):`)

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
// never refusals, and the third every comment reading of them, reported
// or not (Result.CommentReadings).
func decode(name string, src []byte) (*yaml.Node, []parser.Diagnostic, []parser.Diagnostic) {
	if len(src) > maxSourceSize {
		d := docDiag(name, 1, 1, fmt.Sprintf("the document exceeds the maximum size (%d bytes > %d)", len(src), maxSourceSize))
		d.Hint = docSizeHint
		return nil, []parser.Diagnostic{d}, nil
	}
	// yaml.v3 also reads UTF-16; every reader of the source at a node's
	// position reads UTF-8 (sourceLines), as the .bot it stands for is.
	if at := invalidUTF8(src); at >= 0 {
		return nil, []parser.Diagnostic{docDiag(name, len(sourceLines(src[:at])), 1, fmt.Sprintf("the document is not UTF-8 text (byte %d): write it in UTF-8, as the .bot it stands for is", at))}, nil
	}
	budget := parser.ReadBudget()
	if r := workflowfile.CheckYAMLDecode(src, decodeCeiling(budget), true); r != nil {
		d := docDiag(name, r.Line, r.Column, fmt.Sprintf("the document may hold more values than its reader decodes, %d: counted before it is read, %s as the values and comments it can open — the reader stops here", r.Ceiling, r.Counted()))
		d.Hint = docDecodeHint
		return nil, []parser.Diagnostic{d}, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}, nil
		}
		cause := workflowfile.YAMLCauses(workflowfile.YAMLv3, err)[0]
		line := 1
		if cause.Line > 0 {
			line = cause.Line
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, "not YAML: "+err.Error()+YAMLRemedy(cause))}, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, []parser.Diagnostic{docDiag(name, 1, 1, "the document is empty: nothing to read")}, nil
	}
	if at := pastReadBudget(&doc, budget); at != nil {
		d := docDiag(name, at.Line, at.Column, fmt.Sprintf("the document is past the read budget of %d — every value spends one record, a comment line two, as the .bot's reading does: the reader stops here", budget))
		d.Hint = docReadBudgetHint
		return nil, []parser.Diagnostic{d}, nil
	}
	var second yaml.Node
	if err := dec.Decode(&second); !errors.Is(err, io.EOF) {
		line := second.Line
		if len(second.Content) > 0 && second.Content[0].Line > 0 {
			line = second.Content[0].Line
		}
		// The cause of the second document's own refusal, where the second
		// decode failed: its remedy rides the refusal of the second
		// document as such.
		var cause workflowfile.YAMLCause
		if err != nil {
			cause = workflowfile.YAMLCauses(workflowfile.YAMLv3, err)[0]
			if cause.Line > 0 {
				line = cause.Line
			}
			// yaml.v3 names the line BEFORE the token that opened the
			// second-document error's cause, which a comment or a blank
			// line between the two makes a line the author must not touch.
			// For yamlNoDocumentStart alone the source holds the line its
			// remedy names, in the order the remedy names them: the `...`
			// that closed the document, read off the lines between the first
			// document and the line yaml named; then, where the first
			// document is a whole value of its own rather than the
			// `key: value` mapping an author document is — a plain or quoted
			// value, a `|`/`>` block, a `[…]` or a `{…}` — the line of that
			// value, which the node decode filled holds. Any other message
			// is the second document's own syntax, whose remedy names that
			// document's spelling: yaml's line stands.
			if root := doc.Content[0]; root.Line > 0 && cause.Message == yamlNoDocumentStart {
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
			msg += ", and it is not YAML: " + yamlErrorAtLine(err.Error(), line) + YAMLRemedy(cause)
		}
		return nil, []parser.Diagnostic{docDiag(name, line, 1, msg)}, nil
	}
	root := doc.Content[0]
	errs, warns, readings := guardTree(name, root, src)
	if len(errs) > 0 {
		return nil, errs, nil
	}
	// The document reads: the guard's warnings ride back beside the
	// converter's and the parser's (a warning never refuses), and the
	// invisible line separators it holds get their one document-level
	// notice (#1663).
	if n := separatorNotice(name, src); n != nil {
		warns = append(warns, *n)
	}
	return root, warns, readings
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

// yamlRemedies pairs the problem strings the two vendored YAML readers
// build with the spelling that avoids them: a reader's own message says
// what it saw, never what to write. Measured on the authoring probe: a
// shell command holding a `: ` (a jq filter) written unquoted cost a
// mid-size model four validation rounds, the message repeating itself. An
// entry is keyed on the reader's structured cause — which reader raised
// the refusal, its own message, and the Go type the reading was decoding
// into where one message serves several causes — so the sentence an author
// reads is a function of the cause, and a value the reader quotes back
// into its own message is in none of the key: the causes are read where
// the reading runs (workflowfile.YAMLCauses), not matched against the
// words the author is shown.
var yamlRemedies = []struct {
	// readers are the vendored readers that raise these messages, in the
	// spellings listed: an entry answers the readers it names, and the
	// coverage gate walks the constants of the reader that runs.
	readers []workflowfile.YAMLReader
	// messages are the readers' own problem strings, spelled as their
	// sources build them — a constant whole, or the format a quoted value
	// stands in (the value itself is in none of it).
	messages []string
	// target scopes the entry to one Go type the reading was decoding
	// into, where one message serves several causes and that type is what
	// tells them apart; "" where the message names its cause alone.
	target string
	remedy string
}{
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"mapping values are not allowed in this context"},
		remedy:   "a plain value holding a `:` followed by a space or by the end of the line reads as a nested mapping: quote the whole value (a command with a jq filter, an expression, an edge whose with map has a `: `, a text ending in `:`), a `'` inside single quotes written twice (`''`); or a key is indented deeper than the keys beside it: dedent every key that is, to their column — one left behind is refused again; or a line above this one is a plain value where a `key: value` pair was expected (a title, a note): a plain value runs on into the lines below it, so the first `: ` it meets is refused here — delete that line, or write it as `key: value`" + strayLineSpans + "; or a `%YAML`/`%TAG` directive line heads the document, where the author document takes none",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"found character that cannot start any token"},
		remedy:   "the value starts with a character YAML reserves (`%`, `@`, a backtick): quote the whole value; or a tab indents the line, where YAML takes spaces only",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"found a tab character where an indentation space is expected", "found a tab character that violates indentation"},
		remedy:   "YAML indents with spaces only: replace the tab",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected key", "did not find expected '-' indicator"},
		remedy:   "a value that starts with `{` or `[` (a `{{…}}` template, a `[WIP]`) is read by YAML as a mapping or a list, and a value with quotes of its own, written plain or inside double quotes, ends where YAML reads its quote as closed (an edge whose with map holds a quoted value): write either whole in single quotes, its double quotes as they are and a `'` inside written twice (`''`); or indentation: the keys of one mapping start at one column, a value's lines are indented under its key, a list's items under theirs; or a `|N`/`>N` indicator claims more spaces than the body has — the digit counts from the key's own indentation, so YAML reads none of the body: write `|` alone and let the body's indentation say it; or a line is a whole value of its own where a `key: value` pair was expected — a `- item` line reads as an item of the document itself, a `|`/`>` block stands where a key belongs: delete that line, or indent it under the key it belongs to" + strayLineSpans,
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"could not find expected ':'"},
		remedy:   "a key is followed by `: ` (colon, space) and its value; or a `|` or `>` block's body is not indented under its key, which leaves its first line read as a key of its own; or a list's item is written without its `- `, under a key whose other items have one; or this line is a whole value of its own where a `key: value` pair was expected (a title, a note, a line pasted in): delete it, or write it as `key: value`" + strayLineSpans,
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
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
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected alphabetic or numeric character", workflowfile.YAMLUnknownAnchor},
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
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"document contains excessive aliasing", workflowfile.YAMLAnchorSelfRef},
		remedy:   "a `.bot` frontmatter block resolves its anchors and aliases, and this graph it cannot: an anchor whose value holds its own alias (`x: &a {b: *a}`) is a cycle the resolving has no end to, and a chain of aliases each naming the one above it expands into far more values than the block's own lines — write the keys out where they are used; the author document reads no anchor and no alias, and refuses each with its own words",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected comment or line break"},
		remedy:   "`|` and `>` take nothing but a comment after them on the key's line: write `|` alone and the text on the lines under it, indented — or, where the `|` is part of the value (a shell pipe), quote the whole value; a `%YAML`/`%TAG` directive line carrying trailing text gives the same message",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"found an indentation indicator equal to 0"},
		remedy:   "the digit after `|` or `>` is how many spaces the body is indented by, and YAML counts it from 1: write `|` alone and let the indentation of the lines under the key say it",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"block sequence entries are not allowed in this context"},
		remedy:   "a list's items each start a line of their own, indented under their key (`imports:`, then `  - lib/x.bot`): a `- ` on the key's line or following a value is not an item — write the list as `[a, b]` to keep it on one line, or quote the whole value where the `- ` is part of it",
	},
	{
		// The scanner's, raised where a `? ` opens a value: the document
		// reaches it and the frontmatter block does too (a `? ` value
		// under one of the four keys).
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"mapping keys are not allowed in this context"},
		remedy:   "a value starting with `? ` (a question, a `?` flag) reads as YAML's explicit-key indicator: write the whole value in single quotes",
	},
	{
		// The decoder's own, raised where a key that is itself a list or a
		// mapping is read into a Go map — the frontmatter block's reading
		// alone reaches it: a nested key beyond the four is read into a Go
		// map, where a key that is itself a list or a mapping has no
		// place. The document's reader fills a *yaml.Node, where a key is
		// a node and guardTree refuses a complex key with its own words.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{workflowfile.YAMLMapKeyFormat},
		remedy:   "a `? ` key whose text is a list or a mapping is no key at all — write the key as plain text",
	},
	{
		// yaml.v2 resolves a merge key while reading a frontmatter block
		// into the catalog identity's Go fields. The author document's own
		// reader never resolves one — guardTree refuses `<<` with its own
		// words — so no author document reaches this message.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{"map merge requires map or sequence of maps as the value"},
		remedy:   "`<<` is YAML's merge key, which takes a mapping or a list of mappings: write the keys out, or quote the whole value where the `<<` is part of it — the author document takes none, and refuses a merge key with its own words; a `.bot` frontmatter block merges the keys in, as keys of its own",
	},
	{
		// The block's OWN top level: the reading decodes the whole block
		// into frontmatterFields, so a line that is a whole value of its
		// own — not a `key: value` line at all — is refused on that type,
		// and the fix is a key. The other two causes of the same message
		// are the entries below, told apart by the decode target.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{workflowfile.YAMLUnmarshalFormat},
		target:   "workflowfile.frontmatterFields",
		remedy:   "a `.bot` frontmatter block is `key: value` lines, and a line that is a whole value of its own (a line of prose, a `- item` list, a `[…]`, a `|` or `>` block, a bare number, `true`) is one whole value and no key at all: write it as `key: value` (`description: a bot that reviews pull requests`)",
	},
	{
		// A key's name, a one-line value, or a list's item: each is read
		// into a plain string, so a list or a mapping where one is
		// expected is refused on that type — and a complex key of the
		// block's own top level, read into the string key of the map that
		// holds the keys beyond the four, is refused here too.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{workflowfile.YAMLUnmarshalFormat},
		target:   "string",
		remedy:   "a value a text key takes is one line of text: `name` and `description` one line, and so does every item of `triggers` or `capabilities` — every item one line of text; a value of another shape is not read; and a key is plain text, so a `? ` key whose text is a list or a mapping is no key at all: write the key as plain text",
	},
	{
		// A list key read where its shape is a list: `triggers` and
		// `capabilities` take `[]string`, so a scalar or a mapping under
		// one is refused on that type, and the value the tag names under a
		// list key is refused again here.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{workflowfile.YAMLUnmarshalFormat},
		target:   "[]string",
		remedy:   "`triggers` and `capabilities` take a list: `[a, b]` on one line, or one `- item` per line under the key, every item one line of text",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"found unknown escape character", "did not find expected hexdecimal number", "found invalid Unicode character escape code"},
		remedy:   "inside double quotes a `\\` opens an escape, and few characters may follow it (`\\n`, `\\t`, `\\\\`, `\\xNN`, `\\uNNNN`): write the value in single quotes, where a `\\` stands for itself (a regex `\\d`, a Windows path `C:\\temp`), or double every `\\`",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected ',' or ']'", "did not find expected ',' or '}'"},
		remedy:   "inside `[…]` and `{…}` a `,` separates the items, and an item holding a `,`, a `]`, a `}`, a `:` or a `?` of its own goes in single quotes: close the `[` or `{` the value opened, add the missing `,`, or drop the brackets — one `- item` per line for a list, one `key: value` line per entry for a mapping",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected node content"},
		remedy:   "a `- item` line inside a `[…]` is not a list item: either close the `[…]` and separate its items with `,`, or drop the brackets — one `- item` per line for a list, one `key: value` line per entry for a mapping; a `,` with no item before it has no value either; a closing `]` or `}` that opens nothing is no value: delete it and write one, or quote the value it is part of; and a `---` or `...` line ends the document where a value was expected: remove it, or indent it under its key where it belongs to a value",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find expected whitespace or line break", "did not find expected '!'"},
		remedy:   "a value starting with `!` reads as a YAML tag and a line starting with `%` as a `%TAG` directive: quote the whole value, or remove the directive line — the author document carries neither",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"did not find the expected '>'", "did not find expected tag URI", "did not find URI escaped octet", "found an incorrect leading UTF-8 octet", "found an incorrect trailing UTF-8 octet", "found undefined tag handle"},
		remedy:   "a value starting with `!` reads as a YAML tag (`!x`, `!!str`, `!<…>`): write it whole in single quotes — a `! grep -q x f` command, a `!` negation — a value's shape decides its type; the author document carries none, and refuses an explicit tag with its own words",
	},
	{
		// The tag RESOLVED: the frontmatter block's reading decodes the
		// value into its key's Go type, where a tag that names another
		// shape is refused, and where the bytes a `!!binary` tag names
		// fail their base64 decoding — the document's reader fills a
		// *yaml.Node and resolves no tag, refusing an explicit tag with
		// its own words. The clause about the value the tag names is
		// scoped by the KEY, as the shape entries are: the value it then
		// holds is taken where the key's own shape fits it — measured,
		// `name: !!int 3` reads as the name `3` and `triggers: !!int 3`
		// is refused again, into []string.
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2},
		messages: []string{workflowfile.YAMLDecodeFormat, "!!binary value contains invalid base64 data"},
		remedy:   "a value starting with `!` reads as a YAML tag, and a `.bot` frontmatter block reads the tag: `name` and `description` then take the value the tag names (a `!!binary` value base64, a `!!int` value a number), and so does an item of `triggers` or `capabilities`, whose key itself still takes a list — or write it whole in single quotes; the author document carries none, and refuses an explicit tag with its own words",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"found unknown directive name", "could not find expected directive name", "found unexpected non-alphabetical character", "did not find expected digit or '.' character", "found extremely long version number", "did not find expected version number", "did not find expected whitespace", "found duplicate %YAML directive", "found duplicate %TAG directive", "found incompatible YAML document"},
		remedy:   "a line starting with `%` is read as a YAML directive (`%YAML`, `%TAG`), which the author document never needs: remove the line, or — where the text belongs to a value — indent it under its key and quote the value",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{yamlNoDocumentStart},
		remedy:   "a document closed before this line and what follows it reads as a second one, which would need its own `---`, where the author document is one: a `...` line closes the document — remove it, or indent it under its key where it belongs to a value; a `%YAML`/`%TAG` directive line with no `---` after it reads the same way, and the author document takes no directive — remove it; a first line that is a whole value of its own rather than a `key: value` pair — a plain value, a line in quotes, a `[…]` or a `{…}`, a `|` or `>` block — is a whole document on its own: delete that line, write it as `key: value`, or unquote it; and the document opens with an indented block of `key: value` lines, which is a document of its own that the first line below it at a shallower column closes: give the whole document one indentation — every key of its top level at one column, each key's own lines moving with it: dedent them all to the shallowest column the document holds, or indent them all to the block's" + strayLineSpans,
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"exceeded max depth of %d"},
		remedy:   "the document nests deeper than YAML itself reads (10000 levels of `[`, `{` or indentation): flatten it",
	},
	{
		readers:  []workflowfile.YAMLReader{workflowfile.YAMLv2, workflowfile.YAMLv3},
		messages: []string{"control characters are not allowed"},
		remedy:   "the document holds a control character (an ESC from a captured terminal output, a NUL byte): remove it, or write it inside double quotes as an escape (`\\u001b`)",
	},
}

// YAMLRemedy is the remedy of the first entry of yamlRemedies that answers
// cause — the entry must name cause's reader, its message, and, where the
// entry is scoped to one decode target, that target — ready to follow the
// reader's own words; "" when no entry answers. Exported for the other
// author-facing readings of a YAML error: the frontmatter block of a
// `.bot`, whose refusals pkg/cli/fmt_author.go reads the causes of
// (workflowfile.YAMLCauses).
//
// The key is the cause, never the prose: an author's own value stands in
// the words the reader shows them and chooses nothing.
func YAMLRemedy(cause workflowfile.YAMLCause) string {
	for _, r := range yamlRemedies {
		if !slices.Contains(r.readers, cause.Reader) {
			continue
		}
		if r.target != "" && r.target != cause.Target {
			continue
		}
		if slices.Contains(r.messages, cause.Message) {
			return " — " + yamlPositionNote(cause) + r.remedy
		}
	}
	return ""
}

// YAMLRemedies is the remedy of every distinct cause of a refusal, in the
// order the reader raised them, ready to follow its words: a TypeError
// collects one element per value the reading could not read, each its own
// cause and its own sentence. Every sentence but the first is joined
// without the dash its position note hangs on — that dash is the head of
// one sentence, not a clause between two. "" when no cause is answered.
func YAMLRemedies(causes []workflowfile.YAMLCause) string {
	var out []string
	for _, c := range causes {
		r := YAMLRemedy(c)
		if r == "" || slices.Contains(out, r) {
			continue
		}
		if len(out) > 0 {
			r = strings.TrimPrefix(r, " — ")
		}
		out = append(out, r)
	}
	return strings.Join(out, "; ")
}

// YAMLBlockLineNote is the clause a refusal of a `.bot` frontmatter block
// owes its reader where the reader named a line of the block's own text, as
// parser.Frontmatter hands it to the reading. That number counts the lines
// the reading was GIVEN, which are not the file's lines with an offset: a
// run of blank lines inside the block stands for ONE line of the reading,
// and a `dsl:` or `import` line written between two of its comment lines
// for none, so no count from the fence is true of every file — and the
// fence itself is written `# ---` as often as `## ---`. The note quotes the
// line instead, which the author reads off their own file; a refusal naming
// several lines quotes each.
//
// lines are the numbers the reader named itself, read where the reading
// runs (workflowfile.YAMLCause.Line) — never out of the words an author
// reads, where a value the reader quoted back could spell a line nobody
// wrote. "" where the reader named no line the block holds: there is then
// no line to quote, and a clause hung on a number an author's own text
// spells would send them to a line nobody named. A line the block holds
// blank quotes nothing, as an out-of-range number names nothing — a pair
// of empty backticks is no text to read off the file.
func YAMLBlockLineNote(lines []int, block []string) string {
	seen := map[int]bool{}
	var named []int
	for _, n := range lines {
		if n < 1 || n > len(block) || seen[n] || strings.TrimSpace(block[n-1]) == "" {
			continue
		}
		seen[n] = true
		named = append(named, n)
	}
	if len(named) == 0 {
		return ""
	}
	if len(named) == 1 {
		return ", where the line it names is the block's own, not the file's — `" + block[named[0]-1] + "`"
	}
	var b strings.Builder
	b.WriteString(", where the lines it names are the block's own, not the file's —")
	for i, n := range named {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" line " + strconv.Itoa(n) + " `" + block[n-1] + "`")
	}
	return b.String()
}

// yamlPositionNote heads the remedy of cause where the line cause's reader
// named is not the line of the mistake, "" otherwise. Read per message
// rather than per remedy entry: two entries answer a yamlContextProblems
// message beside scanner-raised ones, which do name the line that broke, so
// a clause carried by the entry would be false for them. The note has a
// referent only where the reader wrote a number into its own message — a
// mistake at the very first line leaves both marks at 0 and the message
// carries none.
func yamlPositionNote(cause workflowfile.YAMLCause) string {
	if cause.Line == 0 {
		return ""
	}
	if slices.Contains(yamlContextProblems, cause.Message) {
		return yamlReadsFromAbove + "; "
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
func guardTree(name string, root *yaml.Node, src []byte) (diags, warns, readings []parser.Diagnostic) {
	g := &guard{name: name, lines: sourceLines(src), rawLines: strings.Split(strings.TrimPrefix(string(src), "\ufeff"), "\n")}
	g.walk(root, 0)
	return g.errs.Diagnostics(), g.warns.Diagnostics(), g.readings
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
	// linesAt and rawAt find node positions on lines and rawLines.
	linesAt, rawAt lineCursor
	// errs and warns are the refusals and the warnings, apart: a warning
	// never refuses the document. readings is every comment reading of
	// them, reported or not: the canonical form compares them all between
	// a document and its written form.
	errs, warns parser.Report
	readings    []parser.Diagnostic
}

// lineCursor finds a rune column on a line of the source and remembers
// where it found the last one. The walk visits nodes in document order, so
// on a line holding many of them — a flow sequence written on one line —
// each search starts where the previous one ended, and the line is read
// once rather than once per node.
type lineCursor struct {
	line, col, off int // the last position found: 1-based line and rune column, its byte offset
}

// readSteps counts the characters the position readers decode — the line
// cursor and the character cache (charLines) — for a test to hold the work
// of reading a long line to the line's length. It is the process's, so a
// witness reading it measures every reading running beside its own: no
// test of this package runs in parallel or reads from a goroutine, which
// TestTheStepWitnessesRunAlone holds.
var readSteps atomic.Int64

// tail is lines[line-1] from rune column col (1-based) to the end of the
// line — a substring, never a copy, "" at the line's end — and whether the
// column is on the line or just past its last character; false for a
// position off the source.
func (c *lineCursor) tail(lines []string, line, col int) (string, bool) {
	if line < 1 || line > len(lines) || col < 1 {
		return "", false
	}
	s := lines[line-1]
	at, off := 1, 0
	if c.line == line && c.col <= col {
		at, off = c.col, c.off
	}
	from := at
	defer func() { readSteps.Add(int64(at - from)) }()
	for ; at < col; at++ {
		if off >= len(s) {
			return "", false
		}
		_, size := utf8.DecodeRuneInString(s[off:])
		off += size
	}
	c.line, c.col, c.off = line, col, off
	return s[off:], true
}

// at is the source from node n's position to the end of its line, "" when
// the position is not on a line of the source.
func (g *guard) at(n *yaml.Node) string {
	rest, _ := g.linesAt.tail(g.lines, n.Line, n.Column)
	return rest
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

// excerpt is v cut short for a message: its first line, 60 characters at
// most. It reads no further than it keeps: v may be the rest of a line
// holding a million values, and a caller hands it no more than
// excerptWindow bytes of it.
func excerpt(v string) string {
	n := 0
	for i, r := range v {
		switch {
		case r == '\n':
			return v[:i]
		case n == 60:
			return v[:i] + "\u2026"
		}
		n++
	}
	return v
}

// excerptWindow is the most of a line excerpt is handed: its 60 characters
// and the one that says there are more, four bytes at most each.
const excerptWindow = 61 * utf8.UTFMax

// window is v cut to excerptWindow bytes: what excerpt reads of it, and no
// more however long the line.
func window(v string) string {
	if len(v) > excerptWindow {
		return v[:excerptWindow]
	}
	return v
}

func (g *guard) refuse(n *yaml.Node, msg string) {
	g.errs.Add(docDiag(g.name, n.Line, n.Column, msg))
}

// refuseBound refuses a document past one of the reader's bounds, with the
// bound's own remedy (docDepthHint).
func (g *guard) refuseBound(n *yaml.Node, msg, hint string) {
	d := docDiag(g.name, n.Line, n.Column, msg)
	d.Hint = hint
	g.errs.Add(d)
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
	d := parser.Diagnostic{
		Code: parser.DiagAuthorPromptBody, Severity: parser.SeverityWarning,
		Message: msg, File: g.name, Line: line, Column: col,
	}
	g.warns.Add(d)
	if IsCommentReading(d) {
		g.readings = append(g.readings, d)
	}
}

// rawFoldBreak refuses a quoted scalar whose SOURCE line carries a raw
// CR or NEL byte inside the quotes — the two breaks yaml.v3 folds to a
// space, so the .bot writes the fold in the value's place and nothing
// else says so (#1814). An LS or a PS is kept as written: it reads back
// byte for byte, and stays the author's.
func (g *guard) rawFoldBreak(n *yaml.Node) {
	rest, _ := g.rawAt.tail(g.rawLines, n.Line, n.Column)
	tail := strings.TrimSuffix(rest, "\r")
	if tail == "" {
		return
	}
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
	if depth > maxNestingDepth {
		g.refuseBound(n, fmt.Sprintf("maximum nesting depth exceeded (%d levels)", maxNestingDepth), docDepthHint)
		return
	}
	if n.Anchor != "" {
		g.refuse(n, "an anchor (&"+n.Anchor+") is not read: write the value where it is used")
	}
	if n.Style&yaml.TaggedStyle != 0 {
		g.refuse(n, "an explicit tag ("+n.Tag+") is not read: the value's shape decides its type")
	}
	if g.droppedBang(n) {
		g.refuse(n, "YAML reads a `!` before a value as a tag and drops it — `"+excerpt(window(g.at(n)))+"` would be read without it: quote the whole value if the `!` is part of it")
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
		// Read where the mapping starts, before its entries move the
		// cursor past it.
		template := strings.HasPrefix(g.at(n), "{{")
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch {
			case k.Kind == yaml.MappingNode && template:
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
