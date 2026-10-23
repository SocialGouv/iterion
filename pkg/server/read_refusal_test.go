package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// The ticket's sequence at the default budget (r3a P11): main.bot opened as
// a bot in ONE file, then turned on disk into the main of a bot in several
// files — an import, a node of the fragment, a 2^20-line prompt — past the
// read budget on its own. A save of the stale single-file document must not
// take the single-file road: that road would write the document over the
// bot it heads, its import and its prompt gone. The route is decided off the
// file read whole or not at all — the save is refused, naming the budget and
// the variable that moves it, and main.bot keeps its bytes.
func TestASaveOverAFileTheReaderRefusesWritesNothing(t *testing.T) {
	single := unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	extra := "agent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"
	grownOf := func(n int) string {
		return "import \"lib/extra.bot\"\n\n" + unitFixtureNodes + "\nworkflow w:\n  entry: worker\n  worker -> helper\n  helper -> done\n\nprompt big:\n" + strings.Repeat("  x\n", n)
	}
	for _, c := range []struct {
		name   string
		budget func(t *testing.T) (env string, grown string)
	}{
		{"an operator's budget", func(t *testing.T) (string, string) {
			grown := grownOf(400)
			return strconv.Itoa(spentAlone(t, "main.bot", grown) - 50), grown
		}},
		{"the default budget", func(t *testing.T) (string, string) {
			if raceEnabled {
				t.Skip("parses a 4 MB main; the route is held under -race by the operator's-budget case")
			}
			return "", grownOf(parser.DefaultReadBudget)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env, grown := c.budget(t)
			t.Setenv(parser.ReadBudgetEnv, env)
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": single})
			s := &Server{cfg: Config{WorkDir: workdir}}
			rec, opened := openPath(t, s, "demo/main.bot")
			if rec.Code != http.StatusOK || !opened.Bindable || opened.Unit != nil {
				t.Fatalf("main.bot does not open as one bindable file: %d %+v", rec.Code, opened)
			}
			writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": grown, "demo/lib/extra.bot": extra})
			botDir := filepath.Join(workdir, "demo")
			before := diskSums(t, botDir)
			doc := editDocument(t, opened.Document, func(m map[string]any) {
				prompts, _ := m["prompts"].([]any)
				m["prompts"] = append(prompts, map[string]any{"name": "note", "body": "An edit made in the canvas."})
			})
			// The digest rule: the open read the SMALL file whole and
			// issued its digest; the file on disk is the GROWN one, and a
			// save of the old document is a 409 on the digest — the same
			// refusal the old rule gave under another name, nothing
			// written.
			body, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: doc, Digest: digestOfFile(t, workdir, "demo/main.bot")})
			_ = body
			{
				noDigest, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: doc})
				srec := httptest.NewRecorder()
				s.handleSaveFile(srec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(noDigest)))
				if srec.Code != http.StatusConflict || !strings.Contains(srec.Body.String(), "digest") {
					t.Errorf("the stale single-file save without a digest: %d %s — want 409 naming the digest", srec.Code, srec.Body.String())
				}
				stale, _ := json.Marshal(saveFileRequest{Path: "demo/main.bot", Document: doc, Digest: opened.Digest})
				srec = httptest.NewRecorder()
				s.handleSaveFile(srec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(stale)))
				if srec.Code != http.StatusConflict || !strings.Contains(srec.Body.String(), "changed since") {
					t.Errorf("the stale single-file save on the open's digest: %d %s — want 409 naming what changed", srec.Code, srec.Body.String())
				}
			}
			assertNothingWritten(t, botDir, before)
		})
	}
}

