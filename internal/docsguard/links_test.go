package docsguard

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// The slug rule is held to anchors GitHub itself generated. Each fixture
// under testdata/github-anchors is `raw heading line<TAB>anchor`, in
// document order, taken from the rendered page GitHub serves for the file
// (`gh api -H 'Accept: application/vnd.github.html'
// repos/SocialGouv/iterion/contents/<path>`, 2026-09-19, main at c2d06e35d,
// the raw file byte-identical to the tree). Order matters: the `-1`, `-2`
// numbering of repeated headings is part of what is verified.
//
// A slugger that collapsed runs of spaces, kept `_` out, or dropped the
// leading hyphen an emoji leaves behind reported 41 false breakages on this
// repository before this table existed (#1233); every one of those rules has
// a row here.
func TestHeadingAnchorsMatchGitHub(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "github-anchors", "*.tsv"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixture under testdata/github-anchors — the guard read nothing and would pass on anything (err=%v)", err)
	}
	covered := map[string]bool{}
	for _, fixture := range fixtures {
		raws, anchors := readAnchorFixture(t, fixture)
		doc := Parse(fixture, []byte(strings.Join(raws, "\n\n")+"\n"))
		if len(doc.Headings) != len(anchors) {
			t.Fatalf("%s: parsed %d headings out of %d fixture rows", fixture, len(doc.Headings), len(anchors))
		}
		for i, h := range doc.Headings {
			if h.Anchor != anchors[i] {
				t.Errorf("%s row %d: %q\n  want %q\n  got  %q", filepath.Base(fixture), i+1, raws[i], anchors[i], h.Anchor)
			}
			if strings.Contains(anchors[i], "--") {
				covered["double hyphen from a stripped symbol"] = true
			}
			if strings.HasPrefix(anchors[i], "-") {
				covered["leading hyphen from a leading emoji"] = true
			}
			if strings.Contains(anchors[i], "_") {
				covered["underscore kept"] = true
			}
			if strings.HasSuffix(anchors[i], "-3") {
				covered["repeated heading numbered"] = true
			}
			if strings.HasPrefix(raws[i], " ") {
				covered["heading indented inside a list item"] = true
			}
		}
	}
	for _, rule := range []string{
		"double hyphen from a stripped symbol",
		"leading hyphen from a leading emoji",
		"underscore kept",
		"repeated heading numbered",
		"heading indented inside a list item",
	} {
		if !covered[rule] {
			t.Errorf("no fixture row exercises the rule %q — the table shrank below what it exists to prove", rule)
		}
	}
}

func readAnchorFixture(t *testing.T, path string) (raws, anchors []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		raw, anchor, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			t.Fatalf("%s: a row without a tab: %q", path, sc.Text())
		}
		raws = append(raws, raw)
		anchors = append(anchors, anchor)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return raws, anchors
}

func TestHeadingTextReducesInlineMarkdown(t *testing.T) {
	cases := map[string]string{
		"`claude_code`": "claude_code",
		"Typed terminal failure — `fail <name>`":          "Typed terminal failure — fail <name>",
		"[3.172.3](https://example.com/x) (2026-09-19)":   "3.172.3 (2026-09-19)",
		"![shield](img.png) Title":                        " Title",
		"<a name=\"tiers\"></a>Review tiers":              "Review tiers",
		"Fish &amp; chips":                                "Fish & chips",
		"__Bold__ start and _soft_ end":                   "Bold start and soft end",
		":tada: Release":                                  " Release",
		"a [ref link][id] here":                           "a ref link here",
		"**Strong** and *em* survive the slug regardless": "**Strong** and *em* survive the slug regardless",
	}
	for md, want := range cases {
		if got := HeadingText(md); got != want {
			t.Errorf("HeadingText(%q) = %q, want %q", md, got, want)
		}
	}
	if got := Slug("**Strong** and *em*"); got != "strong-and-em" {
		t.Errorf("emphasis markers must fall out of the slug: got %q", got)
	}
}

