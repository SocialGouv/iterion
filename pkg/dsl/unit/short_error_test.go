package unit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The bot of the short-load witnesses: a main importing lib/big.bot, then
// lib/a.bot; the budget runs out in lib/big.bot, before lib/a.bot is read.
const (
	shortErrMain = "import \"lib/big.bot\"\nimport \"lib/a.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	shortErrA    = "agent worker:\n  model: \"m\"\n"
)

func shortErrFiles(t *testing.T) (map[string]string, int) {
	t.Helper()
	big := "prompt big:\n" + strings.Repeat("  x\n", 200)
	budget := spentOf(t, "main.bot", shortErrMain) + spentOf(t, "lib/big.bot", big) - 20
	return map[string]string{"main.bot": shortErrMain, "lib/big.bot": big, "lib/a.bot": shortErrA}, budget
}

// Every loader answers a load the read budget cuts short as no unit: nil
// and a *ShortError, which unwraps to the refusal (*parser.ReadRefusal) —
// positioned in the file the budget ran out in, its remedy the variable
// that moves it. What the load read is had only by name (Salvage): Whole
// false, Refusal the cause, the files past the cut in no list. A whole load
// is its unit, and Salvage hands it over unchanged.
func TestALoadTheBudgetCutShortIsNoUnit(t *testing.T) {
	files, budget := shortErrFiles(t)
	dir := t.TempDir()
	for rel, src := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mainPath := filepath.Join(dir, "main.bot")
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	loaders := map[string]func() (*Unit, error){
		"LoadMap": func() (*Unit, error) { return LoadMap(files, "main.bot") },
		"LoadDir": func() (*Unit, error) { return LoadDir(mainPath) },
		"LoadDirStaged": func() (*Unit, error) {
			return LoadDirStaged(mainPath, map[string][]byte{"lib/a.bot": []byte(shortErrA)})
		},
		"LoadDirWithMain": func() (*Unit, error) { return LoadDirWithMain(mainPath, mainPath, []byte(shortErrMain)) },
		"Load (files map)": func() (*Unit, error) {
			return Load(func(rel string) ([]byte, error) { return []byte(files[rel]), nil }, "main.bot", func(rel string) string { return rel })
		},
	}
	for name, load := range loaders {
		u, err := load()
		var short *ShortError
		var refusal *parser.ReadRefusal
		if u != nil || !errors.As(err, &short) || !errors.As(err, &refusal) {
			t.Errorf("%s of a bot past the budget = %v, %v — want no unit and a *ShortError unwrapping to the refusal", name, u, err)
			continue
		}
		if refusal.Bound != parser.BoundBudget || !strings.HasSuffix(refusal.Diagnostic.File, "lib/big.bot") || !strings.Contains(err.Error(), "past the read budget of "+strconv.Itoa(budget)) || !strings.Contains(err.Error(), parser.ReadBudgetEnv) {
			t.Errorf("%s: refusal %+v, error %q — want the budget's, in lib/big.bot, with its variable", name, refusal, err)
		}
		salvage := salvageUnit(load())
		var rels []string
		for _, f := range salvage.Files {
			rels = append(rels, f.Rel)
		}
		if salvage.Whole() || !reflect.DeepEqual(rels, []string{"main.bot", "lib/big.bot"}) {
			t.Errorf("%s: the salvage is whole %v with files %v, want what the load read, cut short", name, salvage.Whole(), rels)
		}
		if d, ok := salvage.Refusal(); !ok || d != refusal.Diagnostic {
			t.Errorf("%s: the salvage's refusal %v, want the error's", name, d)
		}
	}
	t.Setenv(parser.ReadBudgetEnv, "0")
	u, err := LoadMap(files, "main.bot")
	if err != nil || u == nil || !u.Whole() || len(u.Files) != 3 || salvageUnit(LoadMap(files, "main.bot")).Digest != u.Digest {
		t.Errorf("a whole load = %v, %v — want its unit, which Salvage hands over unchanged", u, err)
	}
}

