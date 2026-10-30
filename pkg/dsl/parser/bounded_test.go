package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// boundedText is the most a bounded message or hint measures: the bound of
// content plus the ellipsis that marks a cut (expr.Clip).
const boundedText = expr.MaxDiagnosticText + len("…")

// Bounded is the one door every diagnostic that reaches a user passes: its
// Message and Hint are held to expr.MaxDiagnosticText whatever the site
// composed, so no text an author wrote — an anchor name, an import path —
// rides a message whole (#2801).
func TestBoundedClipsMessageAndHint(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	d := Bounded(Diagnostic{Code: DiagExpectedToken, Severity: SeverityError, Message: long, Hint: long})
	if len(d.Message) > boundedText || len(d.Hint) > boundedText {
		t.Fatalf("a 1 MiB text came back %d / %d bytes, want each under %d of content", len(d.Message), len(d.Hint), expr.MaxDiagnosticText)
	}
	if !strings.HasSuffix(d.Message, "…") || !strings.HasSuffix(d.Hint, "…") {
		t.Errorf("a clipped text is marked: %q / %q", d.Message[len(d.Message)-20:], d.Hint[len(d.Hint)-20:])
	}
	short := Diagnostic{Code: DiagExpectedToken, Severity: SeverityError, Message: "kept", Hint: "whole"}
	if got := Bounded(short); got != short {
		t.Errorf("a text under the bound is unchanged: %+v", got)
	}
}

// Report.Add is the recording door the parser, the YAML twin's speller and
// guard and the unit loader's own diagnostics record through: it bounds
// through Bounded.
func TestTheRecordingDoorBounds(t *testing.T) {
	var r Report
	r.Add(Diagnostic{Code: DiagExpectedToken, Severity: SeverityError, Message: strings.Repeat("y", 1<<20), Hint: strings.Repeat("z", 1<<20)})
	d := r.Diagnostics()[0]
	if len(d.Message) > boundedText || len(d.Hint) > boundedText {
		t.Fatalf("the door let a %d-byte message and a %d-byte hint through", len(d.Message), len(d.Hint))
	}
}

