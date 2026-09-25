package ir

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/docfences"
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
// to readers and only this test sees them. The fences are read by
// pkg/dsl/internal/docfences — an indented fence, its body, a body line
// indented less than the fence — the helper the author tests read the
// same documentation through.

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

// fragmentElsewhereMessages are the compile errors a `fragment` fence may
// raise only because it omits what it references, told apart by message
// where the code also covers shape errors of the fragment itself: a
// contract's input whose var the page declares elsewhere (C300) or output
// whose producer it does (C301) — never a contract with `version: 0`, a
// default on a required input, or a port without a producer.
var fragmentElsewhereMessages = []string{
	"is not a declared var",
	"which the program does not declare",
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
		if m != nil && (DiagCode(m[1]) == DiagContractInput || DiagCode(m[1]) == DiagContractOutput) {
			var elsewhere bool
			for _, msg := range fragmentElsewhereMessages {
				elsewhere = elsewhere || strings.Contains(e, msg)
			}
			if elsewhere {
				continue
			}
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

// hoistProfileHeader takes a leading `dsl: N` line off a fragment, to be
// written ABOVE the synthetic header that wraps the fragment: the profile
// header must be a file's first significant line, and a wrapped fragment's
// first line is not.
func hoistProfileHeader(body string) (header, rest string) {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "dsl:") {
			return t + "\n", strings.Join(append(lines[:i:i], lines[i+1:]...), "\n")
		}
		break
	}
	return "", body
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
func compileSnippet(s docfences.Fence) (parseErrs, compileErrs []string, err error) {
	src := s.Body
	tag := s.Info
	switch {
	case tag == "" || tag == "invalid" || strings.HasPrefix(tag, "invalid:"):
	case tag == "fragment":
		src = withSyntheticWorkflow(s.Body)
	case tag == "fragment:edges":
		header, body := hoistProfileHeader(s.Body)
		src = header + indentSnippet("workflow _snippet:\n  entry: "+firstEdgeSource(body), body)
	case tag == "fragment:workflow":
		header, body := hoistProfileHeader(s.Body)
		src = header + indentSnippet("workflow _snippet:", body)
	case strings.HasPrefix(tag, "fragment:"):
		kind := strings.TrimPrefix(tag, "fragment:")
		if !snippetFragmentKinds[kind] {
			return nil, nil, fmt.Errorf("unknown fragment kind %q", kind)
		}
		header, body := hoistProfileHeader(s.Body)
		src = header + indentSnippet(kind+" _snippet:", body)
	default:
		return nil, nil, fmt.Errorf("unknown fence tag %q — use `iter`, `iter fragment`, `iter fragment:edges`, `iter fragment:workflow`, `iter fragment:<kind>` or `iter invalid` (one word, no spaces)", tag)
	}
	pr := parser.Parse(filepath.Base(s.File), src)
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
	root := filepath.Join("..", "..", "..")
	files, err := docfences.Files(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		fences, err := docfences.Extract(path, "iter")
		if err != nil {
			t.Errorf("%v", err)
		}
		for _, s := range fences {
			total++
			rel := strings.TrimPrefix(s.File, root+string(filepath.Separator))
			where := fmt.Sprintf("%s:%d fence `iter %s`", rel, s.Line, s.Info)
			if s.Malformed != "" {
				t.Errorf("%s: %s", where, s.Malformed)
				continue
			}
			parseErrs, compileErrs, err := compileSnippet(s)
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			switch {
			case strings.HasPrefix(s.Info, "invalid:"):
				code := strings.TrimPrefix(s.Info, "invalid:")
				all := strings.Join(append(parseErrs, compileErrs...), "\n")
				if !strings.Contains(all, "["+code+"]") {
					t.Errorf("%s: is tagged as the %s anti-example but does not fail with it:\n      %s", where, code, firstN(append(parseErrs, compileErrs...), 3))
				}
			case s.Info == "invalid":
				if len(parseErrs)+len(compileErrs) == 0 {
					t.Errorf("%s: is tagged as an anti-example but compiles clean — drop the tag or make it invalid again", where)
				}
			case s.Info == "":
				if len(parseErrs) > 0 {
					t.Errorf("%s: does not parse:\n      %s\n    (a standalone fence must parse and compile; tag it `iter fragment…` if it is deliberately partial)", where, firstN(parseErrs, 3))
				} else if len(compileErrs) > 0 {
					t.Errorf("%s: does not compile:\n      %s\n    (declare what it references, or tag it `iter fragment` if it is deliberately partial)", where, firstN(compileErrs, 3))
				}
			case s.Info == "fragment":
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
