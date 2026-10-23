package canon

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// commentedDoc carries a head comment, a line comment on a property, a
// comment above a node and a foot comment, in a form the writer does not
// produce: the sequences sit at the margin.
const commentedDoc = `# keep me: the document's head
dsl: 2
prompts:
  ask: Say hello. # and me, on the property
nodes:
# above the node
- agent: hello
  model: m
  system: ask
workflow:
  name: hello
  entry: hello
  edges:
  - hello -> done
# the foot
`

// A commented document is rewritten in its canonical form with every
// comment where the author wrote it — on the node it hangs on — and the
// result is its own canonical form, the same program.
func TestTheDocumentsCanonicalFormKeepsItsComments(t *testing.T) {
	out, err := Document("x.bot.yaml", []byte(commentedDoc))
	if err != nil {
		t.Fatalf("a commented document was refused: %v", err)
	}
	got := string(out)
	if got == commentedDoc {
		t.Fatal("the fixture is already canonical: it proves nothing about a rewrite")
	}
	want := `# keep me: the document's head
dsl: 2
prompts:
  ask: Say hello. # and me, on the property
nodes:
  # above the node
  - agent: hello
    model: m
    system: ask
workflow:
  name: hello
  entry: hello
  edges:
    - hello -> done
# the foot
`
	if got != want {
		t.Fatalf("the comments are not where they were written:\n%s\nwant\n%s", got, want)
	}
	again, err := Document("x.bot.yaml", out)
	if err != nil || string(again) != got {
		t.Fatalf("the rewritten document is not its own canonical form: %v\n%s", err, again)
	}
	if unparse.Unparse(author.Parse("x.bot.yaml", out).File) != unparse.Unparse(author.Parse("x.bot.yaml", []byte(commentedDoc)).File) {
		t.Fatal("the rewritten document is another program")
	}
}

// A comment rides its node, never its position: the writer orders a node's
// keys by the registry, and the comment of a key the author wrote last
// stays on that key.
func TestADocumentsCommentFollowsItsKey(t *testing.T) {
	src := strings.Replace(commentedDoc, "  model: m\n  system: ask\n", "  system: ask # the system prompt\n  model: m # the model\n", 1)
	out, err := Document("x.bot.yaml", []byte(src))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	for _, line := range []string{"    model: m # the model\n", "    system: ask # the system prompt\n"} {
		if !strings.Contains(string(out), line) {
			t.Fatalf("%q is not on its key:\n%s", line, out)
		}
	}
}

// blockDoc describes looseDoc's program in block style, its last
// declaration nested: a comment indented under it is one yaml.v3 hangs on
// that node, not on the document's foot.
const blockDoc = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// scalarDoc closes on a block scalar whose body's last line starts with
// `#`: the author's text, written where a comment reader could take it for
// a comment of its own.
const scalarDoc = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: |\n    Say hello.\n    # Role\n"

// indicatorDoc closes on a block scalar whose header declares its own
// indentation (`|2`), with a body line above the column its first line sits
// at — and that line starts with `#`.
const indicatorDoc = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: |2\n        echo one\n      # not a comment\n"

// quotedDoc closes on a multi-line QUOTED value whose last line starts with
// `#`: the author's text up to the closing quote. The writer renders that
// value on one line, single-quoted, so the same text comes back after a
// blank instead of opening a line — declared as text on both sides.
const quotedDoc = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: \"Say hello.\n    # not a comment\"\n"

const singleQuotedDoc = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: 'Say hello.\n    # not a comment'\n"

// footTwinDoc carries `# twice` as a mid-document FOOT comment — a blank
// line below it — so a twin of that text in the trailing region is a second
// comment, over nodes that carry none in between.
// emptyDoc closes on a block scalar with NO body: yaml.v3 reads the value
// as "", so the lines below the header are the trailing region's and the
// body's indentation has no first line to be read off. emptyScriptDoc is
// the tool-node shape of it.
const emptyDoc = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: |\n"

const emptyScriptDoc = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: |\n"