// TestEveryConstructorPassesBounded is the structural canary of #2801: a
// diagnostic composed OUTSIDE the parser package — the YAML twin's docDiag,
// the unit loader's refusals, the CLI's twin verdicts, the server's
// rewrites — is handed to a user as composed, with no report cap over it
// (the loader's first refusal joins the unit's list outside the report; the
// twin's refusals are answered directly). Every such construction is
// bounded at the door it passes: a parser.Diagnostic composite literal
// carrying fields is written parser.Bounded(parser.Diagnostic{...}), so no
// site can compose a message around the bound. The zero value (no message
// to bound) is not a construction, and in a collection literal
// ([]parser.Diagnostic{…}) every element is read: an expression element
// (docDiag(…), a door's own diagnostic) passes; a composite element carries
// its type in full, wrapped.
//
// One named limit: the scan sees constructions, not mutations — a zero
// value filled by a later assignment (d := parser.Diagnostic{}; d.Message =
// …) is no composite literal. The one production rewrite of a recorded
// diagnostic (citeStoredMainWorkflow) passes Bounded at the rewrite; a new
// one is a review finding, as it was at #2801.
func TestEveryConstructorPassesBounded(t *testing.T) {
	// The root must be ABSOLUTE: walked relative (../../..), WalkDir visits
	// the root first with de.Name() == "..", which the dot-dir exclusion
	// below would skip — the whole walk, one visit, no file read, green
	// whatever the tree holds (round-3 review: a planted offense passed).
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			// Real dot-dirs only — .git, .works, … — never the root the
			// walk was given (its NAME is no marker of what it holds).
			if path != root && strings.HasPrefix(de.Name(), ".") {
				return filepath.SkipDir
			}
			switch de.Name() {
			case "vendor", "third_party", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The parser package bounds its own messages where it composes them
		// (and Report.Add bounds again at the door).
		if filepath.Dir(path) == filepath.Join(root, "pkg", "dsl", "parser") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range unboundedConstructions(string(src)) {
			offenders = append(offenders, fmt.Sprintf("%s:%d", path, line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("a parser.Diagnostic is composed outside the parser package without passing parser.Bounded — the door that holds a message and a hint to %d bytes (#2801):\n  %s", expr.MaxDiagnosticText, strings.Join(offenders, "\n  "))
	}
}

// The canary's forms, probed against unboundedConstructions: the elided
// composite element of a collection literal (the reviewer's probe — the
// type written once for the slice, the elements' fields reachable by no
// wrapper the scan would see) and the aliased import (an alias composes a
// Diagnostic the marker never names) are offenders; the door forms pass.
func TestTheCanaryReadsEveryForm(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		want bool // an offender
		line int  // its line, when one is wanted (0: not checked)
	}{
		{"an elided element with fields", "package x\n\nvar _ = []parser.Diagnostic{{Code: parser.DiagExpectedToken, Message: msg}}\n", true, 0},
		{"an elided element among a door's own", "package x\n\nvar _ = []parser.Diagnostic{docDiag(name, 1, 1, msg), {Code: parser.DiagExpectedToken}}\n", true, 0},
		{"a keyed elided element", "package x\n\nvar _ = map[string]parser.Diagnostic{\"k\": {Message: msg}}\n", true, 0},
		{"a wrapped element", "package x\n\nvar _ = []parser.Diagnostic{parser.Bounded(parser.Diagnostic{Code: parser.DiagExpectedToken, Message: msg})}\n", false, 0},
		{"a door's own diagnostic as element", "package x\n\nvar _ = []parser.Diagnostic{docDiag(name, 1, 1, msg)}\n", false, 0},
		{"a brace inside a string is no element", "package x\n\nvar _ = []parser.Diagnostic{docDiag(name, 1, 1, \"a { brace\")}\n", false, 0},
		{"an empty collection", "package x\n\nvar _ = []parser.Diagnostic{}\n", false, 0},
		{"a zero-value element", "package x\n\nvar _ = []parser.Diagnostic{{}}\n", false, 0},
		{"the zero value", "package x\n\nvar d = parser.Diagnostic{}\n", false, 0},
		{"a wrapped construction", "package x\n\nvar d = parser.Bounded(parser.Diagnostic{Message: msg})\n", false, 0},
		{"a bare pointer construction", "package x\n\nvar d = &parser.Diagnostic{Message: msg}\n", true, 0},
		{"a bare construction", "package x\n\nvar d = parser.Diagnostic{Message: msg}\n", true, 0},
		{"an aliased import", "package x\n\nimport (\n\tp \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n)\n", true, 4},
		{"an aliased import below empty lines", "package x\n\n\nimport (\n\tp \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n)\n", true, 5},
		{"a single-line plain import is no alias", "package x\n\nimport \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n\nvar d = parser.Bounded(parser.Diagnostic{Message: msg})\n", false, 0},
		{"a single-line aliased import", "package x\n\nimport p \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n", true, 3},
		{"a blank import composes nothing", "package x\n\nimport (\n\t_ \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n)\n", false, 0},
		{"a dot import hides the marker", "package x\n\nimport (\n\t. \"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n)\n", true, 4},
		{"the plain import", "package x\n\nimport (\n\t\"github.com/SocialGouv/iterion/pkg/dsl/parser\"\n)\n\nvar d = parser.Bounded(parser.Diagnostic{Message: msg})\n", false, 0},
	} {
		got := unboundedConstructions(c.src)
		if isOffender := len(got) > 0; isOffender != c.want {
			t.Errorf("%s: offender = %v, want %v (lines %v)", c.name, isOffender, c.want, got)
		}
		if c.want && c.line > 0 && (len(got) != 1 || got[0] != c.line) {
			t.Errorf("%s: reported at %v, want the one offense at line %d", c.name, got, c.line)
		}
	}
}

// unboundedConstructions is the canary's reading of one Go source: the
// 1-based lines where a parser.Diagnostic is composed around the bound —
// a composite literal with fields not written parser.Bounded(...), an
// elided composite element of a collection literal (`{{Code: …}}`, whose
// fields no wrapper can reach), or an import of the parser package under
// another name (the scan reads `parser.Diagnostic{`; an alias or a dot
// import composes it invisibly).
func unboundedConstructions(text string) []int {
	const marker = "parser.Diagnostic{"
	var lines []int
	for _, m := range parserAliasImportRe.FindAllStringSubmatchIndex(text, -1) {
		switch text[m[2]:m[3]] {
		case "import", "_":
			// The keyword of a single-line plain import is no name, and a
			// blank import composes nothing.
			continue
		}
		lines = append(lines, 1+strings.Count(text[:m[0]], "\n"))
	}
	for at := 0; ; {
		i := strings.Index(text[at:], marker)
		if i < 0 {
			break
		}
		i += at
		at = i + len(marker)
		before := strings.TrimRight(text[:i], " \t\n")
		rest := strings.TrimLeft(text[at:], " \t\n")
		switch {
		case strings.HasSuffix(before, "]"):
			// A collection of diagnostics ([]parser.Diagnostic{…}, a map's
			// element type): the type is written once for the collection,
			// so an element written as a bare composite carries its fields
			// where no wrapper can reach them. Every element is read: an
			// expression (a door's own diagnostic — docDiag(…), a bounded
			// refusal) passes; a composite element is a construction and
			// carries its type in full, wrapped — or holds no field at all.
			elems, offs := literalElements(text, at-1)
			for k, el := range elems {
				if elidedCompositeWithFields(el) {
					lines = append(lines, 1+strings.Count(text[:offs[k]], "\n"))
				}
			}
		case strings.HasPrefix(rest, "}"):
			// The zero value: no message to bound.
		default:
			b := strings.TrimSuffix(before, "&")
			b = strings.TrimRight(b, " \t\n")
			if !strings.HasSuffix(b, "parser.Bounded(") {
				lines = append(lines, 1+strings.Count(text[:i], "\n"))
			}
		}
	}
	return lines
}

// parserAliasImportRe is an import of the parser package under another name
// (an alias, a dot, a blank): constructions through it name no
// `parser.Diagnostic{` the scan reads. Group 1 is the name — the keyword
// `import` of a single-line plain import is NOT one (excluded by the
// caller), nor is `_`, which can compose nothing. The leading whitespace
// is horizontal only ([ \t]): \s would cross the empty lines above and the
// offense would be reported on the wrong line.
var parserAliasImportRe = regexp.MustCompile(`(?m)^[ \t]*(?:import[ \t]+)?([\w.]+)[ \t]+"github\.com/SocialGouv/iterion/pkg/dsl/parser"`)

// literalElements splits the composite literal whose opening brace is at
// open into its top-level elements, each with its byte offset in src.
// Strings, runes, raw strings and both comment forms are skipped, so a
// brace or a comma inside one is never read as syntax.
func literalElements(src string, open int) (elems []string, offs []int) {
	depth := 0
	start := open + 1
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '"', '\'', '`':
			i = skipLiteral(src, i)
		case '/':
			i = skipComment(src, i)
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth == 0 {
				return append(elems, src[start:i]), append(offs, start)
			}
		case ',':
			if depth == 1 {
				elems, offs = append(elems, src[start:i]), append(offs, start)
				start = i + 1
			}
		}
	}
	return elems, offs
}