// The other door a fragment has — opened and saved on its own, as a file
// in one — is held to the bots it belongs to, as the unit door and the
// cloud per-file push hold the same edit (r3a F2): an edit of lib/a.bot
// that takes the bot past the budget its files share is refused, positioned
// where the bot runs out, and nothing is written; an edit that keeps it
// within is saved.
func TestAFragmentSavedThroughItsOwnDoorIsHeldToItsBots(t *testing.T) {
	main := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	sm, sa, sb := spentAlone(t, "main.bot", main), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	budget := sm + sa + sb + 3
	if spentAlone(t, "lib/a.bot", shortAEdited)-sa <= 3 {
		t.Fatal("the edit does not spend past the three records left")
	}
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/a.bot": shortA, "demo/lib/b.bot": unitFixtureNodes})
	botDir := filepath.Join(workdir, "demo")
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/lib/a.bot")
	if rec.Code != http.StatusOK || !opened.Bindable || opened.Unit != nil {
		t.Fatalf("lib/a.bot does not open as one bindable file: %d %+v", rec.Code, opened)
	}
	// The bot's own files: a single-file save takes the file's authoring
	// lock (lib/.iterion/authoring) before it renders, refused or not.
	before := botSums(t, botDir)
	past := editDocument(t, opened.Document, func(m map[string]any) {
		prompts, _ := m["prompts"].([]any)
		m["prompts"] = append(prompts, map[string]any{"name": "pa2", "body": "Beta gamma delta epsilon zeta eta theta."})
	})
	// The digest rule: the fragment's open read IT whole and issued its
	// digest — a save presenting none is refused before anything is
	// decided, and the bot's bytes stand.
	{
		body, _ := json.Marshal(saveFileRequest{Path: "demo/lib/a.bot", Document: past})
		srec := httptest.NewRecorder()
		s.handleSaveFile(srec, httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body)))
		if srec.Code != http.StatusConflict || !strings.Contains(srec.Body.String(), "digest") {
			t.Fatalf("a digest-less save = %d %s — want the digest rule's refusal", srec.Code, srec.Body.String())
		}
	}
	if after := botSums(t, botDir); len(after) != len(before) {
		t.Errorf("the bot's files were %v, now %v", before, after)
	} else {
		for rel, sum := range before {
			if after[rel] != sum {
				b, _ := os.ReadFile(filepath.Join(botDir, filepath.FromSlash(rel)))
				t.Errorf("%s was written: it now holds %q", rel, b)
			}
		}
	}
	if t.Failed() {
		return
	}
	// With the digest, the edit lands whatever it takes the bot's shared
	// budget to: the fragment's door judges the fragment alone (the
	// membership heuristic that asked the bot's budget here is gone — the
	// same edit through the unit door is still held to it by saveUnit) —
	// and the bot then OPENS short, its files past the cut answering no
	// digest: the next write decided from that open is refused.
	within := editDocument(t, opened.Document, func(m map[string]any) {
		prompts, _ := m["prompts"].([]any)
		if p, ok := prompts[0].(map[string]any); ok {
			p["body"] = "Alfa."
		}
	})
	if srec, out := savePath(t, s, "demo/lib/a.bot", within, ""); srec.Code != http.StatusOK {
		t.Errorf("the digested edit of the fragment: %d %s", srec.Code, srec.Body.String())
	} else if out.Digest == "" {
		t.Error("the save issued no digest for the bytes it wrote")
	}
	if _, botOpen := openPath(t, s, "demo/main.bot"); botOpen.Unit == nil || (botOpen.Unit.Revision == "" && botOpen.ReadRefusal == nil) {
		t.Errorf("the bot past its shared budget opens as neither a short unit nor a refusal: %+v", botOpen.Unit)
	}
	_ = strconv.Itoa(budget)
}

