package canon

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A profile-2 file: the writer escapes every string, so a value written
// over several lines comes back as one (#1612). The `decoy` tool holds a
// value the AUTHOR already wrote on one line — the very form the writer
// emits — and it is declared first.
const foldDecoyFirst = "dsl: 2\n\ntool decoy:\n  description: \"a\"\n  command: \"echo a\\necho b\"\n\ntool real:\n  description: \"b\"\n  command: `line one\nline two`\n"

// TestOneValueTheAuthorWroteOnOneLineDoesNotShieldTheRest: the refusal asks
// about EVERY value the render puts on a line, not the first one it finds.
// Stopping at the first let one ordinary `"a\nb"` — which loses nothing,
// since its author wrote it that way — hide every real multi-line value
// behind it, and `iterion fmt` then rewrote bots/golden-master/main.bot
// into a single line of 473 101 characters.
func TestOneValueTheAuthorWroteOnOneLineDoesNotShieldTheRest(t *testing.T) {
	_, err := Bytes("decoy.bot", []byte(foldDecoyFirst))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("a file whose second value the writer folds was accepted: %v", err)
	}
	// The line named is the one the FILE writes the folded value on — the
	// `command:` of `real`, not of the decoy, and not a line of the render.
	want := lineOf(t, foldDecoyFirst, "line one\nline two")
	if !strings.Contains(err.Error(), "line "+itoa(want)) {
		t.Fatalf("the refusal names the wrong line (want %d): %v", want, err)
	}
}

// TestAValueItsAuthorWroteOnOneLineIsNotAFold: folding is a before/after
// property. A value already written as `"a\nb"` comes back as `"a\nb"` and
// nothing was folded — refusing it would make every profile-2 file holding
// an escaped newline unsavable from the studio.
func TestAValueItsAuthorWroteOnOneLineIsNotAFold(t *testing.T) {
	src := "dsl: 2\n\ntool t:\n  description: \"a\"\n  command: \"echo a\\necho b\"\n"
	if _, err := Bytes("flat.bot", []byte(src)); err != nil {
		t.Fatalf("a value its author wrote on one line was refused: %v", err)
	}
	if _, _, folds := Folds("flat.bot", src, src); folds {
		t.Fatal("a file compared against itself folds nothing")
	}
}

// TestFoldsComparesTwoTextsAndNeedsABefore: what a write route that takes
// file CONTENT asks. With no stored text there is no before, so there is
// nothing to claim — a NEW file is never a fold.
func TestFoldsComparesTwoTextsAndNeedsABefore(t *testing.T) {
	stored := "dsl: 2\n\ntool t:\n  description: \"a\"\n  command: `line one\nline two`\n"
	folded := "dsl: 2\n\ntool t:\n  description: \"a\"\n  command: \"line one\\nline two\"\n"

	line, size, folds := Folds("t.bot", stored, folded)
	if !folds {
		t.Fatal("a write that puts a stored multi-line value on one line was not seen")
	}
	if want := lineOf(t, stored, "line one\nline two"); line != want {
		t.Fatalf("named line %d, want the stored file's own %d", line, want)
	}
	if size <= 0 {
		t.Fatalf("size %d", size)
	}
	if _, _, folds := Folds("t.bot", "", folded); folds {
		t.Fatal("a file with no stored text folded something")
	}
	// The other direction is not a fold: a write that SPREADS a value over
	// lines is the author un-folding it, which is what #1612 wants.
	if _, _, folds := Folds("t.bot", folded, stored); folds {
		t.Fatal("un-folding a value was refused")
	}
}

