package author

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// endBody is the smallest program a document-end marker can close.
const endBody = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// scalarEndBody closes on a block scalar whose body's last line starts
// with `#`: the author's text, on the one line shape a comment reader can
// take for a comment of its own.
const scalarEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: |\n    Say hello.\n    # Role\n"

// declaredTwinBody closes on a block scalar whose header DECLARES its
// indentation (`|2`) inside a block indented 2, so the body runs at column
// 4 — the declaration counted from the block's indentation, not from the
// header's own line — and writes TWIN comment texts below it, one at the
// body's own column. The twin is what makes a body read two columns short
// visible: a single comment such a body swallows is listed all the same,
// off the foot yaml.v3 hangs it on.
const declaredTwinBody = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: |2\n      Say hello.\n    # Role\n  # twice\n  # twice\n# twice\n"

// indicatorEndBody closes on a block scalar whose header DECLARES its
// indentation (`|2`), so a line of its body sits above the first line's
// column — where the body's own indentation, read off its first line, would
// put the end of the body.
const indicatorEndBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: |2\n        echo one\n      # not a comment\n"

// quotedEndBody closes on a multi-line QUOTED value whose last line starts
// with `#`: the author's text up to the closing quote, and no comment.
// quotedEndBody is the double-quoted shape; the single-quoted, anchored and
// tagged ones follow, each with a quote in a place the scan must not take
// for the opening one.
const quotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: \"Say hello.\n    # not a comment\"\n"

const singleQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: 'Say hello, it''s me.\n    # not a comment'\n"

const anchoredQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: &a \"Say hello.\n    # not a comment\"\n"

const escapedQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: \"Say \\\"hello\\\".\n    # not a comment\"\n"

// anchorAboveQuotedEndBody writes the value's ANCHOR on a line of its own,
// so the position yaml.v3 gives the scalar — which it puts on the anchor —
// sits a line ABOVE the opening quote; tagAboveQuotedEndBody does it with a
// tag, and commentedAnchorEndBody writes a real comment in between, where
// its own quote must not be taken for the value's.
const anchorAboveQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: &a\n    \"Say hello.\n    # not a comment\"\n"

const tagAboveQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: !!str\n    \"Say hello.\n    # not a comment\"\n"

const commentedAnchorEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: &a # \" anchored\n    \"Say hello.\n    # not a comment\"\n"

