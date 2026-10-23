package workflowfile

import (
	"bytes"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// nodesOf counts the nodes of a decoded tree, the document node included:
// what YAMLValuesBound bounds.
func nodesOf(n *yamlv3.Node) int {
	if n == nil {
		return 0
	}
	c := 1
	for _, k := range n.Content {
		c += nodesOf(k)
	}
	return c
}

// nodesBuilt is every node yaml.v3 builds off src, document after document,
// until the stream ends or a document does not read: what a decode of src
// costs, however many documents its reader asks for.
func nodesBuilt(src []byte) int {
	dec := yamlv3.NewDecoder(bytes.NewReader(src))
	total := 0
	for {
		var doc yamlv3.Node
		if err := dec.Decode(&doc); err != nil {
			return total
		}
		total += nodesOf(&doc)
	}
}

// boundShapes are the YAML constructs that open nodes: block and flow
// collections, empty and null entries, explicit keys, nesting, anchors,
// aliases, tags, block scalars, multi-line plain scalars, several
// documents.
var boundShapes = []string{
	"a", "a: b", "a:", "- a", "-", "- - - a", "[]", "{}", "[a]", "{a}", "{a, a, a}", "[a, b, c]",
	"[a: b, c]", "[a: b, c: d]", "{a: b, c}", "{: }", "[? a]", "[? a, ? b]", "{? a, ? b}", "? a\n: b",
	"? a", "? [a, b]\n: c", "[[[[a]]]]", "{a: {b: {c: d}}}", "[{a: b}, {c: d}]", "a: [b, {c: [d, e]}]",
	"a: &x b\nc: *x", "a: !!str b", "a: !t [b]", "a: |\n  b\n  c\n", "a: >\n  b\n\n  c\n",
	"a: b\n  c\n  d", "- a\n- b: c\n  d: e\n- [f]", "a:\n  - b\n  -\n  - c: \n", "--- a\n--- b",
	"[a, ]", "{a: , b: }", "a: 'b: c, d'", "a: \"[{,:?-\"", "a: -b", "x: [\"a\":b]", "{\"a\":b}",
	"%YAML 1.2\n---\na: b", "a: b # c\n# d\n", "[a,\n b,\n c]", "? - a\n  - b\n: - c",
}

// The values bound is never below the values yaml.v3 builds — on every
// YAML file of this tree, on the constructs that open nodes, and on random
// documents built from them: a text within a ceiling decodes to at most the
// ceiling's values, which is what letting its decode run stands on.
func TestYAMLValuesBoundIsAnUpperBound(t *testing.T) {
	check := func(name string, src []byte) bool {
		var doc yamlv3.Node
		if yamlv3.Unmarshal(src, &doc) != nil {
			return false
		}
		if n, u := nodesOf(&doc), YAMLValuesBound(src); n > u {
			t.Errorf("%s: yaml.v3 builds %d nodes, the bound says at most %d", name, n, u)
		}
		return true
	}
	decoded := 0
	err := filepath.WalkDir("../../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && check(p, b) {
			decoded++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range boundShapes {
		if check(fmt.Sprintf("%q", s), []byte(s)) {
			decoded++
		}
	}
	r := rand.New(rand.NewSource(1690))
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j, n := 0, 1+r.Intn(6); j < n; j++ {
			b.WriteString(boundShapes[r.Intn(len(boundShapes))])
			b.WriteString([]string{"\n", ", ", " ", "\n  ", ": "}[r.Intn(5)])
		}
		if check(fmt.Sprintf("generated %d %q", i, b.String()), []byte(b.String())) {
			decoded++
		}
	}
	if decoded < 5000 {
		t.Fatalf("only %d documents decoded: the bound is checked on too few", decoded)
	}
	t.Logf("%d documents decoded, every one within the bound", decoded)
}

// FuzzYAMLValuesBound searches for a text the bound under-counts.
func FuzzYAMLValuesBound(f *testing.F) {
	for _, s := range boundShapes {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var doc yamlv3.Node
		if yamlv3.Unmarshal([]byte(s), &doc) != nil {
			return
		}
		if n, u := nodesOf(&doc), YAMLValuesBound([]byte(s)); n > u {
			t.Fatalf("%q: yaml.v3 builds %d nodes, the bound says at most %d", s, n, u)
		}
	})
}

