package canon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// shippedBots are every `.bot` this repository ships — the bots the
// catalogue serves and the examples the docs point at.
func shippedBots(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{"bots", "examples"} {
		root := filepath.Join("..", "..", "..", dir)
		if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The same rule the CLI's collector walks by: two
				// spellings of "which files are in the tree" put this
				// guard and `iterion fmt --check` in a disagreement no
				// baseline can settle.
				if p != root && workflowfile.SkipWalkDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if workflowfile.IsWorkflowFile(p) {
				out = append(out, p)
			}
			return nil
		}); err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if len(out) < 50 {
		t.Fatalf("only %d shipped .bot found — the walk has gone blind", len(out))
	}
	return out
}

// canonical is a shipped file's canonical form, or nil when canon refuses
// it by name (a refusal is an outcome the pass expects — #1612 — and any
// other error is not).
func canonical(t *testing.T, path string) (src, out []byte, refused bool) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err = Bytes(path, src)
	switch {
	case err == nil:
		return src, out, false
	case errors.Is(err, ErrRefused):
		return src, nil, true
	default:
		t.Fatalf("%s: %v", path, err)
		return nil, nil, false
	}
}

// TestTheCanonicalFormOfEveryShippedBotIsTheSameProgram is what a
// formatting pass over the catalogue rests on (#1301): for every `.bot`
// this repository ships, the canonical form compiles to the SAME program,
// with the same diagnostics, as the file it came from.
//
// ir.SameProgram alone is not the oracle. A fragment of a bot in several
// files compiles to NO workflow on its own — its imports are unresolved —
// and SameProgram then compares nothing but a list of diagnostic codes. For
// those the span-free document mirror is compared instead, and BOTH counts
// are asserted, so a corpus that silently stopped comparing programs fails
// here rather than reading as a success.
//
// The witness: drop a property from any writer (`model:` in writeAgents,
// an edge in writeWorkflows) and this reddens.
func TestTheCanonicalFormOfEveryShippedBotIsTheSameProgram(t *testing.T) {
	files := shippedBots(t)
	programs, documents, refusals := 0, 0, 0
	for _, path := range files {
		src, out, refused := canonical(t, path)
		if refused {
			refusals++
			continue
		}
		before := parser.Parse(path, string(src))
		after := parser.Parse(path, string(out))
		for _, d := range after.Diagnostics {
			if d.Severity == parser.SeverityError {
				t.Errorf("%s: the canonical form does not parse: %s", path, d.Error())
			}
		}
		if got, want := after.File.EffectiveProfile(), before.File.EffectiveProfile(); got != want {
			t.Errorf("%s: the canonical form reads as profile %d, the file is profile %d", path, got, want)
		}
		ca, cb := ir.Compile(before.File), ir.Compile(after.File)
		if ca.Workflow != nil || cb.Workflow != nil {
			programs++
			if why := ir.SameProgram(ca, cb); why != "" {
				t.Errorf("%s: the canonical form is not the same program: %s", path, why)
			}
			continue
		}
		// A fragment: no program to compare, so compare the document.
		documents++
		x, err := ast.MarshalFileWithoutComments(before.File)
		if err != nil {
			t.Fatal(err)
		}
		y, err := ast.MarshalFileWithoutComments(after.File)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Errorf("%s: the canonical form is not the same document", path)
		}
	}
	if programs+documents+refusals != len(files) {
		t.Fatalf("%d files, %d compared as programs + %d as documents + %d refused", len(files), programs, documents, refusals)
	}
	// A floor under the measured 35, not a target: what it catches is a
	// corpus that stopped comparing programs — every file refused, or a
	// walk that found nothing — reading as a success.
	if programs < 30 {
		t.Errorf("only %d of %d files compared a compiled program — the corpus has stopped exercising the oracle", programs, len(files))
	}
	t.Logf("%d shipped .bot: %d compared as programs, %d as documents, %d refused by name", len(files), programs, documents, refusals)
}

