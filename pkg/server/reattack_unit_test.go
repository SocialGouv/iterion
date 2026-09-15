package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

const subbotFixtureMain = "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: worker\n  worker -> kid\n  kid -> done\n"

// The fragment declares a subbot against ITS directory: `kids/k.bot` under
// lib/ — root-relative `lib/kids/k.bot`, which is what the merged document
// carries.
const subbotFixtureNodes = "schema out:\n  ok: bool\n\nprompt mission:\n  Do the thing.\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n\nsubbot kid:\n  source: \"kids/k.bot\"\n"

const subbotFixtureKid = "tool t:\n  command: \"true\"\n\nworkflow k:\n  entry: t\n  t -> done\n"

func subbotsOf(m map[string]any) []any {
	subbots, _ := m["subbots"].([]any)
	return subbots
}

// A fragment's subbot source travels root-relative in the document and is
// written back as the fragment wrote it: a save with no edit rewrites
// nothing, three times over; one that changes the source in the editor
// writes it in the fragment's own terms.
func TestSaveAUnitKeepsAFragmentsSubbotSourceAsWritten(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": subbotFixtureMain, "demo/lib/nodes.bot": subbotFixtureNodes, "demo/lib/kids/k.bot": subbotFixtureKid})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	var doc map[string]any
	if err := json.Unmarshal(opened.Document, &doc); err != nil {
		t.Fatal(err)
	}
	if got := subbotsOf(doc)[0].(map[string]any)["source"]; got != "lib/kids/k.bot" {
		t.Fatalf("the document carries %v, want the root-relative source", got)
	}
	revision := opened.Unit.Revision
	for pass := 1; pass <= 3; pass++ {
		rec, saved := savePath(t, s, "demo/main.bot", opened.Document, revision)
		if rec.Code != http.StatusOK || len(saved.Files) != 0 {
			t.Fatalf("pass %d: %d %s, rewrote %v — a save with no edit rewrote a file", pass, rec.Code, rec.Body.String(), saved.Files)
		}
		if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != subbotFixtureNodes {
			t.Fatalf("pass %d: the fragment changed:\n%s", pass, got)
		}
		revision = saved.Revision
	}
	// Edited in the editor: the source moves, and is written relative to
	// the fragment, so the child still resolves from the root.
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		subbotsOf(m)[0].(map[string]any)["source"] = "lib/kids/other.bot"
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, revision)
	if rec.Code != http.StatusOK || len(saved.Files) != 1 || saved.Files[0] != "lib/nodes.bot" {
		t.Fatalf("edited source: %d %s, rewrote %v", rec.Code, rec.Body.String(), saved.Files)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "source: \"kids/other.bot\"") {
		t.Fatalf("the edited source was not written in the fragment's terms:\n%s", got)
	}
	if u := unit.LoadDir(filepath.Join(workdir, "demo", "main.bot")); u.HasErrors() || u.Merged.Subbots[0].Source != "lib/kids/other.bot" {
		t.Fatalf("after the save the unit resolves the child at %q", u.Merged.Subbots[0].Source)
	}
}

// A save that wrote drops its journal and the displaced originals: nothing
// is left to recover. A save that failed keeps them, and names them.
func TestSaveAUnitLeavesNoRecordOnceWritten(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	revision := opened.Unit.Revision
	models := []string{"anthropic/claude-opus-5", "anthropic/claude-opus-4-8", "anthropic/claude-opus-5"}
	for i, model := range models {
		edited := editDocument(t, opened.Document, func(m map[string]any) {
			agentsOf(m)[0].(map[string]any)["model"] = model
		})
		rec, saved := savePath(t, s, "demo/main.bot", edited, revision)
		if rec.Code != http.StatusOK || len(saved.Files) != 1 {
			t.Fatalf("save %d: %d %s (%v)", i, rec.Code, rec.Body.String(), saved.Files)
		}
		revision = saved.Revision
	}
	control := filepath.Join(workdir, "demo", "lib", ".iterion", "authoring")
	entries, err := os.ReadDir(control)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".record") || strings.HasSuffix(e.Name(), ".before") {
			t.Fatalf("a finished save left %s behind (of %d entries)", e.Name(), len(entries))
		}
	}
	// A failed save keeps its records — the recovery the error names.
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-4-7"
		m["agents"] = append(agentsOf(m), map[string]any{"name": "extra", "backend": "claude_code", "model": "anthropic/claude-opus-4-8", "description": "added in the editor"})
	})
	publishes := 0
	authoringStepHook = func(stage string) error {
		if stage == "publish" {
			publishes++
			if publishes == 2 {
				return errors.New("injected: disk full")
			}
		}
		return nil
	}
	t.Cleanup(func() { authoringStepHook = nil })
	rec, _ := savePath(t, s, "demo/main.bot", edited, revision)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), ".record") {
		t.Fatalf("failed write: %d %s", rec.Code, rec.Body.String())
	}
	records := 0
	for _, dir := range []string{control, filepath.Join(workdir, "demo", ".iterion", "authoring")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".record") {
				records++
			}
		}
	}
	if records == 0 {
		t.Fatal("a failed save kept no record to recover from")
	}
}