// MembersDir reads a bot's membership off its import lines — the files its
// whole load reads, the ones past the budget's cut too — so a caller that
// cannot read the bot whole can still tell whether it reaches a file; an
// import below a declaration (E044) is followed by no load and listed by
// none.
func TestMembersDirListsABotPastTheBudget(t *testing.T) {
	files, budget := shortErrFiles(t)
	files["lib/late.bot"] = "prompt late:\n  Late.\n"
	files["lib/a.bot"] = shortErrA + "\nimport \"lib/late.bot\"\n"
	dir := t.TempDir()
	for rel, src := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mainPath := filepath.Join(dir, "main.bot")
	t.Setenv(parser.ReadBudgetEnv, "0")
	whole, err := LoadDir(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, f := range whole.Files {
		want = append(want, f.Rel)
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	if _, err := LoadDir(mainPath); err == nil {
		t.Fatal("the fixture is not past the budget")
	}
	got, err := MembersDir(mainPath, nil)
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(want, []string{"main.bot", "lib/big.bot", "lib/a.bot"}) {
		t.Errorf("the membership of a bot past the budget = %v, %v — want the whole load's, %v, the late import below a declaration followed by none", got, err, want)
	}
}

// MembersDir reads each file's head on lines; a raw string — the one token
// that spans lines — opened on a head line runs past the line the cut stops
// at. A head that would end inside it is read whole, so the membership is
// the whole load's: the import below the literal's close is followed, never
// dropped (a false non-membership is the unsafe direction).
func TestMembersDirReadsAHeadARawStringCrossesWhole(t *testing.T) {
	t.Setenv(parser.ReadBudgetEnv, "0")
	for name, main := range map[string]string{
		"a raw string over a line":    "dsl: `\nx\n`\nimport \"lib/a.bot\"\n\nworkflow w:\n  entry: x\n",
		"an import inside the string": "dsl: `\nimport \"lib/b.bot\"\n`\nimport \"lib/a.bot\"\n\nworkflow w:\n  entry: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for rel, src := range map[string]string{
				"main.bot":  main,
				"lib/a.bot": "prompt p_a:\n  Text.\n",
				"lib/b.bot": "prompt p_b:\n  Text.\n",
			} {
				p := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			mainPath := filepath.Join(root, "main.bot")
			u, err := LoadDir(mainPath)
			if err != nil {
				t.Fatal(err)
			}
			var whole []string
			for _, f := range u.Files {
				whole = append(whole, f.Rel)
			}
			members, err := MembersDir(mainPath, nil)
			if err != nil || !slices.Equal(whole, members) {
				t.Fatalf("MembersDir = %v (%v), want the whole load's %v", members, err, whole)
			}
		})
	}
}

// A fragment that is no text the reader reads (not UTF-8, E006) is a file
// the load did not read, as one past a bound is: every loader answers the
// bot short, its refusal the E006 diagnostic with its remedy, and the view
// says it is not whole — never a whole unit holding no declaration of it.
func TestAFragmentThatIsNotUTF8LeavesTheLoadShort(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/a.bot\"\nagent m:\n  model: \"m\"\n",
		"lib/a.bot": "agent a:\n  model: \"\xff\"\nagent keepme:\n  model: \"m\"\n",
	}
	u, err := LoadMap(files, "main.bot")
	var short *ShortError
	if u != nil || !errors.As(err, &short) {
		t.Fatalf("a bot with a non-UTF-8 fragment = %v, %v — want no unit and a *ShortError", u, err)
	}
	refusal := short.ReadRefusal()
	if refusal.Diagnostic.Code != parser.DiagNotUTF8 || refusal.Diagnostic.File != "lib/a.bot" || refusal.Diagnostic.Hint != parser.HintFor(parser.DiagNotUTF8) {
		t.Fatalf("the refusal = %+v, want E006 in lib/a.bot with its remedy", refusal.Diagnostic)
	}
	if view := Salvage(LoadMap(files, "main.bot")); view.Whole() {
		t.Fatal("the view of the load says it read the bot whole")
	}
}

