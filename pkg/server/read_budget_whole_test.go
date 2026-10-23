package server

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The bot of the read-budget witnesses: a main importing lib/a.bot then
// lib/b.bot, and the edit of lib/a.bot that adds a prompt.
const (
	shortMain    = "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	shortA       = "prompt pa:\n  Alpha.\n"
	shortAEdited = "prompt pa:\n  Alpha.\n\nprompt pa2:\n  Beta gamma delta epsilon zeta eta theta.\n"
)

// spentAlone is what src spends of the read budget read alone, with none.
func spentAlone(t *testing.T, name, src string) int {
	t.Helper()
	t.Setenv(parser.ReadBudgetEnv, "0")
	res := parsertest.Parse(name, src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("%s alone: %v", name, res.Diagnostics)
	}
	return res.Spent
}

// diskSums is the sha256 of every file under dir, by slash path.
func diskSums(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	sums := map[string][32]byte{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		sums[filepath.ToSlash(rel)] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// assertNothingWritten fails the test when a file under dir was written,
// created or removed since before was taken.
func assertNothingWritten(t *testing.T, dir string, before map[string][32]byte) {
	t.Helper()
	after := diskSums(t, dir)
	for rel, sum := range before {
		if got, ok := after[rel]; !ok {
			t.Errorf("%s was removed", rel)
		} else if got != sum {
			b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			t.Errorf("%s was written: it now holds %q", rel, b)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			// The authoring lock's control directory beside the bot's
			// files is the write's own bookkeeping (every save under the
			// lock creates it), not a write of the bot.
			if strings.HasPrefix(rel, ".iterion/") {
				continue
			}
			t.Errorf("%s was created", rel)
		}
	}
}

// withPrompt is doc with one more prompt, declared in file.
func withPrompt(t *testing.T, doc json.RawMessage, file, name, body string) json.RawMessage {
	t.Helper()
	return editDocument(t, doc, func(m map[string]any) {
		prompts, _ := m["prompts"].([]any)
		m["prompts"] = append(prompts, map[string]any{"file": file, "name": name, "body": body})
	})
}

// The ticket's sequence at the default budget: a bot whose files together
// sit three records under it — a 4 MB prompt in lib/big.bot, then lib/a.bot
// and lib/b.bot — opens whole, and an ordinary edit of lib/a.bot pushes it
// past the budget its files share. The edit reads alone, so the file's own
// parse cannot see it: the apply is refused, positioned where the bot ran
// out, with the operator's count for remedy — never answered with a
// document missing lib/b.bot that a save would write back empty — and the
// same edit made in the canvas is refused by the save, on disk and in a
// bundle alike. No file is written.
func TestAnEditPastTheSharedBudgetWritesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("parses a 4 MB prompt a dozen times; the logic is held under -race by TestABotTheBudgetCutShortOpensAsASalvage")
	}
	t.Setenv(parser.ReadBudgetEnv, "")
	main := "import \"lib/big.bot\"\nimport \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	sm, sa, sb := spentAlone(t, "main.bot", main), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	bigOf := func(n int) string { return "prompt big:\n" + strings.Repeat("  x\n", n) }
	// A prompt of n body lines spends n records and a few: sized so the
	// four files leave three records of the budget.
	n := parser.DefaultReadBudget - sm - sa - sb - 3 - (spentAlone(t, "lib/big.bot", bigOf(1)) - 1)
	big := bigOf(n)
	if got := sm + spentAlone(t, "lib/big.bot", big) + sa + sb; got != parser.DefaultReadBudget-3 {
		t.Fatalf("the four files spend %d, want %d", got, parser.DefaultReadBudget-3)
	}
	if spentAlone(t, "lib/a.bot", shortAEdited)-sa <= 3 {
		t.Fatal("the edit does not spend past the three records left")
	}
	t.Setenv(parser.ReadBudgetEnv, "")
	workdir := t.TempDir()
	files := map[string]string{"main.bot": main, "lib/big.bot": big, "lib/a.bot": shortA, "lib/b.bot": unitFixtureNodes}
	disk := map[string]string{}
	for rel, src := range files {
		disk["demo/"+rel] = src
	}
	writeUnitFixture(t, workdir, disk)
	botDir := filepath.Join(workdir, "demo")
	before := diskSums(t, botDir)
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK || opened.Unit == nil || opened.Unit.LoadErrors != 0 || !opened.Bindable {
		t.Fatalf("the bot does not open whole: %d %+v", rec.Code, opened.Unit)
	}
	refusedPastTheBudget := func(door string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "past the read budget of "+strconv.Itoa(parser.DefaultReadBudget)) || !strings.Contains(body, parser.ReadBudgetEnv) {
			t.Errorf("%s: %d %s — want 422, the bot past the budget, the operator's count for remedy", door, code, body)
		}
	}
	rec, _ = parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/a.bot", "source": shortAEdited})
	refusedPastTheBudget("apply", rec.Code, rec.Body.String())
	if !strings.Contains(rec.Body.String(), "lib/b.bot:") {
		t.Errorf("the apply's refusal names no position in lib/b.bot, where the bot ran out: %s", rec.Body.String())
	}
	srec, _ := savePath(t, s, "demo/main.bot", withPrompt(t, opened.Document, "lib/a.bot", "pa2", "Beta gamma delta epsilon zeta eta theta."), opened.Unit.Revision)
	refusedPastTheBudget("save", srec.Code, srec.Body.String())

	rec, cloud := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK || cloud.Unit == nil || cloud.Unit.LoadErrors != 0 || !cloud.Bindable {
		t.Fatalf("the bundle does not open whole: %d", rec.Code)
	}
	rec, _ = parseCall(t, s, map[string]any{"files": files, "main": "main.bot", "file": "lib/a.bot", "source": shortAEdited})
	refusedPastTheBudget("bundle apply", rec.Code, rec.Body.String())
	urec, out := unparseCall(t, s, map[string]any{"files": files, "main": "main.bot", "document": withPrompt(t, cloud.Document, "lib/a.bot", "pa2", "Beta gamma delta epsilon zeta eta theta."), "revision": cloud.Unit.Revision})
	refusedPastTheBudget("bundle save", urec.Code, urec.Body.String())
	if len(out.Files) != 0 {
		t.Errorf("the bundle save hands back files to patch in: %v", out.Files)
	}
	assertNothingWritten(t, botDir, before)
}