// flowTaggedBody holds a TAGGED quoted value inside a FLOW collection, where
// the comments are read off the source: the tag is a property of the value,
// so the quote behind it opens that value and the ` #` inside it is the
// author's text. flowAnchoredMultiBody writes the anchored shape over lines,
// which the flow read and the trailing region read both reach.
const flowTaggedBody = "dsl: 2\nprompts: {ask: !!str \"Say hello. # not a comment\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowAnchoredBody = "dsl: 2\nprompts: {ask: &a \"Say hello. # not a comment\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowAnchoredMultiBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts: {ask: &a\n    \"Say hello.\n    # not a comment\"}\n"

// anchorAboveBlockEndBody is the BLOCK scalar twin of anchorAboveQuotedEndBody:
// the value's anchor on a line of its own, the `|` indicator below it. The
// body is read down from the node's position, so the indicator's own line
// needs no finding — the shape is clean, and these rows keep it so.
const anchorAboveBlockEndBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: &a\n      |\n      echo one\n      # not a comment\n"

const tagAboveBlockEndBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: !!str\n      |\n      echo one\n      # not a comment\n"

// emptyScalarEndBody closes on a block scalar with no body at all: the
// value yaml.v3 reads is "", so the lines below the header are the trailing
// region's and the body's indentation has no first line to be read off.
// emptyFoldedEndBody is the folded twin.
const emptyScalarEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: |\n"

const emptyFoldedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: >\n"

// tagNameQuoteEndBody writes a TAG whose name carries a `'` — the vendored
// scanner reads one inside a tag name — above a single-quoted value, where
// that `'` must not be taken for the value's opening quote.
const tagNameQuoteEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: !a'b\n    'Say hello.\n    # not a comment'\n"

// flowCommaTagBody and flowBracketTagValueBody write a tag name carrying a
// `,` and a `[` inside a FLOW collection, where reading one as a flow
// indicator or as a bracket loses the value's own quote; flowBracketTagBody
// tags the COLLECTION with such a name, which leaves the source read with no
// closing bracket to report when the token is not skipped whole, and
// flowPlainTagBody is its control.
const flowCommaTagBody = "dsl: 2\nprompts: {ask: !a,b \"Say hello. # not a comment\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowBracketTagValueBody = "dsl: 2\nprompts: {ask: !a[b \"Say hello. # not a comment\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowBracketTagBody = "dsl: 2\nprompts: !a[b {ask: \"Say hello.\"} # real\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// flowBracketTagCommentedBody carries a comment further DOWN the document:
// a tag name's bracket read as the collection's own leaves the source read
// with a depth no bracket closes, and that read then takes every comment
// below for one written inside the collection — the comment of another node
// listed a second time. flowPlainTagCommentedBody is its control.
const flowBracketTagCommentedBody = "dsl: 2\nprompts: !a[b {ask: \"Say hello.\"} # real\nnodes:\n  - agent: hello # on the node\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowPlainTagCommentedBody = "dsl: 2\nprompts: !ab {ask: \"Say hello.\"} # real\nnodes:\n  - agent: hello # on the node\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowPlainTagBody = "dsl: 2\nprompts: !ab {ask: \"Say hello.\"} # real\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// The VERBATIM tag form `!<…>` is DELIMITED by its `>` rather than read by
// the character classes a shorthand name is, so a scan that walks it by
// class stops on the bare `!` and reads the URI as the value: each body
// below is the verbatim twin of a shorthand one already in this file —
// verbatimTagQuoteEndBody of tagNameQuoteEndBody, flowVerbatimTagBody of
// flowTaggedBody, flowVerbatimBracketTagCommentedBody of
// flowPlainTagCommentedBody. verbatimDeclaredEndBody writes a DECLARED
// indentation behind such a tag, where the `>` that closes the tag would be
// taken for the folded indicator and the declaration lost;
// shorthandDeclaredEndBody is its control.
const verbatimTagQuoteEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: !<a'b>\n    'Say hello.\n    # not a comment'\n"

const flowVerbatimTagBody = "dsl: 2\nprompts: {ask: !<a> \"Say hello. # not a comment\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowVerbatimBracketTagCommentedBody = "dsl: 2\nprompts: !<a[b> {ask: \"Say hello.\"} # real\nnodes:\n  - agent: hello # on the node\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const verbatimDeclaredEndBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: !<a> |2\n        echo one\n      # not a comment\n"

const shorthandDeclaredEndBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: !!str |2\n        echo one\n      # not a comment\n"

// verbatimCommentedHeaderBody holds the node's position on a verbatim tag
// whose line carries a COMMENT with a `|9` inside it, the indicator a line
// below: the shape the --force notice counts, and the one a door opening on
// explicit tags would carry to the rewrite.
const verbatimCommentedHeaderBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: !<a> # |9\n      |\n      echo one\n      # not a comment\n"

// accentedKeyQuotedEndBody writes three accented characters inside the
// quoted KEY of a multi-line quoted value: yaml.v3 counts the value's column
// in characters, so a byte index of it lands inside the key — on its closing
// quote, which the scan would take for the value's opening one.
// asciiKeyQuotedEndBody is the same shape with no multibyte character.
const accentedKeyQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  \"ééé ask\": \"Say hello.\n    # not a comment\"\n"

const asciiKeyQuotedEndBody = "dsl: 2\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  \"aaa ask\": \"Say hello.\n    # not a comment\"\n"

// declaredUnderPropertyBody writes a DECLARED indentation (`|2`) with the
// node's property on a line of its own above the indicator, and the
// indicator indented deeper than the body: the one shape blockBodyEnd reads
// short (TestABlockScalarUnderAPropertyLineReadsItsDeclaredBodyShort).
const declaredUnderPropertyBody = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: &a\n          |2\n        echo one\n      # not a comment\n"

const flowRealCommentBody = "dsl: 2\nprompts: {ask: &a \"Say hello.\", # real\n  other: \"x\"}\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// The comments written below a document's last content line are read off
// the source, each listed ONCE and in written order — the ones yaml.v3
// hangs on the document's foot or on a node's, whatever their indentation,
// and the ones it reads and drops (a comment on a marker's line, a block
// the one below it displaces) alike.
func TestCommentsListsTheDocumentEndNeighbourhoodOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		want []string
	}{
		{"a comment on the marker's line", "... # end note", []string{"# end note"}},
		{"after a tab", "...\t# tabbed", []string{"# tabbed"}},
		{"after spaces", "...   # spaced", []string{"# spaced"}},
		{"the block above, displaced by the one below", "# before\n...\n# after\n", []string{"# before", "# after"}},
		{"the block above, attached", "# one\n# two\n...\n", []string{"# one", "# two"}},
		{"a blank between the block and the marker", "# before\n\n...\n# after\n", []string{"# before", "# after"}},
		{"a block on the node and a block on the marker", "# before\n\n# b\n...\n", []string{"# before", "# b"}},
		{"two blocks on the document's foot", "\n# d1\n\n# d2\n...\n", []string{"# d1", "# d2"}},
		{"the same text in two blocks on the document's foot", "\n# x\n\n# x\n...\n", []string{"# x", "# x"}},
		{"a blank between the marker and the block below", "...\n\n# after\n", []string{"# after"}},
		{"the marker's line comment and a block below", "...  # end\n# after\n", []string{"# end", "# after"}},
		{"the same text above and below", "# x\n...\n# x\n", []string{"# x", "# x"}},
		{"a second block below", "...\n# a\n\n# b\n", []string{"# a", "# b"}},
		{"above, on the line and below", "# before\n... # end\n# after\n", []string{"# before", "# end", "# after"}},
		{"a block below, attached", "...\n# a1\n# a2\n", []string{"# a1", "# a2"}},
		{"a blank between the block above and a bare marker", "# a\n\n...\n", []string{"# a"}},
		{"a bare marker", "...\n", nil},
		{"a marker with nothing but blanks after it", "...   \n\n", nil},
		{"a second marker's line comment", "...\n... # second\n", []string{"# second"}},
		{"a comment on each of two markers", "... # one\n...  # two\n", []string{"# one", "# two"}},
		{"a comment on the first of two markers", "... # one\n...\n", []string{"# one"}},
		{"a block between two markers", "...\n# a\n...\n", []string{"# a"}},
		{"a block above the first of two markers", "# above\n...\n...\n# below\n", []string{"# above", "# below"}},
		{"a comment on every line of a marker run", "... # one\n\n# a\n...  # two\n", []string{"# one", "# a", "# two"}},
		// An INDENTED own-line block above the marker is one yaml.v3 hangs
		// on the last node it is written under, not on the document's foot:
		// listing it a second time off the source would refuse a document
		// whose rewrite keeps every comment it carries.
		{"an indented block above the marker", "  # inner\n...\n", []string{"# inner"}},
		{"two indented lines above the marker", "  # i1\n  # i2\n...\n", []string{"# i1", "# i2"}},
		{"an indented block above the marker and a block below", "  # i1\n...\n# below\n", []string{"# i1", "# below"}},
		{"an indented line then a column-0 line above the marker", "  # i1\n# i2\n...\n", []string{"# i1", "# i2"}},
		{"a column-0 line then an indented line above the marker", "# i1\n  # i2\n...\n", []string{"# i1", "# i2"}},
		{"an indented block below the marker", "...\n  # below\n", []string{"# below"}},
		// A block the one below a bare marker displaces off the document's
		// foot is hung on nothing at all: the source is the only place it
		// is written, blank line above it or not.
		{"a block above a blank, displaced by the one below the marker", "\n# p\n\n...\n# q\n", []string{"# p", "# q"}},
		{"the same text above a blank and on the marker's line", "\n# x\n\n...  # x\n", []string{"# x", "# x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Comments([]byte(endBody + tc.tail))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("comments = %q, want %q\n%s", got, tc.want, endBody+tc.tail)
			}
		})
	}
}

// The marker is read at column 0, followed by a blank or the end of its
// line: a `...` written anywhere else is text, and a `#` inside a value or
// inside a block scalar's body starts no comment.
func TestCommentsReadsNoDocumentEndMarkerInText(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"a `#` inside a quoted value", "dsl: 2\nprompts:\n  ask: \"a#b and a # inside quotes\"\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n", nil},
		{"dots inside a block scalar", "dsl: 2\nprompts:\n  ask: |\n    ...  # inside the block\n    Say hello.\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n", nil},
		{"dots closing a block scalar at the end of the document", "dsl: 2\nprompts:\n  ask: |\n    Say hello.\n    ...  # inside the block\n", nil},
		{"dots as a value", "dsl: 2\nprompts:\n  ask: '...'\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n", nil},
		{"a key whose name starts with the indicator", "dsl: 2\nprompts:\n  ask: Say hello.\n...x: 1 # tail\n", []string{"# tail"}},
		{"a comment above the last line", endBody + "# the foot\n", []string{"# the foot"}},
		// A block scalar's body is the author's text: a line of it that
		// starts with `#` is no comment, even written directly above the
		// marker, where nothing but comments can follow.
		{"a body line that starts with a hash above the marker", scalarEndBody + "...\n", nil},
		{"a comment below a body line that starts with a hash", scalarEndBody + "# c\n...\n", []string{"# c"}},
		{"an indented comment below a body line that starts with a hash", scalarEndBody + "  # c\n...\n", []string{"# c"}},
		// A body line the header's own indentation declaration holds, below
		// the column its first line sits at, is the author's text too.
		{"a body line under a declared indentation", indicatorEndBody, nil},
		{"a body line under a declared indentation above the marker", indicatorEndBody + "...\n", nil},
		{"a comment below a body under a declared indentation", indicatorEndBody + "# c\n...\n", []string{"# c"}},
		// A declaration is counted from the indentation of the BLOCK the
		// node sits in, as the scanner counts it, which a sequence dash
		// puts two columns right of the header's own line. The first row
		// runs that path without witnessing it — measured, it answers the
		// same from either baseline, because the one line a shorter body
		// swallows is hung on a foot and footAboveTheRegion lists it. The
		// second witnesses it: with TWIN texts below the body, a body read
		// from the header's own line instead swallows both indented twins,
		// the foot gives one back, and the list comes out a comment short.
		{"a declared body under a sequence dash", "- x: |2\n\n  # keep me\n\n...\n# the foot\n", []string{"# keep me", "# the foot"}},
		{"twin comments below a body declared inside a block", declaredTwinBody + "... # on the marker\n", []string{"# twice", "# twice", "# twice", "# on the marker"}},
		// A multi-line QUOTED value runs to its closing quote, and every
		// line of it is the author's text: `#` opening the last one starts
		// no comment, whatever the quote, the anchor or the escapes the
		// value carries.
		{"a quoted value whose last line starts with a hash", quotedEndBody, nil},
		{"a single-quoted value whose last line starts with a hash", singleQuotedEndBody, nil},
		{"an anchored quoted value whose last line starts with a hash", anchoredQuotedEndBody, nil},
		{"an escaped quote inside such a value", escapedQuotedEndBody, nil},
		{"such a quoted value above the marker", quotedEndBody + "...\n", nil},
		{"a comment below such a quoted value", quotedEndBody + "# c\n...\n", []string{"# c"}},
		{"an indented comment below such a quoted value", quotedEndBody + "  # c\n...\n", []string{"# c"}},
		{"a comment below such a single-quoted value", singleQuotedEndBody + "# c\n", []string{"# c"}},
		// yaml.v3 gives a quoted scalar the position of its ANCHOR or TAG,
		// which may be written a line ABOVE the opening quote: the scan
		// follows the lines in between — where only properties, blanks and
		// comments can sit — so the value's own text is still its own, and
		// a comment written there is still a comment.
		{"an anchor on a line of its own above such a value", anchorAboveQuotedEndBody, nil},
		{"a tag on a line of its own above such a value", tagAboveQuotedEndBody, nil},
		{"such a value below an anchor, above the marker", anchorAboveQuotedEndBody + "...\n", nil},
		{"a comment below such a value", anchorAboveQuotedEndBody + "# c\n...\n", []string{"# c"}},
		{"a comment carrying a quote between the anchor and such a value", commentedAnchorEndBody, []string{`# " anchored`}},
		// Inside a FLOW collection the comments are read off the source: a
		// property there is the value's, not a plain scalar, so the quote
		// behind it opens the value rather than reading as text.
		{"a tagged quoted value inside a flow collection", flowTaggedBody, nil},
		{"an anchored quoted value inside a flow collection", flowAnchoredBody, nil},
		{"an anchored quoted value written over lines inside one", flowAnchoredMultiBody, nil},
		{"a real comment after an anchored value inside one", flowRealCommentBody, []string{"# real"}},
		// The same property above a BLOCK scalar: its body is read DOWN
		// from the node's position, so a `|` written below the anchor is
		// inside the body the scan walks, and the body's own `#` line is
		// its text.
		{"an anchor on a line of its own above a block scalar", anchorAboveBlockEndBody, nil},
		{"a tag on a line of its own above a block scalar", tagAboveBlockEndBody, nil},
		{"a comment below such a block scalar", anchorAboveBlockEndBody + "# c\n...\n", []string{"# c"}},
		// A block scalar with NO body has no first line to read the body's
		// indentation off: the lines below its header are the trailing
		// region's, and the scanner floors a body's indentation at column
		// 1, so a `...` marker or a comment at column 0 opens that region
		// instead of being walked as a body.
		{"a marker and a block below an empty block scalar", emptyScalarEndBody + "...\n\n# c\n", []string{"# c"}},
		{"a block below two markers under an empty block scalar", emptyScalarEndBody + "...\n...\n# c\n", []string{"# c"}},
		{"two blocks below a marker under an empty block scalar", emptyScalarEndBody + "...\n# a\n\n# b\n", []string{"# a", "# b"}},
		{"a marker and a block below an empty folded scalar", emptyFoldedEndBody + "...\n\n# c\n", []string{"# c"}},
		{"a bare marker below an empty block scalar", emptyScalarEndBody + "...\n", nil},
		// A block scalar the whole document is: no block encloses it, so
		// the floor's enclosing-block term is 0 and column 1 is the only
		// one left to keep a line at column 0 out of the body. The two
		// rows that witness that term are the ones whose region holds a
		// line yaml.v3 hangs on NO foot — a bare marker's block, the
		// second block below a marker: the first row below answers off the
		// document's foot, which footAboveTheRegion lists whatever the
		// body walk reads.
		{"a block scalar as the whole document", "--- |\n# x\n", []string{"# x"}},
		{"a marker and a block below such a document", "--- |\n  body\n...\n\n# c\n", []string{"# c"}},
		{"a comment below a bare marker under an empty root block scalar", "--- |\n...\n\n# c\n", []string{"# c"}},
		{"two blocks below a marker under an empty root block scalar", "--- |\n...\n# a\n\n# b\n", []string{"# a", "# b"}},
		// A tag name carries the scanner's tag-URI characters — a `'`, a
		// `,`, a `[` among them: read as a quote or as a flow indicator,
		// one of those turns the value's own text into a comment.
		{"a tag name carrying a quote above a quoted value", tagNameQuoteEndBody, nil},
		{"a comment below such a value", tagNameQuoteEndBody + "# c\n...\n", []string{"# c"}},
		{"a tag name carrying a comma inside a flow collection", flowCommaTagBody, nil},
		{"a tag name carrying a bracket inside a flow collection", flowBracketTagValueBody, nil},
		{"a flow collection whose own tag name carries a bracket", flowBracketTagBody, []string{"# real"}},
		{"a flow collection whose own tag name is plain", flowPlainTagBody, []string{"# real"}},
		{"a comment below such a collection", flowBracketTagCommentedBody, []string{"# real", "# on the node"}},
		{"a comment below a plainly tagged one", flowPlainTagCommentedBody, []string{"# real", "# on the node"}},
		// The VERBATIM form `!<…>` ends at the `>` that closes it, whatever
		// the URI holds: a `'` there is no opening quote, a `[` no bracket
		// of the collection's own, and the `>` itself declares no folded
		// scalar, so the declaration written behind it is still found. Each
		// row sits beside its shorthand control.
		{"a verbatim tag name carrying a quote above a quoted value", verbatimTagQuoteEndBody, nil},
		{"a comment below such a verbatim-tagged value", verbatimTagQuoteEndBody + "# c\n...\n", []string{"# c"}},
		{"a verbatim tag on a quoted value inside a flow collection", flowVerbatimTagBody, nil},
		{"a flow collection whose own verbatim tag name carries a bracket", flowVerbatimBracketTagCommentedBody, []string{"# real", "# on the node"}},
		{"a declared body indentation behind a verbatim tag", verbatimDeclaredEndBody, nil},
		{"a declared body indentation behind a shorthand tag", shorthandDeclaredEndBody, nil},
		{"a comment below a body declared behind a verbatim tag", verbatimDeclaredEndBody + "# c\n...\n", []string{"# c"}},
		// yaml.v3 counts a position's column in CHARACTERS: a multibyte
		// character written before a value moves a byte index of its column
		// back into the key, onto the quote that closes it.
		{"an accented quoted key above a multi-line quoted value", accentedKeyQuotedEndBody, nil},
		{"an ASCII quoted key above the same value", asciiKeyQuotedEndBody, nil},
		{"a comment below such a value under an accented key", accentedKeyQuotedEndBody + "# c\n...\n", []string{"# c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Comments([]byte(tc.src))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("comments = %q, want %q\n%s", got, tc.want, tc.src)
			}
		})
	}
}

// The document's own comments are listed beside the neighbourhood's, in
// written order: the marker closes the document, it does not hide what is
// written above it.
func TestCommentsListsTheDocumentsOwnCommentsBesideTheMarkers(t *testing.T) {
	body := strings.Replace(endBody, "  ask: Say hello.\n", "  ask: Say hello. # on the value\n", 1)
	if body == endBody {
		t.Fatal("the fixture no longer holds the value this test comments")
	}
	got := Comments([]byte("# the head\n" + body + "# above\n... # on the marker\n# below\n"))
	want := []string{"# the head", "# on the value", "# above", "# on the marker", "# below"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("comments = %q, want %q", got, want)
	}
	// A directive line's comment keeps its own place — after the own-line
	// comments written above the first directive — with the neighbourhood last.
	got = Comments([]byte("# the head\n%YAML 1.1 # on the directive\n---\n" + body + "# above\n...\n"))
	want = []string{"# the head", "# on the directive", "# on the value", "# above"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("comments = %q, want %q", got, want)
	}
}

// The neighbourhood is read through the lines yaml.v3's own positions count:
// a BOM is off, and a CRLF document ends its lines as the scanner does.
func TestCommentsReadsTheNeighbourhoodOfABOMOrCRLFDocument(t *testing.T) {
	for name, src := range map[string]string{
		"a BOM":  "\ufeff" + endBody + "... # end note\n",
		"a CRLF": strings.ReplaceAll(endBody+"... # end note\n", "\n", "\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := Comments([]byte(src)); strings.Join(got, "|") != "# end note" {
				t.Fatalf("comments = %q, want [# end note]", got)
			}
		})
	}
}

// footTwinBody carries `# twice` as a MID-DOCUMENT foot comment — a blank
// line below it, which is what makes yaml.v3 hang it on the key above rather
// than on the head of the key below. A twin of that text written in the
// trailing region is a second comment, and the run the walk's tail is
// matched against must not reach back over the comment-less nodes between
// the two.
const footTwinBody = "dsl: 2\nprompts:\n  ask: Say hello.\n  # twice\n\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// A comment of the trailing region is taken off the walk's list by
// position, not by its text: the same text written on a node higher up in
// the document stays listed where it is — on the head of a node below it,
// and on the FOOT of a node the walk left behind alike.
func TestCommentsListsATrailingCommentBesideItsTwinHigherUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{
			"a twin on the head of a node below it",
			"dsl: 2\nprompts:\n  ask: Say hello.\n  # twice\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n  # twice\n...\n# below\n",
			[]string{"# twice", "# twice", "# below"},
		},
		// The twin on the last content line's OWN line is a line comment,
		// which the trailing region's read never saw: only the lines a
		// node's FOOT comment contributed can be a copy of what it holds.
		{
			"a twin on the last content line",
			strings.Replace(endBody, "    - hello -> done\n", "    - hello -> done # twice\n", 1) + "# twice\n...\n",
			[]string{"# twice", "# twice"},
		},
		// A mid-document FOOT twin, with nothing but comment-less nodes
		// between it and the region: the shapes `fmt` rewrote in place,
		// deleting the region's line without a word.
		{"a mid-document foot twin above a marker", footTwinBody + "# twice\n...\n", []string{"# twice", "# twice"}},
		{"a mid-document foot twin and a marker's line comment", footTwinBody + "...  # twice\n", []string{"# twice", "# twice"}},
		{"a mid-document foot twin below a blank under a marker", footTwinBody + "...\n\n# twice\n", []string{"# twice", "# twice"}},
		{"a mid-document foot twin under a marker run", footTwinBody + "...\n...\n# twice\n", []string{"# twice", "# twice"}},
		{"a mid-document foot twin with no marker at all", footTwinBody + "\n# twice\n", []string{"# twice", "# twice"}},
		{"a mid-document foot and a region of another text", footTwinBody + "# other\n...\n", []string{"# twice", "# other"}},
		// The written form of the shape above: the foot at column 0, the
		// twin below the last content line, which `fmt` read one short and
		// refused as a rewrite that loses a comment.
		{
			"a column-0 foot twin and a blank above the region",
			"dsl: 2\nprompts:\n  ask: Say hello.\n# a\n\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n\n# a\n",
			[]string{"# a", "# a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Comments([]byte(tc.src))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("comments = %q, want %q\n%s", got, tc.want, tc.src)
			}
		})
	}
}