const footTwinDoc = "dsl: 2\nprompts:\n  ask: Say hello.\n  # twice\n\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// flowCloseDoc writes the same comment text twice around a flow
// collection — inside it and after its closing bracket — on a closing line
// whose own value holds a `#`, which a reader of that line must look past
// to find the comment; flowBareCloseDoc writes the pair as a bare `#`,
// which a scan one character short of the line's end never meets.
const flowCloseDoc = "dsl: 2\nprompts: {ask: \"Say hello.\", # twice\n  other: \"a # b\"} # twice\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowBareCloseDoc = "dsl: 2\nprompts: {ask: \"Say hello.\", #\n  other: b} #\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// trueComments counts the YAML comment lines text carries INDEPENDENTLY of
// author.Comments — the reader these tables measure, which cannot be its own
// oracle: a reader that reads one short reads one short on both sides of a
// rewrite, and the comparison sees nothing. A comment is a `#` opening a
// line, or a `#` after a blank anywhere else on it, and it runs to the end
// of that line: a line carries at MOST ONE. EVERY such `#` is a candidate,
// since the first may sit inside a quoted value and the comment after it,
// and the texts a fixture declares as its own — a block scalar's body line,
// a quoted scalar's continuation, the run a quoted `#` opens — are
// subtracted, once each, wherever they sit; the first candidate of a line
// that survives that subtraction is the line's comment, and the rest of the
// line is inside it.
func trueComments(text string, notComments []string) []string {
	skip := map[string]int{}
	for _, l := range notComments {
		skip[l]++
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		var found []string
		for ci := 0; ci < len(line); ci++ {
			if line[ci] == '#' && (ci == 0 || line[ci-1] == ' ' || line[ci-1] == '\t') {
				found = append(found, strings.TrimSpace(line[ci:]))
			}
		}
		for _, f := range found {
			if skip[f] > 0 {
				skip[f]--
				continue
			}
			out = append(out, f)
			break
		}
	}
	return out
}

// The oracle these tables measure the reader against is itself held to
// what YAML says a comment is: one per line, running to the end of it. A
// text carrying a ` #` of its own is ONE comment, not two — an oracle that
// split it would inflate both sides of a rewrite, and the refusal branch
// (`kept == carried` fails) is the one an inflated carried count turns
// green for the wrong reason. The candidate the subtraction leaves is the
// line's comment: the quoted `#` of a declared text is skipped and the
// comment after it taken, which is what the flow-close bodies need.
func TestTrueCommentsCountsOneCommentPerLine(t *testing.T) {
	for _, tc := range []struct {
		name        string
		text        string
		notComments []string
		want        string
	}{
		{"a comment carrying a hash of its own", "a: b # c # d\n", nil, "# c # d"},
		{"the same, on its own line", "# c # d\na: b\n", nil, "# c # d"},
		{"three hashes in one comment", "a: b # 1 # 2 # 3\n", nil, "# 1 # 2 # 3"},
		{"a declared quoted hash, the comment after it", "  other: \"a # b\"} # twice\n", []string{`# b"} # twice`}, "# twice"},
		{"a line with no comment at all", "a: b\n", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(trueComments(tc.text, tc.notComments), "|"); got != tc.want {
				t.Fatalf("trueComments(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// commentMultiset is trueComments' answer as a sorted multiset: a rewrite
// moves a comment onto its node, so the ORDER of the two sides is the
// reader's business, not this count's.
func commentMultiset(text string, notComments []string) string {
	got := trueComments(text, notComments)
	sort.Strings(got)
	return strings.Join(got, "|")
}

// Around a `...` document-end marker, a comment yaml.v3 DOES attach rides
// its node as any other: the document is rewritten, every comment line kept,
// and the result is its own canonical form. Counting such a line twice would
// refuse a document a rewrite loses nothing of — an INDENTED block above the
// marker, which yaml.v3 hangs on the last node rather than on the document's
// foot, and a block scalar's body line that starts with `#`, which is no
// comment at all, are the two shapes that counting costs the most.
func TestTheDocumentsCanonicalFormKeepsTheCommentsADocumentEndMarkerLeavesAttached(t *testing.T) {
	// text lists what a fixture writes that LOOKS like a comment and is not,
	// on either side of the rewrite: trueComments subtracts it.
	bodyText := []string{"# Role"}
	declaredText := []string{"# not a comment"}
	quotedText := []string{`# not a comment"`, "# not a comment'"}
	for name, tc := range map[string]struct {
		doc, tail string
		text      []string
	}{
		"the block directly above the marker":              {looseDoc, "# one\n# two\n...\n", nil},
		"a blank between the block and the marker":         {looseDoc, "# before\n\n...\n", nil},
		"a block on the node and a block above":            {looseDoc, "# before\n\n# b\n...\n", nil},
		"two blocks on the document's foot":                {looseDoc, "\n# d1\n\n# d2\n...\n", nil},
		"the block directly below the marker":              {looseDoc, "...\n# a1\n# a2\n", nil},
		"a marker alone":                                   {looseDoc, "...\n", nil},
		"an indented block above the marker":               {blockDoc, "  # one\n  # two\n...\n", nil},
		"an indented block above the marker and one below": {blockDoc, "  # one\n...\n# below\n", nil},
		"an indented and a column-0 line above the marker": {blockDoc, "  # one\n# two\n...\n", nil},
		"an indented block below the marker":               {blockDoc, "...\n  # below\n", nil},
		"a block scalar body line that starts with a hash": {scalarDoc, "...\n", bodyText},
		"a comment below such a body line":                 {scalarDoc, "# c\n...\n", bodyText},
		"an indented comment below such a body line":       {scalarDoc, "  # c\n...\n", bodyText},
		"such a body line under a declared indentation":    {indicatorDoc, "", declaredText},
		"a marker below such a declared body":              {indicatorDoc, "...\n", declaredText},
		"a comment below such a declared body":             {indicatorDoc, "# c\n...\n", declaredText},
		// A multi-line QUOTED value runs to its closing quote: a `#`
		// opening its last line is the author's text, and reading it as a
		// comment refuses a document that carries none.
		"a quoted value whose last line starts with a hash":        {quotedDoc, "", quotedText},
		"a marker below such a quoted value":                       {quotedDoc, "...\n", quotedText},
		"a comment below such a quoted value":                      {quotedDoc, "# c\n...\n", quotedText},
		"an indented comment below such a quoted value":            {quotedDoc, "  # c\n...\n", quotedText},
		"a single-quoted value whose last line starts with a hash": {singleQuotedDoc, "", quotedText},
		// A mid-document FOOT comment and its twin in the trailing region
		// are two comments: the rewrite keeps both where they hang.
		"a mid-document foot twin above a marker": {footTwinDoc, "# twice\n...\n", nil},
		"a mid-document foot twin after a blank":  {footTwinDoc, "\n# twice\n", nil},
		"a mid-document foot and another text":    {footTwinDoc, "# other\n...\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			src := tc.doc + tc.tail
			out, err := Document("x.bot.yaml", []byte(src))
			if err != nil {
				t.Fatalf("a document a rewrite loses no comment of was refused: %v", err)
			}
			// The independent count first: it is the one that can tell a
			// line the reader is blind to on BOTH sides from a kept one.
			if had, kept := commentMultiset(src, tc.text), commentMultiset(string(out), tc.text); had != kept {
				t.Fatalf("the written form carries the comment lines %q, the source %q:\n%s", kept, had, out)
			}
			if had, kept := author.Comments([]byte(src)), author.Comments(out); strings.Join(had, "|") != strings.Join(kept, "|") {
				t.Fatalf("the written form carries %q, the source %q:\n%s", kept, had, out)
			}
			again, err := Document("x.bot.yaml", out)
			if err != nil || string(again) != string(out) {
				t.Fatalf("the rewritten document is not its own canonical form: %v\n%s", err, again)
			}
		})
	}
}

// A block scalar header carries its comment against its own indicators —
// `|2#c`, with no blank before the `#`, which the scanner reads as a comment
// all the same — and the indentation it declares is read all the same: the
// body below it, the line starting with `#` included, is the author's text,
// so the document is rewritten with its one comment kept instead of being
// refused over its own prose. The last row is the control: the same header
// with the blank the writer itself emits.
func TestTheCanonicalFormKeepsADocumentWhoseHeaderCommentOpensAgainstItsIndicators(t *testing.T) {
	bodyText := []string{"# not a comment"}
	for _, header := range []string{"|2#c", "|2+#c", "|2-#c", ">2#c", "|1#c", "|2 #c"} {
		t.Run(header, func(t *testing.T) {
			src := strings.Replace(indicatorDoc, "script: |2\n", "script: "+header+"\n", 1)
			if !strings.Contains(src, header) {
				t.Fatalf("the fixture carries no header %q", header)
			}
			if res := author.Parse("x.bot.yaml", []byte(src)); res.HasErrors() {
				t.Fatalf("the author refuses the document: the rewrite below proves nothing\n%v", res.Diagnostics)
			}
			out, err := Document("x.bot.yaml", []byte(src))
			if err != nil {
				t.Fatalf("a document whose body a rewrite keeps whole was refused: %v", err)
			}
			if !strings.Contains(string(out), "# not a comment") {
				t.Fatalf("the rewrite dropped the body line the header declares:\n%s", out)
			}
			if got, want := commentMultiset(string(out), bodyText), "#c"; got != want {
				t.Fatalf("the written form carries the comment lines %q, want %q:\n%s", got, want, out)
			}
			if got, want := author.Comments([]byte(src)), "#c"; strings.Join(got, "|") != want {
				t.Fatalf("the reader lists %q, want the header's comment %q alone: the body it declares is the author's text", got, want)
			}
		})
	}
}

// The same comment text written twice around a flow collection — once
// INSIDE it, once after its closing bracket — is two comments, whatever
// else the closing line carries: a value of its own holding a `#` before
// the comment, or a comment that is a bare `#` ending the line. yaml.v3
// hands the inside one back hung on the collection, so a reader that takes
// the hung copy for it counts the pair as ONE comment — and then the
// rewrite drops a line with nothing to report where the written form has
// no room for it, and refuses, for a line it would "write twice", a
// document it keeps both of.
//
// The canonical form keeps both where it has room: the inside one on its
// own key, the one after the bracket on the declaration that follows the
// collection, the written form having no bracket to put it after. Written
// on the LAST declaration, the collection has nothing after it, and the
// rewrite is refused naming the line, with nothing written.
func TestTheCanonicalFormKeepsBothCommentsWrittenAroundAFlowCollectionsClose(t *testing.T) {
	const head = "dsl: 2\nprompts: {ask: \"Say hello.\", %s\n  other: %s} %s\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	const last = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow: {name: hello, entry: hello, %s\n  edges: [%s]} %s\n"
	for name, tc := range map[string]struct{ src, want, refused string }{
		"a value holding a hash on the closing line": {
			src:  fmt.Sprintf(head, "# twice", `"a # b"`, "# twice"),
			want: "dsl: 2\nprompts:\n  ask: Say hello. # twice\n  other: 'a # b'\nnodes: # twice\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n",
		},
		"a bare hash on the closing line": {
			src:  fmt.Sprintf(head, "#", `"b"`, "#"),
			want: "dsl: 2\nprompts:\n  ask: Say hello. #\n  other: b\nnodes: #\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n",
		},
		"nothing after the collection to carry it": {
			src:     fmt.Sprintf(last, "# twice", `"hello -> done"`, "# twice"),
			refused: "1 of 2 comment line(s) would be lost (the first: # twice)",
		},
		"nothing after the collection, both bare": {
			src:     fmt.Sprintf(last, "#", "hello -> done", "#"),
			refused: "1 of 2 comment line(s) would be lost (the first: #)",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := author.Comments([]byte(tc.src)); len(got) != 2 {
				t.Fatalf("the reader lists %q: the two comment lines written around the bracket are two", got)
			}
			out, err := Document("x.bot.yaml", []byte(tc.src))
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("the rewrite answered %v and wrote\n%s\nwant a refusal naming %q", err, out, tc.refused)
				}
				if out != nil {
					t.Fatalf("a refused rewrite handed back %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("a document whose comments the rewrite keeps was refused: %v", err)
			}
			if string(out) != tc.want {
				t.Fatalf("the canonical form is\n%s\nwant\n%s", out, tc.want)
			}
			again, err := Document("x.bot.yaml", out)
			if err != nil || string(again) != string(out) {
				t.Fatalf("the rewritten document is not its own canonical form: %v\n%s", err, again)
			}
		})
	}
}

// Every tail of up to three tokens of comment lines, blanks and `...`
// markers, over every body of the table — both inventories are asserted
// below the table, so a body dropped from the map or a token added to the
// tails fails there rather than shrinking the sweep in silence. Each
// shape either refuses the rewrite or keeps every comment line the source
// carries — measured by trueComments, never by the reader the rewrite's own
// guard calls, which reads a shape it is blind to one short on BOTH sides
// and reports no loss. A refusal is checked the other way round: the writer
// run on the same program must really lose a line, or the refusal is a
// false one and the document is unformattable for nothing.
func TestNoRewriteOfADocumentEitherLosesACommentLineOrRefusesForNothing(t *testing.T) {
	bodies := map[string]struct {
		text string
		own  []string
	}{
		"loose":   {looseDoc, nil},
		"block":   {blockDoc, nil},
		"scalar":  {scalarDoc, []string{"# Role"}},
		"quoted":  {quotedDoc, []string{`# not a comment"`, "# not a comment'"}},
		"footrun": {footTwinDoc, nil},
		"empty":   {emptyDoc, nil},
		"script":  {emptyScriptDoc, nil},
		// The quoted `#` of the closing line, and the one the written form
		// spells, are the two texts these bodies declare as their own.
		"flowclose":     {flowCloseDoc, []string{`# b"} # twice`, "# b'"}},
		"flowbareclose": {flowBareCloseDoc, nil},
	}
	var tails []string
	var build func(string, int)
	build = func(prefix string, depth int) {
		if depth == 0 {
			tails = append(tails, prefix)
			return
		}
		for _, tok := range []string{"# a", "  # b", "", "...", "...  # m", "# twice"} {
			build(prefix+tok+"\n", depth-1)
		}
	}
	for d := 1; d <= 3; d++ {
		build("", d)
	}
	// What the sweep runs, named and counted: the product of these two is
	// its shape count, and the guard at the end holds the loop to it.
	wantBodies := []string{"block", "empty", "flowbareclose", "flowclose", "footrun", "loose", "quoted", "scalar", "script"}
	if got := slices.Sorted(maps.Keys(bodies)); !slices.Equal(got, wantBodies) {
		t.Fatalf("the sweep runs the bodies %q, want %q", got, wantBodies)
	}
	if len(tails) != 258 {
		t.Fatalf("the sweep runs %d tails, want 258 — every tail of up to three of its six tokens", len(tails))
	}
	shapes := 0
	for bn, b := range bodies {
		for _, tail := range tails {
			shapes++
			src := b.text + tail
			carried := commentMultiset(src, b.own)
			out, err := Document("x.bot.yaml", []byte(src))
			if err == nil {
				if kept := commentMultiset(string(out), b.own); kept != carried {
					t.Fatalf("[%s] tail %q: the rewrite carries the comment lines %q, the source %q — a line deleted in silence:\n%s", bn, tail, kept, carried, out)
				}
				continue
			}
			// Refused: the writer, run on the same program, must drop a
			// line — the byte guard on a trailing block the writer
			// re-indents is a placement refusal, not a counting one.
			res := author.Parse("x.bot.yaml", []byte(src))
			if res.HasErrors() || strings.Contains(err.Error(), "moves them") {
				continue
			}
			w, werr := author.WriteKeepingComments(res.File, []byte(src))
			if werr != nil {
				continue
			}
			if kept := commentMultiset(string(w), b.own); kept == carried {
				t.Fatalf("[%s] tail %q: refused %v, yet the written form carries every comment line the source does (%q):\n%s", bn, tail, err, carried, w)
			}
		}
	}
	if shapes != len(bodies)*len(tails) {
		t.Fatalf("the sweep ran %d shapes, want one per body and tail (%d x %d)", shapes, len(bodies), len(tails))
	}
}

// The lines no node holds and no written form has a place for — the
// explicit document markers, `---` opening a document and `...` closing it,
// and the `%YAML`/`%TAG` directives of a stream — are what an author wrote
// that a rewrite drops with no refusal: the author writer emits none of
// them. Every COMMENT written around one rides its node or refuses the
// rewrite; the marker's own characters are gone from the canonical form,
// and the comment of a marker's own line lands above the node that follows
// it.
func TestTheDocumentsCanonicalFormDropsTheDocumentMarkersThemselves(t *testing.T) {
	for name, src := range map[string]string{
		"a document-start marker carrying a comment": "--- # start\n" + looseDoc,
		"a document-end marker below a comment":      looseDoc + "# only\n...\n",
		"a document-end marker alone":                looseDoc + "...\n",
		"a %YAML directive":                          "%YAML 1.1\n---\n" + looseDoc,
		"a %TAG directive":                           "%TAG !e! tag:example.com,2000:\n---\n" + looseDoc,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Document("x.bot.yaml", []byte(src))
			if err != nil {
				t.Fatalf("a document whose markers carry no comment a rewrite loses was refused: %v", err)
			}
			if had, kept := commentMultiset(src, nil), commentMultiset(string(out), nil); had != kept {
				t.Fatalf("the written form carries the comment lines %q, the source %q:\n%s", kept, had, out)
			}
			if had, kept := author.Comments([]byte(src)), author.Comments(out); strings.Join(had, "|") != strings.Join(kept, "|") {
				t.Fatalf("the written form carries %q, the source %q:\n%s", kept, had, out)
			}
			// A marker is read at column 0 only, with or without a
			// comment on its line (`--- # start`), and a directive is a
			// `%` there: the guard takes every form the source carried.
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "...") || strings.HasPrefix(line, "%") {
					t.Fatalf("the canonical form kept a document marker or directive the writer does not emit:\n%s", out)
				}
			}
		})
	}
}

