package author

import (
	"strings"
	"testing"
)

// The writer re-emits the explicit document markers the source wrote: a
// `---` opening the document, and the trailing region below its last
// content line — the `...` markers with the comments and blank lines
// written around them, which hang on no node. Written back, the document
// carries every comment line Comments reads off the source, the same marker
// lines, and the written form is its own (WriteKeepingComments of the
// written form is the same text): the fixed point `iterion fmt --check`
// needs.
func TestTheWriterReEmitsTheDocumentMarkersTheSourceWrote(t *testing.T) {
	for name, src := range map[string]string{
		"a bare marker":                        endBody + "...\n",
		"a comment above the marker":           endBody + "# only\n...\n",
		"a comment on the marker's line":       endBody + "...  # only\n",
		"a comment below the marker":           endBody + "...\n# below\n",
		"a blank and a comment below":          endBody + "...\n\n# below\n",
		"a second block below the marker":      endBody + "...\n# a\n\n# b\n",
		"two markers":                          endBody + "...\n...\n",
		"an indented comment above":            endBody + "  # inner\n...\n",
		"a block the one below displaces":      endBody + "\n# above\n\n...\n# below\n",
		"a document-start marker":              "---\n" + endBody,
		"a start marker carrying a comment":    "--- # start\n" + endBody,
		"both markers":                         "---\n" + endBody + "...\n",
		"a marker under a block scalar's body": scalarEndBody + "...\n\n# below\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot.yaml", []byte(src))
			if res.HasErrors() {
				t.Fatalf("the fixture does not read, so the write below proves nothing: %v", res.Diagnostics)
			}
			out, err := WriteKeepingComments(res.File, []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if had, kept := Comments([]byte(src)), Comments(out); strings.Join(had, "|") != strings.Join(kept, "|") {
				t.Fatalf("the written form carries %q, the source %q:\n%s", kept, had, out)
			}
			if had, kept := markerCount(src), markerCount(string(out)); had != kept {
				t.Fatalf("the written form carries %d marker line(s), the source %d:\n%s", kept, had, out)
			}
			back := Parse("x.bot.yaml", out)
			if back.HasErrors() {
				t.Fatalf("the written form does not read back: %v\n%s", back.Diagnostics, out)
			}
			again, err := WriteKeepingComments(back.File, out)
			if err != nil || string(again) != string(out) {
				t.Fatalf("the written form is not its own: %v\n%s\nwant\n%s", err, again, out)
			}
		})
	}
}

// A comment of the trailing region yaml.v3 ALSO hangs on a node — the block
// directly above a marker, which rides the document's foot — is taken off
// the tree before the encode: the region is re-emitted verbatim, so a line
// left on the node as well would be written twice, and `fmt` would refuse a
// document it loses nothing of. Each row names a line the region holds, and
// the written form says it exactly as often as the source does.
func TestTheWriterWritesNoLineOfTheTrailingRegionTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		line string
		want int
	}{
		"a block above the marker": {endBody + "# only\n...\n", "# only", 1},
		// An INDENTED block above the marker is the one yaml.v3 hangs on
		// the last NODE's foot rather than on the document's: the run the
		// strip walks (hungOnTheLastNode) is what takes it off.
		"an indented block above the marker": {endBody + "  # inner\n...\n", "# inner", 1},
		"a block below the marker":           {endBody + "...\n# only\n", "# only", 1},
		"a comment on the marker's line":     {endBody + "...  # only\n", "# only", 1},
		"the same text above and on it":      {endBody + "\n# twice\n\n...  # twice\n", "# twice", 2},
		"a mid-document foot and its twin":   {footTwinEndBody + "...\n# twice\n", "# twice", 2},
	} {
		t.Run(name, func(t *testing.T) {
			res := Parse("x.bot.yaml", []byte(tc.src))
			if res.HasErrors() {
				t.Fatalf("the fixture does not read: %v", res.Diagnostics)
			}
			if got := strings.Count(tc.src, tc.line); got != tc.want {
				t.Fatalf("the fixture writes %q %d time(s), want %d", tc.line, got, tc.want)
			}
			out, err := WriteKeepingComments(res.File, []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(out), tc.line); got != tc.want {
				t.Fatalf("the written form says %q %d time(s), the source %d:\n%s", tc.line, got, tc.want, out)
			}
		})
	}
}