// A block whose entries all live in another file has no header in this
// one; a block the editor emptied keeps its bare header where it was.
func TestSaveAUnitLeavesNoBareHeaderBehind(t *testing.T) {
	workdir := t.TempDir()
	mainWithVars := "import \"lib/nodes.bot\"\n\nvars:\n  goal: string = \"g\"\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	nodesWithVars := "vars:\n  depth: int = 1\n\n" + unitFixtureNodes
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": mainWithVars, "demo/lib/nodes.bot": nodesWithVars})
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	// The main's only var deleted: the fragment keeps its var, the main
	// gets no `vars:` header.
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		vars := m["vars"].(map[string]any)
		var kept []any
		for _, f := range vars["fields"].([]any) {
			if f.(map[string]any)["name"] != "goal" {
				kept = append(kept, f)
			}
		}
		vars["fields"] = kept
	})
	rec, saved := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code != http.StatusOK || len(saved.Files) != 1 || saved.Files[0] != "main.bot" {
		t.Fatalf("delete the main's var: %d %s (%v)", rec.Code, rec.Body.String(), saved.Files)
	}
	if got := readFixture(t, workdir, "demo/main.bot"); strings.Contains(got, "vars:") {
		t.Fatalf("a bare vars: header was left in the main:\n%s", got)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != nodesWithVars {
		t.Fatalf("the fragment changed:\n%s", got)
	}
	// Every var deleted: the block's header stays where it was written,
	// as the writer keeps it on a single file.
	_, reopened := openPath(t, s, "demo/main.bot")
	emptied := editDocument(t, reopened.Document, func(m map[string]any) {
		m["vars"].(map[string]any)["fields"] = []any{}
	})
	rec, saved = savePath(t, s, "demo/main.bot", emptied, reopened.Unit.Revision)
	if rec.Code != http.StatusOK || len(saved.Files) != 1 || saved.Files[0] != "lib/nodes.bot" {
		t.Fatalf("empty the block: %d %s (%v)", rec.Code, rec.Body.String(), saved.Files)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.HasPrefix(got, "vars:\n") || strings.Contains(got, "depth") {
		t.Fatalf("the emptied block lost its header, or kept its entry:\n%s", got)
	}
}

// The control exclusion file is held to its content, not to its length:
// a tampered one is refused for what it is.
func TestSaveAUnitNamesATamperedExclusionFile(t *testing.T) {
	workdir := t.TempDir()
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
	// A private control directory, as the transaction creates it, whose
	// exclusion file says something else.
	control := filepath.Join(workdir, "demo", "lib", ".iterion", "authoring")
	if err := os.MkdirAll(control, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, ".gitignore"), []byte("*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{WorkDir: workdir}}
	_, opened := openPath(t, s, "demo/main.bot")
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
	})
	rec, _ := savePath(t, s, "demo/main.bot", edited, opened.Unit.Revision)
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "expected private * exclusion") || strings.Contains(rec.Body.String(), "read limit") {
		t.Fatalf("tampered exclusion file: %d %s", rec.Code, rec.Body.String())
	}
}