// The pieces the skeleton's guesses are tried on: headers that open a block
// and lines that only look like one — in a flow collection, in a quoted or
// a plain value running over lines, after a key at a deeper column, with a
// comment, an explicit indentation — text lines that would be structure
// outside a block, and the lines that end a block or do not: a sibling at
// a column between the header's and the text's, a closing bracket or
// quote, a document marker, every line break yaml.v3 reads, a tab in the
// indentation, blank lines of every width.
var (
	skelHeads = []string{
		"k: |", "k: >", "k: |-", "k: >+", "k: |2", "k: |2-", "- |", "- >-", "- k: |", "- - |", "? |", ": |",
		"k: | # c", "|", "  k: |", "k: [a |", "k: [\"a |", "k: \"a |", "k: 'a |", "k: a |", "k: {a: |",
		"- k: [x, |", "k: |9", "k: |0", "k: |22", "\tk: |", "k:\t|", "k: x\n  y |",
	}
	skelTexts = []string{
		"{\"a\": [1, 2], \"b\": {\"c\": 3}},", "x: [a, a, a, a]", "- a", "? a", "a, b, c", "[a, a]", "{a, a}",
		"\"", "'", "# c", "---", "...", "]", "}", "\"]", "'}", "a: |", "- |", "\ta", "x\u0085y: [a, a]",
		"x\u2028k: {a, a}", "x\ry: [a]", "x\r\ny", "   ", "\t", "",
	}
	skelIndents = []string{"", " ", "  ", "   ", "    ", "      "}
	skelTails   = []string{
		"k2: {a, a, a}", "  sib: [a, a, a]", " sib: [a, a]", "]", "}", "\"", "'", "---\nd: [a, a]", "...", "- [a, a]",
		"k3: x", "", "  - [a, a]", "    x: [a, a]",
	}
)

// skelDocument is a random text of the shapes the skeleton guesses at.
func skelDocument(r *rand.Rand) string {
	var b strings.Builder
	if r.Intn(8) == 0 {
		b.WriteString("\ufeff")
	}
	for i, n := 0, 1+r.Intn(4); i < n; i++ {
		if r.Intn(3) == 0 {
			b.WriteString(boundShapes[r.Intn(len(boundShapes))])
			b.WriteString("\n")
		}
		b.WriteString(skelHeads[r.Intn(len(skelHeads))])
		b.WriteString([]string{"\n", "\r\n", "\r", "\u0085", "\u2028"}[r.Intn(5)])
		for j, m := 0, r.Intn(6); j < m; j++ {
			b.WriteString(skelIndents[r.Intn(len(skelIndents))])
			b.WriteString(skelTexts[r.Intn(len(skelTexts))])
			b.WriteString([]string{"\n", "\n", "\n", "\r\n", "\u0085", "\u2029"}[r.Intn(6)])
		}
		b.WriteString(skelTails[r.Intn(len(skelTails))])
		b.WriteString("\n")
	}
	return b.String()
}

