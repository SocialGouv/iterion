package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fragments an `import` line names are read ONCE for the two sides E056
// compares, as the bundle's prompts are: the document's unit reads them for
// the verdict and the .bot's load is given those very bytes. A read per side
// leaves a window — the document's unit is loaded, then the comparison runs —
// and a `lib/*.bot` saved inside it is a difference between two files nobody
// touched; the bots of this repo keep their prompts in one (`lib/prompts.bot`),
// so that save is an ordinary edit. This writes the save where it falls, so
// the witness needs no writer racing the verdict, and the second case holds
// the other half of the guarantee: a real difference is still named while the
// fragment both sides import moves.
func TestE056ReadsTheFragmentsTheTwoSidesShareOnce(t *testing.T) {
	const importing = "dsl: 2\nimports:\n  - lib/a.bot\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	// pair is what an author holds after one `fmt --to bot`: the document,
	// the fragment it imports and the .bot of the program the FIRST text
	// describes — the text that conversion writes, which opening the
	// document has produced already (doc.text).
	pair := func(t *testing.T, document string) (doc, fragment string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		fragment = filepath.Join(dir, "lib", "a.bot")
		if err := os.WriteFile(fragment, []byte("prompt from_a:\n  A.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		doc = filepath.Join(dir, "hello.bot.yaml")
		if err := os.WriteFile(doc, []byte(importing), 0o644); err != nil {
			t.Fatal(err)
		}
		written, _, err := openAuthorDocument(doc)
		if err != nil {
			t.Fatalf("openAuthorDocument: %v", err)
		}
		if err := os.WriteFile(written.botPath, []byte(written.text), 0o644); err != nil {
			t.Fatal(err)
		}
		if document != importing {
			if err := os.WriteFile(doc, []byte(document), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return doc, fragment
	}
	savesTheFragment := func(fragment string) func() {
		return func() {
			if err := os.WriteFile(fragment, []byte("prompt from_a:\n  B.\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a fragment both sides import, saved while the verdict is taken", func(t *testing.T) {
		doc, fragment := pair(t, importing)
		// The premise: nothing moving, the two files are one program.
		if said := twinVerdict(t, doc, func() {}); said != "" {
			t.Fatalf("E056 on the pair `fmt --to bot` wrote: %s", said)
		}
		if said := twinVerdict(t, doc, savesTheFragment(fragment)); said != "" {
			t.Fatalf("E056 over a fragment saved while the verdict is taken, on two files nobody touched: %s", said)
		}
	})

	t.Run("a real difference, while that fragment moves", func(t *testing.T) {
		ahead := strings.Replace(importing, "  ask: Say hello.", "  ask: Say hello.\n  check: Judge it.", 1)
		ahead = strings.Replace(ahead, "    system: ask\n", "    system: ask\n  - agent: judge\n    model: m\n    system: check\n", 1)
		ahead = strings.Replace(ahead, "    - hello -> done", "    - hello -> judge\n    - judge -> done", 1)
		doc, fragment := pair(t, ahead)
		said := twinVerdict(t, doc, savesTheFragment(fragment))
		if !strings.Contains(said, `node "judge" is missing`) {
			t.Fatalf("E056 does not name the node the .bot has not, while the fragment both sides import moves: %q", said)
		}
	})
}

// Where the loader reads one of the two sides from a root ABOVE its own
// directory — a file declaring no `workflow:` below a lib/ is read from the
// root that lib/ belongs to, under its lib/ name — the fragments the two
// sides share are read once all the same: a fragment is staged under the
// name THIS load asks for, which the loader rebases as it rebases the
// main's own name. These are the frames where the two loads do not start
// from the same root, each asserted to BE that frame (the loader re-rooted
// the document) and to be live (a program difference in the .bot alone is
// named while the shared fragment moves), so the silence is measured.
func TestE056ReadsTheSharedFragmentOnceWhereverTheLoaderRootsTheSides(t *testing.T) {
	const workflowFragment = "workflow hello:\n  entry: hello\n  hello -> done\n"
	frames := []struct {
		name string
		// files are written beside the .bot first, keyed by slash path from
		// the tree's root; bot is the .bot's own key, shared the fragment
		// both sides import and a save moves.
		files        map[string]string
		bot          string
		shared       string
		anotherModel string
	}{
		{
			// The document the loader re-roots: under lib/, declaring no
			// workflow of its own, reaching one through a fragment.
			name: "a document under lib/ that reaches its workflow through a fragment",
			files: map[string]string{
				"lib/y.bot": workflowFragment,
				"lib/z.bot": "prompt from_z:\n  A.\n",
				"lib/x.bot": "import \"y.bot\"\nimport \"z.bot\"\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: m\n  system: ask\n",
			},
			bot:          "lib/x.bot",
			shared:       "lib/z.bot",
			anotherModel: "import \"y.bot\"\nimport \"z.bot\"\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: n\n  system: ask\n",
		},
		{
			// The same, one directory deeper: the root that lib/ belongs to
			// is two directories above the one the .bot's load starts from,
			// so the name a fragment is staged under is cut by two
			// segments and rebased by the same two.
			name: "a document under lib/sub/, read from the root its lib/ belongs to",
			files: map[string]string{
				"lib/sub/y.bot": workflowFragment,
				"lib/sub/z.bot": "prompt from_z:\n  A.\n",
				"lib/sub/x.bot": "import \"y.bot\"\nimport \"z.bot\"\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: m\n  system: ask\n",
			},
			bot:          "lib/sub/x.bot",
			shared:       "lib/sub/z.bot",
			anotherModel: "import \"y.bot\"\nimport \"z.bot\"\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: n\n  system: ask\n",
		},
	}
	for _, f := range frames {
		t.Run(f.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, text := range f.files {
				at := filepath.Join(dir, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(at, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bot := filepath.Join(dir, filepath.FromSlash(f.bot))
			shared := filepath.Join(dir, filepath.FromSlash(f.shared))
			// The pair one `fmt --to yaml` then one `fmt --to bot` leaves:
			// the document that conversion writes, and the .bot of the
			// program the document describes.
			doc := bot + ".yaml"
			if err := os.WriteFile(doc, theDocumentFmtWrites(t, bot), 0o644); err != nil {
				t.Fatal(err)
			}
			opened, _, err := openAuthorDocument(doc)
			if err != nil {
				t.Fatalf("openAuthorDocument: %v", err)
			}
			if err := os.WriteFile(bot, []byte(opened.text), 0o644); err != nil {
				t.Fatal(err)
			}
			// The frame: the loader reads the document from a root above
			// its own directory, so the two loads do not start alike.
			opu, err := opened.loadUnit(opened.botPath)
			if err != nil {
				t.Fatalf("loadUnit: %v", err)
			}
			if main := opu.Main; main == filepath.Base(opened.botPath) {
				t.Fatalf("the fixture is not the frame it claims: the loader read the document as %q, its own name", main)
			}
			if said := twinVerdict(t, doc, func() {}); said != "" {
				t.Fatalf("E056 on the pair `fmt --to bot` wrote: %s", said)
			}
			saved := func() {
				if err := os.WriteFile(shared, []byte("prompt from_z:\n  B.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if said := twinVerdict(t, doc, saved); said != "" {
				t.Fatalf("E056 over a fragment saved while the verdict is taken, on two files nobody touched: %s", said)
			}
			if err := os.WriteFile(bot, []byte(f.anotherModel), 0o644); err != nil {
				t.Fatal(err)
			}
			if said := twinVerdict(t, doc, saved); !strings.Contains(said, "node(s) differ: hello") {
				t.Fatalf("E056 does not name the node the .bot describes otherwise, in this frame: %q", said)
			}
		})
	}
}

// twinVerdict runs validate's own two steps in validate's own order — the
// document's unit, then the comparison — with save called between them: the
// window a second read of a shared file falls into.
func twinVerdict(t *testing.T, doc string, save func()) string {
	t.Helper()
	d, b, err := openAuthorDocument(doc)
	if err != nil {
		t.Fatalf("openAuthorDocument: %v", err)
	}
	u, err := d.loadUnit(d.botPath)
	if err != nil {
		t.Fatalf("loadUnit: %v", err)
	}
	save()
	said, err := d.twinAnotherProgram(u, b)
	if err != nil {
		t.Fatalf("twinAnotherProgram: %v", err)
	}
	if said == nil {
		return ""
	}
	return said.Message
}

// theDocumentFmtWrites is `fmt --to yaml <bot>`'s own text for a .bot on disk.
func theDocumentFmtWrites(t *testing.T, bot string) []byte {
	t.Helper()
	src, err := os.ReadFile(bot)
	if err != nil {
		t.Fatal(err)
	}
	_, out, _, err := convertTwin("yaml", bot, src)
	if err != nil {
		t.Fatalf("fmt --to yaml %s: %v", bot, err)
	}
	return out
}

// The .bot E056 answers for is the text that was there when the document was
// opened: it is read once, at that opening (twinAt), and the comparison is
// taken off those bytes. A .bot saved while the verdict is taken — an editor
// writing the program the document already describes, or another one — is
// not the file the document was read beside, and the warning would answer
// for a text no step of this validate ever read. Both directions are held
// here: the save cannot raise the warning, and it cannot silence it.
func TestE056JudgesTheBotTextReadAtTheDocumentsOpening(t *testing.T) {
	const document = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	// pair is the document, the .bot of the program it describes — the text
	// `fmt --to bot` writes, which opening the document produces already —
	// and another program's .bot text: one node of it reads otherwise.
	pair := func(t *testing.T) (doc string, bot string, itsProgram, anotherProgram string) {
		t.Helper()
		dir := t.TempDir()
		doc = filepath.Join(dir, "hello.bot.yaml")
		if err := os.WriteFile(doc, []byte(document), 0o644); err != nil {
			t.Fatal(err)
		}
		opened, _, err := openAuthorDocument(doc)
		if err != nil {
			t.Fatalf("openAuthorDocument: %v", err)
		}
		itsProgram = opened.text
		anotherProgram = strings.Replace(itsProgram, "model: \"m\"", "model: \"n\"", 1)
		if anotherProgram == itsProgram {
			t.Fatalf("the fixture is not the frame it claims: the .bot text holds no node to change:\n%s", itsProgram)
		}
		return doc, opened.botPath, itsProgram, anotherProgram
	}
	saves := func(t *testing.T, bot, text string) func() {
		return func() {
			if err := os.WriteFile(bot, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("another program saved over the .bot while the verdict is taken", func(t *testing.T) {
		doc, bot, itsProgram, anotherProgram := pair(t)
		if err := os.WriteFile(bot, []byte(itsProgram), 0o644); err != nil {
			t.Fatal(err)
		}
		// The premise: nothing moving, the two files are one program.
		if said := twinVerdict(t, doc, func() {}); said != "" {
			t.Fatalf("E056 on the pair `fmt --to bot` wrote: %s", said)
		}
		if said := twinVerdict(t, doc, saves(t, bot, anotherProgram)); said != "" {
			t.Fatalf("E056 answers for a .bot written after the document was opened: %s", said)
		}
	})

	t.Run("the document's own program saved over the .bot while the verdict is taken", func(t *testing.T) {
		doc, bot, itsProgram, anotherProgram := pair(t)
		if err := os.WriteFile(bot, []byte(anotherProgram), 0o644); err != nil {
			t.Fatal(err)
		}
		// The premise: the .bot that was there describes another program.
		if said := twinVerdict(t, doc, func() {}); !strings.Contains(said, "node(s) differ: hello") {
			t.Fatalf("E056 does not name the node the .bot beside the document describes otherwise: %q", said)
		}
		if said := twinVerdict(t, doc, saves(t, bot, itsProgram)); !strings.Contains(said, "node(s) differ: hello") {
			t.Fatalf("a .bot written after the document was opened silences E056 over the text that was there: %q", said)
		}
	})
}