// What is not a link, and what is: the false positives the first checker of
// this repository reported came from exactly these shapes.
func TestParseSkipsCodeCommentsAndFrontMatter(t *testing.T) {
	src := strings.Join([]string{
		"---",
		"title: x",
		"# not a heading, a YAML comment",
		"---",
		"# Real",
		"",
		"A [real link](real.md) and `[in code](nope.md)` and ``[double](nope.md)``.",
		"",
		"```md",
		"[fenced](nope.md)",
		"# fenced heading",
		"```",
		"",
		"- item",
		"    ```",
		"    [indented fence](nope.md)",
		"    ```",
		"",
		"> ~~~",
		"> [quoted fence](nope.md)",
		"> ~~~",
		"",
		"<!-- [commented](nope.md) -->",
		"<!--",
		"# commented heading",
		"[multi-line comment](nope.md)",
		"-->",
		"",
		"[^note]: a footnote is not a link definition",
		"[def]: defined.md",
		"[^note] [text][def]",
		"",
		"## Second",
	}, "\n")
	doc := Parse("t.md", []byte(src))

	var targets []string
	for _, l := range doc.Links {
		targets = append(targets, l.Target)
	}
	if want := []string{"real.md", "defined.md"}; strings.Join(targets, " ") != strings.Join(want, " ") {
		t.Errorf("links = %v, want %v", targets, want)
	}
	for _, want := range []string{"real", "second"} {
		if !doc.Anchors[want] {
			t.Errorf("anchor %q missing from %v", want, doc.Anchors)
		}
	}
	for a := range doc.Anchors {
		if strings.Contains(a, "comment") || strings.Contains(a, "fenced") || strings.Contains(a, "yaml") {
			t.Errorf("anchor %q comes from skipped content", a)
		}
	}
}

func TestParseLinkForms(t *testing.T) {
	src := strings.Join([]string{
		`[title](a.md "A title") [angle](<with space.md>) [parens](b.md#x(y)) [nested [x] text](c.md)`,
		`![image](img.png) <a href="raw.md">raw</a> <img src="pic.png"> <https://auto.link/>`,
		`[external](https://example.com/p) [mail](mailto:x@y.z) [proto](//cdn.example.com/x)`,
	}, "\n")
	doc := Parse("t.md", []byte(src))
	var targets []string
	for _, l := range doc.Links {
		targets = append(targets, l.Target)
	}
	want := []string{"a.md", "with space.md", "b.md#x(y)", "c.md", "img.png", "raw.md", "pic.png",
		"https://example.com/p", "mailto:x@y.z", "//cdn.example.com/x"}
	if strings.Join(targets, "|") != strings.Join(want, "|") {
		t.Errorf("links =\n  %v\nwant\n  %v", targets, want)
	}
	for _, ext := range []string{"https://example.com/p", "mailto:x@y.z", "//cdn.example.com/x"} {
		if !IsExternal(ext) {
			t.Errorf("%q must be external", ext)
		}
	}
	if IsExternal("docs/a.md") || IsExternal("#x") || IsExternal("../x.md") {
		t.Error("a repository path is not external")
	}
}

func TestParseHeadingAndAnchorForms(t *testing.T) {
	src := strings.Join([]string{
		"## <a name=\"tiers\"></a>Review tiers — glance / guard / audit",
		"",
		"<a id=\"explicit\"></a>",
		"",
		"Setext title",
		"------------",
		"",
		"<h2>HTML heading</h2>",
		"",
		"> ## Quoted heading",
		"",
		"### Dup",
		"### Dup",
		"### Dup",
		"",
		"    ## Indented in a list",
	}, "\n")
	doc := Parse("t.md", []byte(src))
	for _, want := range []string{
		"tiers", "review-tiers--glance--guard--audit", "explicit", "setext-title",
		"html-heading", "quoted-heading", "dup", "dup-1", "dup-2", "indented-in-a-list",
	} {
		if !doc.Anchors[want] {
			t.Errorf("anchor %q missing from %v", want, doc.Anchors)
		}
	}
}