// blockDocument is a random document that mostly reads: a mapping whose
// entries are values, flow lists, `|` and `>` blocks — under a key, in a
// list, under a nested key — holding text that would be structure outside
// a block, with blank lines of every width; one time in three a line of
// skelTails or skelTexts is dropped in somewhere, at some indentation.
func blockDocument(r *rand.Rand) string {
	heads := []string{"|", ">", "|-", ">+", "|2", "|+", ">-"}
	var lines []string
	for i, n := 0, 1+r.Intn(5); i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		// A block's text at its indentation, a line now and then deeper —
		// and one time in six one line less indented than the first.
		block := func(indent int, head string) {
			for j, m := 1+r.Intn(5), 0; m < j; m++ {
				switch r.Intn(6) {
				case 0:
					lines = append(lines, strings.Repeat(" ", r.Intn(indent+4)))
					continue
				case 1:
					lines = append(lines, strings.Repeat(" ", indent+2)+skelTexts[r.Intn(len(skelTexts))])
					continue
				case 2:
					if m > 0 && r.Intn(6) == 0 {
						lines = append(lines, strings.Repeat(" ", indent-1)+skelTexts[r.Intn(len(skelTexts))])
						continue
					}
				}
				lines = append(lines, strings.Repeat(" ", indent)+skelTexts[r.Intn(len(skelTexts))])
			}
		}
		switch r.Intn(6) {
		case 0:
			lines = append(lines, key+": v")
		case 1:
			lines = append(lines, key+": [a, {b: c}, d]")
		case 2:
			h := heads[r.Intn(len(heads))]
			lines = append(lines, key+": "+h)
			block(2, h)
		case 3:
			lines = append(lines, key+":")
			for j, m := 0, 1+r.Intn(3); j < m; j++ {
				lines = append(lines, "  - "+heads[r.Intn(len(heads))])
				block(4, "")
			}
		case 4:
			lines = append(lines, key+":", "  sub: "+heads[r.Intn(len(heads))])
			block(4, "")
			lines = append(lines, "  other: [a, a]")
		case 5:
			lines = append(lines, "- "+key+": "+heads[r.Intn(len(heads))])
			block(4, "")
			lines = append(lines, "  sib: [a, a]")
		}
	}
	if r.Intn(3) == 0 {
		at := r.Intn(len(lines) + 1)
		extra := skelIndents[r.Intn(len(skelIndents))] + []string{skelTails[r.Intn(len(skelTails))], skelTexts[r.Intn(len(skelTexts))]}[r.Intn(2)]
		lines = append(lines[:at], append([]string{extra}, lines[at:]...)...)
	}
	return strings.Join(lines, []string{"\n", "\r\n"}[r.Intn(2)]) + "\n"
}

// skeletonHolds reports whether the skeleton of src is proven (blocksHold)
// for at least one block, and checks the claim that proof stands on: src
// builds no more values than its skeleton.
func skeletonHolds(t *testing.T, name string, src []byte) bool {
	t.Helper()
	skel, blocks := yamlSkeleton(src)
	if len(blocks) == 0 || !blocksHold(skel, blocks) {
		return false
	}
	if got, want := nodesBuilt(src), nodesBuilt(skel); got > want {
		t.Errorf("%s: the skeleton is proven, yet the text builds %d values and its skeleton %d\ntext: %q\nskeleton: %q", name, got, want, src, skel)
	}
	if n, u := nodesBuilt(skel), YAMLValuesBound(skel); n > u {
		t.Errorf("%s: the skeleton builds %d values, its bound says at most %d", name, n, u)
	}
	return true
}

// Setting a block's text aside is sound: where yaml.v3's reading of the
// skeleton proves every block set aside is one value, the text builds no
// more values than its skeleton — on the tree's YAML files, on the shapes
// that open nodes, and on random texts built to fool the skeleton's guesses
// (a header in a flow collection, in a quoted or a plain value over lines,
// a sibling between the header's column and the text's, every line break
// yaml.v3 reads inside the text). A skeleton that is not proven is not
// trusted: its text is counted.
func TestASkeletonProvenBoundsTheDecode(t *testing.T) {
	proven, tried := 0, 0
	try := func(name string, src []byte) {
		tried++
		if skeletonHolds(t, name, src) {
			proven++
		}
	}
	_ = filepath.WalkDir("../../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml") {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			try(p, b)
		}
		return nil
	})
	r := rand.New(rand.NewSource(1690))
	for i := 0; i < 40000; i++ {
		try(fmt.Sprintf("generated %d", i), []byte(skelDocument(r)))
		try(fmt.Sprintf("block document %d", i), []byte(blockDocument(r)))
	}
	if proven < 5000 {
		t.Fatalf("only %d of %d skeletons proven: the claim is checked on too few", proven, tried)
	}
	t.Logf("%d of %d texts' skeletons proven, every one bounding its text", proven, tried)
}

