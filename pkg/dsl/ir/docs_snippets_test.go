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
//	```iter fragment            top-level declarations — must parse, and compile
//	                            except for what it may omit (see fragmentElsewhereCodes)
//	```iter fragment:edges      edge lines — wrapped in a workflow, must parse
//	```iter fragment:workflow   workflow members — wrapped, must parse
//	```iter fragment:<kind>     node properties — wrapped in `<kind> _:`, must parse
//	```iter invalid:C019        a documented anti-example — must fail with THAT code
//	```iter invalid             an anti-example — must not be clean (any error)
//
// Markdown renderers read only the first word, so the tags are invisible
// to readers and only this test sees them.

// A fence may be indented (inside a list item); the same indentation is
// stripped from its body, its closing fence must sit at exactly that indent
// (a deeper ``` is body text — a prompt teaching an agent to emit a code
// fence), and a body line indented less than the fence is reported rather
// than silently kept. The info string after `iter` is captured whole so a tag
// with a space in it (```iter fragment edges) is reported instead of being
// silently dropped.
var fenceOpenRe = regexp.MustCompile("^(\\s*)```iter\\b(.*)$")

// fragmentElsewhereCodes are the compile errors a `fragment` fence may raise
// only because it omits what it references — declared elsewhere on the page:
// an unknown node, schema, prompt, var, artifact, attachment, secret, cursor
// or group; no workflow, no entry; a node the missing workflow cannot reach;
// a history ref whose loop is outside. Every OTHER compile error is a shape
// error of the fragment itself (a conditional edge with no fallback, an
// undeclared cycle, a bad expression) and fails the guard: with 79 of the
// 108 fences tagged `fragment`, a parse-only policy would leave most of the
// documentation semantically unchecked — and did, until the headline pattern
// of groups-iteration-subbots.md was found to fail C012.
var fragmentElsewhereCodes = map[DiagCode]bool{
	DiagUnknownNode: true, DiagUnknownSchema: true, DiagUnknownPrompt: true,
	DiagNoWorkflow: true, DiagMissingEntry: true, DiagUnreachableNode: true,
	DiagHistoryRefNotInLoop: true, DiagUnknownRefNode: true, DiagUndeclaredVar: true,
	DiagUnknownArtifact: true, DiagRefNodeNotReachable: true, DiagUnknownAttachment: true,
	DiagUnknownSecret: true, DiagUnknownCursor: true, DiagUseUnknownGroup: true,
	// A router's edge-count checks and a node's resource lease read the
	// workflow's edges and `resources:` block — the part a node-only
	// fragment omits (the synthetic workflow appended for it has neither).
	DiagRoundRobinTooFewEdges: true, DiagLLMRouterTooFewEdges: true,
	DiagFanOutEachEdges: true, DiagUnknownResourceInNeeds: true,
}

var diagCodeRe = regexp.MustCompile(`\[(C\d{3})\]`)

