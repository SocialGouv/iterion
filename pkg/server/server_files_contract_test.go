package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const contractUnitMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  contract: pub\n  entry: worker\n  worker -> done\n"

// Every property of the contract surface, in the fragment beside the node.
const contractUnitNodes = "schema out:\n  ok: bool\n\ncontract pub:\n  display_name: \"Public\"\n  responsibility: \"Does a thing\"\n  version: 2\n  inputs:\n    goal: string\n      description: \"what\"\n      required: false\n      default: \"hi\"\n    labels: string[]\n      nullable: true\n      min_items: 0\n      max_items: 5\n      default: null\n    brief: string\n      file:\n        media_type: \"text/markdown\"\n        min_bytes: 1\n        schema: out\n  outputs:\n    url: string\n      from: worker.url\n  criteria:\n    long_enough:\n      kind: min_length\n      port: input.goal\n      params: {min: 2}\n  effects:\n    ships:\n      description: \"Ships it\"\n      paid: true\n\nprompt mission:\n  Do the thing.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n"

// A bot in several files whose fragment holds the contract: the document
// opens with the contract carrying its fragment and the workflow naming
// it; saving the document untouched writes nothing; an edit elsewhere in
// the same fragment rewrites the fragment with the whole contract and
// leaves the main byte-identical. The save path routes by reflection over
// the AST's fields, so this holds the composition, not a contract branch.
func TestContractSurvivesTheStudioSaveInItsFragment(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": contractUnitMain, "demo/lib/nodes.bot": contractUnitNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}

	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(opened.Document, &doc); err != nil {
		t.Fatal(err)
	}
	contracts, _ := doc["contracts"].([]any)
	if len(contracts) != 1 {
		t.Fatalf("contracts in the merged document: %v", doc["contracts"])
	}
	if c := contracts[0].(map[string]any); c["file"] != "lib/nodes.bot" || c["name"] != "pub" {
		t.Errorf("the contract came back as %v, want pub from lib/nodes.bot", c)
	}
	if wf := doc["workflows"].([]any)[0].(map[string]any); wf["contract"] != "pub" {
		t.Errorf("the workflow's contract came back as %v", wf["contract"])
	}

	rec, saved := savePath(t, s, "demo/main.bot", opened.Document, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op save: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 0 {
		t.Fatalf("a no-op save rewrote %v: the contract does not round-trip through the writer", saved.Files)
	}

	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	rec, saved = savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 1 || saved.Files[0] != "lib/nodes.bot" {
		t.Fatalf("files written %v, want the fragment alone", saved.Files)
	}
	after := readFixture(t, workdir, "demo/lib/nodes.bot")
	for _, want := range []string{
		"contract pub:", "display_name: \"Public\"", "responsibility: \"Does a thing\"", "version: 2",
		"description: \"what\"", "required: false", "default: \"hi\"", "nullable: true", "min_items: 0", "max_items: 5", "default: null",
		"file:", "media_type: \"text/markdown\"", "min_bytes: 1", "schema: out",
		"from: worker.url", "kind: min_length", "port: input.goal", "params: {min: 2}", "description: \"Ships it\"", "paid: true",
		"model: \"anthropic/claude-opus-5\"",
	} {
		if !strings.Contains(after, want) {
			t.Errorf("the save lost %q from the fragment:\n%s", want, after)
		}
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != contractUnitMain {
		t.Errorf("the main was rewritten:\n%s", got)
	}
}

// A contract the editor adds carries no provenance: it lands in the main,
// beside the workflow that names it, and the fragment is left alone.
func TestANewContractLandsInTheMain(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		m["contracts"] = []any{map[string]any{
			"name": "fresh", "display_name": "Fresh",
			"inputs": []any{map[string]any{"name": "goal", "type": "string", "default": "x"}},
		}}
		m["workflows"].([]any)[0].(map[string]any)["contract"] = "fresh"
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if len(saved.Files) != 1 || saved.Files[0] != "main.bot" {
		t.Fatalf("files written %v, want the main alone", saved.Files)
	}
	main := readFixture(t, workdir, "demo/main.bot")
	for _, want := range []string{"contract fresh:", "display_name: \"Fresh\"", "default: \"x\"", "  contract: fresh\n"} {
		if !strings.Contains(main, want) {
			t.Errorf("the main lacks %q:\n%s", want, main)
		}
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != unitFixtureNodes {
		t.Errorf("the fragment was rewritten:\n%s", got)
	}
}

// A client that drops the contract's provenance is refused, naming the
// declaration and the fragment it lives in — never a silent move to the
// main — and the fragment keeps its contract.
func TestAContractThatLostItsProvenanceIsRefused(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": contractUnitMain, "demo/lib/nodes.bot": contractUnitNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	stripped := editDocument(t, opened.Document, func(m map[string]any) {
		for _, c := range m["contracts"].([]any) {
			delete(c.(map[string]any), "file")
		}
	})
	rec, _ := savePath(t, s, "demo/main.bot", stripped, opened.Unit.Revision)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `provenance of contract \"pub\"`) || !strings.Contains(rec.Body.String(), "lib/nodes.bot") {
		t.Fatalf("save: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != contractUnitNodes {
		t.Errorf("the fragment changed:\n%s", got)
	}
}