// The cloud editor's write-back presents the revision the document was
// opened at: absent it is refused, stale it is a conflict — a fragment a
// colleague changed since is never rewritten with this document's text —
// and the response carries the revision the patched bundle has.
func TestUnparseUnitPresentsAndReturnsTheRevision(t *testing.T) {
	s := &Server{}
	files := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes, "manifest.yaml": "name: demo\n"}
	rec := postDSL(t, s.handleParse, "/api/parse", map[string]any{"files": files, "main": "main.bot"})
	var opened struct {
		Document json.RawMessage `json:"document"`
		Unit     *unitInfo       `json:"unit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		wf := m["workflows"].([]any)[0].(map[string]any)
		wf["name"] = "renamed"
	})
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": edited, "files": files, "main": "main.bot"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no revision: %d %s", rec.Code, rec.Body.String())
	}
	// A colleague rewrote the fragment since the document was opened.
	changed := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": strings.Replace(unitFixtureNodes, "Do the thing.", "Do the other thing.", 1), "manifest.yaml": "name: demo\n"}
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": edited, "files": changed, "main": "main.bot", "revision": opened.Unit.Revision})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s — the colleague's fragment would be overwritten", rec.Code, rec.Body.String())
	}
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": edited, "files": files, "main": "main.bot", "revision": opened.Unit.Revision})
	if rec.Code != http.StatusOK {
		t.Fatalf("unparse: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Files    map[string]string `json:"files"`
		Revision string            `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	patched := map[string]string{"main.bot": out.Files["main.bot"], "lib/nodes.bot": unitFixtureNodes}
	if len(out.Files) != 1 || out.Revision == "" || out.Revision == opened.Unit.Revision || out.Revision != unit.LoadMap(patched, "main.bot").Digest {
		t.Fatalf("rewritten %v revision %q", out.Files, out.Revision)
	}
	// The main is a workflow file, on both endpoints: a manifest is never
	// written back as DSL.
	for _, h := range []http.HandlerFunc{s.handleParse, s.handleUnparse} {
		rec = postDSL(t, h, "/api/x", map[string]any{"document": edited, "files": files, "main": "manifest.yaml", "revision": opened.Unit.Revision})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("main = manifest.yaml: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// A fragment is checked through every workflow of the bundle that may
// import it — a fragment only a sibling imports included — and a
// diagnostic names the file as the bundle holds it, never the server's
// temporary directory.
func TestBundleValidationChecksAFragmentThroughEverySibling(t *testing.T) {
	sibling := "import \"lib/x.bot\"\n\nworkflow other:\n  entry: t\n  t -> done\n"
	files := map[string]string{
		"main.bot":  "workflow w:\n  entry: done\n",
		"child.bot": sibling,
		"lib/x.bot": "tool t:\n  command: \"true\"\n",
	}
	if diags := validateBundleCompileSelected(files, []string{"lib/x.bot"}); len(diags) != 0 {
		t.Fatalf("a sound fragment: %v", diags)
	}
	files["lib/x.bot"] = "tool t:\n  command: ?\n"
	diags := validateBundleCompileSelected(files, []string{"lib/x.bot"})
	if len(diags) == 0 || !strings.Contains(diags[0], "child.bot") {
		t.Fatalf("a fragment only a sibling imports went unchecked: %v", diags)
	}
	named := false
	for _, d := range diags {
		if strings.Contains(d, os.TempDir()) || strings.Contains(d, "botsource-validate") {
			t.Fatalf("a diagnostic names the server's temporary directory: %s", d)
		}
		if strings.Contains(d, "lib/x.bot:") {
			named = true
		}
	}
	if !named {
		t.Fatalf("no diagnostic names the fragment as the bundle holds it: %v", diags)
	}
}

// Deleting a file of a stored bot keeps the guards a put runs: the bundle
// must still compile without it, and its manifest still declare the floor
// its sources need.
func TestDeleteBotSourceFileKeepsTheGuards(t *testing.T) {
	pinServerBuild(t, "v3.145.0+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.145.0+abc123"}}
	edCtx := auth.WithIdentity(context.Background(), editor)
	files := map[string]string{
		"main.bot":      "import \"lib/nodes.bot\"\n\nworkflow main:\n  entry: worker\n  worker -> done\n",
		"lib/nodes.bot": "schema out:\n  ok: bool\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  description: \"x\"\n  output: out\n",
		"manifest.yaml": "name: multi\nversion: 1.0.0\nrequires:\n  iterion: \">= 3.145.0\"\n",
	}
	body, _ := json.Marshal(botSourcePutReq{Files: files})
	r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/multi", strings.NewReader(string(body))).WithContext(edCtx)
	r.SetPathValue("id", "t1")
	r.SetPathValue("slug", "multi")
	w := httptest.NewRecorder()
	s.handlePutBotSource(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", w.Code, w.Body.String())
	}
	del := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/multi/files/"+path, nil).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", "multi")
		r.SetPathValue("path", path)
		w := httptest.NewRecorder()
		s.handleDeleteBotSourceFile(w, r)
		return w
	}
	if w := del("manifest.yaml"); w.Code != http.StatusConflict {
		t.Fatalf("delete the manifest = %d %s, want 409: the floor the bundle needs went with it", w.Code, w.Body.String())
	}
	if w := del("lib/nodes.bot"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "E046") {
		t.Fatalf("delete an imported fragment = %d %s, want 400 naming the dangling import", w.Code, w.Body.String())
	}
	stored, err := s.botSources.GetBySlug(context.Background(), "t1", "multi")
	if err != nil || len(stored.Files) != 3 {
		t.Fatalf("the refused deletes changed the bundle: %v %v", err, stored.Files)
	}
	_ = botsource.MainBotFile
}