// A block scalar's header declares its body's indentation with the digit of
// `|2`, `>2-` or `|-2`, counted from the header's own line; the header is
// read at the indicator's own column — the one yaml.v3 gives the scalar, an
// anchor or a tag between the two aside, propertyEnd skipping such a token
// whole — so a `|` or a `>` written inside the key, inside the value, inside
// the comment of the header's line or inside a verbatim tag (`!<a>`, whose
// own `>` closes it) is not read as one.
//
// Both halves of the scan END at the header's comment, which the scanner
// opens at a `#` with no blank required before it: the digit of `|2#c` is
// the declaration, and an indicator written inside the comment of a line
// that carries none — a property holding the node's position above the
// indicator — declares nothing.
//
// col0 is what the scan answers when it starts at column 0 instead of the
// indicator's: the row witnesses the column exactly when col0 differs from
// want, which five rows do, in the order the table holds them — `"a|2 b": |`
// and `"a>2 b": |` read a declaration out of their quoted KEY, `"a #b": |2`
// stops on the `#` that key carries, `"a|b": |2` reads a bare `|` out of it
// and misses the one the header declares, and `"éééé|": |2` is the column
// read in characters where a byte index of it lands on the key's own `|`.
// That list is asserted below the table, in that same order, so a row added
// or changed reddens the test rather than leaving this sentence counting
// rows that moved. The other rows carry col0 all the same, so a change to
// that reading reddens the table rather than the claim of a comment.
func TestADeclaredBlockIndentIsReadOffTheHeaderAlone(t *testing.T) {
	rows := []struct {
		line string
		col  int
		want int
		col0 int
	}{
		{"  ask: |", 8, 0, 0},
		{"  ask: >", 8, 0, 0},
		{"  ask: |-", 8, 0, 0},
		{"  ask: |+", 8, 0, 0},
		{"  ask: |2", 8, 2, 2},
		{"  ask: >2", 8, 2, 2},
		{"  ask: |2-", 8, 2, 2},
		{"  ask: |-2", 8, 2, 2},
		// The scanner takes ONE digit (as_digit, then a blank, a `#` or a
		// line break): `|10` is "did not find expected comment or line
		// break", so this row measures the arithmetic, not a document.
		{"  ask: |10", 8, 10, 10},
		{"  ask: |2 # a comment", 8, 2, 2},
		// The header ends at its COMMENT, which the scanner reads at a `#`
		// with no blank before it (yaml_parser_scan_block_scalar eats the
		// indicators, then blanks, then a comment): the digit written
		// against one still declares the body's indentation, and a comment
		// written against a bare indicator declares none.
		{"  ask: |2#c", 8, 2, 2},
		{"  ask: |2+#c", 8, 2, 2},
		{"  ask: |2-#c", 8, 2, 2},
		{"  ask: >1#c", 8, 1, 1},
		{"  ask: |#c", 8, 0, 0},
		// The indicator is behind an anchor or a tag: yaml.v3 puts the
		// column on those, and the header is the indicator past them.
		{"  ask: &an |2", 8, 2, 2},
		{"  ask: !!str |2", 8, 2, 2},
		// The VERBATIM tag form is skipped whole as well, so the `>` that
		// CLOSES it is no folded indicator and the header behind it is
		// still read — including when the header's own indicator is a `>`.
		{"  ask: !<a> |2", 8, 2, 2},
		{"  ask: !<a'b> |2", 8, 2, 2},
		{"  ask: !<a> >2", 8, 2, 2},
		// A `|` or a `>` the header's line carries ELSEWHERE — in a quoted
		// key, in the comment of the header, in a plain value — declares
		// nothing.
		{`  "a|2 b": |`, 12, 0, 2},
		{`  "a>2 b": |`, 12, 0, 2},
		{"  ask: |2 # see |4", 8, 2, 2},
		{"  ask: &an # see |4", 8, 0, 0},
		{"  ask: !<a> # |9", 8, 0, 0},
		{`  "a #b": |2`, 11, 2, 0},
		{`  "a|b": |2`, 10, 2, 0},
		{`  "a>b": |`, 10, 0, 0},
		// The column is a CHARACTER count: indexed as bytes, the four
		// accented characters of this key put the scan on the `|` the key
		// itself carries, which declares nothing.
		{`  "éééé|": |2`, 12, 2, 0},
	}
	for _, tc := range rows {
		t.Run(tc.line, func(t *testing.T) {
			if got := declaredBlockIndent(tc.line, tc.col); got != tc.want {
				t.Fatalf("declaredBlockIndent(%q, %d) = %d, want %d", tc.line, tc.col, got, tc.want)
			}
			if got := declaredBlockIndent(tc.line, 0); got != tc.col0 {
				t.Fatalf("declaredBlockIndent(%q, 0) = %d, want %d", tc.line, got, tc.col0)
			}
		})
	}
	var witnesses []string
	for _, tc := range rows {
		if tc.col0 != tc.want {
			witnesses = append(witnesses, tc.line)
		}
	}
	named := []string{`  "a|2 b": |`, `  "a>2 b": |`, `  "a #b": |2`, `  "a|b": |2`, `  "éééé|": |2`}
	if got, want := strings.Join(witnesses, " / "), strings.Join(named, " / "); got != want {
		t.Fatalf("the rows whose col0 differs from want are %q; the godoc above names %q", got, want)
	}
}