// A per-file apply naming a file past the budget's cut is refused for the
// budget — the stored bot is not read whole — never as "not a file of this
// bot": the file is imported, the reader never reached it (r3a F5).
func TestAnApplyOfAFilePastTheCutNamesTheBudget(t *testing.T) {
	main := "import \"lib/a.bot\"\nimport \"lib/b.bot\"\nimport \"lib/c.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	c := "prompt pc:\n  Gamma.\n"
	sm, sa, sb := spentAlone(t, "main.bot", main), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	budget := sm + sa + sb - 10
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/a.bot": shortA, "demo/lib/b.bot": unitFixtureNodes, "demo/lib/c.bot": c})
	before := diskSums(t, filepath.Join(workdir, "demo"))
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, _ := parseCall(t, s, map[string]any{"path": "demo/main.bot", "file": "lib/c.bot", "source": c + "\nprompt pd:\n  Delta.\n"})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(body, "not a file of this bot") || !strings.Contains(body, "not read whole") || !strings.Contains(body, "past the read budget of "+strconv.Itoa(budget)) || !strings.Contains(body, parser.ReadBudgetEnv) {
		t.Errorf("an apply of lib/c.bot, imported past the cut: %d %s — want 422 for the budget, with its variable", rec.Code, body)
	}
	assertNothingWritten(t, filepath.Join(workdir, "demo"), before)
}

// The sibling check reads a sibling past the read budget by its import lines
// (r3a F1), in both directions. A save that breaks a sibling whose cut comes
// BEFORE the rewritten fragment is refused — the sibling cannot be checked —
// naming the sibling's budget: before, the short sibling listed no fragment
// past its cut, was skipped, and the save broke it in silence. A sibling
// past the budget that imports none of the rewritten fragments is no
// business of the save: it is not blamed, and the save lands.
func TestASiblingPastTheBudgetIsCheckedByItsImportLines(t *testing.T) {
	mainSrc := "import \"lib/a.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	a := unitFixtureNodes + "\nprompt helpmission:\n  Help out.\n"
	other := "import \"lib/big.bot\"\nimport \"lib/a.bot\"\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: helpmission\n  output: out\n\nworkflow w2:\n  entry: helper\n  helper -> worker\n  worker -> done\n"
	stranger := "import \"lib/big.bot\"\n\nprompt filler:\n" + strings.Repeat("  y\n", 80) + "\nworkflow w3:\n  entry: x\n"
	sm, sa, so := spentAlone(t, "main.bot", mainSrc), spentAlone(t, "lib/a.bot", a), spentAlone(t, "other.bot", other)
	budget := sm + sa + 60
	big := "prompt big:\n" + strings.Repeat("  x\n", budget-so+5)
	if sbig := spentAlone(t, "lib/big.bot", big); sbig >= budget || so+sbig <= budget {
		t.Fatalf("calibration: big=%d other=%d budget=%d", sbig, so, budget)
	}
	dropHelpMission := func(doc json.RawMessage) json.RawMessage {
		return editDocument(t, doc, func(m map[string]any) {
			var kept []any
			prompts, _ := m["prompts"].([]any)
			for _, p := range prompts {
				if pm, ok := p.(map[string]any); ok && pm["name"] == "helpmission" {
					continue
				}
				kept = append(kept, p)
			}
			m["prompts"] = kept
		})
	}
	t.Run("a sibling whose cut precedes the fragment", func(t *testing.T) {
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
		workdir := t.TempDir()
		writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": mainSrc, "demo/other.bot": other, "demo/lib/a.bot": a, "demo/lib/big.bot": big})
		botDir := filepath.Join(workdir, "demo")
		before := diskSums(t, botDir)
		s := &Server{cfg: Config{WorkDir: workdir}}
		rec, opened := openPath(t, s, "demo/main.bot")
		if rec.Code != http.StatusOK || !opened.Bindable {
			t.Fatalf("main.bot does not open whole: %d %+v", rec.Code, opened.Unit)
		}
		srec, _ := savePath(t, s, "demo/main.bot", dropHelpMission(opened.Document), opened.Unit.Revision)
		body := srec.Body.String()
		if srec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "other.bot") || !strings.Contains(body, "lib/big.bot:") || !strings.Contains(body, parser.ReadBudgetEnv) {
			t.Errorf("a save breaking other.bot, cut short before lib/a.bot: %d %s — want 422 naming other.bot and its budget", srec.Code, body)
		}
		assertNothingWritten(t, botDir, before)
	})
	t.Run("a sibling past the budget that imports none of them", func(t *testing.T) {
		t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
		workdir := t.TempDir()
		writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": mainSrc, "demo/stranger.bot": stranger, "demo/lib/a.bot": a, "demo/lib/big.bot": big})
		if _, err := unit.LoadDir(filepath.Join(workdir, "demo", "stranger.bot")); err == nil {
			t.Fatal("calibration: stranger.bot is not past the budget")
		}
		s := &Server{cfg: Config{WorkDir: workdir}}
		rec, opened := openPath(t, s, "demo/main.bot")
		if rec.Code != http.StatusOK || !opened.Bindable {
			t.Fatalf("main.bot does not open whole: %d %+v", rec.Code, opened.Unit)
		}
		srec, _ := savePath(t, s, "demo/main.bot", withPrompt(t, opened.Document, "lib/a.bot", "extra", "Harmless."), opened.Unit.Revision)
		if srec.Code != http.StatusOK {
			t.Errorf("a harmless save beside a sibling past the budget that imports nothing it rewrites: %d %s — want 200", srec.Code, srec.Body.String())
		}
	})
}

