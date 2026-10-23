package author

import (
	"errors"
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// witness is one minimal author document and a phrase of the remedy its
// refusal must carry.
type witness struct {
	src, want string
}

func cmdDoc(line string) string {
	return "dsl: 2\nnodes:\n  - tool: t\n    " + line + "\n"
}

func promptDoc(line string) string {
	return "dsl: 2\nprompts:\n  p: " + line + "\n"
}

func edgeDoc(edge string) string {
	return "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - " + edge + "\n"
}

const (
	// readsAloneHead is the first key of a document that reads on its own,
	// readsAloneBody the rest of it: the two make the document a witness is
	// left with once the stray line it adds is deleted.
	readsAloneHead = "dsl: 2\n"
	readsAloneBody = "nodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n"
)

// strayLineSpansWant is the clause every entry that tells the author to
// delete or move a line carries, written out: the table's own constant is
// not read here, so a reworded clause reddens rather than following along.
const strayLineSpansWant = "a value can span several lines: a plain or quoted value runs on into the lines under it, a `|`/`>` block carries the lines indented under it, a `[…]` or `{…}` runs to its closing bracket, a list carries one line per `- item` and an item runs on into the lines indented under it: delete them all, or move them all under one key, with the lines below that key indented under it"

// yamlReadsFromAboveWant is the clause the refusal of a message a YAML
// parser raises with a context mark carries, written out: the line such a
// refusal names need not be the line of the mistake, so the clauses that
// follow it would otherwise name the wrong line to delete. It claims the
// construct's reach, not a direction — both are witnessed below.
const yamlReadsFromAboveWant = "the line this refusal names is not always the line to edit: the value YAML was reading can start above it and end below it — read the whole construct, not this line alone"

// strayDoc writes one line above a document that reads on its own,
// strayMidDoc one between its keys: the stray line an author leaves in — a
// title, a note, a line pasted in — in the two positions it lands in, which
// YAML refuses with two different messages.
func strayDoc(line string) string { return line + "\n" + readsAloneHead + readsAloneBody }

func strayMidDoc(line string) string { return readsAloneHead + line + "\n" + readsAloneBody }

// quotedKeyDoc is that document with its first key written whole in quotes,
// which reads as a value of its own rather than as a `key: value` pair.
func quotedKeyDoc(key string) string { return key + "\n" + readsAloneBody }

// yamlWitnesses holds one document per cause of every YAML message the
// author path surfaces: the scanner's, the parser's, the reader's and the
// composer's. A message several mistakes produce has one document each —
// the line yaml.v3 reports does not tell its causes apart, so its remedy
// names them all — and a remedy that prescribes a spelling has a row for
// that clause too, beside the document TestADocumentWrittenAsItsRemedySays
// Reads writes it on.
var yamlWitnesses = map[string]witness{
	// mapping values are not allowed in this context
	"a plain value holding a colon":           {cmdDoc("command: jq -r .a: .b"), "quote the whole value"},
	"a key indented deeper than its siblings": {"dsl: 2\nnodes:\n  - agent: a\n     model: m\n", "indented deeper"},
	"two keys indented deeper than siblings":  {"dsl: 2\nnodes:\n  - agent: a\n     model: m\n     user: u\n", "dedent every key that is"},
	"a %YAML directive heading the document":  {"%YAML 1.1\ndsl: 2\n", "directive line heads the document"},
	"a stray line above the first key":        {strayDoc("my first bot"), "a plain value runs on into the lines below it"},
	"a stray line named above its refusal":    {strayDoc("a draft title"), "a line above this one is a plain value where a `key: value` pair was expected"},
	"a stray line to delete above a key":      {strayDoc("draft"), "delete that line, or write it as `key: value`"},
	"a two-line title above the first key":    {strayDoc("my first\nbot"), strayLineSpansWant},
	// could not find expected ':'
	"a key with no colon":                     {"dsl: 2\nnodes\n  - agent: a\n", "a key is followed by `: `"},
	"a block body not indented under its key": {"dsl: 2\nnodes:\n  - agent: a\n    user: |\nnot indented\n", "is not indented under its key"},
	"a list item written without its dash":    {"dsl: 2\nimports:\n  - lib/a.bot\n  lib/b.bot\n", "written without its `- `"},
	"a stray line between two keys":           {strayMidDoc("my first bot"), "this line is a whole value of its own"},
	"a stray line to delete between keys":     {strayMidDoc("a note to self"), "delete it, or write it as `key: value`"},
	"a two-line title between two keys":       {strayMidDoc("my first\nbot"), strayLineSpansWant},
	// did not find expected key / did not find expected '-' indicator
	"a key indented shallower than its siblings":  {"dsl: 2\nnodes:\n  - agent: a\n model: m\n", "indentation:"},
	"an edge whose with map holds a quoted value": {edgeDoc(`a -> done with { x: "{{outputs.a.y}}" }`), "single quotes"},
	"an edge in double quotes of its own":         {edgeDoc(`"a -> done with { x: "{{outputs.a.y}}" }"`), "single quotes"},
	"a command in double quotes of its own":       {cmdDoc(`command: "echo "hi""`), "single quotes"},
	"a block indicator past the body's indent":    {promptDoc("|9\n    x"), "claims more spaces than the body has"},
	"a stray list item above the first key":       {strayDoc("- a"), "reads as an item of the document itself"},
	"a stray list item between two keys":          {strayMidDoc("- a"), "a line is a whole value of its own where a `key: value` pair was expected"},
	"a stray list item to delete between keys":    {strayMidDoc("- b"), "delete that line, or indent it under the key it belongs to"},
	"a stray block scalar between two keys":       {strayMidDoc("|\n  a title"), "a `|`/`>` block stands where a key belongs"},
	"a stray block scalar's body between keys":    {strayMidDoc("|\n  a title"), strayLineSpansWant},
	// found character that cannot start any token
	"a value starting with a reserved character": {cmdDoc("command: %s"), "a character YAML reserves"},
	"a tab indenting a line":                     {"dsl: 2\nnodes:\n\t- agent: a\n", "a tab indents the line"},
	// found a tab character where an indentation space is expected / that violates indentation
	"a tab indenting a block body":              {promptDoc("|\n\tx"), "indents with spaces only"},
	"a tab indenting a plain value's next line": {"dsl: 2\nnodes:\n  - agent: a\n    model: m\n\tmore\n", "indents with spaces only"},
	// found unexpected end of stream / found unexpected document indicator
	"a quote left open":                            {cmdDoc(`command: "unclosed`), "every quote closed"},
	"a --- line inside a quoted value":             {cmdDoc("command: \"a\n--- b\""), "every quote closed"},
	"a ... line inside a quoted value":             {cmdDoc("command: \"a\n...\nb\""), "`...` line sits inside the document"},
	"a document indicator that belongs to a value": {cmdDoc("command: \"a\n...\nb\""), "indent it where it belongs to a value"},
	// did not find expected alphabetic or numeric character / unknown anchor
	"a command holding two ampersands": {cmdDoc("command: [ -f x ] && echo y"), "`&` opens a YAML anchor"},
	"an alias with no anchor":          {cmdDoc("command: *nope"), "`&` opens a YAML anchor"},
	// did not find expected comment or line break
	"text after a block indicator on the key's line": {promptDoc("| text"), "nothing but a comment"},
	"text after a directive's arguments":             {"%YAML 1.1 x\n---\ndsl: 2\n", "nothing but a comment"},
	// found an indentation indicator equal to 0
	"a block indented by zero spaces": {promptDoc("|0\n      x"), "YAML counts it from 1"},
	// block sequence entries are not allowed in this context
	"a list item on its key's line": {"dsl: 2\nimports: - lib/x.bot\n", "start a line of their own"},
	// mapping keys are not allowed in this context
	"a value starting with a question mark": {"dsl: 2\nnodes:\n  - agent: a\n    user: ? what now\n", "explicit-key indicator"},
	// found unknown escape character / hexdecimal number / invalid Unicode escape
	"a regex in double quotes":        {cmdDoc(`command: "^\d+$"`), "opens an escape"},
	"a Windows path in double quotes": {cmdDoc(`command: "C:\temp\x"`), "opens an escape"},
	"a lone surrogate escape":         {cmdDoc(`command: "\uD800"`), "opens an escape"},
	// did not find expected ',' or ']' / ',' or '}'
	"a flow list with a comma missing":       {"dsl: 2\nworkflow:\n  name: w\n  edges: [\"a -> done\" \"b -> done\"]\n", "separates the items"},
	"a flow mapping with a comma missing":    {cmdDoc("env: {a: 1 b: 2}"), "separates the items"},
	"a flow list never closed":               {"dsl: 2\nimports: [\"lib/a.bot\", \"lib/b.bot\"\n", "close the `[` or `{` the value opened"},
	"a flow mapping never closed":            {"dsl: 2\nvars: {a: string\n", "close the `[` or `{` the value opened"},
	"a flow list to write one item a line":   {"dsl: 2\nimports: [\"lib/a.bot\" \"lib/b.bot\"]\n", "one `- item` per line for a list"},
	"a flow mapping to write one key a line": {"dsl: 2\nvars: {a: string b: string}\n", "one `key: value` line per entry for a mapping"},
	// did not find expected node content
	"a list item inside a flow list":            {"dsl: 2\nworkflow:\n  name: w\n  edges: [\n    - a -> done\n  ]\n", "is not a list item"},
	"a flow list to write as an indented block": {"dsl: 2\nworkflow:\n  name: w\n  edges: [\n    - a -> done\n  ]\n", "one `- item` per line for a list"},
	"a closing bracket that opens nothing":      {"dsl: 2\nimports: ]\n", "opens nothing is no value"},
	"a closing brace that opens nothing":        {"dsl: 2\nimports: }\n", "opens nothing is no value"},
	"a ... line where a value was expected":     {"...\ndsl: 2\n", "ends the document where a value was expected"},
	"a ... line inside a flow list":             {"dsl: 2\nimports: [lib/a.bot,\n...\n]\n", "ends the document where a value was expected"},
	// did not find expected '!' / did not find expected whitespace or line break
	"a %TAG directive with no handle":     {"%TAG\ndsl: 2\n", "or remove the directive line"},
	"a tag followed by the value it tags": {cmdDoc(`command: !foo"x"`), "or remove the directive line"},
	// the tag of a value
	"a value starting with a verbatim tag":  {cmdDoc("command: !<x y"), "reads as a YAML tag"},
	"a value starting with two bangs":       {cmdDoc("command: !! x"), "reads as a YAML tag"},
	"a tag URI with a bad escape":           {cmdDoc("command: !<%zz> x"), "reads as a YAML tag"},
	"a tag URI escaping a bad lead byte":    {cmdDoc("command: !<%80> x"), "reads as a YAML tag"},
	"a tag URI escaping a bad trail byte":   {cmdDoc("command: !<%C3%28> x"), "reads as a YAML tag"},
	"a value tagged with an unknown handle": {cmdDoc("command: !e!foo x"), "reads as a YAML tag"},
	// the directives of a document
	"an unknown directive":                 {"%FOO bar\ndsl: 2\n", "is read as a YAML directive"},
	"a directive with no name":             {"% x\ndsl: 2\n", "is read as a YAML directive"},
	"a directive name with a colon":        {"%YAML:1.1\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a version that is not a number":       {"%YAML 1x\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a version number far too long":        {"%YAML " + strings.Repeat("1", 20) + "\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a %YAML directive with no version":    {"%YAML x\ndsl: 2\n", "is read as a YAML directive"},
	"a %TAG handle with no space":          {"%TAG !x!y\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a %YAML directive written twice":      {"%YAML 1.1\n%YAML 1.1\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a %TAG directive written twice":       {"%TAG !x! y\n%TAG !x! z\n---\ndsl: 2\n", "is read as a YAML directive"},
	"a YAML version yaml.v3 does not read": {"%YAML 2.0\n---\ndsl: 2\n", "is read as a YAML directive"},
	// did not find expected <document start>
	"a ... line with a document after it":                       {"dsl: 2\nnodes:\n  - agent: a\n    model: m\n...\nworkflow:\n  name: w\n  entry: a\n", "a `...` line closes the document"},
	"a second document with no --- of its own":                  {"dsl: 2\nnodes:\n  - agent: a\n    model: m\n...\nworkflow:\n  name: w\n  entry: a\n", "would need its own `---`"},
	"a directive with no document after it":                     {"%YAML 1.1\n\"x\"\n", "directive line with no `---` after it"},
	"a first key written without its colon":                     {"dsl 2\n# a note\nimports:\n  - lib/x.bot\n", "a whole value of its own rather than a `key: value` pair"},
	"a first key quoted whole":                                  {quotedKeyDoc(`"dsl: 2"`), "unquote it"},
	"a flow list as the first line":                             {strayDoc("[a, b]"), "a `[…]` or a `{…}`"},
	"a block scalar as the first line":                          {strayDoc("|\n  a title"), "a `|` or `>` block — is a whole document on its own"},
	"a block scalar's body as the first lines":                  {strayDoc("|\n  a title"), strayLineSpansWant},
	"a stray line above a comment":                              {"my first bot\n# a note\n" + readsAloneHead + readsAloneBody, "delete that line, write it as `key: value`, or unquote it"},
	"a document opening with an indented block":                 {"  dsl: 2\n  imports:\n    - lib/x.bot\nnodes:\n  - agent: a\n", "the document opens with an indented block of `key: value` lines"},
	"an indented block closed by a shallower line":              {"  dsl: 2\n  imports: x\nnodes:\n", "the first line below it at a shallower column closes"},
	"an indented block with a comment before the column-0 line": {"  b: 2\n# a note\ndsl: 2\n", "give the whole document one indentation"},
	"a head carrying three indentations":                        {"    dsl: 2\n  imports:\n   - lib/a.bot\nnodes:\n  - agent: a\n    model: m\n", "dedent them all to the shallowest column the document holds, or indent them all to the block's"},
	// exceeded max depth of %d
	"a document nested past YAML's own bound": {"dsl: 2\nnodes: " + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + "\n", "nests deeper than YAML itself reads"},
	// control characters are not allowed
	"a control character in a value": {cmdDoc("command: echo \x01"), "holds a control character"},
	"a NUL byte in a value":          {cmdDoc("command: echo \x00"), "holds a control character"},
}

// frontmatterAliasBomb is a frontmatter block whose aliases expand past
// what yaml.v2 reads in one document: the ratio its decoder holds is of the
// expanded size to the source's, so the block is small and its expansion is
// not.
func frontmatterAliasBomb() string {
	var b strings.Builder
	b.WriteString("name: &a " + strings.Repeat("x", 60) + "\n")
	prev := "a"
	for i := 0; i < 12; i++ {
		n := fmt.Sprintf("l%d", i)
		b.WriteString(n + ": &" + n + " [")
		for j := 0; j < 10; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString("*" + prev)
		}
		b.WriteString("]\n")
		prev = n
	}
	return b.String()
}

// frontmatterWitnesses holds one `.bot` frontmatter block per YAML message
// the SECOND consumer of the remedy table surfaces: the `## ---` block
// `fmt --to yaml` reads with the catalogue's own decoder (yaml.v2), whose
// error pkg/cli/fmt_author.go hands to YAMLRemedy. That reading decodes
// into a Go struct, so it resolves the tags, aliases and merge keys the
// document's node tree leaves alone — no author document reaches these
// messages, and each of them reached an author bare while the table
// answered it with a reason argued about the document's reading.
var frontmatterWitnesses = map[string]witness{
	"a merge key in a frontmatter":         {"name: probe\n<<: 3\n", "`<<` is YAML's merge key"},
	"a merge key taking a list of scalars": {"name: probe\n<<: [1, 2]\n", "write the keys out"},
	"a !!binary value that is not base64":  {"name: !!binary nope!\n", "frontmatter block reads the tag"},
	// … and under a list key, where the sentence owes its author the clause
	// that names the list the key itself takes: the clause is what tells
	// this entry's sentence apart, and what the int twin's witness below
	// rides (TestTheTagClauseIsTrueOfEachOfTheFourKeys measures it).
	"a !!binary value that is not base64, under a list key": {"name: probe\ntriggers: !!binary nope!\n", "whose key itself still takes a list"},
	"a value tagged !!int that is not one":                  {"name: !!int notanint\n", "write it whole in single quotes"},
	// The alias GRAPH, whose two refusals name no sigil written in text
	// and no alias naming no anchor: both blocks name their anchor
	// correctly, so the remedy owes them the cause the resolving met.
	"an alias bomb":                         {frontmatterAliasBomb(), "expands into far more values than the block's own lines"},
	"an anchor whose value holds its alias": {"x: &a {b: *a}\n", "is a cycle the resolving has no end to"},
	"a key that is itself a list":           {"x:\n  ? [a, b]\n  : 1\n", "is no key at all"},
	"a key that is itself a mapping":        {"x:\n  ? {a: 1}\n  : 1\n", "write the key as plain text"},
	"a flow list never closed":              {"name: probe\ntriggers: [a,\n b,\n c\ndescription: d\n", "close the `[` or `{` the value opened"},
	"an alias naming no anchor":             {"name: probe\ndescription: *nope\n", "names an anchor written above it"},
	// The shape family: the block's reading decodes into the four keys'
	// Go types, so a value of another shape is refused by the decoder
	// itself — a message no error constructor of either reader builds,
	// which is why only a witness can hold it (yamlVendorModules). One
	// message string, four causes, which differ by the Go type the reading
	// was decoding into: a witness per cause AND per type
	// (TestEveryGoTypeTheFrontmatterReadingDecodesIntoHasAWitness).
	"a name that is a list":           {"name: [a, b]\n", "one line of text"},
	"a name that is a mapping":        {"name: {a: 1}\n", "one line of text"},
	"a name written as a block list":  {"name:\n  - a\n  - b\n", "one line of text"},
	"a description that is a mapping": {"name: probe\ndescription: {a: 1}\n", "one line of text"},
	"triggers that is a string":       {"name: probe\ntriggers: cron\n", "one `- item` per line"},
	"triggers that is a number":       {"name: probe\ntriggers: 3\n", "`[a, b]` on one line"},
	"triggers that is a mapping":      {"name: probe\ntriggers:\n  a: 1\n", "a list"},
	"capabilities that is a string":   {"name: probe\ncapabilities: shell\n", "a list"},
	// A tagged value under a list key: the tag entry's clause sends its
	// author to the SHAPE entry's remedy, which is the one that names a
	// list — the clause says so rather than prescribing the value the tag
	// names (TestTheTagClauseIsTrueOfEachOfTheFourKeys).
	"a tagged value under a list key": {"name: probe\ntriggers: !!int 3\n", "`[a, b]` on one line"},
	// The block's OWN top level, where what the author wrote is one whole
	// value and no `key: value` line at all: the reading is refused on the
	// struct it decodes into, and the fix is a key — a shape the four
	// keys' own clause never names.
	"a block that is a line of prose": {"A bot that reviews pull requests\n", "write it as `key: value`"},
	"a block that is a list":          {"- name: probe\n- description: d\n", "is one whole value and no key at all"},
	"a block that is a bare number":   {"3\n", "a bare number"},
	"a block that is a `|` value":     {"|\n  a\n", "a `|` or `>` block"},
	// A key of the block's top level is read into a plain string, so a
	// `? ` key whose text is a list or a mapping is refused with the shape
	// family's message rather than with `invalid map key:` — the same
	// author mistake as the two rows above, one level up.
	"a complex key at the block's top level":    {"name: probe\n? [a, b]\n: 1\n", "write the key as plain text"},
	"a complex mapping key at the top level":    {"name: probe\n? {a: 1}\n: 1\n", "a key is plain text"},
	"an item of a list that is itself a list":   {"name: probe\ntriggers:\n  - [a, b]\n", "every item one line of text"},
	"an item of a list that is itself a map":    {"name: probe\ncapabilities:\n  - {a: 1}\n", "every item one line of text"},
	"an item of a flow list that is itself one": {"name: probe\ntriggers: [[a, b]]\n", "every item one line of text"},
	// A value a reader writes back into its own message is the author's own
	// text: a value spelling the words of another entry's message took that
	// entry's remedy, measured on all three slots a reader writes one into
	// — the ten characters of `cannot unmarshal !!str `…``, the whole value
	// of `cannot decode !!str `…` as a !!int`, and the Go rendering of a
	// complex key in `invalid map key: %#v`.
	"a value whose text reads as the tag message":  {"name: probe\ntriggers: \" as a !!\"\n", "one `- item` per line"},
	"a tagged value whose text reads as a message": {"name: !!int \"mapping values are not allowed\"\n", "reads as a YAML tag"},
	"a tagged value whose text reads as a clause":  {"name: !!int \"found a tab character\"\n", "reads as a YAML tag"},
	"a list key whose text reads as a message":     {"x:\n  ? [mapping values are not allowed]\n  : 1\n", "is no key at all"},
	"a mapping key whose text reads as a clause":   {"x:\n  ? {found a tab character: 1}\n  : 1\n", "write the key as plain text"},
	// … and one carrying a DELIMITER of its own in each of the three
	// slots: the words bounding an author's value are the reader's own
	// format, so a backtick or a double quote inside the value is text
	// like any other. The `cannot unmarshal ` row costs no remedy on its
	// own — its family's entry stands before the tag entry — and is held
	// here for the slot rather than for the theft; what the same value
	// costs at the real site is a line the reader never named
	// (pkg/cli: "a value whose text names a line beside the reader's").
	"a value whose text holds a backtick and the tag message":  {"name: probe\ntriggers: \"a` as a !!\"\n", "one `- item` per line"},
	"a tagged value whose text holds a backtick and a message": {"name: !!int \"a`mapping values are not allowed\"\n", "reads as a YAML tag"},
	"a complex key whose text holds a quote and a message":     {"x:\n  ? [\"a\\\"mapping values are not allowed\"]\n  : 1\n", "is no key at all"},
	// … and the two bounds a slot's words are read by. A value spelling
	// the closing word of its own slot: the slot ends at the LAST of them,
	// what follows being the tag the reader names, so an earlier one is
	// the author's. And a value spelling the opening of ANOTHER slot: a
	// slot starts at the EARLIEST opening left, an author's text standing
	// after one and never before it.
	"a tagged value whose text spells its slot's closing word": {"name: !!int \"x as a !!int mapping values are not allowed\"\n", "reads as a YAML tag"},
	"a tagged value whose text spells another slot's opening":  {"name: !!int \"mapping values are not allowed invalid map key: x\"\n", "reads as a YAML tag"},
}

// frontmatterRefusal is the words a frontmatter block's reading is refused
// with, composed as pkg/cli/fmt_author.go composes them: DecodeFrontmatter,
// the error's own text with its whitespace collapsed, then the remedy of
// every structured cause the reading returns. "" where the block reads.
func frontmatterRefusal(block string) (read string, causes []workflowfile.YAMLCause, remedies string) {
	_, _, err := workflowfile.DecodeFrontmatter(block, parser.ReadBudget())
	if err == nil {
		return "", nil, ""
	}
	read = strings.Join(strings.Fields(err.Error()), " ")
	causes = workflowfile.YAMLCauses(workflowfile.YAMLv2, err)
	remedies = YAMLRemedies(causes)
	return read, causes, remedies
}

// prescription is one frontmatter block written as a remedy says, and the
// identity the catalogue then reads off it: a block that reads is only half
// the prescription — quoting a value whole silences the refusal AND makes
// the value the text it looks like, which the author must be told rather
// than discover in the catalogue.
type prescription struct {
	// answers is one message string of the yamlRemedies entry whose remedy
	// prescribes this block, and target the decode target that scopes it
	// where the entry is one of a family keyed on it: the link the row
	// owes its clause, so that an entry a block's reading reaches with
	// nothing written as it says reddens rather than passing for executed
	// (TestEveryFrontmatterRemedyReachedHasAPrescription).
	answers string
	target  string
	block   string
	want    workflowfile.Frontmatter
	extra   []string
}

// frontmatterPrescriptions holds one block per spelling the remedies of
// frontmatterWitnesses prescribe, written as they say and rewritten from
// the witness it answers: a prescription whose block the catalogue's reader
// refuses in turn would send an author round the same refusal, and one it
// reads as another identity would cost them the name their bot is found by.
var frontmatterPrescriptions = map[string]prescription{
	// `<<: 3` with its keys written out as keys of the block: the
	// catalogue carries the four it knows and names the rest.
	"a merge key's keys written out":    {answers: "map merge requires map or sequence of maps as the value", block: "name: probe\nk: 1\n", want: workflowfile.Frontmatter{Name: "probe"}, extra: []string{"k"}},
	"a merge key quoted inside a value": {answers: "map merge requires map or sequence of maps as the value", block: "name: probe\ndescription: '<<: 3'\n", want: workflowfile.Frontmatter{Name: "probe", Description: "<<: 3"}},
	"a !!binary value in single quotes": {answers: "!!binary value contains invalid base64 data", block: "name: '!!binary nope!'\n", want: workflowfile.Frontmatter{Name: "!!binary nope!"}},
	"a tagged value in single quotes":   {answers: workflowfile.YAMLDecodeFormat, block: "name: '!!int notanint'\n", want: workflowfile.Frontmatter{Name: "!!int notanint"}},
	// The tag entry's OTHER prescription, scoped by the key: the value the
	// tag names, which `name` and `description` take and an item of a list
	// key takes too.
	"a !!binary value the tag names, under a text key": {answers: "!!binary value contains invalid base64 data", block: "name: !!binary aGk=\n", want: workflowfile.Frontmatter{Name: "hi"}},
	"a !!int value the tag names, under a text key":    {answers: workflowfile.YAMLDecodeFormat, block: "description: !!int 3\n", want: workflowfile.Frontmatter{Description: "3"}},
	"a tagged item of a list key":                      {answers: workflowfile.YAMLDecodeFormat, block: "name: probe\ntriggers: [!!int 3]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"3"}}},
	// The alias graph entry's prescription: the keys written out where
	// they are used, which is what a cycle and an expansion too large both
	// leave their author.
	"an anchor's keys written out where they are used": {answers: workflowfile.YAMLAnchorSelfRef, block: "name: probe\nx:\n  b: 1\n", want: workflowfile.Frontmatter{Name: "probe"}, extra: []string{"x"}},
	// The two readings of a `*`: the text it is part of, in single quotes,
	// and the alias of an anchor the block writes above it.
	"a star quoted whole, as the text it is":  {answers: workflowfile.YAMLUnknownAnchor, block: "name: probe\ndescription: '*nope'\n", want: workflowfile.Frontmatter{Name: "probe", Description: "*nope"}},
	"an alias naming an anchor written above": {answers: workflowfile.YAMLUnknownAnchor, block: "name: &a probe\ndescription: *a\n", want: workflowfile.Frontmatter{Name: "probe", Description: "probe"}},
	"a complex key written as plain text":     {answers: workflowfile.YAMLMapKeyFormat, block: "x:\n  a: 1\n", extra: []string{"x"}},
	"a flow list closed":                      {answers: "did not find expected ',' or ']'", block: "name: probe\ntriggers: [a,\n b,\n c]\ndescription: d\n", want: workflowfile.Frontmatter{Name: "probe", Description: "d", Triggers: []string{"a", "b", "c"}}},
	// The shape of each of the four keys, as the shape remedy names them:
	// one row per decode target the family is keyed on, so the entry a
	// block reaches is the one whose spelling the row writes.
	"a name as one line of text":              {answers: workflowfile.YAMLUnmarshalFormat, target: "string", block: "name: a probe\n", want: workflowfile.Frontmatter{Name: "a probe"}},
	"a description as one line of text":       {answers: workflowfile.YAMLUnmarshalFormat, target: "string", block: "name: probe\ndescription: a line\n", want: workflowfile.Frontmatter{Name: "probe", Description: "a line"}},
	"triggers as a flow list on one line":     {answers: workflowfile.YAMLUnmarshalFormat, target: "[]string", block: "name: probe\ntriggers: [cron, push]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"cron", "push"}}},
	"triggers as one item per line":           {answers: workflowfile.YAMLUnmarshalFormat, target: "[]string", block: "name: probe\ntriggers:\n  - cron\n  - push\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"cron", "push"}}},
	"capabilities as a flow list on one line": {answers: workflowfile.YAMLUnmarshalFormat, target: "[]string", block: "name: probe\ncapabilities: [shell]\n", want: workflowfile.Frontmatter{Name: "probe", Capabilities: []string{"shell"}}},
	"capabilities as one item per line":       {answers: workflowfile.YAMLUnmarshalFormat, target: "[]string", block: "name: probe\ncapabilities:\n  - shell\n", want: workflowfile.Frontmatter{Name: "probe", Capabilities: []string{"shell"}}},
	// The block's own top level, written as `key: value` lines: the prose
	// the remedy's example names, and a complex key written as plain text
	// one level up from the nested spelling above.
	"a line of prose written as a key":                   {answers: workflowfile.YAMLUnmarshalFormat, target: "workflowfile.frontmatterFields", block: "description: a bot that reviews pull requests\n", want: workflowfile.Frontmatter{Description: "a bot that reviews pull requests"}},
	"a list block written as the keys it meant":          {answers: workflowfile.YAMLUnmarshalFormat, target: "workflowfile.frontmatterFields", block: "name: probe\ndescription: d\n", want: workflowfile.Frontmatter{Name: "probe", Description: "d"}},
	"a top-level complex key written as plain text":      {answers: workflowfile.YAMLUnmarshalFormat, target: "string", block: "name: probe\na: 1\n", want: workflowfile.Frontmatter{Name: "probe"}, extra: []string{"a"}},
	"an item of a list written as one line of text":      {answers: workflowfile.YAMLUnmarshalFormat, target: "string", block: "name: probe\ntriggers:\n  - a b\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"a b"}}},
	"an item of a flow list written as one line of text": {answers: workflowfile.YAMLUnmarshalFormat, target: "string", block: "name: probe\ntriggers: [a b]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"a b"}}},
}

// frontmatterUnmarshalTargetOf is the decode target the reading's cause
// names for a whole-value block: the reading names it itself, and
// workflowfile pins the name in its own package.
func frontmatterBlockTarget() string {
	_, causes, _ := frontmatterRefusal("- a\n")
	if len(causes) != 1 || causes[0].Message != workflowfile.YAMLUnmarshalFormat {
		return ""
	}
	return causes[0].Target
}

// frontmatterUnmarshalTargets are the decode targets the family's causes
// name: the leaves of the type the reading decodes a block into, walked off
// a mirror of it rather than listed — a fifth key, or a list of something
// other than text, adds a target here. The block's own target is not
// walked: the reading names it itself (frontmatterBlockTarget), and
// workflowfile pins the struct's name in its own package.
//
// Three rules, each the vendored decoder's own: a struct's keys are read
// into a plain `string` whatever its fields are named (mappingStruct's
// `settableValueOf("")`), an `interface{}` takes any node so no refusal
// ever names it, and a `,inline` field is filled from the parent's own
// mapping — key by key for a map, field by field for a struct — so no node
// is ever decoded into it whole.
func frontmatterUnmarshalTargets() []string {
	var mirror struct {
		workflowfile.Frontmatter `yaml:",inline"`
		Rest                     map[string]any `yaml:",inline"`
	}
	seen := map[string]bool{}
	var walk, walkFields func(reflect.Type)
	walkFields = func(st reflect.Type) {
		seen[reflect.TypeOf("").String()] = true
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			inline := slices.Contains(strings.Split(f.Tag.Get("yaml"), ","), "inline")
			switch {
			case inline && f.Type.Kind() == reflect.Struct:
				walkFields(f.Type)
			case inline && f.Type.Kind() == reflect.Map:
				walk(f.Type.Elem())
			default:
				walk(f.Type)
			}
		}
	}
	walk = func(t reflect.Type) {
		if t.Kind() == reflect.Interface || seen[t.String()] {
			return
		}
		seen[t.String()] = true
		switch t.Kind() {
		case reflect.Struct:
			walkFields(t)
		case reflect.Slice, reflect.Array:
			walk(t.Elem())
		case reflect.Map:
			walk(t.Key())
			walk(t.Elem())
		}
	}
	walk(reflect.TypeOf(mirror))
	// The mirror's own type is dropped: it is the type the reading decodes
	// the whole block into, which the reading names itself
	// (frontmatterBlockTarget) — the walk returns the leaves alone.
	delete(seen, reflect.TypeOf(mirror).String())
	targets := make([]string, 0, len(seen))
	for t := range seen {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	return targets
}

// `cannot unmarshal ` is ONE message whose causes differ by the Go type
// the reading was decoding into, and that type is what a remedy naming one
// cause leaves the other causes' authors without: the family is keyed on
// it, one entry per type the reading can be asked to decode a node into,
// and one witness per type reaches each. A fifth key of another shape, or
// a list of something other than text, reddens here until a block reaches
// it.
//
// The type walked is checked against the one that RUNS twice over: the
// reading's own refusal of a block that is not a mapping names its decode
// target — the named struct, which workflowfile pins in its own package —
// and every target a witness's cause carries is one the walk reaches, so a
// mirror that drifts from workflowfile.DecodeFrontmatter cannot pass for
// it.
func TestEveryGoTypeTheFrontmatterReadingDecodesIntoHasAWitness(t *testing.T) {
	root := frontmatterBlockTarget()
	if root == "" {
		t.Fatal("the reading refuses no block that is not a mapping with the unmarshal format: the block's own decode target is read off it")
	}
	if root != "workflowfile.frontmatterFields" {
		t.Errorf("the reading refuses a whole-value block with the decode target %q, not the named struct: the type the family's first entry is keyed on moved", root)
	}
	reached := map[string]string{}
	for name, w := range frontmatterWitnesses {
		_, causes, _ := frontmatterRefusal(w.src)
		for _, c := range causes {
			if c.Message == workflowfile.YAMLUnmarshalFormat {
				reached[c.Target] = name
			}
		}
	}
	targets := append(frontmatterUnmarshalTargets(), root)
	for _, target := range targets {
		if reached[target] == "" {
			t.Errorf("no frontmatter witness is refused with `cannot unmarshal … into %s`: that cause of the family reaches an author with a remedy nothing proves answers it", target)
		}
	}
	// … and no witness names a type the walk does not know: a target the
	// walk misses is a cause the gate above cannot ask for.
	for target, name := range reached {
		if !slices.Contains(targets, target) {
			t.Errorf("%s is refused with `cannot unmarshal … into %s`, a type the walk of the reading's own type does not reach: the walk is the gate's inventory and it is short", name, target)
		}
	}
}

// A frontmatter block written as its remedy says is one the catalogue's
// reader reads, AS the identity the prescription names: a spelling that
// silences the refusal by changing what the block says is one the remedy
// owes its author in words, not one the gate may accept in silence.
func TestAFrontmatterWrittenAsItsRemedySaysReads(t *testing.T) {
	for name, p := range frontmatterPrescriptions {
		fm, extra, err := workflowfile.DecodeFrontmatter(p.block, parser.ReadBudget())
		if err != nil {
			t.Errorf("%s does not read: %s", name, strings.Join(strings.Fields(err.Error()), " "))
			continue
		}
		if fmt.Sprintf("%+v", *fm) != fmt.Sprintf("%+v", p.want) {
			t.Errorf("%s reads as %+v, not as the identity it prescribes %+v", name, *fm, p.want)
		}
		if len(extra)+len(p.extra) > 0 && fmt.Sprint(extra) != fmt.Sprint(p.extra) {
			t.Errorf("%s leaves the keys %v beside the catalogue's four, not %v", name, extra, p.extra)
		}
	}
}

// yamlRemedyOf is the index of the entry that answers cause, read by
// running the matcher itself, -1 where none does and -2 where several
// would: the entry a refusal reached, read off the cause rather than off a
// name.
func yamlRemedyOf(cause workflowfile.YAMLCause) int {
	found := -1
	for i, r := range yamlRemedies {
		if !slices.Contains(r.readers, cause.Reader) ||
			r.target != "" && r.target != cause.Target ||
			!slices.Contains(r.messages, cause.Message) {
			continue
		}
		if found >= 0 {
			return -2
		}
		found = i
	}
	return found
}

// Every entry a frontmatter block's reading reaches has a block written as
// its remedy says, and every prescription row names the entry it answers —
// by its message, and by its decode target where the entry is one of a
// family keyed on it: the identity test walks hand-written blocks, so
// without this link an entry could carry a spelling nothing ever writes.
// The link is the entry's own cause key, read against the table — a
// reworded message or a moved target reddens here rather than leaving the
// row answering for nothing.
func TestEveryFrontmatterRemedyReachedHasAPrescription(t *testing.T) {
	answered := map[int]bool{}
	for name, p := range frontmatterPrescriptions {
		at := -1
		for i, r := range yamlRemedies {
			if slices.Contains(r.messages, p.answers) && r.target == p.target {
				at = i
				break
			}
		}
		if at < 0 {
			t.Errorf("%s answers %q at target %q, which no entry of yamlRemedies is keyed on: the row prescribes a spelling the table does not say", name, p.answers, p.target)
			continue
		}
		answered[at] = true
	}
	for name, w := range frontmatterWitnesses {
		_, causes, remedies := frontmatterRefusal(w.src)
		if remedies == "" {
			// Said by TestEveryFrontmatterYAMLMessageNamesItsRemedy.
			continue
		}
		for _, c := range causes {
			switch at := yamlRemedyOf(c); {
			case at == -2:
				t.Errorf("the refusal of %s is answered by more than one entry: the entry a block reaches cannot be read off its cause %v", name, c)
			case at < 0:
				t.Errorf("the cause %v of %s is answered by no entry: %s", c, name, remedies)
			case !answered[at]:
				t.Errorf("%s reaches remedy %d, which no row of frontmatterPrescriptions answers: its spelling is prescribed to an author and written nowhere", name, at)
			}
		}
	}
}

// The tag entry's clause about the block's reading is true of EACH of the
// four keys, executed through that reading: a `name` and a `description`
// take the value the tag names, an item of `triggers` or `capabilities`
// takes it too, and the two list keys themselves still take a list — a
// tagged value under one is refused again, with the shape entry's remedy.
// The class is the four the clause used to be false for: two list keys
// times two tag families, each of which the clause sent to that second
// refusal with the prescription of a value no key would hold.
func TestTheTagClauseIsTrueOfEachOfTheFourKeys(t *testing.T) {
	read, _, remedy := frontmatterRefusal("name: !!int notanint\n")
	if remedy == "" {
		t.Fatalf("%q reaches the author bare, so no clause of the tag entry is under test", read)
	}
	for _, want := range []string{
		"`name` and `description` then take the value the tag names",
		"an item of `triggers` or `capabilities`, whose key itself still takes a list",
	} {
		if !strings.Contains(remedy, want) {
			t.Errorf("the tag remedy does not scope its clause by the key (%q): %s", want, remedy)
		}
	}
	for name, p := range map[string]prescription{
		"a !!binary value under name":     {block: "name: !!binary aGk=\n", want: workflowfile.Frontmatter{Name: "hi"}},
		"a !!int value under name":        {block: "name: !!int 3\n", want: workflowfile.Frontmatter{Name: "3"}},
		"a !!int value under description": {block: "description: !!int 3\n", want: workflowfile.Frontmatter{Description: "3"}},
		"a tagged item of triggers":       {block: "name: probe\ntriggers: [!!int 3]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"3"}}},
		"a tagged item of capabilities":   {block: "name: probe\ncapabilities: [!!binary aGk=]\n", want: workflowfile.Frontmatter{Name: "probe", Capabilities: []string{"hi"}}},
		"a tagged item on its own line":   {block: "name: probe\ntriggers:\n  - !!int 3\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"3"}}},
	} {
		fm, _, err := workflowfile.DecodeFrontmatter(p.block, parser.ReadBudget())
		switch {
		case err != nil:
			t.Errorf("%s is refused (%s), where the clause says the value the tag names is taken", name, strings.Join(strings.Fields(err.Error()), " "))
		case fmt.Sprintf("%+v", *fm) != fmt.Sprintf("%+v", p.want):
			t.Errorf("%s reads as %+v, not as the identity the clause names %+v", name, *fm, p.want)
		}
	}
	// The other half, for the class the clause was false for: the key
	// itself takes a list, so the author of one is sent to the remedy that
	// names a list rather than to a value their key would refuse again.
	for _, key := range []string{"triggers", "capabilities"} {
		for _, value := range []string{"!!int 3", "!!binary aGk="} {
			block := "name: probe\n" + key + ": " + value + "\n"
			read, _, remedy := frontmatterRefusal(block)
			switch {
			case read == "":
				t.Errorf("`%s: %s` reads, where the clause tells its author the key takes a list", key, value)
			case !strings.Contains(remedy, "`[a, b]` on one line"):
				t.Errorf("`%s: %s` is refused with %q, whose remedy does not name the list the key takes: %s", key, value, read, remedy)
			}
		}
	}
}

// The sentences of one refusal with several causes compose as sentences:
// each sentence is built with the dash its position note hangs on, so the
// join takes the dash off every sentence but the first — a doubled space
// and a second dash mid-notice read as a clause of their own, and are
// neither.
func TestTheSentencesOfAMultiCauseRefusalJoinBare(t *testing.T) {
	_, causes, remedies := frontmatterRefusal("name: [a]\ntriggers: x\n")
	if len(causes) != 2 || remedies == "" {
		t.Fatalf("the block is refused with %d causes (%q): two shape mistakes are the pair the join is read on", len(causes), remedies)
	}
	if strings.Contains(remedies, ";  ") {
		t.Errorf("a doubled space survives between the two sentences: %q", remedies)
	}
	if !strings.Contains(remedies, "; `triggers` and `capabilities` take a list") {
		t.Errorf("the second sentence reaches the author with the first sentence's dash still on it: %q", remedies)
	}
	if !strings.HasPrefix(remedies, " — ") {
		t.Errorf("the first sentence lost the dash head its position note hangs on: %q", remedies)
	}
}

// Two causes of the same family answer with one sentence, not two copies
// of it: the notice names the fix, and saying it twice says it once too
// often.
func TestTheSameSentenceAnswersOnce(t *testing.T) {
	_, causes, remedies := frontmatterRefusal("triggers: x\ncapabilities: y\n")
	if len(causes) != 2 {
		t.Fatalf("the block is refused with %d causes: two list keys read as scalars are the pair the dedup is read on", len(causes))
	}
	if n := strings.Count(remedies, "take a list"); n != 1 {
		t.Errorf("the list sentence appears %d times in %q: the same sentence answers once", n, remedies)
	}
}

// A line the reader names that the block holds blank quotes nothing: the
// note exists to hand the author the text they read off their own file, and
// a pair of empty backticks quotes none. A blank line is skipped as an
// out-of-range number is, alone or among several.
func TestABlockLineNoteQuotesNoBlankLine(t *testing.T) {
	if got := YAMLBlockLineNote([]int{2}, []string{"name: \"unterminated", ""}); got != "" {
		t.Errorf("the note quotes the block's blank line: %q", got)
	}
	mixed := YAMLBlockLineNote([]int{1, 2}, []string{"name: \"unterminated", ""})
	if want := ", where the line it names is the block's own, not the file's — `name: \"unterminated`"; mixed != want {
		t.Errorf("the note of a blank line beside a named one is %q, not the one named line alone (%q)", mixed, want)
	}
	plural := YAMLBlockLineNote([]int{1, 2, 3}, []string{"a: 1", "", "b: 2"})
	if want := ", where the lines it names are the block's own, not the file's — line 1 `a: 1`, line 3 `b: 2`"; plural != want {
		t.Errorf("the note of a blank line among several named ones is %q, not the two named lines alone (%q)", plural, want)
	}
}

// Every YAML message the frontmatter block's reading surfaces arrives with
// the spelling that avoids it. Read through the two calls fmt_author.go
// makes, in its order: a message this table answers with a reason instead
// of a remedy reaches an author bare in the notice `fmt --to yaml` prints.
func TestEveryFrontmatterYAMLMessageNamesItsRemedy(t *testing.T) {
	for name, w := range frontmatterWitnesses {
		read, _, remedy := frontmatterRefusal(w.src)
		switch {
		case read == "":
			t.Errorf("%s: the frontmatter block reads, so it witnesses no message: %q", name, w.src)
		case remedy == "":
			t.Errorf("%s: %q reaches the author bare", name, read)
		case !strings.Contains(remedy, w.want):
			t.Errorf("%s: the remedy of %q does not name %q: %s", name, read, w.want, remedy)
		}
	}
}

// The anchors, the explicit tags and the merge keys a `.bot` frontmatter
// block RESOLVES are said of that reading, and the author document's
// refusal of them of the document's: one sentence asserting both readings
// refuse a construct is false for the reading that reads it, and sends the
// author of a block looking for a mistake the block does not hold. Both
// halves are executed — the identity the block reads off the construct, and
// the words the document refuses it with.
func TestAConstructOneReadingResolvesIsSaidOfThatReading(t *testing.T) {
	for _, tc := range []struct {
		name string
		// refused is a block whose message reaches the entry under test.
		refused string
		// resolved is a block holding the construct, and the identity the
		// catalogue's reader builds from it.
		resolved string
		reads    workflowfile.Frontmatter
		// doc is an author document holding the same construct, and the
		// words its own reader refuses it with.
		doc, docSays string
		// saysOfTheBlock and saysOfTheDocument are the clauses the remedy
		// owes each reading.
		saysOfTheBlock, saysOfTheDocument string
	}{
		{
			name:              "an anchor and its alias",
			refused:           "name: probe\ndescription: *nope\n",
			resolved:          "name: &a probe\ndescription: *a\n",
			reads:             workflowfile.Frontmatter{Name: "probe", Description: "probe"},
			doc:               "dsl: 2\nvars: &a {}\nmeta: *a\nnodes:\n  - agent: a\n    model: m\n",
			docSays:           "an alias (*a) is not read: write the value where it is used",
			saysOfTheBlock:    "a `.bot` frontmatter block resolves both",
			saysOfTheDocument: "the author document reads neither",
		},
		{
			name:              "an alias graph the resolving cannot read",
			refused:           "x: &a {b: *a}\n",
			resolved:          "name: probe\nx: &a {b: 1}\nk: *a\n",
			reads:             workflowfile.Frontmatter{Name: "probe"},
			doc:               "dsl: 2\nvars: &a {}\nmeta: *a\nnodes:\n  - agent: a\n    model: m\n",
			docSays:           "an anchor (&a) is not read: write the value where it is used",
			saysOfTheBlock:    "a `.bot` frontmatter block resolves its anchors and aliases",
			saysOfTheDocument: "the author document reads no anchor and no alias",
		},
		{
			name:              "an explicit tag",
			refused:           "name: !!int notanint\n",
			resolved:          "name: !!str probe\n",
			reads:             workflowfile.Frontmatter{Name: "probe"},
			doc:               "dsl: 2\nnodes:\n  - agent: a\n    model: !!str m\n",
			docSays:           "an explicit tag (!!str) is not read",
			saysOfTheBlock:    "a `.bot` frontmatter block reads the tag",
			saysOfTheDocument: "the author document carries none",
		},
		{
			name:              "a merge key",
			refused:           "name: probe\n<<: 3\n",
			resolved:          "x: &a {k: 1}\n<<: *a\nname: probe\n",
			reads:             workflowfile.Frontmatter{Name: "probe"},
			doc:               "dsl: 2\n<<: {j: 2}\nnodes:\n  - agent: a\n    model: m\n",
			docSays:           "a merge key (<<) is not read",
			saysOfTheBlock:    "a `.bot` frontmatter block merges the keys in",
			saysOfTheDocument: "the author document takes none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read, _, remedy := frontmatterRefusal(tc.refused)
			if remedy == "" {
				t.Fatalf("%q reaches the author bare, so no sentence of the entry is under test", read)
			}
			// The reading that resolves the construct, executed: a remedy
			// claiming it refuses one would be false of the identity below.
			fm, _, err := workflowfile.DecodeFrontmatter(tc.resolved, parser.ReadBudget())
			if err != nil {
				t.Fatalf("the block %q is refused — the construct is no longer one this reading resolves: %v", tc.resolved, err)
			}
			if got := fmt.Sprintf("%+v", *fm); got != fmt.Sprintf("%+v", tc.reads) {
				t.Errorf("the block %q reads as %s, not as %+v", tc.resolved, got, tc.reads)
			}
			if !strings.Contains(remedy, tc.saysOfTheBlock) {
				t.Errorf("the remedy does not say what the block's reading does with it (%q): %s", tc.saysOfTheBlock, remedy)
			}
			// The reading that refuses it, in its own words.
			if ok, got := refusedWith(tc.doc, parser.DiagAuthorDocument, tc.docSays); !ok {
				t.Errorf("the author document no longer refuses the construct with %q: %s", tc.docSays, got)
			}
			if !strings.Contains(remedy, tc.saysOfTheDocument) {
				t.Errorf("the remedy does not say what the document's reading does with it (%q): %s", tc.saysOfTheDocument, remedy)
			}
			// The sentence that asserted one refusal for both readings.
			if strings.Contains(remedy, "nor a `.bot` frontmatter block") {
				t.Errorf("the remedy tells the author a `.bot` frontmatter block refuses what it resolves: %s", remedy)
			}
		})
	}
}

// The anchor name is the one author text a reader writes into its own
// message with no slot around it: it ends at the first space — `name: &a b
// probe` reads as the value `b probe` — so the skeleton the cause carries
// (`unknown anchor '%s' referenced`, `anchor '%s' value contains itself`)
// is recognized by its own head and tail alone, and the name inside
// chooses nothing. The document's reader writes the same message, and the
// entry its refusal takes is the anchor's: an alias spelling the words of
// another entry's message takes that other entry's remedy nowhere.
func TestAnAnchorNameChoosesNothing(t *testing.T) {
	fm, _, err := workflowfile.DecodeFrontmatter("name: &a b probe\n", parser.ReadBudget())
	switch {
	case err != nil:
		t.Fatalf("`name: &a b probe` is refused (%v): an anchor name's bound is no longer the first space, and the formats below are recognizable by their own words alone", err)
	case fm.Name != "b probe":
		t.Fatalf("`name: &a b probe` reads as %q, not as the value `b probe`: an anchor name can now hold a space", fm.Name)
	}
	read, causes, remedies := frontmatterRefusal("name: probe\ndescription: *mapping values are not allowed\n")
	if want := "unknown anchor 'mapping' referenced"; !strings.Contains(read, want) {
		t.Fatalf("an alias spelling a message of the table is refused with %q, not with %q: the name the reader writes back is no longer bounded by a space", read, want)
	}
	if len(causes) != 1 || causes[0].Message != workflowfile.YAMLUnknownAnchor {
		t.Errorf("the block is refused with the causes %v: an anchor name spelling the words of another entry's message moved the cause off the anchor's own", causes)
	}
	if strings.Contains(remedies, "quote the whole value (a command with a jq filter") {
		t.Errorf("the refusal takes the colon entry's remedy, which the anchor name spelled: %s", remedies)
	}
	if at := yamlRemedyOf(causes[0]); at < 0 {
		t.Errorf("the anchor refusal's cause is answered by no entry: %s", remedies)
	}
	// The document's reader writes the same message: the same cause, the
	// anchor entry's sentence.
	if ok, got := refusedWith("dsl: 2\nvars: *mapping values are not allowed\nnodes:\n  - agent: a\n    model: m\n", parser.DiagAuthorDocument, "names an anchor written above it"); !ok {
		t.Errorf("the document's reader no longer refuses an unknown anchor with the anchor entry's remedy:\n%s", got)
	}
	// The line regex serves the document site's own position rewrite alone
	// (yamlErrorAtLine): the note and the remedies are keyed on numbers the
	// reading names, never on these words.
	if yamlLineRe.MatchString("line1:") {
		t.Errorf("%s matches a number with no space before it: a value a reader quotes could then add a line to a message the rewrite touches", yamlLineRe)
	}
}

// Every message the remedy table is keyed on is one a vendored reader
// builds: a constant the reader's source raises, or the format its source
// quotes a value into, spelled as the scan of that reader's own files
// reads it. A key no reader builds is a sentence hung on nothing, and a
// reader that rewords one reddens here rather than leaving an entry
// answering for a message nobody writes.
func TestEveryEntryMessageIsABuiltLiteral(t *testing.T) {
	built := map[workflowfile.YAMLReader]map[string]bool{}
	for _, mod := range yamlVendorModules {
		reader := workflowfile.YAMLReader(mod.name)
		built[reader] = map[string]bool{}
		for lit := range yamlProblemStrings(t, mod.path) {
			// The terrors' own line head is the format's first words, not
			// part of its message: the entries are keyed on the message
			// after it, as the reading is.
			built[reader][strings.TrimPrefix(lit, "line %d: ")] = true
		}
	}
	for i, r := range yamlRemedies {
		for _, m := range r.messages {
			for _, reader := range r.readers {
				if !built[reader][m] {
					t.Errorf("remedy %d is keyed on %q, which the vendored %s builds nowhere: the entry's message is not the reader's own", i, m, reader)
				}
			}
		}
	}
}

// No two entries answer one cause: the key is (reader, message, decode
// target), so a cause has one sentence wherever it reaches an author. The
// matcher takes the first entry that answers — with exact keys nothing can
// shadow anything, and a key written twice is a copy that will drift.
func TestEveryEntryKeyIsItsOwn(t *testing.T) {
	seen := map[[3]string]int{}
	for i, r := range yamlRemedies {
		for _, reader := range r.readers {
			for _, m := range r.messages {
				key := [3]string{string(reader), m, r.target}
				if had, dup := seen[key]; dup {
					t.Errorf("remedy %d answers %v, which remedy %d is keyed on already: one cause, one sentence", i, key, had)
				}
				seen[key] = i
			}
		}
	}
}

// yamlRefusalEntry is the entry whose remedy the document refusal ends
// with, -1 where none does and -2 where several would: the entry the
// document's refusal reached, read off the words it carries. The remedies
// are pairwise distinct tails, which TestEveryEntryRemedyIsItsOwn holds.
func yamlRefusalEntry(refusal string) int {
	found := -1
	for i, r := range yamlRemedies {
		if !strings.HasSuffix(refusal, r.remedy) {
			continue
		}
		if found >= 0 {
			return -2
		}
		found = i
	}
	return found
}

// Every YAML message the author path surfaces arrives with the spelling
// that avoids it, and a message several mistakes produce names each of
// them: a reader's own words say what it saw, never what to write. No
// remedy of the table is dead and no message of a remedy unreached: each is
// one a document above is refused with, or one the block's reading names
// as its own cause.
func TestEveryYAMLMessageNamesTheRemedyOfEachOfItsCauses(t *testing.T) {
	reached := map[[2]int]bool{}
	for name, w := range yamlWitnesses {
		var refusal string
		var all []string
		for _, d := range Parse("x.bot.yaml", []byte(w.src)).Diagnostics {
			all = append(all, d.Error())
			if refusal == "" && d.Code == parser.DiagAuthorDocument && d.Severity == parser.SeverityError {
				refusal = d.Message
			}
		}
		switch {
		case refusal == "":
			t.Errorf("%s: no E050 refusal, got:\n%s", name, strings.Join(all, "\n"))
		case len(all) != 1:
			t.Errorf("%s: a document that is not YAML is refused once, got %d:\n%s", name, len(all), strings.Join(all, "\n"))
		case !strings.Contains(refusal, w.want):
			t.Errorf("%s: the refusal does not name the remedy %q: %s", name, w.want, refusal)
		}
		switch at := yamlRefusalEntry(refusal); {
		case at == -2:
			t.Errorf("the refusal of %s ends with the remedy of more than one entry: the entry it reached cannot be read off its words", name)
		case at < 0:
			t.Errorf("the refusal of %s ends with no entry's remedy: %s", name, refusal)
		default:
			// The refusal's own words carry the message that fired, with a
			// quoted value where the format's placeholder stands: its head
			// — the words before the first one — is in the refusal, and no
			// two messages of one entry share a head.
			for j, m := range yamlRemedies[at].messages {
				if head, _, _ := strings.Cut(m, "%"); strings.Contains(refusal, head) {
					reached[[2]int{at, j}] = true
				}
			}
		}
	}
	// The table has two consumers, so its coverage has two witness sets: a
	// message no author document can raise is reached through the
	// frontmatter block instead, never left unwitnessed.
	for name, w := range frontmatterWitnesses {
		_, causes, remedies := frontmatterRefusal(w.src)
		if causes == nil || remedies == "" {
			// Said by TestEveryFrontmatterYAMLMessageNamesItsRemedy.
			continue
		}
		for _, c := range causes {
			switch at := yamlRemedyOf(c); {
			case at == -2:
				t.Errorf("the refusal of %s is answered by more than one entry: %s", name, remedies)
			case at < 0:
				t.Errorf("the cause %v of %s is answered by no entry: %s", c, name, remedies)
			default:
				for j, m := range yamlRemedies[at].messages {
					if m == c.Message {
						reached[[2]int{at, j}] = true
					}
				}
			}
		}
	}
	for i, r := range yamlRemedies {
		for j, m := range r.messages {
			if !reached[[2]int{i, j}] {
				t.Errorf("remedy %d answers %q, which no document and no frontmatter block above is refused with", i, m)
			}
		}
	}
}

// No entry's sentence is another's tail: the entry a refusal reached is
// read off the words it ends with, and one sentence ending with another's
// leaves that read ambiguous.
func TestEveryEntryRemedyIsItsOwn(t *testing.T) {
	for i, a := range yamlRemedies {
		for j, b := range yamlRemedies {
			if i != j && strings.HasSuffix(b.remedy, a.remedy) {
				t.Errorf("remedy %d ends with remedy %d's sentence: the entry a refusal reached could not be read off its words", j, i)
			}
		}
	}
}

// A value standing where a `key: value` pair belongs can span several
// lines, in each of the four messages that answer one: the refusal says so,
// the document the prescription leaves — the construct deleted whole —
// reads, and one line of the construct deleted instead is refused again,
// which is why the clause names the lines rather than "that line". The
// clause is one shared sentence naming all four shapes, so no row asserts
// which shape it names: that assertion is implied by the whole clause and
// reddens for nothing.
func TestARefusalOfAStrayValueNamesTheLinesItSpans(t *testing.T) {
	for name, tc := range map[string]struct{ src, deletedWhole, oneLineGone string }{
		"a two-line title above the first key": {"my first\nbot\n" + readsAloneHead + readsAloneBody, readsAloneHead + readsAloneBody, "bot\n" + readsAloneHead + readsAloneBody},
		"a two-line title between two keys":    {readsAloneHead + "my first\nbot\n" + readsAloneBody, readsAloneHead + readsAloneBody, readsAloneHead + "bot\n" + readsAloneBody},
		"a two-line title above a comment":     {"my first\nbot\n# a note\n" + readsAloneHead + readsAloneBody, "# a note\n" + readsAloneHead + readsAloneBody, "bot\n# a note\n" + readsAloneHead + readsAloneBody},
		"a two-line quoted first value":        {"\"a\nb\"\n# a note\n" + readsAloneHead + readsAloneBody, "# a note\n" + readsAloneHead + readsAloneBody, "b\"\n# a note\n" + readsAloneHead + readsAloneBody},
		"a flow list broken over two lines":    {"[a,\n b]\n# a note\n" + readsAloneHead + readsAloneBody, "# a note\n" + readsAloneHead + readsAloneBody, " b]\n# a note\n" + readsAloneHead + readsAloneBody},
		"a flow list broken between two keys":  {readsAloneHead + "[a,\n b]\n" + readsAloneBody, readsAloneHead + readsAloneBody, readsAloneHead + " b]\n" + readsAloneBody},
		"a block scalar between two keys":      {readsAloneHead + "|\n  a title\n" + readsAloneBody, readsAloneHead + readsAloneBody, readsAloneHead + "  a title\n" + readsAloneBody},
		"a block scalar as the first lines":    {"|\n  a title\n" + readsAloneHead + readsAloneBody, readsAloneHead + readsAloneBody, "  a title\n" + readsAloneHead + readsAloneBody},
		"a two-item list between two keys":     {readsAloneHead + "- a\n- b\n" + readsAloneBody, readsAloneHead + readsAloneBody, readsAloneHead + "- b\n" + readsAloneBody},
		"a two-item list as the first lines":   {"- a\n- b\n" + readsAloneHead + readsAloneBody, readsAloneHead + readsAloneBody, "- b\n" + readsAloneHead + readsAloneBody},
		"a list of mapping items between keys": {readsAloneHead + "- a: 1\n  b: 2\n" + readsAloneBody, readsAloneHead + readsAloneBody, readsAloneHead + "  b: 2\n" + readsAloneBody},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, got := refusedWith(tc.src, parser.DiagAuthorDocument, strayLineSpansWant); !ok {
				t.Errorf("the refusal does not say the value spans the lines under it:\n%s", got)
			}
			if res := Parse("x.bot.yaml", []byte(tc.deletedWhole)); res.HasErrors() {
				t.Errorf("the document the prescription leaves does not read: %v", res.Diagnostics)
			}
			if res := Parse("x.bot.yaml", []byte(tc.oneLineGone)); !res.HasErrors() {
				t.Errorf("one line of the construct deleted reads, so the clause need not name the lines it spans:\n%s", tc.oneLineGone)
			}
		})
	}
}

// A refusal whose line need not be the line of the mistake says so:
// yaml.v3's fail() prefers the context mark — where it began reading the
// value — over the mark of the token that broke, so the messages of
// yamlContextProblems name a line of the construct's header, which a
// document opening with a comment puts in the file's own. Every author
// document of the conformance corpus opens with one. A scanner-raised
// message is not in the class: its line IS the line that broke, so the note
// would be noise and must be absent. Each row carries the line the refusal
// lands on and the line the author must edit, and the note's claim — the
// value can start above the line named and end below it — is witnessed in
// both directions rather than assumed: a context mark on the file's first
// line is 0, which sends yaml.v3 to the problem mark and leaves the
// construct's own start above the line named.
func TestARefusalWhoseLineIsNotTheMistakesSaysSo(t *testing.T) {
	rows := map[string]struct {
		src     string
		at      int // the line the refusal lands on
		mistake int // the line the author must edit
		saysSo  bool
	}{
		"a stray item under a leading comment":        {"# my bot\n" + readsAloneHead + "imports:\n  - lib/a.bot\n- a\n", 1, 5, true},
		"a stray item under two leading comments":     {"# my bot\n# a second line\n" + readsAloneHead + "imports:\n  - lib/a.bot\n- a\n", 2, 6, true},
		"a stray item with nothing above the mapping": {readsAloneHead + "imports:\n  - lib/a.bot\n- a\n", 3, 4, true},
		"a stray item on the very first line":         {"- a\n" + readsAloneHead, 1, 1, true},
		// A construct that opens on the file's FIRST line leaves yaml.v3's
		// context mark at 0, so the number written is the problem mark's
		// and the construct starts above the line named: the list the span
		// clause says to delete whole begins on line 1, and one line of it
		// left behind is refused again (TestARefusalOfAStrayValueNamesThe
		// LinesItSpans, "a two-item list as the first lines").
		"a two-item list opening on the first line":   {"- a\n- b\n" + readsAloneHead, 2, 1, true},
		"a three-item list opening on the first line": {"- a\n- b\n- c\n" + readsAloneHead, 3, 1, true},
		"a flow list never closed under a comment":    {"# my bot\n" + readsAloneHead + "imports: [\"lib/a.bot\"\n", 2, 3, true},
		"a flow mapping never closed under a comment": {"# my bot\n" + readsAloneHead + "vars: {a: string\n", 2, 3, true},
		"a comma with no item under a comment":        {"# my bot\n" + readsAloneHead + "imports: [, a]\n", 2, 3, true},
		"an undefined tag handle under a comment":     {"# my bot\n" + readsAloneHead + "vars: !e!x 2\n", 2, 3, true},
		// yaml.v3 writes no line number where both marks sit on the first
		// line: the note would name a line the refusal does not.
		"a flow mapping never closed on one line": {"{a: 1 b: 2}\n", 1, 1, false},
		"a comma with no item on the first line":  {"dsl: ]\n", 1, 1, false},
		// Scanner-raised, measured on the same shape: the line is the line
		// that broke, so nothing is owed.
		"a key with no colon under a comment":    {"# my bot\n" + readsAloneHead + "nodes\n  - agent: a\n", 3, 3, false},
		"an item with no dash under a comment":   {"# my bot\n" + readsAloneHead + "imports:\n  - lib/a.bot\n  lib/b.bot\n", 5, 5, false},
		"a stray plain line under a comment":     {"# my bot\n" + readsAloneHead + "a draft title\nimports:\n  - lib/x.bot\n", 3, 3, false},
		"a tab indenting a line under a comment": {"# my bot\n" + readsAloneHead + "imports:\n\t- lib/a.bot\n", 4, 4, false},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			var refusal *parser.Diagnostic
			for _, d := range Parse("x.bot.yaml", []byte(tc.src)).Diagnostics {
				if d.Code == parser.DiagAuthorDocument && d.Severity == parser.SeverityError {
					refusal = &d
					break
				}
			}
			if refusal == nil {
				t.Fatalf("the document is not refused as a document: %s", tc.src)
			}
			if refusal.Line != tc.at {
				t.Errorf("the refusal lands on line %d, not %d: %s", refusal.Line, tc.at, refusal.Message)
			}
			if says := strings.Contains(refusal.Message, yamlReadsFromAboveWant); says != tc.saysSo {
				t.Errorf("the refusal says the line is not the mistake's = %v, want %v: %s", says, tc.saysSo, refusal.Message)
			}
			// A refusal that carries no note names the mistake's own line,
			// in EITHER direction: a scanner-raised message whose line
			// drifted either way is a refusal pointing an author at a line
			// they must not edit, with nothing saying so.
			if !tc.saysSo && tc.mistake != refusal.Line {
				t.Errorf("the refusal lands on line %d while the mistake is on line %d, and carries no note saying so: %s", refusal.Line, tc.mistake, refusal.Message)
			}
		})
	}
	// Both halves of the note's claim are witnessed: a construct whose
	// start is above the line named, and one whose mistake is below it. A
	// row set that lost one direction would leave half the sentence
	// unmeasured.
	above, below := 0, 0
	for _, tc := range rows {
		switch {
		case !tc.saysSo:
		case tc.mistake < tc.at:
			above++
		case tc.mistake > tc.at:
			below++
		}
	}
	if above == 0 || below == 0 {
		t.Errorf("the note says the value can start above the line named and end below it; the rows witness %d above and %d below", above, below)
	}
}

// The move the span clause prescribes carries its indentation: the lines a
// key takes are the ones indented under it, so a two-line value moved under
// a key with its second line left flat is refused with the very message the
// author read before moving it, and the indented spelling reads.
func TestTheMovePrescriptionIndentsTheLinesBelowTheKey(t *testing.T) {
	const flat = readsAloneHead + "prompts:\n  p: my first\nbot\n" + readsAloneBody
	if ok, got := refusedWith(flat, parser.DiagAuthorDocument, "could not find expected ':'"); !ok {
		t.Errorf("a two-line value moved under a key, flat, is no longer refused — the clause need not prescribe the indentation:\n%s", got)
	}
	const indented = readsAloneHead + "prompts:\n  p: my first\n    bot\n" + readsAloneBody
	if res := Parse("x.bot.yaml", []byte(indented)); res.HasErrors() {
		t.Errorf("the spelling the clause prescribes does not read: %v", res.Diagnostics)
	}
}

// The one-indentation prescription holds past two columns. A head whose
// lines carry three indentations is closed by a line at a column that is
// neither the block's nor the document's: dedenting the block to THAT
// column — the only indentation move the clause named before — leaves the
// very refusal the author had just read, one line down. The two spellings
// the clause prescribes name a column every line of the top level shares,
// and both read.
func TestTheOneIndentationPrescriptionHoldsPastTwoColumns(t *testing.T) {
	const head = "    dsl: 2\n  imports:\n   - lib/a.bot\nnodes:\n  - agent: a\n    model: m\n"
	if ok, got := refusedWith(head, parser.DiagAuthorDocument, "give the whole document one indentation"); !ok {
		t.Errorf("a head carrying three indentations is not refused with the clause:\n%s", got)
	}
	// The block dedented to the column of the line that closed it: the
	// head still holds two columns, and the message is the same.
	const toTheClosingColumn = "  dsl: 2\n  imports:\n   - lib/a.bot\nnodes:\n  - agent: a\n    model: m\n"
	if ok, got := refusedWith(toTheClosingColumn, parser.DiagAuthorDocument, "give the whole document one indentation"); !ok {
		t.Errorf("dedenting the block to the closing line's column now reads, so the clause may name that column:\n%s", got)
	}
}

// The one-indentation clause's two spellings, over the whole family whose
// ONLY defect is the column of its top-level keys — each key's own lines at
// their offset under it, so the document's structure is unambiguous and the
// clause is all that stands between the author and a reading document.
// Measured: the keys moved alone leaves 24 of those 60 arrangements refused
// with the clause the author has just read, since a key indented past its
// own lines no longer carries them; both spellings read on 60 of 60 once
// the lines move with their key, which is why the clause says so.
func TestTheOneIndentationClauseMovesTheLinesBelowEachKey(t *testing.T) {
	const clause = "give the whole document one indentation"
	// build writes a six-line document: three top-level keys at the
	// columns cols names, each key's own lines at its column + offset.
	build := func(cols [3]int) string {
		pad := func(n int) string { return strings.Repeat(" ", n) }
		var sb strings.Builder
		sb.WriteString(pad(cols[0]) + "dsl: 2\n")
		sb.WriteString(pad(cols[1]) + "imports:\n")
		sb.WriteString(pad(cols[1]+2) + "- lib/a.bot\n")
		sb.WriteString(pad(cols[2]) + "nodes:\n")
		sb.WriteString(pad(cols[2]+2) + "- agent: a\n")
		sb.WriteString(pad(cols[2]+4) + "model: m\n")
		return sb.String()
	}
	var refused, keysAloneRefused int
	var remedy string
	for a := 0; a <= 4; a++ {
		for b := 0; b <= 4; b++ {
			for c := 0; c <= 4; c++ {
				cols := [3]int{a, b, c}
				if ok, _ := refusedWith(build(cols), parser.DiagAuthorDocument, clause); !ok {
					continue
				}
				refused++
				remedy = strings.Join(errorsOfDoc(build(cols)), "\n")
				shallowest := a
				for _, n := range cols {
					if n < shallowest {
						shallowest = n
					}
				}
				// The two spellings, each with the lines below moving with
				// their key: every key at the shallowest column the
				// document holds, and every key at the opening block's.
				for _, to := range [2]int{shallowest, a} {
					if res := Parse("x.bot.yaml", []byte(build([3]int{to, to, to}))); res.HasErrors() {
						t.Errorf("the arrangement %v, written as the clause says with every key at column %d and its own lines moving with it, is still refused: %v", cols, to, res.Diagnostics)
					}
				}
				// The keys moved alone, which the clause's wording must
				// not leave as the whole move.
				if res := Parse("x.bot.yaml", []byte(buildKeysAlone(cols, a))); res.HasErrors() {
					keysAloneRefused++
				}
			}
		}
	}
	if refused != 60 {
		t.Errorf("the family whose only defect is the keys' columns holds 60 arrangements refused with the clause, not %d: the measure the clause's wording rests on moved", refused)
	}
	if keysAloneRefused == 0 {
		t.Errorf("every arrangement now reads with the keys moved alone: the clause need no longer say the lines below move with their key")
	}
	if !strings.Contains(remedy, "each key's own lines moving with it") {
		t.Errorf("the clause does not say the lines below a key move with it, which %d of the %d arrangements need: %s", keysAloneRefused, refused, remedy)
	}
}

// buildKeysAlone writes the same document with its three top-level keys at
// column to and every other line left where it was: the move the clause
// prescribes read as naming the keys only.
func buildKeysAlone(cols [3]int, to int) string {
	pad := func(n int) string { return strings.Repeat(" ", n) }
	var sb strings.Builder
	sb.WriteString(pad(to) + "dsl: 2\n")
	sb.WriteString(pad(to) + "imports:\n")
	sb.WriteString(pad(cols[1]+2) + "- lib/a.bot\n")
	sb.WriteString(pad(to) + "nodes:\n")
	sb.WriteString(pad(cols[2]+2) + "- agent: a\n")
	sb.WriteString(pad(cols[2]+4) + "model: m\n")
	return sb.String()
}

// A second document in the file is refused for being a second one AND for
// its own syntax: the remedy rides that message too.
func TestASecondDocumentThatIsNotYAMLNamesItsRemedy(t *testing.T) {
	const src = "dsl: 2\n---\nimports: - lib/x.bot\n"
	if ok, got := refusedWith(src, parser.DiagAuthorDocument, "start a line of their own"); !ok {
		t.Errorf("the second document's syntax error arrives bare:\n%s", got)
	}
	if ok, got := refusedWith(src, parser.DiagAuthorDocument, "one document per file"); !ok {
		t.Errorf("the refusal no longer says the file holds two documents:\n%s", got)
	}
}

// The table is read by key, not in order: an entry answers the cause it
// names and nothing else (TestEveryEntryKeyIsItsOwn), so no message can
// shadow another and the entries stand in any order.

const (
	notUTF8First    = "every author-facing reading refuses a source that is not UTF-8 before a decoder reads it: decode's own guard for the document, and the lexer's E006 for a frontmatter block, whose lines a parse has already replaced"
	streamInvariant = "an invariant of the event stream the parser feeds the composer, not of a document"
	emitDirection   = "the emit direction: both readings read a document, neither writes one"
	// v2StrictOnly is the reason of the three messages yaml.v2 collects:
	// all three spellings are its own, and it gates each on d.strict.
	v2StrictOnly = "the strict reading alone collects it — yaml.v2 gates its three on d.strict, which UnmarshalStrict and (*yaml.Decoder).SetStrict set: the one reading of a block calls yaml.Unmarshal (workflowfile.DecodeFrontmatter) and the document is read into a *yaml.Node, which has no field and no map. Executed by TestTheBlocksTheOutOfReachRefusalsNameRead, which reddens where a reading turns strict and these messages start reaching an author bare"
	// v3DuplicatesByDefault is the reason of the three messages yaml.v3
	// collects decoding a Go value. Its duplicate keys are not strict at
	// all — its own newDecoder sets d.uniqueKeys, so plain Unmarshal
	// refuses a key written twice; the field spelling rides the same flag
	// but never comes out, the uniqueKeys pass returning before the struct
	// check that would raise it — and its field not found is gated on
	// d.knownFields, which only Decoder.KnownFields sets. What keeps all
	// three from an author is that no author-facing reading decodes into
	// Go values with yaml.v3: the document is read into a *yaml.Node,
	// where a duplicate key is kept and guardTree refuses it with its own
	// words, and the frontmatter block's reading is yaml.v2's. Executed by
	// TestTheBlocksTheOutOfReachRefusalsNameRead: a plain v3 Unmarshal
	// refuses a duplicate key, and the message it carries classifies to no
	// remedy.
	v3DuplicatesByDefault = "yaml.v3 collects the duplicate-key spelling on PLAIN Unmarshal — its own newDecoder sets d.uniqueKeys, so a key written twice is refused with no strict reading in sight; the field spelling rides the same flag but never comes out, the uniqueKeys pass returning before the struct check that would raise it — and the field not found under Decoder.KnownFields alone; what keeps all three from an author is that no author-facing reading decodes into Go values with it: the document is read into a *yaml.Node, where a duplicate key is kept and guardTree refuses it with its own words, and the frontmatter block's reading is yaml.v2's. Executed by TestTheBlocksTheOutOfReachRefusalsNameRead: a plain v3 Unmarshal refuses a duplicate key, and the message it carries classifies to no remedy"
	// v3NodeDecodesNoTag is the reason of the messages yaml.v3 raises
	// decoding a scalar an explicit tag names into a Go value.
	v3NodeDecodesNoTag = "the document's node tree resolves no scalar into a Go value — an explicit tag is refused by guardTree with its own words before any tag could fail to fit one"
)

// yamlProblemsOutOfReach are the problem strings of either vendored
// reader's error constructors no author-facing reading reaches — neither
// author.decode for the document nor workflowfile.DecodeFrontmatter for a
// `.bot` frontmatter block, whose message `fmt --to yaml` shows — each with
// the reason it cannot. Keyed by reader and problem: a reason is asked of
// the reader that builds the string, so a reason that holds for one
// reader's reading alone says so (reader set, the string being both
// readers'), and the other reader's spelling of the same words is its own
// row — the frontmatter block decodes into a Go struct, so it resolves the
// tags, aliases and merge keys the document's node tree leaves alone.
type yamlProblemKey struct {
	reader  string // "" where the reason holds for both readers
	problem string
}

var yamlProblemsOutOfReach = map[yamlProblemKey]string{
	{problem: "did not find expected <stream-start>"}: "the stream start is the first token of every stream the decoder reads",
	{problem: "input error: "}:                        "the source is bytes already in memory — a bytes.Reader for the document, the block's own text for a frontmatter — which never fails to read",
	// A UTF-16 BOM is not UTF-8 either, so the reader never decodes a
	// UTF-16 unit on this path.
	{problem: "invalid leading UTF-8 octet"}:        notUTF8First,
	{problem: "incomplete UTF-8 octet sequence"}:    notUTF8First,
	{problem: "invalid trailing UTF-8 octet"}:       notUTF8First,
	{problem: "invalid length of a UTF-8 sequence"}: notUTF8First,
	{problem: "invalid Unicode character"}:          notUTF8First,
	{problem: "incomplete UTF-16 character"}:        notUTF8First,
	{problem: "unexpected low surrogate area"}:      notUTF8First,
	{problem: "incomplete UTF-16 surrogate pair"}:   notUTF8First,
	{problem: "expected low surrogate area"}:        notUTF8First,
	// The tags, aliases and merge keys a frontmatter block's reading
	// resolves ARE reachable, and carry a remedy: the two left here are
	// the ones neither reading asks for.
	{problem: "cannot decode node with unknown kind %d"}: "yaml.v3's Node.Decode raises it, which neither reading calls: the document is read as a node tree, a frontmatter block with yaml.v2",
	// The messages a decoder COLLECTS rather than raises: one `terrors`
	// element per value it cannot read. `cannot unmarshal ` is the family
	// the frontmatter block's reading raises and carries a remedy; the
	// others are keyed per reader — the two spell their collected messages
	// differently and gate them differently (yaml.v2 strict-only, yaml.v3
	// by default), so a reason that holds for one says so.
	{reader: "yaml.v2", problem: "line %d: key %#v already set in map"}:                 v2StrictOnly,
	{reader: "yaml.v2", problem: "line %d: field %s already set in type %s"}:            v2StrictOnly,
	{reader: "yaml.v2", problem: "line %d: field %s not found in type %s"}:              v2StrictOnly,
	{reader: "yaml.v3", problem: "line %d: mapping key %#v already defined at line %d"}: v3DuplicatesByDefault,
	{reader: "yaml.v3", problem: "line %d: field %s already set in type %s"}:            v3DuplicatesByDefault,
	{reader: "yaml.v3", problem: "line %d: field %s not found in type %s"}:              v3DuplicatesByDefault,
	{problem: "invalid array: want %d elements but got %d"}:                             "neither reading decodes into a Go ARRAY: the document becomes a *yaml.Node and a frontmatter block the four fields of workflowfile.Frontmatter, whose lists are slices",
	{problem: "attempted to go past the end of stream; corrupted value?"}:               streamInvariant,
	{problem: "expected %s event but got %s"}:                                           streamInvariant,
	{problem: "%s%s"}: "a format wrapper: the text it carries is the scanner's, the parser's, the reader's own problem or the fallback below",
	{problem: "unknown problem parsing YAML content"}: "the composer's message where the parser left its problem empty: no error constructor of either vendored reader passes an empty problem, which the scan below holds",
	{problem: "duplicated key '"}:                     "yaml.v3's spelling, raised where a Go struct's own yaml tags collide: the document is decoded into a *yaml.Node, and the frontmatter block's reader is yaml.v2, which spells it with a capital below",
	// The frontmatter block's reader spells the same panic with a capital,
	// and it does decode into a Go struct (workflowfile.Frontmatter): the
	// message is about that struct's own tags, which no document changes.
	{problem: "Duplicated key '"}: "a Go struct whose yaml tags collide, raised while reading the struct rather than the document: the four keys of a frontmatter carry four distinct tags",
	// The emit direction, read so that no error constructor of the package
	// is a silent allowlist: the author write path encodes (author.Write),
	// and what an encoder refuses is a program's defect, not a document's
	// syntax — no remedy of this table answers it.
	{problem: "%s"}: "a format wrapper of the emit direction: the text it carries is the emitter's own problem",
	{problem: "unknown problem generating YAML content"}:                              emitDirection,
	{problem: "cannot encode node with unknown kind %d"}:                              emitDirection,
	{problem: "cannot marshal invalid UTF-8 data as %s"}:                              emitDirection,
	{problem: "explicitly tagged !!binary data must be base64-encoded"}:               emitDirection,
	{problem: "anchor value must not be empty"}:                                       emitDirection,
	{problem: "anchor value must contain alphanumerical characters only"}:             emitDirection,
	{problem: "alias value must not be empty"}:                                        emitDirection,
	{problem: "alias value must contain alphanumerical characters only"}:              emitDirection,
	{problem: "expected STREAM-START"}:                                                emitDirection,
	{problem: "expected DOCUMENT-START or STREAM-END"}:                                emitDirection,
	{problem: "expected DOCUMENT-END"}:                                                emitDirection,
	{problem: "expected nothing after STREAM-END"}:                                    emitDirection,
	{problem: "expected SCALAR, SEQUENCE-START, MAPPING-START, or ALIAS, but got %v"}: emitDirection,
	{problem: "incompatible %YAML directive"}:                                         emitDirection,
	{problem: "duplicate %TAG directive"}:                                             emitDirection,
	{problem: "neither tag nor implicit flags are specified"}:                         emitDirection,
	{problem: "tag handle must not be empty"}:                                         emitDirection,
	{problem: "tag handle must start with '!'"}:                                       emitDirection,
	{problem: "tag handle must end with '!'"}:                                         emitDirection,
	{problem: "tag handle must contain alphanumerical characters only"}:               emitDirection,
	{problem: "tag prefix must not be empty"}:                                         emitDirection,
	{problem: "tag value must not be empty"}:                                          emitDirection,
	{problem: "write error: "}:                                                        emitDirection,

	// The Go-value refusals, built by BOTH readers and reached by the
	// frontmatter block's reading alone: the document's is decoded into a
	// *yaml.Node, where a value fills the node whatever it is — no Go
	// type, no such refusal — and guardTree refuses the constructs with
	// its own words instead. Their remedies are the block's entries, keyed
	// on the decode targets of its reading.
	{reader: "yaml.v3", problem: "line %d: cannot unmarshal %s%s into %s"}:                  "the document is decoded into a *yaml.Node, which any node fills: the reading collects no unmarshal errors, and the targets these remedies are keyed on are the frontmatter block's own Go types",
	{reader: "yaml.v3", problem: "cannot decode %s `%s` as a %s"}:                           v3NodeDecodesNoTag,
	{reader: "yaml.v3", problem: "invalid map key: %#v"}:                                    "the document is decoded into a *yaml.Node, where a key is a node and none is invalid — guardTree refuses a key that is not a plain word with its own words",
	{reader: "yaml.v3", problem: "map merge requires map or sequence of maps as the value"}: "the document's node tree resolves no merge key — guardTree refuses `<<` with its own words",
	// yaml.v3 raises the base64 failure decoding a scalar the !!binary tag
	// names into Go bytes: the document's node tree gets nowhere near it,
	// and the frontmatter block's reader is yaml.v2, whose spelling of the
	// same words carries the block entry's remedy.
	{reader: "yaml.v3", problem: "!!binary value contains invalid base64 data"}: v3NodeDecodesNoTag,
}

// yamlProblemArg is the index of the problem argument of every error
// constructor of the vendored yaml.v3 — the decode direction's six and the
// emit direction's two. It is an allowlist, and
// TestEveryYAMLErrorConstructorIsNamedInTheScan is what holds it: every
// function that raises an error is named here, so a constructor a bump adds
// cannot carry a message the scan reads off nobody.
var yamlProblemArg = map[string]int{
	"yaml_parser_set_reader_error":         1,
	"yaml_parser_set_scanner_error":        3,
	"yaml_parser_set_scanner_tag_error":    3,
	"yaml_parser_set_parser_error":         1,
	"yaml_parser_set_parser_error_context": 3,
	"yaml_emitter_set_emitter_error":       1,
	"yaml_emitter_set_writer_error":        1,
	"failf":                                0,
}

// yamlProblemForwarders are the sites the scan cannot read a message off —
// a call whose problem argument is neither a string literal nor a constant
// naming one, an assignment to a problem field whose value comes from
// elsewhere — each with the number of sites that spelling stands for and
// the reason its message is read somewhere else. A site that is not here
// fails the test, and so does a count that moved: a problem factored into a
// variable, or a second site spelled exactly like the one accounted for,
// would otherwise be dropped in silence and its message would reach an
// author bare.
var yamlProblemForwarders = map[string]struct {
	sites  int
	reason string
}{
	"yaml_parser_set_scanner_error(problem)":  {1, "yaml_parser_set_scanner_tag_error passes its own problem parameter on; the literals are its callers'"},
	"yaml_emitter_set_emitter_error(problem)": {2, "yaml_emitter_analyze_anchor raises an empty anchor and a non-alphanumerical one through a local its two branches write a line above each call; the assignment branch reads the four literals"},
	"parser.problem = problem":                {4, "the reader's, the scanner's and the parser's error constructors write the problem their caller passes; those literals are read at the call"},
	"emitter.problem = problem":               {2, "the writer's and the emitter's error constructors write the problem their caller passes; those literals are read at the call"},
	"msg = p.parser.problem":                  {1, "the composer carries the scanner's or the parser's own problem into its error; the fallback where that problem is empty has its own reason above"},
	"msg = e.emitter.problem":                 {1, "the encoder carries the emitter's own problem into its panic, in the emit direction: what an encoder refuses is a program's defect, not a document an author typed"},
}

// yamlProblemFields are the names a problem string is written to before it
// is raised: the scanner, the parser and the emitter set their `problem`
// field, the composer and the encoder build their message in a local `msg`.
var yamlProblemFields = map[string]bool{"problem": true, "msg": true}

// yamlMessageSlices are the fields a reader appends a message to instead of
// raising it through an error constructor: both decoders collect one
// `terrors` element per value they cannot read, each a fmt.Sprintf of its
// own literal (go.yaml.in/yaml/v2/decode.go:265, 687, 756, 776), and a
// TypeError carries them to the caller whole. `cannot unmarshal ` — the
// family the frontmatter block's reading raises most — is one of them, so
// the scan reads them as it reads a constructor's.
var yamlMessageSlices = map[string]bool{"terrors": true}

// yamlCollectedMessages are the message literals a reader appends to one of
// its collectors, read off the fmt.Sprintf of each appended argument. A
// site that appends anything else is said, never dropped: it carries the
// elements of an error the reader built itself (`e.Errors`) or truncates
// the collector, and a site that is neither would put a message no scan
// reads in front of an author.
func yamlCollectedMessages(t *testing.T, fset *gotoken.FileSet, lhs, rhs goast.Expr) []string {
	t.Helper()
	at := fset.Position(rhs.Pos()).String()
	if slice, ok := rhs.(*goast.SliceExpr); ok {
		if argName(slice.X) != argName(lhs) {
			t.Errorf("%s writes %s from %s rather than from the collector itself: a message built elsewhere reaches an author through it", at, argName(lhs), argName(slice.X))
		}
		return nil
	}
	call, ok := rhs.(*goast.CallExpr)
	if !ok || calleeName(call.Fun) != "append" || len(call.Args) == 0 || argName(call.Args[0]) != argName(lhs) {
		t.Errorf("%s writes %s with neither an append to it nor a truncation of it: the scan cannot read the messages it then carries", at, argName(lhs))
		return nil
	}
	var lits []string
	for _, arg := range call.Args[1:] {
		if lit, ok := stringLiteral(arg); ok {
			lits = append(lits, lit)
			continue
		}
		if !strings.HasSuffix(argName(arg), ".Errors") {
			t.Errorf("%s appends %s to %s, which is neither a message literal nor the elements of an error the reader built: its spelling is read off nobody", at, argName(arg), argName(lhs))
		}
	}
	return lits
}

// yamlVendorModules are the vendored YAML readers whose problem strings
// reach an author: the author document's own, and the `.bot` frontmatter
// block's, which `fmt` reads with the catalog reader's decoder
// (workflowfile.DecodeFrontmatter) and whose error YAMLRemedy answers at
// pkg/cli/fmt_author.go. Both are scanned: the table answers one set of
// spellings, so a bump of EITHER reader that adds or rewords a message must
// redden here rather than reach an author bare. count is the number of
// distinct problem strings the scan reads off that module — exact, not a
// floor: a message added, reworded, factored out or a file renamed moves it,
// where losing a remedied message would otherwise redden nothing.
var yamlVendorModules = []struct {
	name  string
	path  []string
	count int
}{
	{"yaml.v3", []string{"gopkg.in", "yaml.v3"}, 94},
	{"yaml.v2", []string{"go.yaml.in", "yaml", "v2"}, 92},
}

// yamlModule is the vendored path of the reader named name, which the scans
// written about ONE reader's own structure are asked of. Looked up by name
// rather than by a row's position: a table reordered when a third reader
// arrives would otherwise point them at another module in silence.
func yamlModule(t *testing.T, name string) []string {
	t.Helper()
	for _, mod := range yamlVendorModules {
		if mod.name != name {
			continue
		}
		// A row's name and its vendored path must agree: the name is all a
		// scan written about one reader's own structure has to go by, and
		// the scan reads the path.
		if major := name[strings.LastIndex(name, ".")+1:]; !strings.Contains(mod.path[len(mod.path)-1], major) {
			t.Fatalf("the row named %s is vendored at %s, which does not name %s: a scan written about one reader's structure would read the other", name, filepath.Join(mod.path...), major)
		}
		return mod.path
	}
	t.Fatalf("no vendored YAML reader named %s in yamlVendorModules: the scans written about its own structure have nothing to read", name)
	return nil
}

// yamlVendorFiles parses every Go file of one vendored YAML reader — the
// one source every scan below reads, so none reads a narrower tree than the
// other — and returns them by name, the names sorted, and the file set the
// positions belong to.
func yamlVendorFiles(t *testing.T, mod []string) (*gotoken.FileSet, map[string]*goast.File, []string) {
	t.Helper()
	dir := filepath.Join(append([]string{"..", "..", "..", "vendor"}, mod...)...)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("the vendored %s does not read: %v", dir, err)
	}
	fset := gotoken.NewFileSet()
	names := []string{}
	files := map[string]*goast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("%s of the vendored %s does not read: %v", e.Name(), dir, err)
		}
		files[e.Name()] = f
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no file of a vendored YAML reader read off %s", dir)
	}
	return fset, files, names
}

// yamlProblemStrings reads every problem string one vendored YAML reader
// builds, off every file of its package: the problem argument of each error
// constructor (a literal, or the constant naming it), and the literals
// assigned to a problem field or message. The value is where the string was
// read, for the messages below.
func yamlProblemStrings(t *testing.T, mod []string) map[string]string {
	t.Helper()
	fset, files, names := yamlVendorFiles(t, mod)
	// A problem may be written as a constant: its value is the message.
	consts := map[string]string{}
	for _, name := range names {
		for _, d := range files[name].Decls {
			gen, ok := d.(*goast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				val, ok := spec.(*goast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range val.Names {
					if i >= len(val.Values) {
						continue
					}
					if lit, ok := stringLiteral(val.Values[i]); ok {
						consts[id.Name] = lit
					}
				}
			}
		}
	}
	found := map[string]string{}
	forwarders := map[string][]string{}
	for _, name := range names {
		goast.Inspect(files[name], func(n goast.Node) bool {
			switch v := n.(type) {
			case *goast.CallExpr:
				callee := calleeName(v.Fun)
				i, wanted := yamlProblemArg[callee]
				if !wanted || i >= len(v.Args) {
					return true
				}
				if lit, ok := stringLiteral(v.Args[i]); ok {
					found[lit] = callee + " of " + name
					return true
				}
				if id, ok := v.Args[i].(*goast.Ident); ok {
					if lit, ok := consts[id.Name]; ok {
						found[lit] = callee + " of " + name + ", through " + id.Name
						return true
					}
				}
				key := callee + "(" + argName(v.Args[i]) + ")"
				forwarders[key] = append(forwarders[key], fset.Position(v.Args[i].Pos()).String())
			case *goast.AssignStmt:
				for i, lhs := range v.Lhs {
					if i >= len(v.Rhs) {
						continue
					}
					var field string
					switch l := lhs.(type) {
					case *goast.SelectorExpr:
						field = l.Sel.Name
					case *goast.Ident:
						field = l.Name
					}
					if yamlMessageSlices[field] {
						for _, lit := range yamlCollectedMessages(t, fset, lhs, v.Rhs[i]) {
							found[lit] = "an append to " + field + " in " + name
						}
						continue
					}
					if !yamlProblemFields[field] {
						continue
					}
					if lit, ok := stringLiteral(v.Rhs[i]); ok {
						found[lit] = "an assignment to " + field + " in " + name
						continue
					}
					key := argName(lhs) + " = " + argName(v.Rhs[i])
					forwarders[key] = append(forwarders[key], fset.Position(v.Rhs[i].Pos()).String())
				}
			}
			return true
		})
	}
	// A problem the scan cannot read the message of is said, never dropped:
	// a message no reader of this table sees is one that reaches an author
	// with no remedy. The count stands beside the spelling — a second site
	// written exactly like the one accounted for carries its own message.
	for key, at := range forwarders {
		sort.Strings(at)
		listed, ok := yamlProblemForwarders[key]
		switch {
		case !ok:
			t.Errorf("the problem of %s (%s) is no literal and no constant: the scan cannot read its message — add its remedy and list the site in yamlProblemForwarders", key, strings.Join(at, ", "))
		case listed.sites != len(at):
			t.Errorf("%s stands at %d sites (%s) and yamlProblemForwarders accounts for %d: read the new one against the vendored source — a message this scan does not read reaches an author bare", key, len(at), strings.Join(at, ", "), listed.sites)
		}
	}
	for key := range yamlProblemForwarders {
		if _, ok := forwarders[key]; !ok {
			t.Errorf("yamlProblemForwarders names %s, which the vendored yaml.v3 writes nowhere that way", key)
		}
	}
	return found
}

// Every problem string either vendored YAML reader builds through one of
// its error constructors — or assigns to a problem field or message — is
// answered BY THE READER THAT BUILDS IT: a remedy in yamlRemedies of that
// reader, or a reason in yamlProblemsOutOfReach that holds for that
// reader's reading. The constants of the reader that runs are walked — an
// entry of one reader cannot cover the other's spelling of the same words,
// and a bump of either that adds or rewords a message fails here instead of
// letting it reach an author bare. What this test cannot see is a reason
// gone stale because a reading changed rather than a reader:
// frontmatterWitnesses above is what holds that side, one block per message
// the second reading really raises.
func TestEveryYAMLProblemStringHasARemedyOrAReason(t *testing.T) {
	built := map[workflowfile.YAMLReader]map[string]bool{}
	for _, mod := range yamlVendorModules {
		reader := workflowfile.YAMLReader(mod.name)
		found := yamlProblemStrings(t, mod.path)
		built[reader] = map[string]bool{}
		for lit := range found {
			built[reader][lit] = true
		}
		// Said, not fatal: a reach that moved must not hide the message the
		// move left without a remedy.
		if len(found) != mod.count {
			t.Errorf("the scan reads %d problem strings off the vendored %s, not %d: its reach moved — read the table against the vendored source before writing the new number", len(found), mod.name, mod.count)
		}
		for lit, from := range found {
			if lit == "" {
				t.Errorf("%s of %s builds an empty problem: the composer's own message takes its place, which is listed as out of reach", from, mod.name)
				continue
			}
			// The cause the reading of this reader carries for the literal:
			// the terrors' own line head off, and — where the family is
			// keyed on the decode target — one question per target the
			// frontmatter reading decodes into, the block's own included.
			causes := workflowfile.YAMLCauses(reader, errors.New(strings.TrimPrefix(lit, "line %d: ")))
			targets := []string{""}
			if len(causes) == 1 && causes[0].Message == workflowfile.YAMLUnmarshalFormat {
				targets = append(frontmatterUnmarshalTargets(), frontmatterBlockTarget())
			}
			answered := ""
			for _, target := range targets {
				causes[0].Target = target
				if r := YAMLRemedy(causes[0]); r != "" {
					answered = r
					break
				}
			}
			reason := ""
			listed := false
			for _, r := range []string{string(reader), ""} {
				if got, ok := yamlProblemsOutOfReach[yamlProblemKey{reader: r, problem: lit}]; ok {
					reason, listed = got, true
					break
				}
			}
			switch {
			case answered != "" && listed:
				t.Errorf("%s of %s builds %q, which has a remedy and the reason %q: it is one or the other", from, mod.name, lit, reason)
			case answered == "" && !listed:
				t.Errorf("%s of %s builds %q, which reaches the author bare: add its remedy to yamlRemedies, or its reason to yamlProblemsOutOfReach", from, mod.name, lit)
			}
		}
	}
	// A reason is stale where no reader the reason holds for builds the
	// string it names: the two spell a few of their own messages
	// differently, so a reason without a reader is asked of the union —
	// either reader builds it — and one with a reader of that one alone.
	for key := range yamlProblemsOutOfReach {
		ok := built[workflowfile.YAMLv3][key.problem] || built[workflowfile.YAMLv2][key.problem]
		if key.reader != "" {
			ok = built[workflowfile.YAMLReader(key.reader)][key.problem]
		}
		if !ok {
			t.Errorf("yamlProblemsOutOfReach names %q for the reader %q, which builds it nowhere", key.problem, key.reader)
		}
	}
}

// The reasons the decoder's duplicate-key and unknown-field messages carry
// are EXECUTED, not argued. yaml.v2's three are strict-only, and the one
// reading calls yaml.Unmarshal while the document is read into a
// *yaml.Node: a block each of them would refuse READS today, and a reading
// turned strict reddens here, where the reason in yamlProblemsOutOfReach
// would otherwise go stale in silence. yaml.v3's duplicates are the other
// way round — its decoder refuses them on PLAIN Unmarshal — so what keeps
// the message from an author is the absence of any author-facing reading
// that decodes into Go values with yaml.v3; that half is executed the same
// way: the refusal exists, and it classifies to no remedy.
func TestTheBlocksTheOutOfReachRefusalsNameRead(t *testing.T) {
	for name, block := range map[string]string{
		"a key of the four written twice":     "name: probe\nname: twice\n",
		"a key beyond the four written twice": "name: probe\nk: 1\nk: 2\n",
		"a key the four do not name":          "name: probe\nunknown: 1\n",
	} {
		if _, _, err := workflowfile.DecodeFrontmatter(block, parser.ReadBudget()); err != nil {
			t.Errorf("%s is refused: %s — the reading is strict, so the messages yamlProblemsOutOfReach calls out of reach for that reason reach an author bare", name, strings.Join(strings.Fields(err.Error()), " "))
		}
	}
	// The document's own reading: a *yaml.Node carries no field and no map,
	// so a key written twice is kept in the tree and guardTree refuses it
	// with its own words rather than the decoder with its.
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("dsl: 2\ndsl: 3\n"), &node); err != nil {
		t.Errorf("the document's reading refuses a key written twice (%v): it no longer fills a *yaml.Node, and the decoder's strict-only messages can reach an author", err)
	}
	// The yaml.v3 half: plain Unmarshal refuses a duplicate key BY DEFAULT
	// — not a strict reading — and the message it collects quotes the
	// author's key back and classifies to no remedy. A reading that starts
	// decoding Go values with yaml.v3 reaches an author with it and must
	// key a remedy there.
	var v3map map[string]string
	err := yaml.Unmarshal([]byte("name: a\nname: b\n"), &v3map)
	switch {
	case err == nil:
		t.Fatalf("plain yaml.v3 Unmarshal reads a key written twice: the duplicates its decoder collects by default moved, and the reason yamlProblemsOutOfReach carries for them is stale")
	case !strings.Contains(err.Error(), `already defined at line 1`):
		t.Fatalf("plain yaml.v3 Unmarshal refuses a duplicate key with %q, not with the collected message the out-of-reach row names", err)
	}
	causes := workflowfile.YAMLCauses(workflowfile.YAMLv3, err)
	if len(causes) != 1 || causes[0].Message != `mapping key "name" already defined at line 1` {
		t.Fatalf("the refusal classifies to %v, not to the message the reader collected with the author's key in it", causes)
	}
	if r := YAMLRemedy(causes[0]); r != "" {
		t.Errorf("the duplicate-key refusal of plain yaml.v3 Unmarshal answers a remedy, which the out-of-reach row says cannot reach an author: %s", r)
	}
}

// yamlContextRaiser is the constructor a vendored YAML parser calls with a
// context mark beside the problem's own: of the two marks it then holds,
// neither is the line to edit — both are 0-based and the parser does not
// increment them — so the line its messages name is above the token that
// broke, by one line or by the whole header depending on which mark the
// reader's fail() picks.
const yamlContextRaiser = "yaml_parser_set_parser_error_context"

// Every problem either vendored reader raises through yamlContextRaiser is
// named in yamlContextProblems, and every message named there is one they
// raise that way: the remedy of such a message owes the reader the fact
// that its line need not be the mistake's, and the remedy of a message
// raised with the problem's own mark alone must not claim it. A bump that
// raises a seventh message through that constructor — or stops raising one
// of these that way — reddens here rather than sending an author to a line
// they must not edit.
func TestEveryContextRaisedYAMLProblemIsNamed(t *testing.T) {
	arg, ok := yamlProblemArg[yamlContextRaiser]
	if !ok {
		t.Fatalf("%s is not in yamlProblemArg: the scan cannot read the problems it carries", yamlContextRaiser)
	}
	raised := map[string]string{}
	for _, mod := range yamlVendorModules {
		_, files, names := yamlVendorFiles(t, mod.path)
		for _, name := range names {
			goast.Inspect(files[name], func(n goast.Node) bool {
				call, isCall := n.(*goast.CallExpr)
				if !isCall || calleeName(call.Fun) != yamlContextRaiser || arg >= len(call.Args) {
					return true
				}
				lit, isLit := stringLiteral(call.Args[arg])
				if !isLit {
					t.Errorf("%s of %s raises a context-marked problem the scan cannot read as a literal: read it against the vendored source and name it in yamlContextProblems", name, mod.name)
					return true
				}
				raised[lit] = mod.name + "/" + name
				return true
			})
		}
	}
	named := map[string]bool{}
	for _, p := range yamlContextProblems {
		named[p] = true
		if _, ok := raised[p]; !ok {
			t.Errorf("yamlContextProblems names %q, which neither vendored reader raises with a context mark: its remedy claims a line it does not misname", p)
		}
	}
	for p, from := range raised {
		if !named[p] {
			t.Errorf("%s raises %q with a context mark, which yamlContextProblems does not name: its refusal names a line the mistake need not be on and says nothing of it", from, p)
		}
	}
}

// yamlErrorField is the field the vendored yaml.v3 writes to raise an
// error — the parser's and the emitter's own — and yamlNoError the value
// that clears that field instead of raising.
const (
	yamlErrorField = "error"
	yamlNoError    = "yaml_NO_ERROR"
)

// Every function of the vendored yaml.v3 that raises an error — it writes
// an error kind into the parser's or the emitter's `error` field — is named
// in yamlProblemArg, so the scan above reads the problem it carries beside
// it. Without this the allowlist is a reader's guess: a constructor written
// as the six existing ones are, with one reachable call site, leaves the
// count and the remedy checks above green while the message it carries
// reaches an author bare.
func TestEveryYAMLErrorConstructorIsNamedInTheScan(t *testing.T) {
	fset, files, names := yamlVendorFiles(t, yamlModule(t, "yaml.v3"))
	raisers := map[string]string{}
	for _, name := range names {
		for _, d := range files[name].Decls {
			fn, ok := d.(*goast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			goast.Inspect(fn.Body, func(n goast.Node) bool {
				as, ok := n.(*goast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range as.Lhs {
					sel, ok := lhs.(*goast.SelectorExpr)
					if !ok || sel.Sel.Name != yamlErrorField || i >= len(as.Rhs) {
						continue
					}
					if id, ok := as.Rhs[i].(*goast.Ident); ok && id.Name == yamlNoError {
						continue
					}
					raisers[fn.Name.Name] = fset.Position(as.Pos()).String()
				}
				return true
			})
		}
	}
	if len(raisers) == 0 {
		t.Fatalf("no function of the vendored yaml.v3 writes a %q field: this scan reads nothing, and holds nothing", yamlErrorField)
	}
	found := make([]string, 0, len(raisers))
	for n := range raisers {
		found = append(found, n)
	}
	sort.Strings(found)
	for _, n := range found {
		if _, listed := yamlProblemArg[n]; !listed {
			t.Errorf("%s (%s) raises an error and is not named in yamlProblemArg: the problem it carries is read by no scan, so its message would reach an author bare — add it with the index of its problem argument, and the remedy of every message its callers pass", n, raisers[n])
		}
	}
}

// calleeName is the name a call names, whether written plain or through a
// package or receiver.
func calleeName(fun goast.Expr) string {
	switch v := fun.(type) {
	case *goast.Ident:
		return v.Name
	case *goast.SelectorExpr:
		return v.Sel.Name
	}
	return ""
}

// argName names an argument for a message: the identifier or field it is
// written as, its Go type otherwise.
func argName(e goast.Expr) string {
	switch v := e.(type) {
	case *goast.Ident:
		return v.Name
	case *goast.SelectorExpr:
		return argName(v.X) + "." + v.Sel.Name
	}
	return fmt.Sprintf("%T", e)
}

// stringLiteral is the text of a string literal expression: the literal
// itself, the left side of a concatenation (`"input error: " + err`), or
// the format of a call that builds the problem (`fmt.Sprintf`).
func stringLiteral(e goast.Expr) (string, bool) {
	switch v := e.(type) {
	case *goast.BasicLit:
		if v.Kind != gotoken.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *goast.BinaryExpr:
		return stringLiteral(v.X)
	case *goast.CallExpr:
		if len(v.Args) == 0 {
			return "", false
		}
		return stringLiteral(v.Args[0])
	}
	return "", false
}

// documentPrescription is one author document written as a remedy says,
// and the entry whose remedy prescribes it.
type documentPrescription struct {
	// answers is one message string of the yamlRemedies entry whose remedy
	// prescribes this document: the link the row owes its clause, so that
	// an entry a document's refusal reaches with nothing written as it
	// says reddens rather than passing for executed
	// (TestEveryDocumentRemedyReachedHasAPrescription).
	answers string
	src     string
}

// remedyPrescriptions holds one document per spelling the table prescribes,
// written as it says. The prescriptions that delete a stray line all leave
// the same document — the one that reads on its own — so they share one
// row: every other row carries a document of its own.
var remedyPrescriptions = map[string]documentPrescription{
	"a command in single quotes":                {answers: "mapping values are not allowed in this context", src: cmdDoc(`command: 'jq -r .a: .b'`)},
	"a regex in single quotes":                  {answers: "found unknown escape character", src: cmdDoc(`command: '^\d+$'`)},
	"a Windows path in single quotes":           {answers: "found unknown escape character", src: cmdDoc(`command: 'C:\temp\x'`)},
	"a command holding two ampersands":          {answers: "did not find expected alphabetic or numeric character", src: cmdDoc(`command: '[ -f x ] && echo y'`)},
	"a value starting with a bang":              {answers: "did not find the expected '>'", src: cmdDoc(`command: '! grep -q x f'`)},
	"a value starting with a percent":           {answers: "found character that cannot start any token", src: cmdDoc(`command: '%s'`)},
	"a value starting with a question mark":     {answers: "mapping keys are not allowed in this context", src: "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    user: '? what now'\n"},
	"an escape for a control character":         {answers: "control characters are not allowed", src: cmdDoc(`command: "a\u001bb"`)},
	"an edge in single quotes":                  {answers: "did not find expected key", src: edgeDoc(`'a -> done with { x: "{{outputs.a.y}}" }'`)},
	"a flow list with its commas":               {answers: "did not find expected ',' or ']'", src: "dsl: 2\nworkflow:\n  name: w\n  edges: [\"a -> done\", \"b -> done\"]\n"},
	"a block body indented under its key":       {answers: "found an indentation indicator equal to 0", src: promptDoc("|\n      x")},
	"a list with one item per line":             {answers: "block sequence entries are not allowed in this context", src: "dsl: 2\nimports:\n  - lib/x.bot\n"},
	"a list in one flow line":                   {answers: "block sequence entries are not allowed in this context", src: "dsl: 2\nimports: [lib/x.bot, lib/y.bot]\n"},
	"a glob in single quotes":                   {answers: "did not find expected alphabetic or numeric character", src: cmdDoc(`command: 'rm *.tmp'`)},
	"a shell pipe in single quotes":             {answers: "did not find expected comment or line break", src: cmdDoc(`command: 'grep x f | wc -l'`)},
	"a list whose every item carries its dash":  {answers: "could not find expected ':'", src: "dsl: 2\nimports:\n  - lib/a.bot\n  - lib/b.bot\n"},
	"a flow list closed":                        {answers: "did not find expected ',' or ']'", src: "dsl: 2\nimports: [\"lib/a.bot\", \"lib/b.bot\"]\n"},
	"a flow mapping closed, with its commas":    {answers: "did not find expected ',' or '}'", src: "dsl: 2\nvars: {a: string, b: string}\n"},
	"a mapping with one key: value line a line": {answers: "did not find expected ',' or '}'", src: "dsl: 2\nvars:\n  a: string\n  b: string\n"},
	"a closing bracket quoted in a value":       {answers: "did not find expected node content", src: cmdDoc(`command: ']'`)},
	"a document indicator indented in a value":  {answers: "found unexpected document indicator", src: cmdDoc("command: \"a\n    ...\n    b\"")},
	"a tagged value in single quotes":           {answers: "did not find expected '!'", src: cmdDoc(`command: '!foo"x'`)},
	"a duplicate directive removed":             {answers: "found duplicate %YAML directive", src: "%YAML 1.1\n---\ndsl: 2\n"},
	"a tab replaced with the indent spaces":     {answers: "found a tab character where an indentation space is expected", src: promptDoc("|\n    x")},
	// The document a stray line deleted leaves: the prescription of every
	// clause that names one — a title above the first key, a `- item` or a
	// `|`/`>` block between two keys or as the first line, a `[…]` first
	// line, a first key quoted whole and unquoted again.
	"a document with no ... line":                   {answers: yamlNoDocumentStart, src: readsAloneHead + readsAloneBody},
	"a stray block moved under its key":             {answers: "did not find expected key", src: readsAloneHead + "prompts:\n  p: |\n    a title\n" + readsAloneBody},
	"a stray list item moved under its key":         {answers: "did not find expected '-' indicator", src: readsAloneHead + "imports:\n  - lib/a.bot\n" + readsAloneBody},
	"a first-line block moved under its key":        {answers: yamlNoDocumentStart, src: "prompts:\n  p: |\n    a title\n" + readsAloneHead + readsAloneBody},
	"a first-line flow list moved under its key":    {answers: yamlNoDocumentStart, src: "imports: [lib/a.bot, lib/b.bot]\n" + readsAloneHead + readsAloneBody},
	"a first key written with its colon":            {answers: "could not find expected ':'", src: "dsl: 2\n# a note\nimports:\n  - lib/x.bot\n"},
	"a block body indented as its indicator claims": {answers: "did not find expected key", src: promptDoc("|9\n           x")},
	// The lines a multi-line stray spans, moved under one key: the clause
	// prescribes the move with the lines below the key indented under it,
	// one spelling per shape. Written flat, each of them is refused with
	// the message the author just read.
	"a two-line title moved under one key":      {answers: "could not find expected ':'", src: readsAloneHead + "prompts:\n  p: my first\n    bot\n" + readsAloneBody},
	"a two-line quoted value moved under a key": {answers: "could not find expected ':'", src: readsAloneHead + "prompts:\n  p: \"my first\n    bot\"\n" + readsAloneBody},
	"a broken flow list moved under its key":    {answers: "mapping values are not allowed in this context", src: readsAloneHead + "imports: [lib/a.bot,\n  lib/b.bot]\n" + readsAloneBody},
	"a two-item list moved under its key":       {answers: "did not find expected key", src: readsAloneHead + "imports:\n  - lib/a.bot\n  - lib/b.bot\n" + readsAloneBody},
	// Every key indented deeper than the keys beside it, dedented to their
	// column: one left behind is refused with the same message.
	"two keys indented deeper, both dedented": {answers: "mapping values are not allowed in this context", src: "dsl: 2\nimports:\n  - lib/x.bot\nvars:\n  a: string\n"},
	// The two spellings that give a document opening with an indented block
	// one indentation: the block dedented to the shallower column, and the
	// lines below it indented to the block's.
	"an indented first block dedented whole":         {answers: yamlNoDocumentStart, src: "dsl: 2\nimports:\n  - lib/x.bot\nnodes:\n  - agent: a\n    model: m\n"},
	"the lines below an indented block indented too": {answers: yamlNoDocumentStart, src: "  dsl: 2\n  imports:\n    - lib/x.bot\n  nodes:\n    - agent: a\n      model: m\n"},
	// The same two spellings on a head carrying THREE indentations, where
	// the column of the line that closed the block is neither of them.
	"a three-indentation head dedented to one column": {answers: yamlNoDocumentStart, src: "dsl: 2\nimports:\n   - lib/a.bot\nnodes:\n  - agent: a\n    model: m\n"},
	"a three-indentation head indented to one column": {answers: yamlNoDocumentStart, src: "    dsl: 2\n    imports:\n     - lib/a.bot\n    nodes:\n      - agent: a\n        model: m\n"},
}

// yamlDocumentRemedyWithoutPrescription are the entries the author
// document's refusals reach whose remedy prescribes no rewrite of a
// document that one row can carry, each with the reason: the exclusion
// table of TestEveryDocumentRemedyReachedHasAPrescription, where an entry
// answered by no row must be said rather than passed over in silence.
var yamlDocumentRemedyWithoutPrescription = map[yamlEntryKey]string{
	{message: "exceeded max depth of %d"}: "the construct is the document's own size — the remedy names the flattening, and no one document is its spelling: any document the author meant reads, and none of them is THE flattened one",
}

// yamlEntryKey names one entry of yamlRemedies the way the rows above do:
// by one of its messages, and by the decode target that scopes it where
// the entry is one of a family keyed on it.
type yamlEntryKey struct {
	message string
	target  string
}

// A document written as its remedy says reads: the spelling each one
// prescribes is one the author path accepts.
func TestADocumentWrittenAsItsRemedySaysReads(t *testing.T) {
	for name, p := range remedyPrescriptions {
		if res := Parse("x.bot.yaml", []byte(p.src)); res.HasErrors() {
			t.Errorf("%s does not read: %v", name, res.Diagnostics)
		}
	}
}

// A row whose document another row already carries proves nothing the
// first does not: the prescription of each clause has a document of its
// own, so a clause whose spelling is never written cannot hide behind a
// row that answers for it.
func TestEveryPrescribedDocumentIsItsOwn(t *testing.T) {
	first := make(map[string]string, len(remedyPrescriptions))
	for _, name := range sortedKeys(remedyPrescriptions) {
		if had, dup := first[remedyPrescriptions[name].src]; dup {
			t.Errorf("%q carries the document %q already carries: one of the two proves nothing the other does not", name, had)
			continue
		}
		first[remedyPrescriptions[name].src] = name
	}
}

// The note is keyed on the lines the reading named: the block's own text
// may spell any number of `line N:` without the note quoting one of them —
// the numbers come from the TypeError the reading returned
// (workflowfile.YAMLCause.Line), never from the words an author reads.
func TestTheBlockLineNoteTakesTheReaderLines(t *testing.T) {
	block := []string{"name: probe", "description: \"line 2: x\"", "triggers: 3"}
	if got, want := YAMLBlockLineNote([]int{3}, block), ", where the line it names is the block's own, not the file's — `triggers: 3`"; got != want {
		t.Errorf("the note named %q, not the line the reading gave: %q", got, want)
	}
	if got, want := YAMLBlockLineNote([]int{1, 3}, block), ", where the lines it names are the block's own, not the file's — line 1 `name: probe`, line 3 `triggers: 3`"; got != want {
		t.Errorf("the note named %q, not the lines the reading gave: %q", got, want)
	}
	for _, lines := range [][]int{nil, {}, {9}, {0}, {2, 2, 3}} {
		named := map[int]bool{}
		for _, n := range lines {
			if n >= 1 && n <= len(block) {
				named[n] = true
			}
		}
		got := YAMLBlockLineNote(lines, block)
		switch len(named) {
		case 0:
			if got != "" {
				t.Errorf("the note answered %q where the reading named no line the block holds: a clause hung on a number nobody named", got)
			}
		case 1:
			if !strings.Contains(got, "the line it names is the block's own") || strings.Contains(got, "line 2") && !named[2] {
				t.Errorf("the note answered %q for the lines %v: one line, quoted once", got, lines)
			}
		default:
			if !strings.Contains(got, "the lines it names are the block's own") {
				t.Errorf("the note answered %q for the lines %v: one clause per named line", got, lines)
			}
		}
	}
}

// The sentence the family prints is a function of the decode target: a
// value refused into one type is told the shape that type takes, and no
// other cause's prescription — the four causes used to share one sentence
// naming all of them, which sent the author of one to the fix of another.
func TestTheUnmarshalRemedyIsAFunctionOfTheDecodeTarget(t *testing.T) {
	const (
		textKey  = "a value a text key takes is one line of text"
		listKey  = "`triggers` and `capabilities` take a list"
		blockKey = "a `.bot` frontmatter block is `key: value` lines"
	)
	for name, tc := range map[string]struct {
		block, says, notSays string
	}{
		"a name that is a list":           {block: "name: [a, b]\n", says: textKey, notSays: listKey},
		"triggers that is a string":       {block: "name: probe\ntriggers: cron\n", says: listKey, notSays: textKey},
		"a block that is a line of prose": {block: "A bot that reviews pull requests\n", says: blockKey, notSays: listKey},
	} {
		read, _, remedies := frontmatterRefusal(tc.block)
		switch {
		case remedies == "":
			t.Errorf("%s: %q reaches the author bare", name, read)
		case !strings.Contains(remedies, tc.says):
			t.Errorf("%s: the refusal does not name the shape its decode target takes (%q): %s%s", name, tc.says, read, remedies)
		case strings.Contains(remedies, tc.notSays):
			t.Errorf("%s: the refusal names the prescription of another decode target: %s%s", name, read, remedies)
		}
	}
}

// The author-document half of the prescription link: every entry a
// document's refusal reaches has a document written as its remedy says, or
// is one whose remedy prescribes no rewrite of a document at all and says
// so in yamlDocumentRemedyWithoutPrescription. The identity test walks
// hand-written documents, so without this link an entry could carry a
// spelling nothing ever writes.
func TestEveryDocumentRemedyReachedHasAPrescription(t *testing.T) {
	answered := map[int]bool{}
	for name, p := range remedyPrescriptions {
		at := -1
		for i, r := range yamlRemedies {
			if slices.Contains(r.messages, p.answers) && r.target == "" {
				at = i
				break
			}
		}
		if at < 0 {
			t.Errorf("%s answers %q, which no entry of yamlRemedies lists unscoped: the row prescribes a spelling the table does not say", name, p.answers)
			continue
		}
		answered[at] = true
	}
	excluded := map[int]string{}
	for key, reason := range yamlDocumentRemedyWithoutPrescription {
		found := false
		for i, r := range yamlRemedies {
			if slices.Contains(r.messages, key.message) && r.target == key.target {
				excluded[i] = reason
				found = true
			}
		}
		if !found {
			t.Errorf("the exclusion table names %q, which no entry of yamlRemedies is keyed on: it excludes nothing", key.message)
		}
	}
	for name, w := range yamlWitnesses {
		var refusal string
		for _, d := range Parse("x.bot.yaml", []byte(w.src)).Diagnostics {
			if refusal == "" && d.Code == parser.DiagAuthorDocument && d.Severity == parser.SeverityError {
				refusal = d.Message
			}
		}
		if refusal == "" {
			// Said by TestEveryYAMLMessageNamesTheRemedyOfEachOfItsCauses.
			continue
		}
		switch at := yamlRefusalEntry(refusal); {
		case at == -2:
			t.Errorf("the refusal of %s ends with the remedy of more than one entry: the entry it reached cannot be read off its words", name)
		case at < 0:
			t.Errorf("the refusal of %s ends with no entry's remedy: %s", name, refusal)
		case !answered[at] && excluded[at] == "":
			t.Errorf("%s reaches remedy %d, which no row of remedyPrescriptions prescribes and no exclusion accounts for: its spelling is prescribed to an author and written nowhere — answer it with a document, or say why no document carries it (%s)", name, at, yamlRemedies[at].remedy)
		}
	}
}

// sortedKeys is m's keys in order: a duplicate is reported against the
// same row at every run.
func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