// The routes check what they change: a fragment put or deleted is checked
// through every workflow that may import it — a fragment only a sibling
// imports included — and a bundle that never compiled can still shed a
// file the compiler never reads.
func TestBotSourceFileRoutesCheckWhatTheyChange(t *testing.T) {
	pinServerBuild(t, "v3.145.0+deadbeef")
	s, editor, _ := newBotSourceTestServer(t)
	s.runnerBuilds = &fakeBuildObserver{builds: []string{"v3.145.0+abc123"}}
	edCtx := auth.WithIdentity(context.Background(), editor)
	files := map[string]string{
		"main.bot":      "workflow main:\n  entry: done\n",
		"child.bot":     "import \"lib/x.bot\"\n\nworkflow other:\n  entry: t\n  t -> done\n",
		"lib/x.bot":     "tool t:\n  command: \"true\"\n",
		"README.md":     "# demo\n",
		"manifest.yaml": "name: multi\nversion: 1.0.0\nrequires:\n  iterion: \">= 3.145.0\"\n",
	}
	seed := func(slug string, files map[string]string) {
		body, _ := json.Marshal(botSourcePutReq{Files: files})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug, strings.NewReader(string(body))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", slug)
		w := httptest.NewRecorder()
		s.handlePutBotSource(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("seed %s: %d %s", slug, w.Code, w.Body.String())
		}
	}
	seed("multi", files)
	putFile := func(slug, path, content string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"content": content})
		r := httptest.NewRequest("PUT", "/api/teams/t1/bot-sources/"+slug+"/files/"+path, strings.NewReader(string(body))).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", slug)
		r.SetPathValue("path", path)
		w := httptest.NewRecorder()
		s.handlePutBotSourceFile(w, r)
		return w
	}
	del := func(slug, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/teams/t1/bot-sources/"+slug+"/files/"+path, nil).WithContext(edCtx)
		r.SetPathValue("id", "t1")
		r.SetPathValue("slug", slug)
		r.SetPathValue("path", path)
		w := httptest.NewRecorder()
		s.handleDeleteBotSourceFile(w, r)
		return w
	}
	if w := putFile("multi", "lib/x.bot", "tool t:\n  command: ?\n"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "child.bot") {
		t.Fatalf("a broken fragment only a sibling imports was stored: %d %s", w.Code, w.Body.String())
	}
	if w := del("multi", "lib/x.bot"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "E046") {
		t.Fatalf("a fragment only a sibling imports was deleted: %d %s", w.Code, w.Body.String())
	}
	if w := del("multi", "README.md"); w.Code != http.StatusOK {
		t.Fatalf("a file the compiler never reads could not be deleted: %d %s", w.Code, w.Body.String())
	}
	// A root workflow deleted is not compiled from a path that is gone.
	if w := del("multi", "child.bot"); w.Code != http.StatusOK {
		t.Fatalf("a companion workflow could not be deleted: %d %s", w.Code, w.Body.String())
	}
	// A bundle that never compiled sheds a file the compiler never reads.
	broken := map[string]string{"main.bot": "workflow main:\n  entry: nope\n", "README.md": "# broken\n", "manifest.yaml": "name: broken\nversion: 1.0.0\n"}
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{TenantID: "t1", Slug: "broken", Files: broken, Origin: "tenant"}); err != nil {
		t.Fatal(err)
	}
	if w := del("broken", "README.md"); w.Code != http.StatusOK {
		t.Fatalf("a broken bundle could not shed README.md: %d %s", w.Code, w.Body.String())
	}
	stored, err := s.botSources.GetBySlug(context.Background(), "t1", "broken")
	if err != nil || len(stored.Files) != 2 {
		t.Fatalf("the delete did not land: %v %v", err, stored.Files)
	}
}