// The scans that walk over a property (`&a`, `!!str`) to reach the value
// behind it skip the token whole, on the character classes the vendored
// scanner reads a name with: an anchor name is alphanumerics, `_` and `-`
// (is_alpha, yaml_parser_scan_anchor), a tag name those plus the tag-URI
// punctuation `;/?:@&=+$,.!~*'()[]%` (yaml_parser_scan_tag_uri). Both halves
// are measured here against the version that is vendored: where propertyEnd
// ends the token, and whether yaml.v3 reads a name carrying that character
// at all — a `"`, a `{`, a `}` and a `#` it refuses, so no scan of this file
// has to read one, while a `'`, a `,`, a `[` and a `]` inside a TAG name it
// takes, and reading one as a quote or as a flow indicator is what makes the
// author's prose look like a comment.
//
// The VERBATIM form `!<…>` escapes both classes — `<` is in neither — and is
// DELIMITED instead (yaml_parser_scan_tag eats `!<` and reads the URI up to
// its `>`): the token ends at that `>`, which is where the name may hold a
// character the classes refuse. A name yaml.v3 does NOT read ends there all
// the same, the conservative end — no scan reads a document it refuses.
func TestAPropertyNamesCharactersAreTheScannersOwn(t *testing.T) {
	for _, tc := range []struct {
		token  string
		end    int
		parses bool
	}{
		{"!a'b", 3, true},
		{"!a,b", 3, true},
		{"!a[b", 3, true},
		{"!a]b", 3, true},
		{"!a(b", 3, true},
		{"!a*b", 3, true},
		{"!a%41b", 5, true},
		{"!a.b", 3, true},
		{"!!str", 4, true},
		{"!a b", 1, true},
		{"!a\"b", 1, false},
		{"!a{b", 1, false},
		{"!a}b", 1, false},
		{"!a#b", 1, false},
		{"&an", 2, true},
		{"&a-n_1", 5, true},
		{"&a'b", 1, false},
		{"&a[b", 1, false},
		{"&a#b", 1, false},
		{"&a\"b", 1, false},
		{"&a.b", 1, false},
		{"!<a>", 3, true},
		{"!<a'b>", 5, true},
		{"!<a[b>", 5, true},
		{"!<a,b>", 5, true},
		{"!<a!b>", 5, true},
		{"!<a%41b>", 7, true},
		{"!<a\"b>", 5, false},
		{"!<a{b>", 5, false},
		{"!<a b>", 5, false},
		{"!<>", 2, false},
		{"!<a", 2, false},
	} {
		t.Run(tc.token, func(t *testing.T) {
			if got := propertyEnd([]rune(tc.token), 0); got != tc.end {
				t.Fatalf("propertyEnd(%q) = %d, want %d", tc.token, got, tc.end)
			}
			var doc yaml.Node
			err := yaml.Unmarshal([]byte("b: "+tc.token+" \"one\n  # two\"\n"), &doc)
			if (err == nil) != tc.parses {
				t.Fatalf("yaml.v3 reads the property name %q otherwise now: err = %v, want a document that parses = %v", tc.token, err, tc.parses)
			}
		})
	}
}

