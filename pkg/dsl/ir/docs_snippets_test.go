package ir

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every ```iter fence in the repository's documentation is a program the
// reader — a human or an agent given the authoring skill — will copy. This
// test compiles each of them with the real parser and compiler, so a
// snippet can no longer teach a syntax the language does not have.
//
// Measured before the guard existed (2026-09-09): of 91 fences across the
// authoring docs, 2 compiled, 48 did not even parse, and the three
// canonical examples of the DSL quickref skill (tool node, workflow block,
// sandbox block) all failed — with exactly the mistakes an agent then
// reproduces (YAML-style lists, `#` comments, a plausible property that
// does not exist).
//
// The fence's info string carries the policy, after the language tag:
//
//	```iter                     standalone — must parse AND compile clean
//	```iter fragment            top-level declarations — must parse
//	```iter fragment:edges      edge lines — wrapped in a workflow, must parse
//	```iter fragment:workflow   workflow members — wrapped, must parse
//	```iter fragment:<kind>     node properties — wrapped in `<kind> _:`, must parse
//	```iter invalid             a documented anti-example — must NOT be clean
//
// Markdown renderers read only the first word, so the tags are invisible
// to readers and only this test sees them.

var (
	fenceOpenRe = regexp.MustCompile("^```iter(?:\\s+(\\S+))?\\s*$")
	fenceEndRe  = regexp.MustCompile("^```\\s*$")
)

// snippetFragmentKinds are the declaration kinds a `fragment:<kind>` fence
// may be wrapped in.
var snippetFragmentKinds = map[string]bool{
	"agent": true, "judge": true, "router": true, "human": true, "tool": true,
	"compute": true, "subbot": true, "emit": true, "wait": true, "await_answers": true,
	"fail": true, "supervisor": true, "cursor": true, "mcp_server": true,
	"schema": true, "prompt": true, "group": true,
}

type docSnippet struct {
	file string
	line int // line of the opening fence, 1-based
	tag  string
	body string
}

// docSnippetFiles lists the markdown a reader may take DSL from: the docs
// tree, the two root skills, the README, and every bot/example README and
// skill.
func docSnippetFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	var files []string
	for _, p := range []string{"README.md", "SKILL.md", "SKILL-run-and-refine.md"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			files = append(files, filepath.Join(root, p))
		}
	}
	walk := func(dir string) {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".md") {
				files = append(files, path)
			}
			return nil
		})
	}
	walk("docs")
	for _, g := range []string{"bots/*/skills/*.md", "bots/*/README.md", "examples/*/README.md", "examples/*/skills/*.md"} {
		m, _ := filepath.Glob(filepath.Join(root, g))
		files = append(files, m...)
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no documentation files found — is the test running from pkg/dsl/ir?")
	}
	return files
}

func extractDocSnippets(t *testing.T, path string) []docSnippet {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []docSnippet
	var cur *docSnippet
	var body []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if cur == nil {
			if m := fenceOpenRe.FindStringSubmatch(line); m != nil {
				cur = &docSnippet{file: path, line: lineNo, tag: m[1]}
				body = body[:0]
			}
			continue
		}
		if fenceEndRe.MatchString(line) {
			cur.body = strings.Join(body, "\n") + "\n"
			out = append(out, *cur)
			cur = nil
			continue
		}
		body = append(body, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if cur != nil {
		t.Errorf("%s:%d: ```iter fence is never closed", path, cur.line)
	}
	return out
}

// indentSnippet nests a fragment under a synthetic declaration header.
func indentSnippet(header, body string) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// firstEdgeSource returns the source node of the first edge line, for the
// synthetic `entry:` of an edges fragment.
func firstEdgeSource(body string) string {
	for _, l := range strings.Split(body, "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if i := strings.Index(s, "->"); i > 0 {
			return strings.TrimSpace(s[:i])
		}
	}
	return "_entry"
}

// compileSnippet returns the parse errors and the compile errors of a
// snippet assembled per its tag.
func compileSnippet(s docSnippet) (parseErrs, compileErrs []string, err error) {
	src := s.body
	tag := s.tag
	switch {
	case tag == "" || tag == "fragment" || tag == "invalid":
	case tag == "fragment:edges":
		src = indentSnippet("workflow _snippet:\n  entry: "+firstEdgeSource(s.body), s.body)
	case tag == "fragment:workflow":
		src = indentSnippet("workflow _snippet:", s.body)
	case strings.HasPrefix(tag, "fragment:"):
		kind := strings.TrimPrefix(tag, "fragment:")
		if !snippetFragmentKinds[kind] {
			return nil, nil, fmt.Errorf("unknown fragment kind %q", kind)
		}
		src = indentSnippet(kind+" _snippet:", s.body)
	default:
		return nil, nil, fmt.Errorf("unknown fence tag %q — use `iter`, `iter fragment`, `iter fragment:edges`, `iter fragment:workflow`, `iter fragment:<kind>` or `iter invalid`", tag)
	}
	pr := parser.Parse(filepath.Base(s.file), src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			parseErrs = append(parseErrs, d.Error())
		}
	}
	if pr.File == nil {
		return parseErrs, nil, nil
	}
	cr := Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == SeverityError {
			compileErrs = append(compileErrs, d.Error())
		}
	}
	return parseErrs, compileErrs, nil
}

func firstN(items []string, n int) string {
	if len(items) > n {
		items = append(items[:n:n], fmt.Sprintf("… and %d more", len(items)-n))
	}
	return strings.Join(items, "\n      ")
}

// TestDocsIterFencesCompile is the guard: every ```iter fence in the docs
// honours the policy its tag declares.
func TestDocsIterFencesCompile(t *testing.T) {
	total := 0
	for _, path := range docSnippetFiles(t) {
		for _, s := range extractDocSnippets(t, path) {
			total++
			rel := strings.TrimPrefix(s.file, filepath.Join("..", "..", "..")+string(filepath.Separator))
			where := fmt.Sprintf("%s:%d fence `iter %s`", rel, s.line, s.tag)
			parseErrs, compileErrs, err := compileSnippet(s)
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			switch s.tag {
			case "invalid":
				if len(parseErrs)+len(compileErrs) == 0 {
					t.Errorf("%s: is tagged as an anti-example but compiles clean — drop the tag or make it invalid again", where)
				}
			case "":
				if len(parseErrs) > 0 {
					t.Errorf("%s: does not parse:\n      %s\n    (a standalone fence must parse and compile; tag it `iter fragment…` if it is deliberately partial)", where, firstN(parseErrs, 3))
				} else if len(compileErrs) > 0 {
					t.Errorf("%s: does not compile:\n      %s\n    (declare what it references, or tag it `iter fragment` if it is deliberately partial)", where, firstN(compileErrs, 3))
				}
			default: // fragment*
				if len(parseErrs) > 0 {
					t.Errorf("%s: does not parse:\n      %s", where, firstN(parseErrs, 3))
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("found no ```iter fences at all — the extractor is broken")
	}
	t.Logf("checked %d ```iter fences", total)
}