// A fragment that is there and is not read — its permissions deny it, or
// the root's confinement refuses it (a symlink) — leaves a full load short,
// its E046 the refusal, the file out of ReadWhole; the files read around it
// are in ReadWhole. A fragment that is not there is an error, not a short
// load. A budget's cut leaves the files before it in ReadWhole, the file it
// ran out in and every file past it out.
func TestAFragmentThatIsThereAndNotReadLeavesTheLoadShort(t *testing.T) {
	main := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	write := func(t *testing.T, files map[string]string) string {
		t.Helper()
		dir := t.TempDir()
		for rel, src := range files {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	files := map[string]string{"main.bot": main, "lib/a.bot": "prompt pa:\n  a\n", "lib/b.bot": shortErrA}
	for _, how := range []string{"permission", "symlink"} {
		t.Run(how, func(t *testing.T) {
			dir := write(t, files)
			a := filepath.Join(dir, "lib", "a.bot")
			switch how {
			case "permission":
				if err := os.Chmod(a, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(a, 0o644) })
				if _, err := os.ReadFile(a); err == nil {
					t.Skip("this user reads a mode-0 file")
				}
			case "symlink":
				if err := os.Rename(a, filepath.Join(dir, "elsewhere.bot")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "elsewhere.bot"), a); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			u, err := LoadDir(filepath.Join(dir, "main.bot"))
			var short *ShortError
			if u != nil || !errors.As(err, &short) {
				t.Fatalf("LoadDir with lib/a.bot there and not read (%s) = %v, %v — want no unit and a *ShortError", how, u != nil, err)
			}
			if d := short.ReadRefusal().Diagnostic; d.Code != parser.DiagImportUnreadable || !strings.Contains(d.Message, "lib/a.bot") {
				t.Fatalf("the refusal is %+v, want lib/a.bot's E046", d)
			}
			if got := short.ReadWhole(); !reflect.DeepEqual(got, []string{"main.bot", "lib/b.bot"}) {
				t.Fatalf("ReadWhole = %v, want the files read around lib/a.bot", got)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		dir := write(t, map[string]string{"main.bot": main, "lib/b.bot": shortErrA})
		u, err := LoadDir(filepath.Join(dir, "main.bot"))
		if err != nil || !u.Whole() || !u.HasErrors() {
			t.Fatalf("LoadDir with lib/a.bot absent = %v (whole %v, errors %v), want a unit, whole, its E046 an error", err, u != nil && u.Whole(), u != nil && u.HasErrors())
		}
	})
	t.Run("budget", func(t *testing.T) {
		cut, budget := shortErrFiles(t)
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
		_, err := LoadMap(cut, "main.bot")
		var short *ShortError
		if !errors.As(err, &short) {
			t.Fatalf("precondition: a load past the budget, got %v", err)
		}
		if got := short.ReadWhole(); !reflect.DeepEqual(got, []string{"main.bot"}) {
			t.Fatalf("ReadWhole = %v, want main.bot alone: lib/big.bot is where the budget ran out, lib/a.bot past it", got)
		}
	})
}

// The display of a load cut short is marked a salvage, and the decode
// refuses it wherever it runs (ast.ErrSalvage): handed on as bytes, it
// becomes no program. The display of a load read whole is the bot: it
// decodes.
func TestTheDisplayOfAShortLoadDecodesIntoNoProgram(t *testing.T) {
	files, budget := shortErrFiles(t)
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	doc, err := Salvage(LoadMap(files, "main.bot")).MarshalDocument("")
	if err != nil {
		t.Fatal(err)
	}
	if f, err := ast.UnmarshalFile(doc); f != nil || !errors.Is(err, ast.ErrSalvage) {
		t.Fatalf("decoding the display of a load cut short = %v, %v — want ast.ErrSalvage and no program", f != nil, err)
	}
	// EVERY encoding/json decode of a File meets the same refusal: the
	// type's own UnmarshalJSON routes through UnmarshalFile, so a helper
	// decoding the display by the language's decoder — not by the one door
	// — decodes into no program either.
	var throughDecoder ast.File
	if err := json.Unmarshal(doc, &throughDecoder); !errors.Is(err, ast.ErrSalvage) {
		t.Fatalf("json.Unmarshal of the display into an ast.File = %v — want ast.ErrSalvage (File.UnmarshalJSON routes through UnmarshalFile)", err)
	}
	var throughPointer *ast.File
	if err := json.Unmarshal(doc, &throughPointer); !errors.Is(err, ast.ErrSalvage) {
		t.Fatalf("json.Unmarshal of the display into *ast.File = %v — want ast.ErrSalvage", err)
	}
	t.Setenv(parser.ReadBudgetEnv, "")
	doc, err = Salvage(LoadMap(files, "main.bot")).MarshalDocument("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ast.UnmarshalFile(doc); err != nil {
		t.Fatalf("decoding the display of a load read whole = %v, want its program", err)
	}
}