// The checker against a tree, then the mutations that must redden it. Each
// mutation rewrites the production fixture, never the assertion, and puts
// back the defect the report exists to catch.
func TestCheckIsGreenOnAResolvingTreeAndRedOnEachBreakage(t *testing.T) {
	root := writeLinkFixture(t)
	if broken := check(t, root); len(broken) != 0 {
		t.Fatalf("a tree whose every link resolves was refused:\n%s", join(broken))
	}

	t.Run("a renamed heading", func(t *testing.T) {
		root := writeLinkFixture(t)
		b := read(t, root, "docs/b.md")
		write(t, root, "docs/b.md", strings.ReplaceAll(b, "## Second section", "## Second part"))
		broken := check(t, root)
		// Four links and the reference definition name the first heading,
		// one names its numbered duplicate; the explicit anchor still holds.
		if len(broken) != 5 {
			t.Fatalf("five links point at the renamed headings, got %d:\n%s", len(broken), join(broken))
		}
		for _, b := range broken {
			switch {
			case strings.HasSuffix(b.Target, "#second-section-1"):
				if b.Nearest != "second-part-1" {
					t.Errorf("the numbered duplicate must suggest its renamed twin: %s", b)
				}
			case strings.Contains(b.Reason, "anchor #second-section not found in docs/b.md") && b.Nearest == "second-part":
			default:
				t.Errorf("unexpected report: %s", b)
			}
		}
	})
	t.Run("a deleted file", func(t *testing.T) {
		root := writeLinkFixture(t)
		if err := os.Remove(filepath.Join(root, "docs", "img.png")); err != nil {
			t.Fatal(err)
		}
		broken := check(t, root)
		if len(broken) != 2 {
			t.Fatalf("the link and the image both point at the deleted file, got %d:\n%s", len(broken), join(broken))
		}
		for _, b := range broken {
			if b.File != "docs/a.md" || b.Target != "img.png" || !strings.Contains(b.Reason, "file not found: docs/img.png") {
				t.Errorf("unexpected report: %s", b)
			}
		}
	})
	t.Run("a directory without README for a fragment", func(t *testing.T) {
		root := writeLinkFixture(t)
		if err := os.Remove(filepath.Join(root, "docs", "sub", "README.md")); err != nil {
			t.Fatal(err)
		}
		broken := check(t, root)
		if len(broken) != 1 || broken[0].Target != "sub/#intro" || !strings.Contains(broken[0].Reason, "without README.md") {
			t.Fatalf("want the fragment-on-directory report alone, got:\n%s", join(broken))
		}
	})
	t.Run("a fragment in the wrong case", func(t *testing.T) {
		root := writeLinkFixture(t)
		a := read(t, root, "docs/a.md")
		write(t, root, "docs/a.md", strings.Replace(a, "(#local-heading)", "(#Local-Heading)", 1))
		broken := check(t, root)
		if len(broken) != 1 || broken[0].Nearest != "local-heading" {
			t.Fatalf("want one case-mismatch report naming the real anchor, got:\n%s", join(broken))
		}
	})
	t.Run("a link that escapes the repository", func(t *testing.T) {
		root := writeLinkFixture(t)
		write(t, root, "README.md", "# Top\n\n[out](../secrets.md)\n")
		broken := check(t, root)
		if len(broken) != 1 || !strings.Contains(broken[0].Reason, "outside the repository") {
			t.Fatalf("want the outside-the-repository report, got:\n%s", join(broken))
		}
	})
}

// A fixture whose every link resolves: relative and root-absolute paths, a
// directory (and its README through a fragment), an explicit anchor, a
// numbered duplicate heading, a non-markdown fragment, an image, a
// reference definition, and external targets the checker must leave alone.
func writeLinkFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "README.md", "# Top\n\nSee [a](docs/a.md#local-heading) and [b](docs/b.md).\n")
	write(t, root, "docs/a.md", strings.Join([]string{
		"# A",
		"",
		"## Local heading",
		"",
		"[b](b.md) [sec](b.md#second-section) [dup](b.md#second-section-1) [angle](<b.md#second-section>)",
		"[own](#local-heading) [dir](sub/) [dir anchor](sub/#intro) [up](../README.md#top)",
		"[root](/docs/b.md#second-section) [img](img.png) ![img](img.png) [src](../pkg/x.go#L12)",
		"[explicit](b.md#explicit) [encoded](with%20space.md) [ext](https://example.com) [mail](mailto:a@b.c)",
		"",
		"[ref]: b.md#second-section",
	}, "\n")+"\n")
	write(t, root, "docs/b.md", "# First\n\n<a id=\"explicit\"></a>\n\n## Second section\n\n## Second section\n")
	write(t, root, "docs/with space.md", "# Spaced\n")
	write(t, root, "docs/sub/README.md", "# Intro\n")
	write(t, root, "docs/img.png", "png")
	write(t, root, "pkg/x.go", "package x\n")
	return root
}