// fragmentShapeErrors keeps the compile errors a fragment cannot excuse.
func fragmentShapeErrors(compileErrs []string) []string {
	var out []string
	for _, e := range compileErrs {
		m := diagCodeRe.FindStringSubmatch(e)
		if m != nil && fragmentElsewhereCodes[DiagCode(m[1])] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// snippetFragmentKinds are the declaration kinds a `fragment:<kind>` fence
// may be wrapped in.
var snippetFragmentKinds = map[string]bool{
	"agent": true, "judge": true, "router": true, "human": true, "tool": true,
	"compute": true, "subbot": true, "emit": true, "wait": true, "await_answers": true,
	"fail": true, "supervisor": true, "cursor": true, "mcp_server": true,
	"schema": true, "prompt": true, "group": true,
}

type docSnippet struct {
	file      string
	line      int // line of the opening fence, 1-based
	tag       string
	body      string
	malformed string // non-empty when the fence's own shape is wrong (a body line less indented than the fence)
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
	indent := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if cur == nil {
			if m := fenceOpenRe.FindStringSubmatch(line); m != nil {
				indent = m[1]
				cur = &docSnippet{file: path, line: lineNo, tag: strings.TrimSpace(m[2])}
				body = body[:0]
			}
			continue
		}
		if strings.TrimRight(line, " \t") == indent+"```" {
			cur.body = strings.Join(body, "\n") + "\n"
			out = append(out, *cur)
			cur = nil
			continue
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, indent) && cur.malformed == "" {
			cur.malformed = fmt.Sprintf("line %d is indented less than its fence", lineNo)
		}
		body = append(body, strings.TrimPrefix(line, indent))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if cur != nil {
		t.Errorf("%s:%d: ```iter fence is never closed", path, cur.line)
	}
	return out
}

// withSyntheticWorkflow appends a minimal workflow to a fragment that declares
// nodes but no workflow, so the compiler runs its node-level passes on them.
// Without it the compiler returns at "no workflow" (C006) before expanding
// groups, resolving prompts or checking any node — 52 of the 79 `fragment`
// fences — and the "compile" half of the fragment policy checked nothing.
// The synthetic workflow only names an entry: the nodes stay unreachable
// (C016, excused) and unwired (excused), which is what a fragment is.
var firstNodeDeclRe = regexp.MustCompile(`(?m)^(?:agent|judge|router|human|tool|compute|subbot|emit|wait|await_answers|fail)\s+([A-Za-z_]\w*)\s*:`)

func withSyntheticWorkflow(body string) string {
	if regexp.MustCompile(`(?m)^workflow\s`).MatchString(body) {
		return body
	}
	m := firstNodeDeclRe.FindStringSubmatch(body)
	if m == nil {
		return body
	}
	return body + "\nworkflow _snippet:\n  entry: " + m[1] + "\n"
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
	case tag == "" || tag == "invalid" || strings.HasPrefix(tag, "invalid:"):
	case tag == "fragment":
		src = withSyntheticWorkflow(s.body)
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
		return nil, nil, fmt.Errorf("unknown fence tag %q — use `iter`, `iter fragment`, `iter fragment:edges`, `iter fragment:workflow`, `iter fragment:<kind>` or `iter invalid` (one word, no spaces)", tag)
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
		if d.Severity != SeverityError {
			continue
		}
		// C018 (`model:`/`backend:` missing) is waived when the HOST can
		// auto-detect a credential (compile.go canAutoResolveBackend), so
		// its verdict differs between a laptop with a Claude login and CI
		// with none — the one compile check that reads the environment.
		// The guard cannot judge auto-detection, and an example that
		// relies on it is legitimate, so it never counts here.
		if d.Code == DiagMissingModelOrBackend {
			continue
		}
		compileErrs = append(compileErrs, d.Error())
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
			if s.malformed != "" {
				t.Errorf("%s: %s", where, s.malformed)
				continue
			}
			parseErrs, compileErrs, err := compileSnippet(s)
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			switch {
			case strings.HasPrefix(s.tag, "invalid:"):
				code := strings.TrimPrefix(s.tag, "invalid:")
				all := strings.Join(append(parseErrs, compileErrs...), "\n")
				if !strings.Contains(all, "["+code+"]") {
					t.Errorf("%s: is tagged as the %s anti-example but does not fail with it:\n      %s", where, code, firstN(append(parseErrs, compileErrs...), 3))
				}
			case s.tag == "invalid":
				if len(parseErrs)+len(compileErrs) == 0 {
					t.Errorf("%s: is tagged as an anti-example but compiles clean — drop the tag or make it invalid again", where)
				}
			case s.tag == "":
				if len(parseErrs) > 0 {
					t.Errorf("%s: does not parse:\n      %s\n    (a standalone fence must parse and compile; tag it `iter fragment…` if it is deliberately partial)", where, firstN(parseErrs, 3))
				} else if len(compileErrs) > 0 {
					t.Errorf("%s: does not compile:\n      %s\n    (declare what it references, or tag it `iter fragment` if it is deliberately partial)", where, firstN(compileErrs, 3))
				}
			case s.tag == "fragment":
				if len(parseErrs) > 0 {
					t.Errorf("%s: does not parse:\n      %s", where, firstN(parseErrs, 3))
				} else if shape := fragmentShapeErrors(compileErrs); len(shape) > 0 {
					t.Errorf("%s: has a shape error a fragment cannot excuse:\n      %s\n    (a fragment may omit what it references; it may not be wrong about what it shows)", where, firstN(shape, 3))
				}
			default: // fragment:edges, fragment:workflow, fragment:<kind> — wrapped, parse only
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