// TestTheCanonicalFormOfEveryShippedBotIsAFixedPoint: formatting a file
// twice is formatting it once. Without it a `fmt --check` gate can never be
// green on a tree a developer has just formatted — the check would report
// the writer's own output as needing the writer.
func TestTheCanonicalFormOfEveryShippedBotIsAFixedPoint(t *testing.T) {
	checked := 0
	for _, path := range shippedBots(t) {
		_, once, refused := canonical(t, path)
		if refused {
			continue
		}
		twice, err := Bytes(path, once)
		if err != nil {
			t.Errorf("%s: its own canonical form was refused: %v", path, err)
			continue
		}
		if string(twice) != string(once) {
			t.Errorf("%s: the canonical form is not a fixed point", path)
		}
		checked++
	}
	if checked < 40 {
		t.Errorf("only %d files were checked for the fixed point", checked)
	}
}

// TestEveryCommentOfEveryShippedBotSurvivesTheCanonicalForm: the pass of
// #1301 rewrites files whose comments are their documentation. Not one is
// lost, and none lands on another declaration — comments are compared by
// the ADDRESS they are written at (the declaration and the line they name),
// not by their text alone, so a writer that kept every text and scrambled
// every placement fails here.
func TestEveryCommentOfEveryShippedBotSurvivesTheCanonicalForm(t *testing.T) {
	total, checked := 0, 0
	for _, path := range shippedBots(t) {
		src, out, refused := canonical(t, path)
		if refused {
			continue
		}
		checked++
		before := commentAddresses(path, string(src))
		after := commentAddresses(path, string(out))
		total += len(before)
		for addr, n := range before {
			if after[addr] != n {
				t.Errorf("%s: the comment %q is not where it was written (%d before, %d after)", path, addr, n, after[addr])
				break
			}
		}
		for addr, n := range after {
			if before[addr] != n {
				t.Errorf("%s: the canonical form invented a comment at %q (%d)", path, addr, n)
				break
			}
		}
	}
	if checked < 40 {
		t.Fatalf("only %d files were checked — the corpus has stopped exercising the guarantee", checked)
	}
	// A floor under the measured 1 541: the rest of the catalogue's 11 809
	// comments are in the files canon refuses (#1612), and this is what
	// tells a corpus that stopped comparing them from one that passes.
	if total < 1200 {
		t.Errorf("only %d comments were compared across %d files", total, checked)
	}
	t.Logf("%d comments across %d shipped files, each still where it was written", total, checked)
}

// commentAddresses counts the comments of a text by the address each is
// written at: the declaration that holds it, the line it names inside that
// declaration, how it sits there, and what it says.
func commentAddresses(name, text string) map[string]int {
	out := map[string]int{}
	_, comments := parser.ScanSource(name, text)
	for _, c := range comments {
		out[fmt.Sprintf("%s %s | %s | %d | %s", c.Decl.Kind, c.Decl.Name, c.Path, c.Place, c.Text)]++
	}
	return out
}

// TestTheRefusedBaselineMatchesWhatIsRefused: `.fmt-refused` is the list
// that lets `task fmt:check` be GREEN on a catalogue that is not yet wholly
// canonical (#1612). A list that drifts from what the rule actually refuses
// is worse than no list — it makes the check green on a file nobody looked
// at, or red on one nobody can fix. This is the ratchet's own guard, and it
// reddens in the unit suite rather than only in CI.
//
// The witness: delete a line from `.fmt-refused` and this names it.
func TestTheRefusedBaselineMatchesWhatIsRefused(t *testing.T) {
	const baselinePath = "../../../.fmt-refused"
	known, err := ReadBaseline(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) == 0 {
		t.Fatal(".fmt-refused lists nothing — a baseline that empties itself makes the check green on everything")
	}
	var refused []string
	for _, path := range shippedBots(t) {
		_, _, isRefused := canonical(t, path)
		if !isRefused {
			continue
		}
		// The paths the check compares are the ones the Taskfile's
		// invocation produces, from the repository root.
		refused = append(refused, NormalizeBaselinePath(strings.TrimPrefix(filepath.ToSlash(path), "../../../")))
	}
	newlyRefused, noLongerRefused := DiffBaseline(known, refused)
	for _, p := range newlyRefused {
		t.Errorf("%s is refused and is not in .fmt-refused — format it, or add it with the reason", p)
	}
	for _, p := range noLongerRefused {
		t.Errorf(".fmt-refused names %s, which nothing refuses any more — remove the line", p)
	}
	t.Logf("%d files refused, %d listed in .fmt-refused, agreed", len(refused), len(known))
}