// lineOf is the line src writes value on, over several lines.
func lineOf(t *testing.T, src, value string) int {
	t.Helper()
	for _, tok := range parser.NewLexer("x.bot", src).All() {
		if tok.Type == parser.TokenString && tok.Value == value && tok.EndLine > tok.Line {
			return tok.Line
		}
	}
	t.Fatalf("the fixture does not write %q over several lines", value)
	return 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestEditingTheFoldedValueItselfIsStillAFold: the refusal cannot be
// keyed on which values the two texts have in COMMON. Keyed that way it
// missed the one value being edited — which is the `command:` block the
// author came to change — so a save that touched it wrote the file folded
// and answered 200. The writer's form is the fact, not any value's
// spelling.
func TestEditingTheFoldedValueItselfIsStillAFold(t *testing.T) {
	stored := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two`\n"
	// The same file with the multi-line value EDITED and written folded —
	// what the writer produces once the author changes that script.
	edited := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: \"line one\\nline two\\nline three\"\n"
	if _, _, folds := Folds("t.bot", stored, edited); !folds {
		t.Fatal("editing the value the writer folds made the fold invisible")
	}
	// And through the document path, which is what the studio's saves use.
	pr := parser.Parse("t.bot", edited)
	if parseErrs(pr) != "" {
		t.Fatalf("fixture: %s", parseErrs(pr))
	}
	if _, err := Text("t.bot", pr.File, []byte(stored)); !errors.Is(err, ErrRefused) {
		t.Fatalf("Text accepted an edited-and-folded value: %v", err)
	}
	// The control, so the test cannot pass on a predicate that refuses
	// everything: the same edit keeping the value over its lines.
	kept := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two\nline three`\n"
	if _, _, folds := Folds("t.bot", stored, kept); folds {
		t.Fatal("an edit that keeps the value over its lines was called a fold")
	}
}

// TestTheLineNamedIsTheFilesOwn: the writer reorders declarations, so a
// line of the render points at unrelated text in the file. This fixture is
// built so the two differ — with declarations in an order the writer
// changes — and the refusal must name the FILE's.
func TestTheLineNamedIsTheFilesOwn(t *testing.T) {
	// The writer emits prompts, then schemas, then agents, then tools: a
	// tool declared FIRST here comes back last.
	src := "dsl: 2\n\ntool t:\n  description: \"probe\"\n  command: `line one\nline two`\n\nprompt p:\n  a\n  b\n  c\n  d\n  e\n\nagent a:\n  system: p\n\nworkflow w:\n  entry: t\n  t -> a\n  a -> done\n"
	pr := parser.Parse("t.bot", src)
	if parseErrs(pr) != "" {
		t.Fatalf("fixture: %s", parseErrs(pr))
	}
	_, err := Text("t.bot", pr.File, []byte(src))
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("not refused: %v", err)
	}
	fileLine := lineOf(t, src, "line one\nline two")
	renderLine := 0
	for _, tok := range parser.NewLexer("t.bot", mustText(t, pr)).All() {
		if tok.Type == parser.TokenString && strings.Contains(tok.Value, "\n") && tok.EndLine == tok.Line {
			renderLine = tok.Line
			break
		}
	}
	if renderLine == fileLine {
		t.Fatalf("the fixture does not separate the two lines (both %d): it cannot witness anything", fileLine)
	}
	if !strings.Contains(err.Error(), "line "+itoa(fileLine)) {
		t.Fatalf("the refusal names line %d (the render's) and not %d (the file's): %v", renderLine, fileLine, err)
	}
}

func parseErrs(pr *parser.ParseResult) string {
	var out []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			out = append(out, d.Error())
		}
	}
	return strings.Join(out, "; ")
}

func mustText(t *testing.T, pr *parser.ParseResult) string {
	t.Helper()
	return unparse.Unparse(pr.File)
}

