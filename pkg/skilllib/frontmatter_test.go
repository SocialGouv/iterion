package skilllib

import (
	"strings"
	"testing"
)

// ScanFrontmatter is the single shared parser for SKILL.md frontmatter. These
// cases lock down every branch of its tolerance contract — each assertion is
// chosen so a broken stub (dropped quote-strip, swallowed colon, missing
// content-line break, etc.) fails.
func TestScanFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantName string
		wantDesc string
	}{
		{
			name:     "basic block",
			in:       "---\nname: foo\ndescription: bar\n---\n# body\n",
			wantName: "foo",
			wantDesc: "bar",
		},
		{
			name:     "no frontmatter at all",
			in:       "# Title\n\nname: not-a-field\n",
			wantName: "",
			wantDesc: "",
		},
		{
			name:     "empty input",
			in:       "",
			wantName: "",
			wantDesc: "",
		},
		{
			// The break-on-first-content-line guard: a markdown horizontal
			// rule further down must NOT be mistaken for a frontmatter opener.
			// Without the guard, `name:` below the hr would leak in.
			name:     "content then hr then fake frontmatter is ignored",
			in:       "# Title\n\n---\nname: leaked\n---\n",
			wantName: "",
			wantDesc: "",
		},
		{
			name:     "leading blank lines before opener are tolerated",
			in:       "\n\n---\nname: x\ndescription: y\n---\n",
			wantName: "x",
			wantDesc: "y",
		},
		{
			name:     "double-quoted values are unquoted",
			in:       "---\nname: \"quoted-name\"\ndescription: \"quoted desc\"\n---\n",
			wantName: "quoted-name",
			wantDesc: "quoted desc",
		},
		{
			// strings.Cut splits on the FIRST colon only — a colon inside the
			// value must be preserved, not truncated.
			name:     "colon inside value is preserved",
			in:       "---\ndescription: use when: a happens\n---\n",
			wantName: "",
			wantDesc: "use when: a happens",
		},
		{
			name:     "CRLF line endings are stripped",
			in:       "---\r\nname: winnt\r\ndescription: crlf\r\n---\r\n",
			wantName: "winnt",
			wantDesc: "crlf",
		},
		{
			name:     "keys are case-insensitive",
			in:       "---\nNAME: up\nDescription: mixed\n---\n",
			wantName: "up",
			wantDesc: "mixed",
		},
		{
			name:     "lines without a colon are skipped",
			in:       "---\ngarbage line no colon\nname: survives\n---\n",
			wantName: "survives",
			wantDesc: "",
		},
		{
			// EOF before the closing --- must still yield what was collected,
			// not error or drop everything.
			name:     "unclosed frontmatter still yields collected fields",
			in:       "---\nname: unclosed\ndescription: still here",
			wantName: "unclosed",
			wantDesc: "still here",
		},
		{
			// The scan stops at the closing ---; fields after it are ignored.
			name:     "fields after closing delimiter are ignored",
			in:       "---\nname: real\n---\nname: should-be-ignored\ndescription: nope\n",
			wantName: "real",
			wantDesc: "",
		},
		{
			name:     "whitespace around key and value is trimmed",
			in:       "---\n  name  :   spaced   \n---\n",
			wantName: "spaced",
			wantDesc: "",
		},
		{
			name:     "unknown keys are ignored, known ones still captured",
			in:       "---\nversion: 3\nauthor: jo\nname: kept\n---\n",
			wantName: "kept",
			wantDesc: "",
		},
		{
			// Folded block scalar (`>`): continuation lines join into one
			// whitespace-normalized string. Without block-scalar support the
			// router would see just ">" as the whole description.
			name:     "folded block-scalar description",
			in:       "---\nname: n\ndescription: >\n  first line\n  second line\n---\n# body\n",
			wantName: "n",
			wantDesc: "first line second line",
		},
		{
			// Literal block scalar (`|`): newlines preserved.
			name:     "literal block-scalar description",
			in:       "---\nname: n\ndescription: |\n  line one\n  line two\n---\n",
			wantName: "n",
			wantDesc: "line one\nline two",
		},
		{
			// A block scalar is terminated by a dedent to another key, which
			// is then parsed normally.
			name:     "block scalar ends at next key",
			in:       "---\ndescription: >\n  folded text\nname: after\n---\n",
			wantName: "after",
			wantDesc: "folded text",
		},
		{
			// Chomping/indent indicators after |/> are ignored (we only need
			// the text), and the block may end at EOF/`---` mid-collection.
			name:     "block scalar with chomping indicator",
			in:       "---\nname: n\ndescription: |-\n  kept text\n---\n",
			wantName: "n",
			wantDesc: "kept text",
		},
		{
			// Regression: an indented "---" INSIDE a folded block is content,
			// not a frontmatter terminator — later keys must survive.
			name:     "indented --- inside block is content",
			in:       "---\ndescription: >\n  before\n  ---\n  after\nname: kept\n---\n",
			wantName: "kept",
			wantDesc: "before --- after",
		},
		{
			// Regression: a value with text after >/| on the same line is an
			// inline scalar, NOT a block header — assign it verbatim.
			name:     "inline > value is not a block scalar",
			in:       "---\nname: n\ndescription: > inline text\n---\n",
			wantName: "n",
			wantDesc: "> inline text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc := ScanFrontmatter(strings.NewReader(tc.in))
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if desc != tc.wantDesc {
				t.Errorf("description = %q, want %q", desc, tc.wantDesc)
			}
		})
	}
}
