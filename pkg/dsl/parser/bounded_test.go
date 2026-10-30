package parser

import (
	"fmt"
	"os"
	"path/filepath"
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
// to bound) and the element type of a slice literal ([]parser.Diagnostic{…}
// — its elements are bounded diagnostics of a door) are not constructions.
func TestEveryConstructorPassesBounded(t *testing.T) {
	const marker = "parser.Diagnostic{"
	root := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			base := de.Name()
			if base == "vendor" || base == "third_party" || base == "node_modules" || strings.HasPrefix(base, ".") {
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
		text := string(src)
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
				// The element type of a []parser.Diagnostic{…} literal.
			case strings.HasPrefix(rest, "}"):
				// The zero value: no message to bound.
			default:
				b := strings.TrimSuffix(before, "&")
				b = strings.TrimRight(b, " \t\n")
				if !strings.HasSuffix(b, "parser.Bounded(") {
					line := 1 + strings.Count(text[:i], "\n")
					offenders = append(offenders, fmt.Sprintf("%s:%d", path, line))
				}
			}
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