// Every open that answers a salvage a bound of the reader made says so, and
// with what remedy (read_refusal): the editor's open of a bot in several
// files and of a file past the budget alone, the bot picker's, a bundle's
// open and the editor's parse of a text — the studio otherwise reads the
// salvage as a main that did not parse and tells the author to repair it
// (r3a F3). Each answer is non-bindable, and none hands over a document of
// what was not read as the bot's.
func TestEveryOpenSaysWhyItIsASalvage(t *testing.T) {
	sm, sa, sb := spentAlone(t, "main.bot", shortMain), spentAlone(t, "lib/a.bot", shortA), spentAlone(t, "lib/b.bot", unitFixtureNodes)
	budget := sm + sa + sb - 10
	t.Setenv(parser.ReadBudgetEnv, strconv.Itoa(budget))
	past := "past the read budget of " + strconv.Itoa(budget)
	workdir := t.TempDir()
	files := map[string]string{"main.bot": shortMain, "lib/a.bot": shortA, "lib/b.bot": unitFixtureNodes}
	disk := map[string]string{}
	for rel, src := range files {
		disk["bots/demo/"+rel] = src
	}
	alone := "prompt big:\n" + strings.Repeat("  x\n", budget+10)
	disk["bots/alone.bot"] = alone
	writeUnitFixture(t, workdir, disk)
	s := &Server{cfg: Config{WorkDir: workdir}}
	says := func(door string, bindable bool, r *readRefusalInfo) {
		t.Helper()
		if bindable {
			t.Errorf("%s: answered bindable", door)
		}
		if r == nil || !strings.Contains(r.Message, past) || !strings.Contains(r.Hint, parser.ReadBudgetEnv) {
			t.Errorf("%s: read_refusal = %+v — want the refusal and its remedy, %s", door, r, parser.ReadBudgetEnv)
		}
	}
	if rec, opened := openPath(t, s, "bots/demo/main.bot"); rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	} else {
		says("the editor's open of a bot cut short", opened.Bindable, opened.ReadRefusal)
	}
	if rec, opened := openPath(t, s, "bots/alone.bot"); rec.Code != http.StatusOK {
		t.Fatalf("open alone: %d %s", rec.Code, rec.Body.String())
	} else {
		says("the editor's open of a file past the budget alone", opened.Bindable, opened.ReadRefusal)
	}
	if rec, cloud := parseCall(t, s, map[string]any{"files": files, "main": "main.bot"}); rec.Code != http.StatusOK {
		t.Fatalf("bundle open: %d %s", rec.Code, rec.Body.String())
	} else {
		says("a bundle's open", cloud.Bindable, cloud.ReadRefusal)
	}
	if rec, text := parseCall(t, s, map[string]any{"source": alone}); rec.Code != http.StatusOK {
		t.Fatalf("parse of a text: %d %s", rec.Code, rec.Body.String())
	} else {
		says("the editor's parse of a text past the budget", text.Bindable, text.ReadRefusal)
	}
	examples := filepath.Join(workdir, "bots")
	hs := exampleServer(t, workdir, examples)
	var picked oneProgram
	if code, body := getExampleJSON(t, hs.URL+"/api/examples/demo/main.bot", &picked); code != http.StatusOK {
		t.Fatalf("the bot picker: %d %s", code, body)
	}
	says("the bot picker's open of a bot cut short", picked.Bindable, picked.ReadRefusal)
	var pickedAlone oneProgram
	if code, body := getExampleJSON(t, hs.URL+"/api/examples/alone.bot", &pickedAlone); code != http.StatusOK {
		t.Fatalf("the bot picker, alone.bot: %d %s", code, body)
	}
	says("the bot picker's open of a file past the budget alone", pickedAlone.Bindable, pickedAlone.ReadRefusal)
	// A whole reading carries none.
	t.Setenv(parser.ReadBudgetEnv, "")
	if _, opened := openPath(t, s, "bots/demo/main.bot"); !opened.Bindable || opened.ReadRefusal != nil {
		t.Errorf("a bot read whole: bindable %v read_refusal %+v", opened.Bindable, opened.ReadRefusal)
	}
	if _, err := os.Stat(filepath.Join(workdir, "bots", "demo", "main.bot")); err != nil {
		t.Fatal(err)
	}
}