// A bot the read budget cut short opens as a salvage — its document lacks
// what was not read, and the budget is the reader's, so no revision of the
// files tells a later save that a larger budget reads them all: no door
// hands it out bindable (the editor's open, the bot picker, a bundle's
// open), and the document it shows is marked a salvage, which every door
// that takes a document back refuses at its decode (ast.ErrSalvage) —
// rendered, flattened, saved or pushed, before the budget is raised and
// after. A document of the bot opened whole before the budget fell is held
// to the stored bot as the server reads it now: the Source view refuses to
// render any of its files, positioned; the save and the bundle push are
// refused with the operator's count; the flatten refuses a merged text past
// the budget the server reads with. An apply is refused while the stored bot
// is short, even one that would bring it back under the budget. Nothing is
// written.
func TestABotTheBudgetCutShortOpensAsASalvage(t *testing.T) {
	sm, sa, sb := spentAlone(t, "main.bot", shortMain), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	budget := sm + sa + sb - 10
	workdir := t.TempDir()
	files := map[string]string{"main.bot": shortMain, "lib/a.bot": shortA, "lib/b.bot": unitFixtureNodes}
	disk := map[string]string{}
	for rel, src := range files {
		disk["bots/demo/"+rel] = src
	}
	writeUnitFixture(t, workdir, disk)
	botDir := filepath.Join(workdir, "bots", "demo")
	before := diskSums(t, botDir)
	s := &Server{cfg: Config{WorkDir: workdir}}
	past := "past the read budget of " + strconv.Itoa(budget)

	t.Setenv(parser.ReadBudgetEnv, "")
	rec, whole := openPath(t, s, "bots/demo/main.bot")
	if rec.Code != http.StatusOK || !whole.Bindable || whole.Unit == nil {
		t.Fatalf("the open before the budget fell: %d %+v", rec.Code, whole.Unit)
	}
	rec, wholeCloud := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK || !wholeCloud.Bindable || wholeCloud.Unit == nil {
		t.Fatalf("the bundle open before the budget fell: %d %s", rec.Code, rec.Body.String())
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))

	rec, opened := openPath(t, s, "bots/demo/main.bot")
	if rec.Code != http.StatusOK || opened.Unit == nil || opened.Unit.LoadErrors != 1 {
		t.Fatalf("open: %d %+v", rec.Code, opened.Unit)
	}
	if opened.Bindable {
		t.Error("the editor's open hands out bindable a document missing what the budget left unread")
	}
	salvageRefused := func(door string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ast.ErrSalvage.Error()) {
			t.Errorf("%s of the document opened short: %d %s — want it refused at its decode as a salvage", door, rec.Code, rec.Body.String())
		}
	}
	for _, rel := range []string{"lib/b.bot", "lib/a.bot", "main.bot"} {
		rec, _ := unparseCall(t, s, map[string]any{"path": "bots/demo/main.bot", "file": rel, "document": opened.Document})
		salvageRefused("the render of "+rel, rec)
		rec, out := unparseCall(t, s, map[string]any{"path": "bots/demo/main.bot", "file": rel, "document": whole.Document})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "lib/b.bot:") || !strings.Contains(rec.Body.String(), past) {
			t.Errorf("render of %s from the document opened whole: %d %q %s — want 422 positioned in lib/b.bot", rel, rec.Code, out.Source, rec.Body.String())
		}
	}
	rec, _ = parseCall(t, s, map[string]any{"path": "bots/demo/main.bot", "file": "lib/b.bot", "source": "prompt mission:\n  Do the thing.\n"})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), past) {
		t.Errorf("an apply on a stored bot read short: %d %s — want 422", rec.Code, rec.Body.String())
	}
	// The save is refused with the remedy of a bot read short — the
	// operator's count — not the one of a file to fix on disk.
	srec, _ := savePath(t, s, "bots/demo/main.bot", opened.Document, opened.Unit.Revision)
	salvageRefused("the save", srec)
	srec, _ = savePath(t, s, "bots/demo/main.bot", whole.Document, whole.Unit.Revision)
	if srec.Code != http.StatusUnprocessableEntity || !strings.Contains(srec.Body.String(), past) || !strings.Contains(srec.Body.String(), parser.ReadBudgetEnv) {
		t.Errorf("the save of a bot read short, from the document opened whole: %d %s — want 422 with the operator's count for remedy", srec.Code, srec.Body.String())
	}
	// The merged program a flatten shows is said to hold only what was read.
	rec, _ = unparseCall(t, s, map[string]any{"path": "bots/demo/main.bot", "document": opened.Document, "flatten": true})
	salvageRefused("the flatten", rec)
	rec, _ = unparseCall(t, s, map[string]any{"path": "bots/demo/main.bot", "document": whole.Document, "flatten": true})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), past) || !strings.Contains(rec.Body.String(), parser.ReadBudgetEnv) {
		t.Errorf("the flatten of the document opened whole, its merged text past the budget the server reads with: %d %s — want 422 naming the budget", rec.Code, rec.Body.String())
	}

	examples := filepath.Join(workdir, "bots")
	hs := exampleServer(t, workdir, examples)
	var picked oneProgram
	if code, body := getExampleJSON(t, hs.URL+"/api/examples/demo/main.bot", &picked); code != http.StatusOK {
		t.Fatalf("the bot picker: %d %s", code, body)
	}
	if picked.Bindable || picked.Unit == nil {
		t.Errorf("the bot picker hands out bindable (%v) a document missing what the budget left unread", picked.Bindable)
	}

	rec, cloud := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"})
	if rec.Code != http.StatusOK || cloud.Unit == nil {
		t.Fatalf("bundle open: %d %s", rec.Code, rec.Body.String())
	}
	if cloud.Bindable {
		t.Error("a bundle's open hands out bindable a document missing what the budget left unread")
	}
	urec, _ := unparseCall(t, s, map[string]any{"files": files, "main": "main.bot", "document": cloud.Document, "revision": cloud.Unit.Revision})
	salvageRefused("the bundle save", urec)
	urec, out := unparseCall(t, s, map[string]any{"files": files, "main": "main.bot", "document": wholeCloud.Document, "revision": wholeCloud.Unit.Revision})
	if urec.Code != http.StatusUnprocessableEntity || len(out.Files) != 0 || !strings.Contains(urec.Body.String(), past) || !strings.Contains(urec.Body.String(), parser.ReadBudgetEnv) {
		t.Errorf("a bundle save of a bot read short, from the document opened whole: %d %v %s — want 422 with the operator's count for remedy, nothing handed back", urec.Code, out.Files, urec.Body.String())
	}

	// The budget raised, the bot reads whole: the document opened short is
	// still the display of what was read, and no door takes it back.
	t.Setenv(parser.ReadBudgetEnv, "")
	srec, _ = savePath(t, s, "bots/demo/main.bot", opened.Document, opened.Unit.Revision)
	salvageRefused("the save, once the bot reads whole,", srec)
	urec, _ = unparseCall(t, s, map[string]any{"files": files, "main": "main.bot", "document": cloud.Document, "revision": cloud.Unit.Revision})
	salvageRefused("the bundle save, once the bot reads whole,", urec)
	assertNothingWritten(t, botDir, before)
	if t.Failed() {
		return
	}
	// The control: whole, the same document opened again saves.
	rec, opened = openPath(t, s, "bots/demo/main.bot")
	if rec.Code != http.StatusOK || !opened.Bindable || opened.Unit.LoadErrors != 0 {
		t.Fatalf("reopened under the default budget: %d %+v", rec.Code, opened.Unit)
	}
	if srec, _ := savePath(t, s, "bots/demo/main.bot", withPrompt(t, opened.Document, "lib/a.bot", "pa2", "Beta."), opened.Unit.Revision); srec.Code != http.StatusOK {
		t.Errorf("the control save: %d %s", srec.Code, srec.Body.String())
	}
	got, _ := os.ReadFile(filepath.Join(workdir, "bots", "demo", "lib", "b.bot"))
	if string(got) != unitFixtureNodes {
		t.Errorf("lib/b.bot after the control save: %q", got)
	}
}