// The property forms, swept as a CLASS against an oracle that knows nothing
// of this file: a `#` is a comment exactly when overwriting its text IN
// PLACE with the same number of characters leaves every scalar value yaml.v3
// reads untouched — text inside a block or a quoted scalar is a value, and a
// comment can change none. The rows above pin the forms a reviewer found;
// this sweep is what holds the NEXT form to the same rule, over every
// placement a property can move a scan's reading in: a multi-line quoted
// value of either quote, a flow collection's value, a flow collection's own
// tag, a block body indentation declared or auto-detected, a real comment
// below such a value, and the trailing region of a `...` marker.
//
// Overwriting rather than cutting the line is load-bearing: deleting a
// comment line below an EMPTY block scalar promotes the next line into its
// body, which an oracle would read as a lost comment.
func TestNoPropertyFormMakesTheReaderInventOrLoseAComment(t *testing.T) {
	const probe = "# PROBE"
	forms := []string{
		"!a", "!!str", "!a'b", "!a[b", "!a,b", "!a]b", "!a(b", "!a%41b", "!a.b",
		"&an", "!e!s", "!", "!!",
		"!<a>", "!<a'b>", "!<a[b>", "!<a,b>", "!<a]b>", "!<a!b>", "!<a%41b>",
		"!<a.b>", "!<a(b>", "!<a:b>",
	}
	templates := []string{
		"prompts:\n  ask: %s\n    'Say hello.\n    " + probe + "'\n",
		"prompts:\n  ask: %s\n    \"Say hello.\n    " + probe + "\"\n",
		"prompts: {ask: %s \"Say hello. " + probe + "\"}\n",
		"prompts: {ask: %s 'Say hello. " + probe + "'}\n",
		"prompts: %s {ask: \"x\"}\nnodes:\n  - agent: hello " + probe + "\n",
		"prompts:\n  ask: %s |2\n      echo one\n    " + probe + "\n",
		"prompts:\n  ask: %s |\n    echo one\n    " + probe + "\n",
		"prompts:\n  ask: %s \"Say hello.\"\n" + probe + "\n",
		"prompts:\n  ask: %s\n    \"Say hello.\"\n" + probe + "\n...\n",
		"prompts:\n  ask: %s >2\n      fold one\n    " + probe + "\n",
		"prompts:\n  ask: %s |\n    echo one\n...\n" + probe + "\n",
		// The property holds the node's position a line ABOVE the
		// indicator and its line carries a COMMENT: an indicator written
		// inside that comment declares nothing, so the body below is read
		// whole. The control carries a comment with no indicator in it.
		"prompts:\n  ask: %s # |9\n    |\n    echo one\n    " + probe + "\n",
		"prompts:\n  ask: %s # plain\n    |\n    echo one\n    " + probe + "\n",
	}
	compared := 0
	for _, form := range forms {
		for ti, tpl := range templates {
			src := fmt.Sprintf(tpl, form)
			if _, err := scalarValuesYAMLReads(src); err != nil {
				continue // yaml.v3 refuses the shape: no scan of it reads one
			}
			want, err := probeReadsAsAComment(src, probe)
			if err != nil {
				t.Fatalf("the oracle failed on %q: %v", src, err)
			}
			listed := 0
			for _, c := range Comments([]byte(src)) {
				if strings.HasPrefix(c, probe) {
					listed++
				}
			}
			compared++
			if (want && listed == 1) || (!want && listed == 0) {
				continue
			}
			verdict := "INVENTED a comment out of the author's text"
			if want {
				verdict = "LOST or repeated a comment"
				if listed > 1 {
					verdict = "repeated a comment"
				}
			}
			t.Errorf("%s: form %q, placement %d listed it %d time(s)\n%s", verdict, form, ti, listed, src)
		}
	}
	// A template that stops parsing would empty the sweep in silence.
	if compared < 200 {
		t.Fatalf("the sweep compared %d shapes, too few to hold the class", compared)
	}
}