// The document's foot keeps the lines written ABOVE the trailing region —
// the ones that region's source read never saw, which a block scalar's body
// can swallow — and loses only the ones the region re-states: the strip
// mirrors the accounting Comments does (footAboveTheRegion), where a foot
// cleared whole would delete a line no other place of the written form
// holds. Read on the field, since no shape of the canon sweep separates the
// two: a document whose foot carries a line the region does not.
func TestTheDocumentsFootKeepsWhatTheTrailingRegionDoesNotHold(t *testing.T) {
	src := []byte(endBody + "# only\n...\n")
	doc, _, err := readFirst(src)
	if err != nil || doc == nil {
		t.Fatalf("the fixture does not decode: %v", err)
	}
	if got := commentLines(doc.FootComment); strings.Join(got, "|") != "# only" {
		t.Fatalf("yaml.v3 hangs %q on the document's foot, want the region's one line", got)
	}
	doc.FootComment = "# above the region\n# only"
	stripTrailingRegion(doc, sourceLines(src))
	if got := doc.FootComment; got != "# above the region" {
		t.Fatalf("the document's foot is %q, want the one line the trailing region does not hold", got)
	}
}

// Directives lists the `%YAML`/`%TAG` lines a document opens its stream
// with — the one thing the writer does not re-emit, and which the canonical
// form refuses by name rather than dropping in silence. A `%` below the
// first content line is a value's text, not a directive, and so is one
// written after the document has begun.
func TestDirectivesListsTheStreamDirectivesADocumentOpensWith(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want []string
	}{
		"none":                    {endBody, nil},
		"a %YAML directive":       {"%YAML 1.1\n---\n" + endBody, []string{"%YAML 1.1"}},
		"a %TAG directive":        {"%TAG !e! tag:example.com,2000:\n---\n" + endBody, []string{"%TAG !e! tag:example.com,2000:"}},
		"two of them":             {"%YAML 1.1\n%TAG !e! x\n---\n" + endBody, []string{"%YAML 1.1", "%TAG !e! x"}},
		"one below a comment":     {"# above\n%YAML 1.1\n---\n" + endBody, []string{"%YAML 1.1"}},
		"one below a blank":       {"\n%YAML 1.1\n---\n" + endBody, []string{"%YAML 1.1"}},
		"a percent in a value":    {strings.Replace(endBody, "ask: Say hello.", "ask: 100% sure", 1), nil},
		"a percent line after it": {endBody + "...\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Directives([]byte(tc.src)); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("Directives = %q, want %q", got, tc.want)
			}
		})
	}
}

// A `---` or `...` is a marker at column 0 only, followed by a blank or the
// end of its line: indented it is a block scalar's body or a value, glued
// to another character it is text (`...x`, a key of that name).
func TestAMarkerIsReadAtColumnZeroAlone(t *testing.T) {
	for _, tc := range []struct {
		line  string
		start bool
		end   bool
	}{
		{line: "---", start: true},
		{line: "--- # c", start: true},
		{line: "---\t", start: true},
		{line: "----"},
		{line: "  ---"},
		{line: "...", end: true},
		{line: "...  # c", end: true},
		{line: "...x"},
		{line: "  ..."},
	} {
		t.Run(tc.line, func(t *testing.T) {
			if got := isDocStart(tc.line); got != tc.start {
				t.Errorf("isDocStart(%q) = %v, want %v", tc.line, got, tc.start)
			}
			if got := isDocEnd(tc.line); got != tc.end {
				t.Errorf("isDocEnd(%q) = %v, want %v", tc.line, got, tc.end)
			}
		})
	}
	if got := documentStartMarker([]string{"# a", "", "%YAML 1.1", "---", "dsl: 2"}); got != "---" {
		t.Errorf("a marker below a comment and a directive is %q, want ---", got)
	}
	if got := documentStartMarker([]string{"dsl: 2", "---"}); got != "" {
		t.Errorf("a `---` below the first content line is %q, want none", got)
	}
}

// markerCount counts the explicit document markers of text.
func markerCount(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if isDocStart(line) || isDocEnd(line) {
			n++
		}
	}
	return n
}

// footTwinEndBody writes a mid-document foot comment whose text the
// trailing region repeats below the marker: two comments, written where
// each is written, and neither is the other's copy.
const footTwinEndBody = "dsl: 2\nprompts:\n  ask: Say hello.\n  # twice\n\nnodes:\n  - agent: hello\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