func check(t *testing.T, root string) []Broken {
	t.Helper()
	var docs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(root, p)
			docs = append(docs, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := (&Checker{FS: OSFS{Root: root}}).Check(docs)
	if err != nil {
		t.Fatal(err)
	}
	return broken
}

func join(broken []Broken) string {
	var lines []string
	for _, b := range broken {
		lines = append(lines, "  "+b.String())
	}
	return strings.Join(lines, "\n")
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The delta is what lets the CI step be honest about a change without being
// blocked by the backlog the change inherited: the same link at another
// line is the same defect, a new target is a new one.
func TestNewSinceKeysOnFileAndTargetNotLine(t *testing.T) {
	base := []Broken{{Link: Link{File: "a.md", Line: 3, Target: "gone.md"}, Reason: "file not found: gone.md"}}
	head := []Broken{
		{Link: Link{File: "a.md", Line: 30, Target: "gone.md"}, Reason: "file not found: gone.md"},
		{Link: Link{File: "a.md", Line: 31, Target: "b.md#nope"}, Reason: "anchor #nope not found in b.md"},
	}
	fresh := NewSince(head, base)
	if len(fresh) != 1 || fresh[0].Target != "b.md#nope" {
		t.Fatalf("want the new target alone, got %v", fresh)
	}
	if got := NewSince(head, nil); len(got) != 2 {
		t.Fatalf("with no base everything is new, got %d", len(got))
	}
}

// The base side of the delta reads a committed tree through git, so what the
// CI step compares against is the merge base as committed — not the working
// tree, and not a checkout.
func TestGitTreeFSReadsTheCommittedTreeNotTheWorkingTree(t *testing.T) {
	repo := t.TempDir()
	gittest.InitRepo(t, repo)
	write(t, repo, "docs/a.md", "# A\n\n[old](b.md#gone)\n")
	write(t, repo, "docs/b.md", "# B\n")
	write(t, repo, "docs/sub/README.md", "# Sub\n")
	gittest.Run(t, repo, "add", "docs")
	gittest.Run(t, repo, "commit", "-q", "-m", "docs")

	// The working tree moves on: a new page with a new broken link, and the
	// committed page rewritten so its inherited defect sits on another line.
	write(t, repo, "docs/a.md", "# A\n\nprose\n\n[old](b.md#gone)\n")
	write(t, repo, "docs/c.md", "# C\n\n[new](b.md#also-gone)\n")

	tree := &GitTreeFS{Root: repo, Rev: "HEAD"}
	files, err := tree.Files(".md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, " ") != "README.md docs/a.md docs/b.md docs/sub/README.md" {
		t.Fatalf("committed markdown = %v", files)
	}
	if isDir, err := tree.Stat("docs/sub"); err != nil || !isDir {
		t.Errorf("docs/sub is a committed directory: isDir=%v err=%v", isDir, err)
	}
	if _, err := tree.Stat("docs/c.md"); err == nil {
		t.Error("docs/c.md is not committed, yet the tree says it exists")
	}
	if content, err := tree.ReadFile("docs/a.md"); err != nil || strings.Contains(string(content), "prose") {
		t.Errorf("ReadFile must return the committed content: err=%v content=%q", err, content)
	}

	baseBroken, err := (&Checker{FS: tree}).Check(files)
	if err != nil {
		t.Fatal(err)
	}
	headBroken := check(t, repo)
	fresh := NewSince(headBroken, baseBroken)
	if len(baseBroken) != 1 || len(headBroken) != 2 || len(fresh) != 1 || fresh[0].File != "docs/c.md" {
		t.Fatalf("base=%d head=%d new=%v — the inherited defect must not count as new", len(baseBroken), len(headBroken), fresh)
	}
}

func TestExcludedMatchesDirectoryPrefixesOnly(t *testing.T) {
	prefixes := []string{"vendor", "third_party", "node_modules"}
	for name, want := range map[string]bool{
		"vendor/github.com/x/README.md":   true,
		"docs/vendor/notes.md":            true,
		"studio/node_modules/a/README.md": true,
		"docs/vendored-tools.md":          false,
		"third_party_notes.md":            false,
		"README.md":                       false,
	} {
		if got := Excluded(name, prefixes); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", name, got, want)
		}
	}
}
