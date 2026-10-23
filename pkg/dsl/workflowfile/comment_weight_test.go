package workflowfile

import (
	"strings"
	"testing"
)

// A comment the reader keeps is weighed before the decode, two units a `#`
// (r3b F1): yaml.v3 holds every comment token it scans beside the node it
// hangs on, some 500 bytes against some 200 for the node, so one comment a
// value — `#` and `-` on lines of their own — passed a ceiling sized for
// values alone at three times the memory it admits. The count passes the
// ceiling on the line the running count says, and a `#` in the text of a
// block is set aside with the text.
func TestACommentIsWeighedBeforeTheDecode(t *testing.T) {
	// 400 entries and 400 comments: 2 + 400 + 2×400 = 1202 units.
	src := []byte(strings.Repeat("#\n-\n", 400))
	r := CheckYAMLDecode(src, 1000, false)
	if r == nil {
		t.Fatalf("one comment a value, %d units by the values alone, passes a ceiling of 1000", 2+400)
	}
	// 2 + 3k after k entries: the 333rd entry's `-` (line 666) passes 1000.
	if r.Line != 666 || !strings.Contains(r.Counted(), "`#`") {
		t.Errorf("refusal %+v counted %q, want it on line 666, the `#` named among what it counts", r, r.Counted())
	}
	if n := nodesBuilt(src); n < 400 {
		t.Fatalf("the fixture is not a value a line: yaml.v3 builds %d", n)
	}
	// Markdown headings in a prompt are the block's text, never comments.
	block := []byte("p: |\n" + strings.Repeat("  # a heading\n  text\n", 600))
	if r := CheckYAMLDecode(block, 1000, true); r != nil {
		t.Errorf("the `#` of a block's text is counted: %v", r)
	}
}

// A `|` header may carry a comment after its indicator, which yaml.v3 reads
// as one there (r3b F5): the block's text is set aside as under a bare
// header, the skeleton's reading proving it.
func TestABlockHeaderWithACommentSetsItsTextAside(t *testing.T) {
	json := strings.Repeat("    {\"id\": 1, \"name\": \"row\", \"ok\": [true, false]},\n", 2000)
	for name, src := range map[string]string{
		"a comment after the indicator": "dsl: 2\nprompts:\n  p: | # the review brief\n" + json + "nodes: [a, b]\n",
		"after a chomping indicator":    "dsl: 2\nprompts:\n  p: >- # folded\n" + json + "nodes: [a, b]\n",
		// No blank before the `#`: yaml.v3 still reads a header and a
		// comment there, and builds the same values.
		"right after the indicator":        "dsl: 2\nprompts:\n  p: |#the review brief\n" + json + "nodes: [a, b]\n",
		"right after a chomping indicator": "dsl: 2\nprompts:\n  p: >-#folded\n" + json + "nodes: [a, b]\n",
	} {
		if r := CheckYAMLDecode([]byte(src), 1000, true); r != nil {
			t.Errorf("%s: the text of the block is counted: %v", name, r)
		}
		if n := nodesBuilt([]byte(src)); n > 20 {
			t.Errorf("%s: the fixture is not one block and a few keys: yaml.v3 builds %d values", name, n)
		}
	}
	// A `#` that is no comment — inside a quoted key — opens no header.
	quoted := []byte("dsl: 2\n\"a #b\": [" + strings.Repeat("x, ", 600) + "x]\n")
	if r := CheckYAMLDecode(quoted, 1000, true); r == nil {
		t.Error("a quoted key holding ` #` is taken for a block header: the list after it goes uncounted")
	}
}