// FuzzASkeletonProvenBoundsTheDecode searches for a text whose proven
// skeleton builds fewer values than the text.
func FuzzASkeletonProvenBoundsTheDecode(f *testing.F) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		f.Add(skelDocument(r))
		f.Add(blockDocument(r))
	}
	f.Fuzz(func(t *testing.T, s string) {
		skeletonHolds(t, "fuzz", []byte(s))
	})
}

// A prompt's text is no structure: a document whose `|` block holds a JSON
// example — every line punctuation the count reads as values — is let
// through on the count of what is outside it, where the count of the whole
// text passes the ceiling. Text in quotes is counted, and so is the text
// of a block the skeleton cannot prove one — a header inside a flow
// collection — refused where the count passes, on the line yaml.v3 names.
func TestTheTextOfABlockIsNotCounted(t *testing.T) {
	json := strings.Repeat("    {\"id\": 1, \"name\": \"row\", \"ok\": [true, false]},\n", 2000)
	doc := []byte("dsl: 2\nprompts:\n  p: |\n" + json + "nodes: [a, b]\n")
	if u := YAMLValuesBound(doc); u <= 1000 {
		t.Fatalf("the fixture counts %d, under the ceiling of 1000", u)
	}
	if r := CheckYAMLDecode(doc, 1000, true); r != nil {
		t.Errorf("a JSON example in a `|` block is refused: %v", r)
	}
	if r := CheckYAMLDecode(doc, 1000, false); r == nil {
		t.Error("with no block set aside, the JSON example's punctuation passes the ceiling and is let through")
	}
	// The block's text is found where yaml.v3 finds it: past the column of
	// the key a list item opens with, so a sibling under an empty block is
	// no text of it; at the column an indentation digit names, under a
	// first line deeper than the rest; ended by a comment between the key's
	// column and the text's.
	for name, src := range map[string]string{
		"an empty block under a list item's key": "p: |\n" + json + "nodes:\n  - system: |\n    output: o\n",
		"an indentation digit":                   "p: |2\n    first\n" + strings.ReplaceAll(json, "    ", "  ") + "q: x\n",
		"a comment after the text":               "p: >\n" + json + "  # c\nq: x\n",
	} {
		if r := CheckYAMLDecode([]byte(src), 1000, true); r != nil {
			t.Errorf("%s: the text of a block is counted: %v", name, r)
		}
		if n := nodesBuilt([]byte(src)); n > 20 {
			t.Errorf("%s: the fixture is not blocks and a few keys: yaml.v3 builds %d values", name, n)
		}
	}
	quoted := []byte("dsl: 2\nprompts:\n  p: \"" + strings.Repeat("{\\\"a\\\": [1, 2]}, ", 300) + "\"\n")
	if r := CheckYAMLDecode(quoted, 1000, true); r == nil || r.Line != 3 {
		t.Errorf("a quoted value's punctuation is not counted: %v", r)
	}
	// The header stands inside a flow collection, so its lines are no
	// block: yaml.v3 refuses the `|`, and the count stands.
	flow := []byte("dsl: 2\nk: [x,\n  y: |\n" + json + "]\n")
	if r := CheckYAMLDecode(flow, 1000, true); r == nil {
		t.Error("a header inside a flow collection is trusted: its lines are set aside unproven")
	}
}

// The refusal is placed where the count passes the ceiling, read off the
// text with the lines yaml.v3 counts — CR, NEL, LS and PS end a line — and
// a leading BOM aside, as the reader places a node.
func TestARefusalIsPlacedOnTheLinesYAMLCounts(t *testing.T) {
	src := []byte("\ufeffa: 1\rb: 2\u0085c: 3\u2028d: [x, x, x]\n")
	// 2, then 3 a `:`: a 5, b 8, c 11, d 14, `[` 16, the first `,` 18.
	r := CheckYAMLDecode(src, 17, false)
	if r == nil || r.Line != 4 || r.Column != 6 {
		t.Fatalf("refusal %+v, want line 4 column 6 (the `,` after the first x)", r)
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(src, &doc); err != nil {
		t.Fatal(err)
	}
	seq := doc.Content[0].Content[7]
	if seq.Line != 4 {
		t.Fatalf("yaml.v3 places the list on line %d: the fixture does not hold the breaks it names", seq.Line)
	}
}