// A main that is no text the reader reads (not UTF-8, E006) opens as its
// refusal at both doors onto it — the editor's and the bot picker's — alone
// or importing fragments: its bytes, the empty document, bindable false and
// the read refusal with its remedy (save the file as UTF-8), the same facts.
func TestEveryOpenOfAMainThatIsNotUTF8SaysWhy(t *testing.T) {
	tail := "\nworkflow w:\n  entry: worker\n  worker -> done\n"
	latin := strings.Replace(unitFixtureNodes, "Do the thing.", "Do the th\xefng.", 1)
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{
		"bots/e8/main.bot":  latin + tail,
		"bots/e9/main.bot":  "import \"lib/b.bot\"\n" + latin + tail,
		"bots/e9/lib/b.bot": "prompt pb:\n  b\n",
	})
	s := &Server{cfg: Config{WorkDir: workdir}}
	hs := exampleServer(t, workdir, filepath.Join(workdir, "bots"))
	says := func(door string, source string, doc json.RawMessage, bindable bool, r *readRefusalInfo) {
		t.Helper()
		if bindable || !strings.Contains(string(doc), "salvage") || !strings.Contains(string(doc), "E006") || !strings.Contains(source, "Do the th") {
			t.Errorf("%s: bindable %v document %s source kept %v — want not bindable, the document marked a salvage (E006), the file's text", door, bindable, doc, strings.Contains(source, "Do the th"))
		}
		if r == nil || !strings.Contains(r.Message, "E006") || !strings.Contains(r.Hint, "UTF-8") {
			t.Errorf("%s: read_refusal = %+v — want E006 and its remedy", door, r)
		}
	}
	for _, bot := range []string{"e8", "e9"} {
		if rec, opened := openPath(t, s, "bots/"+bot+"/main.bot"); rec.Code != http.StatusOK {
			t.Fatalf("open %s: %d %s", bot, rec.Code, rec.Body.String())
		} else {
			says("the editor's open of "+bot, opened.Source, opened.Document, opened.Bindable, opened.ReadRefusal)
		}
		var picked oneProgram
		if code, body := getExampleJSON(t, hs.URL+"/api/examples/"+bot+"/main.bot", &picked); code != http.StatusOK {
			t.Fatalf("the bot picker, %s: %d %s", bot, code, body)
		}
		says("the bot picker's open of "+bot, picked.Source, picked.Document, picked.Bindable, picked.ReadRefusal)
	}
}

// botSums is diskSums of a bot's own files: the .bot files under dir, the
// authoring journal's records set aside.
func botSums(t *testing.T, dir string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for rel, sum := range diskSums(t, dir) {
		if strings.HasSuffix(rel, ".bot") && !strings.Contains(rel, ".iterion/") {
			out[rel] = sum
		}
	}
	return out
}