// A block scalar's HEADER is read by the same oracle: the shapes a header
// can take — an indentation indicator, a chomping one, the comment the
// scanner reads at a `#` with no blank required before it — over the body
// shapes a reader can take the author's text out of. The scanner eats the
// indicators, then blanks, then a comment (yaml_parser_scan_block_scalar),
// so `|2#c` declares 2 and comments `#c`: a header read short auto-detects
// the body off its first line and lists the line above that column — the
// author's own text — as a comment.
func TestNoBlockScalarHeaderMakesTheReaderInventOrLoseAComment(t *testing.T) {
	const probe = "# PROBE"
	headers := []string{
		"|", "|2", "|2 #c", "|2#c", "|2+#c", "|2-#c", "|+2#c", "|-", "|+",
		"|1#c", "|#c", "|2 # see |4", "|2#see >4",
		">", ">2", ">2 #c", ">2#c", ">2-#c", ">1#c", ">-", ">+", ">#c",
	}
	templates := []string{
		// The probe sits BELOW the body's first line and above its column:
		// the author's text while the declaration is read, a comment when
		// the header is read short.
		"prompts:\n  ask: %s\n      echo one\n    " + probe + "\n",
		"prompts:\n  ask: %s\n      echo one\n    " + probe + "\n    echo two\n",
		"prompts:\n  ask: %s\n      echo one\n    " + probe + "\n...\n",
		"prompts:\n  ask: %s\n      echo one\n    " + probe + "\nnodes:\n  - agent: hello\n",
		// The body at the declared column exactly, and deeper.
		"prompts:\n  ask: %s\n    echo one\n    " + probe + "\n",
		"prompts:\n  ask: %s\n        echo one\n      " + probe + "\n",
	}
	compared, comments, texts := 0, 0, 0
	headersParsed := map[string]bool{}
	for _, header := range headers {
		for ti, tpl := range templates {
			src := fmt.Sprintf(tpl, header)
			if _, err := scalarValuesYAMLReads(src); err != nil {
				continue // yaml.v3 refuses the shape: no scan of it reads one
			}
			headersParsed[header] = true
			want, err := probeReadsAsAComment(src, probe)
			if err != nil {
				t.Fatalf("the oracle failed on %q: %v", src, err)
			}
			listed := 0
			for _, c := range Comments([]byte(src)) {
				if strings.HasPrefix(c, probe) {
					listed++
				}
			}
			compared++
			if want {
				comments++
			} else {
				texts++
			}
			if (want && listed == 1) || (!want && listed == 0) {
				continue
			}
			verdict := "INVENTED a comment out of the author's text"
			if want {
				verdict = "LOST or repeated a comment"
				if listed > 1 {
					verdict = "repeated a comment"
				}
			}
			t.Errorf("%s: header %q, placement %d listed it %d time(s)\n%s", verdict, header, ti, listed, src)
		}
	}
	// A header form yaml.v3 refuses in every placement would leave the
	// sweep claiming a shape it never ran.
	for _, header := range headers {
		if !headersParsed[header] {
			t.Errorf("no placement of the header %q parses: the form proves nothing", header)
		}
	}
	// A sweep every shape of which falls on one side of the oracle holds
	// one direction only: the class needs both — a probe listed because it
	// IS a comment, and one left out because it is the body's own text.
	if compared < 100 || comments == 0 || texts == 0 {
		t.Fatalf("the sweep compared %d shapes (%d comment, %d text): too few, or a direction it never ran", compared, comments, texts)
	}
}