// A save whose claimed files the reader cannot read whole within the budget
// they share — the claim adds a fragment that fills it — is refused with
// the bot's refusal, positioned in that fragment, and the operator's count
// for remedy; nothing is written.
func TestAClaimPastTheSharedBudgetIsRefused(t *testing.T) {
	sm, sa, sb := spentAlone(t, "main.bot", shortMain), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	budget := sm + sa + sb + 40
	big := "prompt big:\n" + strings.Repeat("  x\n", budget)
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": shortMain, "demo/lib/a.bot": shortA, "demo/lib/b.bot": unitFixtureNodes, "demo/lib/big.bot": big})
	botDir := filepath.Join(workdir, "demo")
	before := diskSums(t, botDir)
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK || !opened.Bindable || opened.Unit.LoadErrors != 0 {
		t.Fatalf("the bot does not open whole: %d %+v", rec.Code, opened.Unit)
	}
	claimed := []unitFileInfo{
		{Rel: "main.bot", Imports: []string{"lib/a.bot", "lib/b.bot", "lib/big.bot"}, Digest: opened.Unit.Files[0].Digest},
		{Rel: "lib/a.bot", Digest: opened.Unit.Files[1].Digest},
		{Rel: "lib/b.bot", Digest: opened.Unit.Files[2].Digest},
		{Rel: "lib/big.bot", Digest: fileDigest([]byte(big))},
	}
	srec, _ := savePathClaimed(t, s, "demo/main.bot", opened.Document, opened.Unit.Revision, claimed)
	body := srec.Body.String()
	if srec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "lib/big.bot:") || !strings.Contains(body, "past the read budget of "+strconv.Itoa(budget)) || !strings.Contains(body, parser.ReadBudgetEnv) {
		t.Errorf("a claim past the shared budget: %d %s — want 422 positioned in lib/big.bot, the operator's count for remedy", srec.Code, body)
	}
	assertNothingWritten(t, botDir, before)
}
