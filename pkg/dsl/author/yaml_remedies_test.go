package author

import (
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
	"a !!binary value that is not base64":  {"name: !!binary nope!\n", "reads as a YAML tag"},
	"a value tagged !!int that is not one": {"name: !!int notanint\n", "write it whole in single quotes"},
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
// the error's own text with its whitespace collapsed, then the remedy. "",
// "" where the block reads.
func frontmatterRefusal(block string) (read, remedy string) {
	_, _, err := workflowfile.DecodeFrontmatter(block)
	if err == nil {
		return "", ""
	}
	read = strings.Join(strings.Fields(err.Error()), " ")
	return read, YAMLRemedy(read)
}

// prescription is one frontmatter block written as a remedy says, and the
// identity the catalogue then reads off it: a block that reads is only half
// the prescription — quoting a value whole silences the refusal AND makes
// the value the text it looks like, which the author must be told rather
// than discover in the catalogue.
type prescription struct {
	// answers is one message string of the yamlRemedies entry whose remedy
	// prescribes this block: the link the row owes its clause, so that an
	// entry a block's reading reaches with nothing written as it says
	// reddens rather than passing for executed
	// (TestEveryFrontmatterRemedyReachedHasAPrescription).
	answers string
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
	"a tagged value in single quotes":   {answers: " as a ", block: "name: '!!int notanint'\n", want: workflowfile.Frontmatter{Name: "!!int notanint"}},
	// The tag entry's OTHER prescription, scoped by the key: the value the
	// tag names, which `name` and `description` take and an item of a list
	// key takes too.
	"a !!binary value the tag names, under a text key": {answers: "!!binary value contains invalid base64 data", block: "name: !!binary aGk=\n", want: workflowfile.Frontmatter{Name: "hi"}},
	"a !!int value the tag names, under a text key":    {answers: " as a ", block: "description: !!int 3\n", want: workflowfile.Frontmatter{Description: "3"}},
	"a tagged item of a list key":                      {answers: " as a ", block: "name: probe\ntriggers: [!!int 3]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"3"}}},
	// The alias graph entry's prescription: the keys written out where
	// they are used, which is what a cycle and an expansion too large both
	// leave their author.
	"an anchor's keys written out where they are used": {answers: "value contains itself", block: "name: probe\nx:\n  b: 1\n", want: workflowfile.Frontmatter{Name: "probe"}, extra: []string{"x"}},
	// The two readings of a `*`: the text it is part of, in single quotes,
	// and the alias of an anchor the block writes above it.
	"a star quoted whole, as the text it is":  {answers: "unknown anchor", block: "name: probe\ndescription: '*nope'\n", want: workflowfile.Frontmatter{Name: "probe", Description: "*nope"}},
	"an alias naming an anchor written above": {answers: "unknown anchor", block: "name: &a probe\ndescription: *a\n", want: workflowfile.Frontmatter{Name: "probe", Description: "probe"}},
	"a complex key written as plain text":     {answers: "invalid map key: ", block: "x:\n  a: 1\n", extra: []string{"x"}},
	"a flow list closed":                      {answers: "did not find expected ',' or ']'", block: "name: probe\ntriggers: [a,\n b,\n c]\ndescription: d\n", want: workflowfile.Frontmatter{Name: "probe", Description: "d", Triggers: []string{"a", "b", "c"}}},
	// The shape of each of the four keys, as the shape remedy names them.
	"a name as one line of text":              {answers: "cannot unmarshal ", block: "name: a probe\n", want: workflowfile.Frontmatter{Name: "a probe"}},
	"a description as one line of text":       {answers: "cannot unmarshal ", block: "name: probe\ndescription: a line\n", want: workflowfile.Frontmatter{Name: "probe", Description: "a line"}},
	"triggers as a flow list on one line":     {answers: "cannot unmarshal ", block: "name: probe\ntriggers: [cron, push]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"cron", "push"}}},
	"triggers as one item per line":           {answers: "cannot unmarshal ", block: "name: probe\ntriggers:\n  - cron\n  - push\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"cron", "push"}}},
	"capabilities as a flow list on one line": {answers: "cannot unmarshal ", block: "name: probe\ncapabilities: [shell]\n", want: workflowfile.Frontmatter{Name: "probe", Capabilities: []string{"shell"}}},
	"capabilities as one item per line":       {answers: "cannot unmarshal ", block: "name: probe\ncapabilities:\n  - shell\n", want: workflowfile.Frontmatter{Name: "probe", Capabilities: []string{"shell"}}},
	// The block's own top level, written as `key: value` lines: the prose
	// the remedy's example names, and a complex key written as plain text
	// one level up from the nested spelling above.
	"a line of prose written as a key":                   {answers: "cannot unmarshal ", block: "description: a bot that reviews pull requests\n", want: workflowfile.Frontmatter{Description: "a bot that reviews pull requests"}},
	"a list block written as the keys it meant":          {answers: "cannot unmarshal ", block: "name: probe\ndescription: d\n", want: workflowfile.Frontmatter{Name: "probe", Description: "d"}},
	"a top-level complex key written as plain text":      {answers: "cannot unmarshal ", block: "name: probe\na: 1\n", want: workflowfile.Frontmatter{Name: "probe"}, extra: []string{"a"}},
	"an item of a list written as one line of text":      {answers: "cannot unmarshal ", block: "name: probe\ntriggers:\n  - a b\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"a b"}}},
	"an item of a flow list written as one line of text": {answers: "cannot unmarshal ", block: "name: probe\ntriggers: [a b]\n", want: workflowfile.Frontmatter{Name: "probe", Triggers: []string{"a b"}}},
}

// frontmatterUnmarshalTails are the Go types the reading named in its
// refusal of block: the `into %s` tail of each `cannot unmarshal`, one per
// line of a yaml.v2 type error. Read after the LAST ` into ` of the line,
// which a value holding those words of its own cannot move.
func frontmatterUnmarshalTails(block string) []string {
	_, _, err := workflowfile.DecodeFrontmatter(block)
	if err == nil {
		return nil
	}
	var tails []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if !strings.Contains(line, "cannot unmarshal ") {
			continue
		}
		if i := strings.LastIndex(line, " into "); i >= 0 {
			tails = append(tails, strings.TrimSpace(line[i+len(" into "):]))
		}
	}
	return tails
}

// frontmatterUnmarshalTargets are the Go types a node can be decoded into
// on the way to ty — the tail of yaml.v2's `cannot unmarshal %s into %s`,
// which is the only thing that tells that family's causes apart. Walked off
// the type the reading decodes into rather than listed: a fifth key, or a
// list of something other than text, adds a target here.
//
// Three rules, each the vendored decoder's own: a struct's keys are read
// into a plain `string` whatever its fields are named (mappingStruct's
// `settableValueOf("")`), an `interface{}` takes any node so no refusal
// ever names it, and a `,inline` field is filled from the parent's own
// mapping — key by key for a map, field by field for a struct — so no node
// is ever decoded into it whole.
func frontmatterUnmarshalTargets(ty reflect.Type) []string {
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
	walk(ty)
	targets := make([]string, 0, len(seen))
	for t := range seen {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	return targets
}

// `cannot unmarshal ` is ONE message string whose causes differ by the Go
// type the reading was decoding into, and that type is what a remedy
// naming one cause leaves the other causes' authors without: the family is
// held at the class, one witness per type the reading can be asked to
// decode a node into. A fifth key of another shape, or a list of something
// other than text, reddens here until a block reaches it.
//
// The type walked is checked against the one that RUNS: the reading's own
// refusal of a block that is not a mapping names it, so a mirror that
// drifts from workflowfile.DecodeFrontmatter cannot pass for it.
func TestEveryGoTypeTheFrontmatterReadingDecodesIntoHasAWitness(t *testing.T) {
	var mirror struct {
		workflowfile.Frontmatter `yaml:",inline"`
		Rest                     map[string]any `yaml:",inline"`
	}
	root := reflect.TypeOf(mirror)
	read, _ := frontmatterRefusal("- a\n")
	if want := "cannot unmarshal !!seq into " + root.String(); !strings.Contains(read, want) {
		t.Fatalf("the reading refuses a block that is not a mapping with %q, which does not hold %q: the type walked below is not the type it decodes into", read, want)
	}
	reached := map[string]string{}
	for name, w := range frontmatterWitnesses {
		for _, tail := range frontmatterUnmarshalTails(w.src) {
			reached[tail] = name
		}
	}
	for _, target := range frontmatterUnmarshalTargets(root) {
		if reached[target] == "" {
			t.Errorf("no frontmatter witness is refused with `cannot unmarshal … into %s`: that cause of the family reaches an author with a remedy nothing proves answers it", target)
		}
	}
	// … and no witness names a type the walk does not know: a tail the
	// walk misses is a cause the gate above cannot ask for.
	for tail, name := range reached {
		if !slices.Contains(frontmatterUnmarshalTargets(root), tail) {
			t.Errorf("%s is refused with `cannot unmarshal … into %s`, a type the walk of the reading's own type does not reach: the walk is the gate's inventory and it is short", name, tail)
		}
	}
}

// A frontmatter block written as its remedy says is one the catalogue's
// reader reads, AS the identity the prescription names: a spelling that
// silences the refusal by changing what the block says is one the remedy
// owes its author in words, not one the gate may accept in silence.
func TestAFrontmatterWrittenAsItsRemedySaysReads(t *testing.T) {
	for name, p := range frontmatterPrescriptions {
		fm, extra, err := workflowfile.DecodeFrontmatter(p.block)
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

// yamlRemedyOf is the index of the entry whose remedy ends remedy, -1
// where none does and -2 where several would: the entry a refusal reached,
// read off the words it carries rather than off a name.
func yamlRemedyOf(remedy string) int {
	found := -1
	for i, r := range yamlRemedies {
		if !strings.HasSuffix(remedy, r.remedy) {
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
// its remedy says, and every prescription row names the entry it answers:
// the identity test walks hand-written blocks, so without this link an
// entry could carry a spelling nothing ever writes. The link is the
// entry's own message, read against the table — a reworded message
// reddens here rather than leaving the row answering for nothing.
//
// Scope: the frontmatter half. The author document's own prescriptions
// (remedyPrescriptions) carry no such link, and several entries prescribe
// no rewrite of a document at all ("flatten it", "remove it"), so the
// same gate there needs an exclusion table of its own.
func TestEveryFrontmatterRemedyReachedHasAPrescription(t *testing.T) {
	answered := map[int]bool{}
	for name, p := range frontmatterPrescriptions {
		at := -1
		for i, r := range yamlRemedies {
			if slices.Contains(r.messages, p.answers) {
				at = i
				break
			}
		}
		if at < 0 {
			t.Errorf("%s answers %q, which no entry of yamlRemedies lists: the row prescribes a spelling the table does not say", name, p.answers)
			continue
		}
		answered[at] = true
	}
	for name, w := range frontmatterWitnesses {
		_, remedy := frontmatterRefusal(w.src)
		if remedy == "" {
			// Said by TestEveryFrontmatterYAMLMessageNamesItsRemedy.
			continue
		}
		switch at := yamlRemedyOf(remedy); {
		case at == -2:
			t.Errorf("the refusal of %s ends with the remedy of more than one entry: the entry a block reaches cannot be read off its words", name)
		case at < 0:
			t.Errorf("the refusal of %s ends with no entry's remedy: %s", name, remedy)
		case !answered[at]:
			t.Errorf("%s reaches remedy %d, which no row of frontmatterPrescriptions answers: its spelling is prescribed to an author and written nowhere", name, at)
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
	read, remedy := frontmatterRefusal("name: !!int notanint\n")
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
		fm, _, err := workflowfile.DecodeFrontmatter(p.block)
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
			read, remedy := frontmatterRefusal(block)
			switch {
			case read == "":
				t.Errorf("`%s: %s` reads, where the clause tells its author the key takes a list", key, value)
			case !strings.Contains(remedy, "`[a, b]` on one line"):
				t.Errorf("`%s: %s` is refused with %q, whose remedy does not name the list the key takes: %s", key, value, read, remedy)
			}
		}
	}
}

// Every YAML message the frontmatter block's reading surfaces arrives with
// the spelling that avoids it. Read through the two calls fmt_author.go
// makes, in its order: a message this table answers with a reason instead
// of a remedy reaches an author bare in the notice `fmt --to yaml` prints.
func TestEveryFrontmatterYAMLMessageNamesItsRemedy(t *testing.T) {
	for name, w := range frontmatterWitnesses {
		read, remedy := frontmatterRefusal(w.src)
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
			read, remedy := frontmatterRefusal(tc.refused)
			if remedy == "" {
				t.Fatalf("%q reaches the author bare, so no sentence of the entry is under test", read)
			}
			// The reading that resolves the construct, executed: a remedy
			// claiming it refuses one would be false of the identity below.
			fm, _, err := workflowfile.DecodeFrontmatter(tc.resolved)
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

// The author text that reaches the remedy table is bounded by the words
// the reader's own format writes (yamlValueSlots); what no slot bounds is
// the name of an anchor, which a reader writes into `unknown anchor '%s'
// referenced` and `anchor '%s' value contains itself`. That name ends at
// the first space — measured, `name: &a b probe` reads as the value
// `b probe`, and `*mapping values are not allowed` names the anchor
// `mapping` alone — where every message of the table holds a space, and so
// does the `line N:` the block's line note reads: no anchor name can spell
// either.
func TestNoRemedyMessageIsSpelledByTheTextNoSlotBounds(t *testing.T) {
	fm, _, err := workflowfile.DecodeFrontmatter("name: &a b probe\n")
	switch {
	case err != nil:
		t.Fatalf("`name: &a b probe` is refused (%v): an anchor name's bound is no longer the first space, and the messages below are spellable by one", err)
	case fm.Name != "b probe":
		t.Fatalf("`name: &a b probe` reads as %q, not as the value `b probe`: an anchor name can now hold a space", fm.Name)
	}
	read, _ := frontmatterRefusal("name: probe\ndescription: *mapping values are not allowed\n")
	if want := "unknown anchor 'mapping' referenced"; !strings.Contains(read, want) {
		t.Errorf("an alias spelling a message of the table is refused with %q, not with %q: the name the reader writes back is no longer bounded by a space", read, want)
	}
	for i, r := range yamlRemedies {
		for _, m := range r.messages {
			if !strings.Contains(m, " ") {
				t.Errorf("remedy %d answers %q, which holds no space: an anchor name — the author text no slot of yamlValueSlots bounds — can spell it", i, m)
			}
		}
	}
	if yamlLineRe.MatchString("line1:") {
		t.Errorf("%s matches a number with no space before it: an anchor name can then spell a line of the block", yamlLineRe)
	}
}

// Every slot of yamlValueSlots is bounded by words a vendored reader
// really writes: its opening, and the closing word that ends it standing
// after that opening, in one message string of one of the two readers.
// A slot keyed on words no reader writes bounds nothing, and a reader that
// rewords one of the three formats reddens here rather than letting an
// author's value answer for the message again.
func TestEveryValueSlotIsAReaderFormat(t *testing.T) {
	var all []string
	for _, mod := range yamlVendorModules {
		for lit := range yamlProblemStrings(t, mod.path) {
			all = append(all, lit)
		}
	}
	for i, s := range yamlValueSlots {
		written := false
		for _, lit := range all {
			at := strings.Index(lit, s.open)
			if at < 0 || s.close != "" && !strings.Contains(lit[at+len(s.open):], s.close) {
				continue
			}
			written = true
			break
		}
		if !written {
			t.Errorf("slot %d bounds a value between %q and %q, which no message of either vendored reader writes that way: the slot bounds nothing", i, s.open, s.close)
		}
	}
}

// Every YAML message the author path surfaces arrives with the spelling
// that avoids it, and a message several mistakes produce names each of
// them: yaml.v3's own words say what the scanner saw, never what to write.
// No remedy of the table is dead and no message of a remedy unreached:
// each is one a document above is refused with.
func TestEveryYAMLMessageNamesTheRemedyOfEachOfItsCauses(t *testing.T) {
	seen := make([]string, 0, len(yamlWitnesses))
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
		default:
			seen = append(seen, refusal)
		}
	}
	// The table has two consumers, so its coverage has two witness sets: a
	// message no author document can raise is reached through the
	// frontmatter block instead, never left unwitnessed.
	for _, w := range frontmatterWitnesses {
		if read, remedy := frontmatterRefusal(w.src); remedy != "" {
			seen = append(seen, read+remedy)
		}
	}
	for i, r := range yamlRemedies {
		for _, m := range r.messages {
			reached := false
			for _, msg := range seen {
				// The remedy ends the refusal; what stands between it and
				// yaml.v3's own words is the position note, which only the
				// messages raised with a context mark carry.
				if strings.Contains(msg, m) && strings.Contains(msg, " — ") && strings.HasSuffix(msg, r.remedy) {
					reached = true
					break
				}
			}
			if !reached {
				t.Errorf("remedy %d answers %q, which no document and no frontmatter block above is refused with", i, m)
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

// The table is read in order and the first entry whose message the text
// holds answers it: an entry's message that is a substring of a later
// entry's would answer in its place (`did not find expected whitespace` of
// `did not find expected whitespace or line break`).
func TestTheYAMLRemedyOrderIsUnambiguous(t *testing.T) {
	for i, early := range yamlRemedies {
		for j := i + 1; j < len(yamlRemedies); j++ {
			for _, a := range early.messages {
				for _, b := range yamlRemedies[j].messages {
					if strings.Contains(b, a) {
						t.Errorf("remedy %d answers %q, which hides remedy %d's %q: order them the other way", i, a, j, b)
					}
				}
			}
		}
	}
}

const (
	notUTF8First    = "every author-facing reading refuses a source that is not UTF-8 before a decoder reads it: decode's own guard for the document, and the lexer's E006 for a frontmatter block, whose lines a parse has already replaced"
	streamInvariant = "an invariant of the event stream the parser feeds the composer, not of a document"
	emitDirection   = "the emit direction: both readings read a document, neither writes one"
	strictOnly      = "the strict reading alone collects it — yaml.v2 gates its three on d.strict and yaml.v3 on d.uniqueKeys, which only UnmarshalStrict sets, and the fields one on d.knownFields, which only Decoder.KnownFields does: the one reading of a block calls yaml.Unmarshal (workflowfile.DecodeFrontmatter) and the document is read into a *yaml.Node, which has no field and no map. Executed by TestTheBlocksTheStrictOnlyRefusalsNameRead, which reddens where a reading turns strict and these messages start reaching an author bare"
)

// yamlProblemsOutOfReach are the problem strings of either vendored
// reader's error constructors no author-facing reading reaches — neither
// author.decode for the document nor workflowfile.DecodeFrontmatter for a
// `.bot` frontmatter block, whose message `fmt --to yaml` shows — each with
// the reason it cannot, for BOTH of them: the escape hatch of the remedy
// table, where a message listed here carries no remedy on purpose. A reason
// that holds for one reading and not the other is a silent allowlist: the
// frontmatter block decodes into a Go struct, so it resolves the tags,
// aliases and merge keys the document's node tree leaves alone.
var yamlProblemsOutOfReach = map[string]string{
	"did not find expected <stream-start>": "the stream start is the first token of every stream the decoder reads",
	"input error: ":                        "the source is bytes already in memory — a bytes.Reader for the document, the block's own text for a frontmatter — which never fails to read",
	// A UTF-16 BOM is not UTF-8 either, so the reader never decodes a
	// UTF-16 unit on this path.
	"invalid leading UTF-8 octet":        notUTF8First,
	"incomplete UTF-8 octet sequence":    notUTF8First,
	"invalid trailing UTF-8 octet":       notUTF8First,
	"invalid length of a UTF-8 sequence": notUTF8First,
	"invalid Unicode character":          notUTF8First,
	"incomplete UTF-16 character":        notUTF8First,
	"unexpected low surrogate area":      notUTF8First,
	"incomplete UTF-16 surrogate pair":   notUTF8First,
	"expected low surrogate area":        notUTF8First,
	// The tags, aliases and merge keys a frontmatter block's reading
	// resolves ARE reachable, and carry a remedy: the two left here are
	// the ones neither reading asks for.
	"cannot decode node with unknown kind %d": "yaml.v3's Node.Decode raises it, which neither reading calls: the document is read as a node tree, a frontmatter block with yaml.v2",
	// The messages a decoder COLLECTS rather than raises: one `terrors`
	// element per value it cannot read. `cannot unmarshal ` is the family
	// the frontmatter block's reading raises and carries a remedy; the
	// five below are the strict reading's own.
	"line %d: key %#v already set in map":                      strictOnly,
	"line %d: field %s already set in type %s":                 strictOnly,
	"line %d: field %s not found in type %s":                   strictOnly,
	"line %d: mapping key %#v already defined at line %d":      strictOnly,
	"invalid array: want %d elements but got %d":               "neither reading decodes into a Go ARRAY: the document becomes a *yaml.Node and a frontmatter block the four fields of workflowfile.Frontmatter, whose lists are slices",
	"attempted to go past the end of stream; corrupted value?": streamInvariant,
	"expected %s event but got %s":                             streamInvariant,
	"%s%s":                                                     "a format wrapper: the text it carries is the scanner's, the parser's, the reader's own problem or the fallback below",
	"unknown problem parsing YAML content":                     "the composer's message where the parser left its problem empty: no error constructor of either vendored reader passes an empty problem, which the scan below holds",
	"duplicated key '":                                         "yaml.v3's spelling, raised where a Go struct's own yaml tags collide: the document is decoded into a *yaml.Node, and the frontmatter block's reader is yaml.v2, which spells it with a capital below",
	// The frontmatter block's reader spells the same panic with a capital,
	// and it does decode into a Go struct (workflowfile.Frontmatter): the
	// message is about that struct's own tags, which no document changes.
	"Duplicated key '": "a Go struct whose yaml tags collide, raised while reading the struct rather than the document: the four keys of a frontmatter carry four distinct tags",
	// The emit direction, read so that no error constructor of the package
	// is a silent allowlist: the author write path encodes (author.Write),
	// and what an encoder refuses is a program's defect, not a document's
	// syntax — no remedy of this table answers it.
	"%s": "a format wrapper of the emit direction: the text it carries is the emitter's own problem",
	"unknown problem generating YAML content":                              emitDirection,
	"cannot encode node with unknown kind %d":                              emitDirection,
	"cannot marshal invalid UTF-8 data as %s":                              emitDirection,
	"explicitly tagged !!binary data must be base64-encoded":               emitDirection,
	"anchor value must not be empty":                                       emitDirection,
	"anchor value must contain alphanumerical characters only":             emitDirection,
	"alias value must not be empty":                                        emitDirection,
	"alias value must contain alphanumerical characters only":              emitDirection,
	"expected STREAM-START":                                                emitDirection,
	"expected DOCUMENT-START or STREAM-END":                                emitDirection,
	"expected DOCUMENT-END":                                                emitDirection,
	"expected nothing after STREAM-END":                                    emitDirection,
	"expected SCALAR, SEQUENCE-START, MAPPING-START, or ALIAS, but got %v": emitDirection,
	"incompatible %YAML directive":                                         emitDirection,
	"duplicate %TAG directive":                                             emitDirection,
	"neither tag nor implicit flags are specified":                         emitDirection,
	"tag handle must not be empty":                                         emitDirection,
	"tag handle must start with '!'":                                       emitDirection,
	"tag handle must end with '!'":                                         emitDirection,
	"tag handle must contain alphanumerical characters only":               emitDirection,
	"tag prefix must not be empty":                                         emitDirection,
	"tag value must not be empty":                                          emitDirection,
	"write error: ":                                                        emitDirection,
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
// answered: a remedy in yamlRemedies, or a reason in
// yamlProblemsOutOfReach that holds for BOTH author-facing readings. A bump
// of the author document's reader or of the frontmatter block's that adds
// or rewords a message fails here instead of letting it reach an author
// bare. What this test cannot see is a reason gone stale because a reading
// changed rather than a reader: frontmatterWitnesses above is what holds
// that side, one block per message the second reading really raises.
func TestEveryYAMLProblemStringHasARemedyOrAReason(t *testing.T) {
	anyModule := map[string]bool{}
	for _, mod := range yamlVendorModules {
		found := yamlProblemStrings(t, mod.path)
		// Said, not fatal: a reach that moved must not hide the message the
		// move left without a remedy.
		if len(found) != mod.count {
			t.Errorf("the scan reads %d problem strings off the vendored %s, not %d: its reach moved — read the table against the vendored source before writing the new number", len(found), mod.name, mod.count)
		}
		for lit, from := range found {
			anyModule[lit] = true
			if lit == "" {
				t.Errorf("%s of %s builds an empty problem: the composer's own message takes its place, which is listed as out of reach", from, mod.name)
				continue
			}
			remedy := YAMLRemedy(lit)
			reason, listed := yamlProblemsOutOfReach[lit]
			switch {
			case remedy != "" && listed:
				t.Errorf("%s of %s builds %q, which has a remedy and the reason %q: it is one or the other", from, mod.name, lit, reason)
			case remedy == "" && !listed:
				t.Errorf("%s of %s builds %q, which reaches the author bare: add its remedy to yamlRemedies, or its reason to yamlProblemsOutOfReach", from, mod.name, lit)
			}
		}
	}
	// A reason is stale where NEITHER reader builds the string it names: the
	// two spell a few of their own messages differently, and a reason that
	// holds for one holds for the other's spelling.
	for lit := range yamlProblemsOutOfReach {
		if !anyModule[lit] {
			t.Errorf("yamlProblemsOutOfReach names %q, which no error constructor of either vendored YAML reader builds", lit)
		}
	}
}

// The reason the decoder's strict-only messages carry is EXECUTED, not
// argued: a block each of them would refuse READS today, because the one
// reading calls yaml.Unmarshal and the document is read into a *yaml.Node.
// A reading turned strict — the natural way to make a block refuse a key
// the four do not name — reddens here, where the reason in
// yamlProblemsOutOfReach would otherwise go stale in silence and those
// messages would reach an author bare.
func TestTheBlocksTheStrictOnlyRefusalsNameRead(t *testing.T) {
	for name, block := range map[string]string{
		"a key of the four written twice":     "name: probe\nname: twice\n",
		"a key beyond the four written twice": "name: probe\nk: 1\nk: 2\n",
		"a key the four do not name":          "name: probe\nunknown: 1\n",
	} {
		if _, _, err := workflowfile.DecodeFrontmatter(block); err != nil {
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

// remedyPrescriptions holds one document per spelling the table prescribes,
// written as it says. The prescriptions that delete a stray line all leave
// the same document — the one that reads on its own — so they share one
// row: every other row carries a document of its own.
var remedyPrescriptions = map[string]string{
	"a command in single quotes":                cmdDoc(`command: 'jq -r .a: .b'`),
	"a regex in single quotes":                  cmdDoc(`command: '^\d+$'`),
	"a Windows path in single quotes":           cmdDoc(`command: 'C:\temp\x'`),
	"a command holding two ampersands":          cmdDoc(`command: '[ -f x ] && echo y'`),
	"a value starting with a bang":              cmdDoc(`command: '! grep -q x f'`),
	"a value starting with a percent":           cmdDoc(`command: '%s'`),
	"a value starting with a question mark":     "dsl: 2\nnodes:\n  - agent: a\n    model: m\n    user: '? what now'\n",
	"an escape for a control character":         cmdDoc(`command: "a\u001bb"`),
	"an edge in single quotes":                  edgeDoc(`'a -> done with { x: "{{outputs.a.y}}" }'`),
	"a flow list with its commas":               "dsl: 2\nworkflow:\n  name: w\n  edges: [\"a -> done\", \"b -> done\"]\n",
	"a block body indented under its key":       promptDoc("|\n      x"),
	"a list with one item per line":             "dsl: 2\nimports:\n  - lib/x.bot\n",
	"a list in one flow line":                   "dsl: 2\nimports: [lib/x.bot, lib/y.bot]\n",
	"a glob in single quotes":                   cmdDoc(`command: 'rm *.tmp'`),
	"a shell pipe in single quotes":             cmdDoc(`command: 'grep x f | wc -l'`),
	"a list whose every item carries its dash":  "dsl: 2\nimports:\n  - lib/a.bot\n  - lib/b.bot\n",
	"a flow list closed":                        "dsl: 2\nimports: [\"lib/a.bot\", \"lib/b.bot\"]\n",
	"a flow mapping closed, with its commas":    "dsl: 2\nvars: {a: string, b: string}\n",
	"a mapping with one key: value line a line": "dsl: 2\nvars:\n  a: string\n  b: string\n",
	"a closing bracket quoted in a value":       cmdDoc(`command: ']'`),
	"a document indicator indented in a value":  cmdDoc("command: \"a\n    ...\n    b\""),
	// The document a stray line deleted leaves: the prescription of every
	// clause that names one — a title above the first key, a `- item` or a
	// `|`/`>` block between two keys or as the first line, a `[…]` first
	// line, a first key quoted whole and unquoted again.
	"a document with no ... line":                   readsAloneHead + readsAloneBody,
	"a stray block moved under its key":             readsAloneHead + "prompts:\n  p: |\n    a title\n" + readsAloneBody,
	"a stray list item moved under its key":         readsAloneHead + "imports:\n  - lib/a.bot\n" + readsAloneBody,
	"a first-line block moved under its key":        "prompts:\n  p: |\n    a title\n" + readsAloneHead + readsAloneBody,
	"a first-line flow list moved under its key":    "imports: [lib/a.bot, lib/b.bot]\n" + readsAloneHead + readsAloneBody,
	"a first key written with its colon":            "dsl: 2\n# a note\nimports:\n  - lib/x.bot\n",
	"a block body indented as its indicator claims": promptDoc("|9\n           x"),
	// The lines a multi-line stray spans, moved under one key: the clause
	// prescribes the move with the lines below the key indented under it,
	// one spelling per shape. Written flat, each of them is refused with
	// the message the author just read.
	"a two-line title moved under one key":      readsAloneHead + "prompts:\n  p: my first\n    bot\n" + readsAloneBody,
	"a two-line quoted value moved under a key": readsAloneHead + "prompts:\n  p: \"my first\n    bot\"\n" + readsAloneBody,
	"a broken flow list moved under its key":    readsAloneHead + "imports: [lib/a.bot,\n  lib/b.bot]\n" + readsAloneBody,
	"a two-item list moved under its key":       readsAloneHead + "imports:\n  - lib/a.bot\n  - lib/b.bot\n" + readsAloneBody,
	// Every key indented deeper than the keys beside it, dedented to their
	// column: one left behind is refused with the same message.
	"two keys indented deeper, both dedented": "dsl: 2\nimports:\n  - lib/x.bot\nvars:\n  a: string\n",
	// The two spellings that give a document opening with an indented block
	// one indentation: the block dedented to the shallower column, and the
	// lines below it indented to the block's.
	"an indented first block dedented whole":         "dsl: 2\nimports:\n  - lib/x.bot\nnodes:\n  - agent: a\n    model: m\n",
	"the lines below an indented block indented too": "  dsl: 2\n  imports:\n    - lib/x.bot\n  nodes:\n    - agent: a\n      model: m\n",
	// The same two spellings on a head carrying THREE indentations, where
	// the column of the line that closed the block is neither of them.
	"a three-indentation head dedented to one column": "dsl: 2\nimports:\n   - lib/a.bot\nnodes:\n  - agent: a\n    model: m\n",
	"a three-indentation head indented to one column": "    dsl: 2\n    imports:\n     - lib/a.bot\n    nodes:\n      - agent: a\n        model: m\n",
}

// A document written as its remedy says reads: the spelling each one
// prescribes is one the author path accepts.
func TestADocumentWrittenAsItsRemedySaysReads(t *testing.T) {
	for name, src := range remedyPrescriptions {
		if res := Parse("x.bot.yaml", []byte(src)); res.HasErrors() {
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
		src := remedyPrescriptions[name]
		if had, dup := first[src]; dup {
			t.Errorf("%q carries the document %q already carries: one of the two proves nothing the other does not", name, had)
			continue
		}
		first[src] = name
	}
}

// sortedKeys is m's keys in order: a duplicate is reported against the
// same row at every run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