// scalarValuesYAMLReads is every scalar value yaml.v3 reads out of src, in
// document order — the oracle's view of what is the author's text.
func scalarValuesYAMLReads(src string) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.ScalarNode {
			out = append(out, n.Value)
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	return out, nil
}

// probeReadsAsAComment reports whether yaml.v3 reads the probe written in
// src as a comment: its text is overwritten in place, and the values the
// document gives back decide. An overwrite that stops the document from
// parsing is text too — a comment could not.
func probeReadsAsAComment(src, probe string) (bool, error) {
	at := strings.Index(src, probe)
	if at < 0 {
		return false, fmt.Errorf("the probe %q is not written in the source", probe)
	}
	over := src[:at+1] + strings.Repeat("z", len(probe)-1) + src[at+len(probe):]
	if len(over) != len(src) {
		return false, fmt.Errorf("the overwrite changed the source's length")
	}
	was, err := scalarValuesYAMLReads(src)
	if err != nil {
		return false, err
	}
	now, err := scalarValuesYAMLReads(over)
	if err != nil {
		return false, nil
	}
	return strings.Join(was, "\x00") == strings.Join(now, "\x00"), nil
}

// A flow collection whose own TAG name carries a `[` or a `{` leaves the
// source read with no closing bracket to report when the token is not
// skipped whole — the tag's own bracket opens the depth the real one never
// closes. The read answers for such a collection as it does for a plainly
// tagged one, and commentsAfterClose, which no document reaches with a
// closing line of 0, counts none for either slot rather than indexing the
// line before the first.
func TestAFlowCollectionTagNameCarryingABracketIsRead(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the comment read crashed on a flow collection whose tag name carries a bracket: %v", r)
		}
	}()
	got, want := Comments([]byte(flowBracketTagBody)), Comments([]byte(flowPlainTagBody))
	if strings.Join(got, "|") != strings.Join(want, "|") || len(want) == 0 {
		t.Fatalf("comments = %q, want the plainly tagged collection's %q", got, want)
	}
	for _, foot := range []bool{false, true} {
		if n := commentsAfterClose([]string{"# real"}, map[string]bool{"# real": true}, sourceLines([]byte(flowBracketTagBody)), 0, foot)["# real"]; n != 0 {
			t.Fatalf("%d comments were counted in the region of a closing bracket the read never found (foot=%v)", n, foot)
		}
	}
}