// elidedCompositeWithFields reports whether elem composes a value with the
// type elided: a brace at the element's top level (outside parens and
// brackets — the wrapped form parser.Bounded(parser.Diagnostic{…}) opens
// none there) whose literal holds a field. The empty element `{}` is the
// zero value.
func elidedCompositeWithFields(elem string) bool {
	depth := 0
	for i := 0; i < len(elem); i++ {
		switch elem[i] {
		case '"', '\'', '`':
			i = skipLiteral(elem, i)
		case '/':
			i = skipComment(elem, i)
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case '{':
			if depth > 0 {
				continue
			}
			// The literal's content, to its matching close: empty is the
			// zero value.
			bdepth := 0
			j := i
			for ; j < len(elem); j++ {
				switch elem[j] {
				case '"', '\'', '`':
					j = skipLiteral(elem, j)
				case '/':
					j = skipComment(elem, j)
				case '{':
					bdepth++
				case '}':
					bdepth--
					if bdepth == 0 {
						return strings.TrimSpace(elem[i+1:j]) != ""
					}
				}
			}
			return false
		}
	}
	return false
}

// skipLiteral returns the index of the closing quote of the string, raw
// string or rune literal opening at i (the last byte of src for one never
// closed).
func skipLiteral(src string, i int) int {
	q := src[i]
	for i++; i < len(src); i++ {
		if q != '`' && src[i] == '\\' {
			i++
			continue
		}
		if src[i] == q {
			return i
		}
	}
	return len(src) - 1
}

// skipComment returns the index of the last byte of the comment opening at
// i, or i itself when src[i] opens none (a division).
func skipComment(src string, i int) int {
	if i+1 >= len(src) {
		return i
	}
	switch src[i+1] {
	case '/':
		for i < len(src) && src[i] != '\n' {
			i++
		}
		return i
	case '*':
		i += 2
		for i+1 < len(src) && (src[i] != '*' || src[i+1] != '/') {
			i++
		}
		return i + 1
	}
	return i
}