// TestAFileGetsItsOwnBytesBack: the writer's all-or-nothing form is the
// whole story for a RENDER and false of AUTHOR text, which may hold an
// escaped `"a\nb"` on one line beside a value spread over its lines. Asked
// only "does this text fold something", the two CONTENT routes refused
// such a file its own bytes back — a 422 saying a write would fold a value
// it left exactly where it was, with no way to satisfy it.
func TestAFileGetsItsOwnBytesBack(t *testing.T) {
	// This repo's own fixture is that shape: a decoy the author wrote on
	// one line, and a value spread over two.
	if _, _, folds := Folds("decoy.bot", foldDecoyFirst, foldDecoyFirst); folds {
		t.Fatal("a byte-identical write was called a fold")
	}
	withComment := foldDecoyFirst + "\n## a note\n"
	if _, _, folds := Folds("decoy.bot", foldDecoyFirst, withComment); folds {
		t.Fatal("a comment-only edit was called a fold")
	}
	// And the same file with the spread value deliberately folded IS one:
	// without this the test would pass on a predicate that never refuses.
	folded := strings.Replace(foldDecoyFirst, "command: `line one\nline two`", "command: \"line one\\nline two\"", 1)
	if folded == foldDecoyFirst {
		t.Fatal("the fixture did not change: the control cannot bite")
	}
	if _, _, folds := Folds("decoy.bot", foldDecoyFirst, folded); !folds {
		t.Fatal("folding the spread value of a mixed file was not seen")
	}
}

// TestDroppingADeclarationIsNotAFold: a value that leaves because its
// declaration did is not a value the writer folded. Counting alone would
// refuse the author's delete; the writer's form is what separates them.
func TestDroppingADeclarationIsNotAFold(t *testing.T) {
	stored := "tool a:\n  command: `one\ntwo`\n\ntool b:\n  command: `three\nfour`\n\nworkflow w:\n  entry: a\n  a -> b\n  b -> done\n"
	pr := parser.Parse("p.bot", stored)
	if parseErrs(pr) != "" {
		t.Fatalf("fixture: %s", parseErrs(pr))
	}
	// The same file with one tool removed, still written over its lines.
	dropped := "tool a:\n  command: `one\ntwo`\n\nworkflow w:\n  entry: a\n  a -> done\n"
	if _, _, folds := Folds("p.bot", stored, dropped); folds {
		t.Fatal("removing a declaration was called a fold")
	}
}

// TestDeletingTheDeclarationThatHeldASpreadValueIsNotAFold: the count of
// spread values dropping is not a fold — the value left because its
// declaration did. Keyed on the count alone, a legitimate delete through
// the cloud write route was refused with advice its author had already
// followed: "push the file with that value over its lines" — which they
// had.
func TestDeletingTheDeclarationThatHeldASpreadValueIsNotAFold(t *testing.T) {
	// A file mixing both shapes: a value its author wrote over lines, and
	// one they wrote escaped on a single line.
	stored := "dsl: 2\n\ntool a:\n  description: \"x\"\n  command: `one\ntwo`\n\ntool b:\n  description: \"y\"\n  command: `three\nfour`\n\ntool c:\n  description: \"z\"\n  command: \"five\\nsix\"\n"
	// `tool b` removed; `tool a` is still written over its lines.
	dropped := "dsl: 2\n\ntool a:\n  description: \"x\"\n  command: `one\ntwo`\n\ntool c:\n  description: \"z\"\n  command: \"five\\nsix\"\n"
	if _, _, folds := Folds("m.bot", stored, dropped); folds {
		t.Fatal("deleting a declaration that held a spread value was called a fold")
	}
	// The control, so this cannot pass on a predicate that never refuses:
	// the same file with every spread value folded — what the writer
	// produces for it, all or nothing.
	rendered := "dsl: 2\n\ntool a:\n  description: \"x\"\n  command: \"one\\ntwo\"\n\ntool b:\n  description: \"y\"\n  command: \"three\\nfour\"\n\ntool c:\n  description: \"z\"\n  command: \"five\\nsix\"\n"
	line, _, folds := Folds("m.bot", stored, rendered)
	if !folds {
		t.Fatal("a render that folds every spread value was not seen")
	}
	// Every spread value folds, so the first is the right one to name.
	if want := lineOf(t, stored, "one\ntwo"); line != want {
		t.Fatalf("named line %d, want the first spread value's %d", line, want)
	}
}