// A block scalar whose header DECLARES its indentation (`|2`) counts it, in
// this read, from the indicator's own line, where yaml.v3 counts it from the
// enclosing block's indentation: with a property (`&a`, `!!str`) carrying
// the node's position a line ABOVE the indicator, and the indicator indented
// deeper than the body, the read ends the body early and the body's last
// line is listed as a comment. That is a REFUSAL — `fmt` names a line it
// would not lose — never a comment dropped in silence, and the author
// refuses such a document for its property before any rewrite: these rows
// pin the limit rather than leave it unsaid, with the sibling shape the
// read answers exactly on.
func TestABlockScalarUnderAPropertyLineReadsItsDeclaredBodyShort(t *testing.T) {
	if got, want := Comments([]byte(declaredUnderPropertyBody)), []string{"# not a comment"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("comments = %q, want the pinned limit %q", got, want)
	}
	if !Parse("x.bot.yaml", []byte(declaredUnderPropertyBody)).HasErrors() {
		t.Fatal("the author now reads a property on a value: the limit above reaches a rewrite, and the read owes the indicator's own line")
	}
	// The same body with no declaration: the indentation is read off the
	// body's first line, below the indicator, and the body is whole.
	if got := Comments([]byte(anchorAboveBlockEndBody)); len(got) != 0 {
		t.Fatalf("comments = %q, want none: a bare indicator below a property is read whole", got)
	}
}

// An explicit tag is refused by the author (E050) before any rewrite, the
// verbatim form included: the `--force` notice, which reads a target with no
// .bot validity check, is the one surface a document carrying one reaches —
// which is where cli.TestFmtForceCountsNoCommentInsideAPropertiedValue
// proves the count. A door that started letting such a document through
// would widen that reach, and this test is what says so.
func TestAVerbatimTagIsRefusedBeforeAnyRewrite(t *testing.T) {
	for _, src := range []string{
		verbatimTagQuoteEndBody,
		flowVerbatimTagBody,
		flowVerbatimBracketTagCommentedBody,
		verbatimDeclaredEndBody,
		shorthandDeclaredEndBody,
		verbatimCommentedHeaderBody,
	} {
		if !Parse("x.bot.yaml", []byte(src)).HasErrors() {
			t.Fatalf("the author now reads an explicit tag: a rewrite reaches this document, and the reach above is wider than it says\n%s", src)
		}
	}
}

// The column the table above reads the header at is the one yaml.v3 hands a
// block scalar: a row measured against a hand-written column would not say
// anything about the reader that runs.
func TestABlockScalarsColumnIsItsIndicators(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"a: |\n  body\n", 4},
		{"  ask: |2\n    body\n", 8},
		{"\"a|2 b\": |\n  body\n", 10},
		{"a: &an |2\n  body\n", 4},   // the anchor holds the column
		{"a: !!str |2\n  body\n", 4}, // the tag holds the column
		{"a: !<x> |2\n  body\n", 4},  // a verbatim tag holds it the same way
		{"  \"a #b\": |2\n      body\n", 11},
		{"  \"éééé|\": |2\n      body\n", 12}, // counted in characters
	} {
		t.Run(tc.src, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.src), &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			cols := []int{}
			var walk func(*yaml.Node)
			walk = func(n *yaml.Node) {
				if n == nil {
					return
				}
				if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
					cols = append(cols, n.Column)
				}
				for _, c := range n.Content {
					walk(c)
				}
			}
			walk(&doc)
			if len(cols) != 1 || cols[0] != tc.want {
				t.Fatalf("block scalar columns = %v, want [%d]", cols, tc.want)
			}
		})
	}
}

// The document's foot is handed to the trailing region's read, which holds
// every one of its lines on every document measured — the region starts at
// the last content line, and that is where yaml.v3 reads a document's foot.
// A line the read does NOT hold is listed all the same, and counted: a
// comment nothing lists is a comment a rewrite loses in silence, so the
// guard answers for a foot the region's own reading of the source misses.
func TestAFootLineTheTrailingRegionDoesNotHoldIsStillListed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		foot     string
		trailing []string
		want     []string
	}{
		{"a foot line the region misses", "# ghost\n\n# twin", []string{"# twin"}, []string{"# ghost"}},
		{"the same text twice, held once", "# twin\n\n# twin", []string{"# twin"}, []string{"# twin"}},
		{"a foot the region holds whole", "# a\n\n# b", []string{"# a", "# b"}, nil},
		{"no foot at all", "", []string{"# a"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := footAboveTheRegion(tc.foot, tc.trailing)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("footAboveTheRegion(%q, %q) = %q, want %q", tc.foot, tc.trailing, got, tc.want)
			}
		})
	}
}

// A document that does not end on the marker ends on no neighbourhood: the
// comments of a second document after one are no rewrite's to lose, since a
// file holding two documents is refused when it is read (E050).
func TestASecondDocumentAfterTheMarkerIsRefusedNotRead(t *testing.T) {
	src := endBody + "...\n# c\n---\nb: 2\n"
	if got := Comments([]byte(src)); len(got) != 0 {
		t.Fatalf("comments = %q, want none: the text ends on a second document, not on a marker", got)
	}
	res := Parse("x.bot.yaml", []byte(src))
	if !res.HasErrors() {
		t.Fatal("a second document after the marker is not refused")
	}
}
