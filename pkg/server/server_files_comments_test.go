package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commentedBot is a workflow whose author wrote a note beside each thing it
// explains — at the file's head, above a declaration, above a property,
// inside a nested block and beside an edge.
const commentedBot = `## What this bot is for.

dsl: 2

## The one dial.
vars:
  ## Keep it short.
  topic: string = "x"

prompt sys:
  Be brief.

schema out:
  ok: bool

## The worker.
agent plan:
  ## Cheap enough for this.
  model: "sonnet"
  output: out
  system: sys
  sandbox:
    image: "alpine"
    ## Reaching the network is the point.
    network:
      mode: open

workflow main:

  entry: plan

  ## Nothing else to do afterwards.
  plan -> done
`

// TestSaveFileKeepsTheCommentsOfTheFile exercises the studio's save at its
// own site — open the file the way the canvas does, save the document it
// gets back, unchanged — and asserts the file on disk still carries every
// comment. This is the path #1282 was reported on: the note an author wrote
// beside a criterion did not survive the first save.
func TestSaveFileKeepsTheCommentsOfTheFile(t *testing.T) {
	workdir := t.TempDir()
	path := filepath.Join(workdir, "commented.bot")
	if err := os.WriteFile(path, []byte(commentedBot), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	// Open: what the canvas is handed.
	body, err := json.Marshal(openFileRequest{Path: "commented.bot"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var opened struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	if len(opened.Document) == 0 {
		t.Fatalf("the open response carries no document: %s", rec.Body.String())
	}

	// Save: the same document back, untouched — a canvas that changed
	// nothing must leave the file saying what it said.
	body, err = json.Marshal(saveFileRequest{Path: "commented.bot", Document: opened.Document})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	s.handleSaveFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## What this bot is for.",
		"## The one dial.",
		"  ## Keep it short.",
		"## The worker.",
		"  ## Cheap enough for this.",
		"    ## Reaching the network is the point.",
		"  ## Nothing else to do afterwards.",
	} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("the save dropped or moved %q:\n%s", want, saved)
		}
	}
	if string(saved) != commentedBot {
		t.Errorf("the saved file is not the file that was opened:\n%s", saved)
	}
}

// unitMain and unitFragment are a bot in two files whose author documented
// both of them — around a declaration, around a keyed block, and around one
// of that block's entries.
const unitMain = `dsl: 2
import "lib/nodes.bot"

## What this bot is for.

## The dials.
vars:
  ## REQUIRED — the thing to do.
  topic: string

## The gate.
schema out:
  ok: bool

workflow main:

  entry: plan

  plan -> done
`

const unitFragment = `## The fragment's own note.

## The worker.
agent plan:
  ## Cheap enough for this.
  model: "sonnet"
  output: out
`

// TestSaveUnitKeepsTheCommentsOfEveryFile: a bot in several files, opened
// the way the canvas opens it and saved back unchanged, keeps every comment
// in the file it was written in — the block's own included. A keyed block
// (`vars:`, `presets:`, `secrets:`, `attachments:`) is routed entry by
// entry across the unit's files; its own comments were routed nowhere and
// were dropped, silently, with the save reporting success.
func TestSaveUnitKeepsTheCommentsOfEveryFile(t *testing.T) {
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"main.bot": unitMain, filepath.Join("lib", "nodes.bot"): unitFragment}
	for rel, src := range files {
		if err := os.WriteFile(filepath.Join(workdir, rel), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{cfg: Config{WorkDir: workdir}}

	body, err := json.Marshal(openFileRequest{Path: "main.bot"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/open", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleOpenFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var opened struct {
		Document json.RawMessage `json:"document"`
		Unit     *unitInfo       `json:"unit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	if opened.Unit == nil {
		t.Fatalf("the open response carries no unit: %s", rec.Body.String())
	}

	body, err = json.Marshal(saveFileRequest{Path: "main.bot", Document: opened.Document, Revision: opened.Unit.Revision})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/files/save", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	s.handleSaveFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Every comment is still in the file it was written in.
	for rel, want := range map[string][]string{
		"main.bot": {
			"## What this bot is for.",
			"## The dials.",
			"  ## REQUIRED — the thing to do.",
			"## The gate.",
		},
		filepath.Join("lib", "nodes.bot"): {
			"## The fragment's own note.",
			"## The worker.",
			"  ## Cheap enough for this.",
		},
	} {
		got, err := os.ReadFile(filepath.Join(workdir, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(string(got), w) {
				t.Errorf("%s lost %q after a save that reported success:\n%s", rel, w, got)
			}
		}
	}
	// The block's comments did not cross into the fragment.
	frag, err := os.ReadFile(filepath.Join(workdir, "lib", "nodes.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(frag), "## The dials.") {
		t.Errorf("a comment of main.bot was written into the fragment:\n%s", frag)
	}
}
