package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/mdcode"
)

// A bundle's own text — its persona, its description, its version — is
// spliced into a cell of the bots map's table. A line break in one of them
// ends the row and puts the rest of the value at column 0, where three
// backticks open a fenced block that reads the whole map below it as code.
// The bundle is a third party's, so the map owes its own rows that guard.
func TestBotsMapKeepsAMultiLineValueInsideItsRow(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		dir := filepath.Join(root, "bots", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha", "name: alpha\ndisplay_name: \"Revi\\n```suggestion\"\ndescription: The first bundle.\nversion: 0.1.0\n")
	write("omega", "name: omega\ndisplay_name: Omega\ndescription: The row that follows.\nversion: 0.2.0\n")

	out, err := botBundles{}.Extract(root)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	lines := mdcode.Lines(out)
	var fence mdcode.FenceScanner
	for _, line := range lines {
		fence.Code(line)
	}
	if fence.Open() {
		t.Errorf("the bots map leaves a fenced block open — every row after it reads as code:\n%s", out)
	}
	masked := mdcode.Lines(mdcode.Mask(out))
	for i, line := range lines {
		if strings.Contains(line, "The row that follows.") && strings.TrimSpace(masked[i]) == "" {
			t.Errorf("line %d %q reads as CODE in the map:\n%s", i+1, line, out)
		}
	}
	if !strings.Contains(out, "| `alpha` | Revi ```suggestion |") {
		t.Errorf("the persona cell did not keep the whole value on its row:\n%s", out)
	}
}

// The same rows against the other byte a cell cannot carry: its separator. A
// row is cut on its unescaped pipes before any of its text is read — inside a
// code span as readily as in prose — so a pipe a bundle writes shifts every
// column after it and drops the row's last cell. Every value here is a
// manifest's own, which is why the escape belongs to the cell renderer and
// not to the sentence helper that only some of them pass through.
func TestBotsMapKeepsAPipeInsideItsCell(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bots", "alpha")
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	// version carries a pipe the MANIFEST already escaped: a second escape
	// would write `\\|`, an escaped backslash followed by a live separator,
	// and the row below would carry one cell too many.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(
		"name: alpha\ndisplay_name: \"Revi | the reviewer\"\n"+
			"description: \"Reviews a branch | posts the findings. Nothing else.\"\nversion: '0.1.0 \\| rc'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "s.md"), []byte(
		"---\nname: review | deep\ndescription: Reviews | deeply.\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := botBundles{}.Extract(root)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	rows := 0
	for _, line := range mdcode.Lines(out) {
		if !strings.HasPrefix(line, "| `") {
			continue // a heading row or the separator, not a data row
		}
		rows++
		// Four columns in the bundles table, three in the skills table.
		if n := separatorPipes(line); n != 5 && n != 4 {
			t.Errorf("a row carries %d cell separator(s) — its cells no longer line up with its header: %s", n, line)
		}
	}
	if rows != 2 {
		t.Fatalf("expected one bundle row and one skill row, found %d:\n%s", rows, out)
	}
	for _, want := range []string{`Revi \| the reviewer`, `Reviews a branch \| posts the findings.`, `0.1.0 \| rc`, `review \| deep`, `Reviews \| deeply.`} {
		if !strings.Contains(out, want) {
			t.Errorf("a cell lost its pipe, or escaped one the manifest had already escaped (%q missing):\n%s", want, out)
		}
	}
}

// separatorPipes counts the pipes of a row that CUT it into cells, by the
// renderer's rule: a pipe is escaped when an odd number of backslashes
// precedes it. The parity is the whole point — measured on pandoc 3.1.3 gfm,
// a doubly-escaped `\\|` is an escaped backslash followed by a LIVE
// separator, so a cell escaped twice splits exactly like one never escaped.
func separatorPipes(row string) int {
	n := 0
	for i := 0; i < len(row); i++ {
		if row[i] != '|' {
			continue
		}
		slashes := 0
		for j := i - 1; j >= 0 && row[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			n++
		}
	}
	return n
}

// The two cells that reach the maps WITHOUT the sentence helper — an ADR's
// status, a package's doc comment — go through the same cell renderer as
// every other. The helper escapes nothing on its own, because escaping twice
// is not twice as safe: a value escaped by both carries `\\|`, and the two
// renderers of these maps then disagree about it — pandoc 3.1.3 gfm reads an
// escaped backslash followed by a LIVE separator and splits the row (`x \` |
// `y` | `b`, the last cell dropped), GitHub shows `x | y` in one cell. The
// single escape is the one form both read as the author's pipe, so exactly
// once is the rule, and a doubled one reddens here.
func TestEveryMapCellGoesThroughTheCellRenderer(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/adr/0001-pipes.md", "# Pipes | and cells\n\n- **Status**: Accepted | superseded by 0002\n\nA page whose sentence carries a | pipe.\n")
	write("pkg/piped/doc.go", "// Package piped documents a | pipe in its first sentence.\npackage piped\n")

	for _, tc := range []struct {
		stem  string
		cells []string
	}{
		{stem: "docs", cells: []string{`Pipes \| and cells`, `Accepted \| superseded by 0002`, `A page whose sentence carries a \| pipe.`}},
		{stem: "packages", cells: []string{`documents a \| pipe in its first sentence.`}},
	} {
		t.Run(tc.stem, func(t *testing.T) {
			var ex Extractor
			for _, e := range Extractors() {
				if e.Stem() == tc.stem {
					ex = e
				}
			}
			if ex == nil {
				t.Fatalf("no extractor with stem %q", tc.stem)
			}
			out, err := ex.Extract(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.cells {
				if !strings.Contains(out, want) {
					t.Errorf("a cell is not escaped exactly once (%q missing):\n%s", want, out)
				}
			}
			if strings.Contains(out, `\\|`) {
				t.Errorf("a cell is escaped twice — a renderer cuts the row on the pipe after the backslash:\n%s", out)
			}
			rows := 0
			for _, line := range mdcode.Lines(out) {
				if !strings.HasPrefix(line, "| [`") && !strings.HasPrefix(line, "| `") {
					continue
				}
				rows++
				if n := separatorPipes(line); n != 5 && n != 4 {
					t.Errorf("a row carries %d cell separator(s): %s", n, line)
				}
			}
			if rows == 0 {
				t.Fatalf("the fixture never reached a table row:\n%s", out)
			}
		})
	}
}