// A ` #` ending a plain value is kept only while the written form reads the
// same value cut by the same comment: a reading the written form no longer
// reports — the value quoted, the comment moved off it — is named.
func TestACommentReadingTheWrittenFormLosesIsNamed(t *testing.T) {
	src := author.Parse("x.bot.yaml", []byte(commentedDoc)).CommentReadings
	if len(src) != 1 || !author.IsCommentReading(src[0]) {
		t.Fatalf("the fixture carries %d comment reading(s), want 1: %v", len(src), src)
	}
	if why := commentReadingLost(src, src); why != "" {
		t.Fatalf("a written form reporting the same reading lost it: %s", why)
	}
	if why := commentReadingLost(src, nil); !strings.Contains(why, `"Say hello."`) {
		t.Fatalf("a reading the written form lost is not named: %q", why)
	}
	other := parser.Diagnostic{Code: parser.DiagAuthorPromptBody, Severity: parser.SeverityWarning, Message: "a prompt body the lexer settles"}
	if author.IsCommentReading(other) {
		t.Fatal("an E053 reading of another kind is taken for a comment reading")
	}
}

// The canonical form keeps every comment reading of a document however
// many it holds: they are compared whole, never the thousand of each that
// a reading reports — which, the writer reordering the nodes, are not the
// same thousand in the source and in its written form. A document of 1200
// readings, its prompts written before its schemas, is rewritten, its
// readings all kept.
func TestManyCommentReadingsSurviveTheRewrite(t *testing.T) {
	t.Setenv("ITERION_DSL_READ_BUDGET", "")
	var b strings.Builder
	b.WriteString("dsl: 2\nprompts:\n")
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "  p%d: x # c\n", i)
	}
	b.WriteString("schemas:\n  s:\n")
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "    f%d: string # c\n", i)
	}
	src := []byte(b.String())
	out, err := Document("r.bot.yaml", src)
	if err != nil {
		t.Fatalf("a document of 1200 comment readings, reordered by the writer, was refused: %v", err)
	}
	if string(out) == string(src) {
		t.Fatal("the fixture is already canonical: the writer did not reorder it")
	}
	if got := len(author.Parse("r.bot.yaml", out).CommentReadings); got != 1200 {
		t.Errorf("the written form reads %d comment readings, want 1200", got)
	}
}