// A document of a bot in several files is never folded into one file:
// the single-file write-back refuses it, and only a render asked for
// display — the Source view — flattens it.
func TestUnparseRefusesToFoldAUnitIntoOneFile(t *testing.T) {
	s := &Server{}
	files := map[string]string{"main.bot": unitFixtureMain, "lib/nodes.bot": unitFixtureNodes}
	rec := postDSL(t, s.handleParse, "/api/parse", map[string]any{"files": files, "main": "main.bot"})
	var opened struct {
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatal(err)
	}
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": opened.Document})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a unit document rendered as one file: %d %s", rec.Code, rec.Body.String())
	}
	rec = postDSL(t, s.handleUnparse, "/api/unparse", map[string]any{"document": opened.Document, "flatten": true})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "agent worker") || strings.Contains(rec.Body.String(), "import ") {
		t.Fatalf("flatten for display: %d %s", rec.Code, rec.Body.String())
	}
}

// A text two files wrote inline is one prompt in the document and comes
// back in each file that refers to it: a save with no edit rewrites
// nothing, and a rewritten fragment keeps its inline text.
func TestSaveAUnitKeepsASharedInlinePromptInEachFile(t *testing.T) {
	workdir := t.TempDir()
	main := "import \"lib/nodes.bot\"\n\nagent one:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: \"Same text.\"\n\nworkflow w:\n  entry: one\n  one -> two\n  two -> done\n"
	nodes := "agent two:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: \"Same text.\"\n"
	writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": main, "demo/lib/nodes.bot": nodes})
	s := &Server{cfg: Config{WorkDir: workdir}}
	rec, opened := openPath(t, s, "demo/main.bot")
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	rec, saved := savePath(t, s, "demo/main.bot", opened.Document, opened.Unit.Revision)
	if rec.Code != http.StatusOK || len(saved.Files) != 0 {
		t.Fatalf("a save with no edit: %d %s rewrote %v", rec.Code, rec.Body.String(), saved.Files)
	}
	edited := editDocument(t, opened.Document, func(m map[string]any) {
		for _, a := range agentsOf(m) {
			if agent := a.(map[string]any); agent["name"] == "two" {
				agent["model"] = "anthropic/claude-opus-5"
			}
		}
	})
	rec, saved = savePath(t, s, "demo/main.bot", edited, saved.Revision)
	if rec.Code != http.StatusOK || len(saved.Files) != 1 || saved.Files[0] != "lib/nodes.bot" {
		t.Fatalf("edit the fragment's agent: %d %s rewrote %v", rec.Code, rec.Body.String(), saved.Files)
	}
	if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "system: \"Same text.\"") || strings.Contains(got, "_inline_") {
		t.Fatalf("the fragment lost its inline text:\n%s", got)
	}
	if got := readFixture(t, workdir, "demo/main.bot"); got != main {
		t.Fatalf("the main changed:\n%s", got)
	}
}
