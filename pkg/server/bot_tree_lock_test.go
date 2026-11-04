package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// treeChoreography drives the writers of one test by signals: it holds a
// writer at a seam of its write (treeGate), sees a writer queue on the bot
// tree's write lock (botTreeWriteHook), and joins every request it started
// before the test ends. Its 30-second bounds only report a choreography
// that broke down — a writer that never reached a seam — never a verdict
// on timing.
type treeChoreography struct {
	t     *testing.T
	mu    sync.Mutex
	waits map[string]chan struct{}
	gates []*treeGate
	reqs  []*treeRequest
}

// treeGate holds the first writer that arrives at its seam, when match (if
// set) holds then: a step of an authoring transaction (authoringStepHook) —
// "publish" is the moment the live file is moved aside and its successor not
// yet moved in — or treeAuditSeam, the end of a per-file lock's control
// audit (authoringListingHook ""), once the writer resolved, read and
// checked and before it takes its file's lock.
type treeGate struct {
	seam     string
	match    func() bool
	arrived  bool
	reached  chan struct{}
	released chan struct{}
	release  sync.Once
}

const treeAuditSeam = "audit"

func newTreeChoreography(t *testing.T) *treeChoreography {
	t.Helper()
	c := &treeChoreography{t: t, waits: map[string]chan struct{}{}}
	botTreeWriteHook = func(event, writer string) {
		key := writer
		switch event {
		case botTreeWaitEvent:
		case authoringGitWaitEvent:
			key = gitWaitKey(writer)
		default:
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		ch := c.waitChan(key)
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	authoringStepHook = func(step string) error {
		c.pass(step)
		return nil
	}
	authoringListingHook = func(name string) {
		if name == "" {
			c.pass(treeAuditSeam)
		}
	}
	t.Cleanup(func() {
		c.mu.Lock()
		gates, reqs := c.gates, c.reqs
		c.mu.Unlock()
		for _, g := range gates {
			g.let()
		}
		for _, r := range reqs {
			select {
			case <-r.done:
			case <-time.After(30 * time.Second):
				t.Errorf("a request of the choreography never answered")
			}
		}
		botTreeWriteHook, authoringStepHook, authoringListingHook = nil, nil, nil
	})
	return c
}

// gitWaitKey is the key first and waitChan know a wait on the repository's
// lock by: a writer's name on its own is its wait on the bot tree's.
func gitWaitKey(writer string) string { return authoringGitWaitEvent + "/" + writer }

// waitChan is writer's wait signal, closed at the first wait on the tree's
// lock of a writer of that name — on the repository's, for a gitWaitKey.
// c.mu is held.
func (c *treeChoreography) waitChan(writer string) chan struct{} {
	ch, ok := c.waits[writer]
	if !ok {
		ch = make(chan struct{})
		c.waits[writer] = ch
	}
	return ch
}

func (c *treeChoreography) hold(seam string, match func() bool) *treeGate {
	g := &treeGate{seam: seam, match: match, reached: make(chan struct{}), released: make(chan struct{})}
	c.mu.Lock()
	c.gates = append(c.gates, g)
	c.mu.Unlock()
	return g
}

func (c *treeChoreography) pass(seam string) {
	c.mu.Lock()
	var g *treeGate
	for _, candidate := range c.gates {
		if candidate.seam == seam && !candidate.arrived && (candidate.match == nil || candidate.match()) {
			candidate.arrived = true
			g = candidate
			break
		}
	}
	c.mu.Unlock()
	if g == nil {
		return
	}
	close(g.reached)
	<-g.released
}

func (g *treeGate) let() { g.release.Do(func() { close(g.released) }) }

// at waits for a writer to arrive at g.
func (c *treeChoreography) at(g *treeGate, what string) {
	c.t.Helper()
	select {
	case <-g.reached:
	case <-time.After(30 * time.Second):
		c.t.Fatalf("the choreography never reached %s", what)
	}
}

type treeRequest struct {
	done chan struct{}
	rec  *httptest.ResponseRecorder
}

// start serves r through handler aside, so a request that waits on a held
// writer cannot hang the test.
func (c *treeChoreography) start(handler http.HandlerFunc, r *http.Request) *treeRequest {
	req := &treeRequest{done: make(chan struct{}), rec: httptest.NewRecorder()}
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	go func() {
		defer close(req.done)
		handler(req.rec, r)
	}()
	return req
}

func (c *treeChoreography) answer(req *treeRequest, who string) *httptest.ResponseRecorder {
	c.t.Helper()
	select {
	case <-req.done:
		return req.rec
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s never answered", who)
		return nil
	}
}

// treeOutcome is what a request did first once started behind a held
// writer.
type treeOutcome string

const (
	treeQueued   treeOutcome = "queued on the bot tree's write lock"
	treeAnswered treeOutcome = "answered without queuing"
	treeAtGate   treeOutcome = "reached a held seam without queuing"
)

// first waits for req to queue on the tree's lock as writer, to answer, or
// to reach one of gates — whichever comes first.
func (c *treeChoreography) first(req *treeRequest, writer string, gates ...*treeGate) treeOutcome {
	c.t.Helper()
	c.mu.Lock()
	waited := c.waitChan(writer)
	c.mu.Unlock()
	reached := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	for _, g := range gates {
		go func() {
			select {
			case <-g.reached:
				select {
				case reached <- struct{}{}:
				case <-stop:
				}
			case <-stop:
			}
		}()
	}
	select {
	case <-waited:
		return treeQueued
	case <-req.done:
		return treeAnswered
	case <-reached:
		return treeAtGate
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s neither queued, answered nor reached a held seam", writer)
		return ""
	}
}

func treeJSON(t *testing.T, method, target string, payload any, pathValues ...string) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	for i := 0; i+1 < len(pathValues); i += 2 {
		r.SetPathValue(pathValues[i], pathValues[i+1])
	}
	return r
}

// requireTreeAnswer holds rec to code and, when want is set, to an error
// whose first line starts with want.
func requireTreeAnswer(t *testing.T, who string, rec *httptest.ResponseRecorder, code int, want string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("%s = %d %s; want %d %q", who, rec.Code, rec.Body.String(), code, want)
	}
	if want == "" {
		return
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s = %d %s: no error body", who, rec.Code, rec.Body.String())
	}
	if line, _, _ := strings.Cut(body.Error, "\n"); !strings.HasPrefix(line, want) {
		t.Fatalf("%s = %d %s; want %d %q", who, rec.Code, rec.Body.String(), code, want)
	}
}

func requireTreeQueued(t *testing.T, who string, outcome treeOutcome) {
	t.Helper()
	if outcome != treeQueued {
		t.Errorf("%s %s while another writer of the tree held its lock", who, outcome)
	}
}

func treeSnapshot(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contentSHA256(string(body))
}

func treeRequireAbsent(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the held writer left %s in place (%v): %s lands in no window", path, err, what)
	}
}

func absentNow(path string) func() bool {
	return func() bool {
		_, err := os.Lstat(path)
		return errors.Is(err, os.ErrNotExist)
	}
}

func treeReplace(editor, path, snapshot, before, after string) authoringChangeRequest {
	return authoringChangeRequest{EditorPath: editor, Changes: []authoringFileChange{{Scope: "bundle", Path: path, ExpectedSHA256: snapshot, Replacements: []authoringReplacement{{Before: before, After: after}}}}}
}

// treeCommit is the assistant's commit route of s.
func treeCommit(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.handleAuthoringChange(w, r, true) }
}

// Two commits decided on one snapshot have one winner, and the other
// decides on what the winner wrote: the conflict, never the request error a
// half-written bundle answers. The winner publishes by moving the live file
// aside before it moves its successor in; a loser resolving, reading or
// validating in between found the file absent (400 "unavailable", 404 on its
// resolution, 400 bot_compile_failed). Under the bot tree's write lock the
// loser queues until the winner is done, wherever the winner is.
func TestAuthoringCommitLoserWaitsForTheWinner(t *testing.T) {
	for _, tc := range []struct {
		name string
		seam string // where the winner is held when the loser comes in
	}{
		{name: "the winner has moved the file aside", seam: "publish"},
		{name: "the winner is past its checks, before its file lock", seam: treeAuditSeam},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, editor := authoringFixture(t)
			path := filepath.Join(s.cfg.WorkDir, filepath.FromSlash(editor))
			change := treeReplace(editor, "main.bot", treeSnapshot(t, path), "workflow main:", "workflow changed:")
			c := newTreeChoreography(t)
			gate := c.hold(tc.seam, nil)
			winner := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", change))
			c.at(gate, "the winner at "+tc.seam)
			if tc.seam == "publish" {
				treeRequireAbsent(t, path, "the loser")
			}
			loser := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", change))
			outcome := c.first(loser, botTreeWriterAuthoringCommit)
			gate.let()
			requireTreeAnswer(t, "loser", c.answer(loser, "the loser"), http.StatusConflict, "bundle:main.bot changed since the assistant snapshot")
			requireTreeAnswer(t, "winner", c.answer(winner, "the winner"), http.StatusOK, "")
			requireTreeQueued(t, "the loser", outcome)
			authoringBytes(t, path, "workflow changed:\n  entry: done\n")
		})
	}
}

// A git commit attests the bytes its snapshot read of each file. Another
// writer's publication of one of them moves it aside before it moves its
// successor in: the commit's read found the name absent, and answered 400
// "unavailable" for a file that is there. It queues behind the writer and
// answers the conflict its stale snapshot is.
func TestAuthoringGitCommitWaitsForTheWriterOfItsFiles(t *testing.T) {
	s, editor := authoringFixture(t)
	initAuthoringGit(t, s.cfg.WorkDir)
	path := filepath.Join(s.cfg.WorkDir, "project-bot", "subbots", "work.bot")
	snapshot := treeSnapshot(t, path)
	c := newTreeChoreography(t)
	gate := c.hold("publish", nil)
	writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "subbots/work.bot", snapshot, "workflow work:", "workflow reworked:")))
	c.at(gate, "the writer at publish")
	treeRequireAbsent(t, path, "the git commit")
	gitCommit := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: snapshot}},
		Message:    "chore(test): commit the snapshot",
	}))
	outcome := c.first(gitCommit, botTreeWriterAuthoringGitCommit)
	gate.let()
	requireTreeAnswer(t, "git commit", c.answer(gitCommit, "the git commit"), http.StatusConflict, "bundle:subbots/work.bot changed since the host snapshot")
	requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
	requireTreeQueued(t, "the git commit", outcome)
	authoringBytes(t, path, "workflow reworked:\n  entry: done\n")
}

// A commit of one file validates the whole bundle as it would leave it.
// Another writer's publication of the bundle's main.bot moved it aside: the
// validation read a bundle without it and refused a change that compiles
// (400 bot_compile_failed, which sends the assistant to fix code that is
// fine). The commit queues behind the writer and validates the bundle the
// writer left: both changes land.
func TestAuthoringCommitValidatesTheBundleTheWriterLeft(t *testing.T) {
	s, editor := authoringFixture(t)
	bundleDir := filepath.Join(s.cfg.WorkDir, "project-bot")
	mainBot, workBot := filepath.Join(bundleDir, "main.bot"), filepath.Join(bundleDir, "subbots", "work.bot")
	c := newTreeChoreography(t)
	gate := c.hold("publish", nil)
	writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "main.bot", treeSnapshot(t, mainBot), "workflow main:", "workflow changed:")))
	c.at(gate, "the writer at publish")
	treeRequireAbsent(t, mainBot, "the validation")
	reader := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "subbots/work.bot", treeSnapshot(t, workBot), "workflow work:", "workflow reworked:")))
	outcome := c.first(reader, botTreeWriterAuthoringCommit)
	gate.let()
	requireTreeAnswer(t, "reader", c.answer(reader, "the reader"), http.StatusOK, "")
	requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
	requireTreeQueued(t, "the reader", outcome)
	authoringBytes(t, mainBot, "workflow changed:\n  entry: done\n")
	authoringBytes(t, workBot, "workflow reworked:\n  entry: done\n")
}

// authoringNestedFixture is a bundle, project-bot, declaring its manifest,
// notes.md and subbots/work.bot — nested, when enclosing, in a workdir that
// is a bundle of its own declaring notes.md too.
func authoringNestedFixture(t *testing.T, enclosing bool) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"project-bot/manifest.yaml":    "schema_version: 1\nname: project-bot\nauthoring:\n  editable_files:\n    - {scope: bundle, path: manifest.yaml}\n    - {scope: bundle, path: notes.md}\n    - {scope: bundle, path: subbots/work.bot}\n",
		"project-bot/main.bot":         "workflow main:\n  entry: done\n",
		"project-bot/subbots/work.bot": "workflow work:\n  entry: done\n",
	}
	if enclosing {
		files["manifest.yaml"] = "schema_version: 1\nname: workspace\nauthoring:\n  editable_files:\n    - {scope: bundle, path: notes.md}\n"
	}
	writeFiles(t, root, files)
	return &Server{cfg: Config{WorkDir: root}}, "project-bot/main.bot"
}

// A commit resolves its bundle by walking up from the editor file to the
// nearest manifest. Another writer replacing that manifest moved it aside:
// the walk found no bundle (400 "no bundle manifest found"), or passed it by
// for the enclosing one and wrote a creation there (200, the file in the
// wrong bundle). The commit queues behind the writer and resolves the
// bundle the writer left.
func TestAuthoringCommitResolvesTheBundleTheWriterLeft(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enclosing bool
	}{
		{name: "no bundle encloses it"},
		{name: "the workdir is a bundle declaring the same file", enclosing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, editor := authoringNestedFixture(t, tc.enclosing)
			manifest := filepath.Join(s.cfg.WorkDir, "project-bot", "manifest.yaml")
			c := newTreeChoreography(t)
			gate := c.hold("publish", nil)
			writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "manifest.yaml", treeSnapshot(t, manifest), "schema_version: 1", "# edited\nschema_version: 1")))
			c.at(gate, "the writer at publish")
			treeRequireAbsent(t, manifest, "the resolution")
			var change authoringChangeRequest
			if tc.enclosing {
				change = authoringChangeRequest{EditorPath: editor, Changes: []authoringFileChange{{Scope: "bundle", Path: "notes.md", Create: &authoringCreate{Content: "notes\n"}}}}
			} else {
				change = treeReplace(editor, "subbots/work.bot", treeSnapshot(t, filepath.Join(s.cfg.WorkDir, "project-bot", "subbots", "work.bot")), "workflow work:", "workflow reworked:")
			}
			reader := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", change))
			outcome := c.first(reader, botTreeWriterAuthoringCommit)
			gate.let()
			requireTreeAnswer(t, "reader", c.answer(reader, "the reader"), http.StatusOK, "")
			requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
			if tc.enclosing {
				if _, err := os.Stat(filepath.Join(s.cfg.WorkDir, "notes.md")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the creation landed in the enclosing bundle (%v)", err)
				}
				authoringBytes(t, filepath.Join(s.cfg.WorkDir, "project-bot", "notes.md"), "notes\n")
			} else {
				authoringBytes(t, filepath.Join(s.cfg.WorkDir, "project-bot", "subbots", "work.bot"), "workflow reworked:\n  entry: done\n")
			}
			requireTreeQueued(t, "the reader", outcome)
		})
	}
}

// Two saves of a bot in several files opened at one revision have one
// winner, and the other decides on what the winner wrote: the conflict a
// reopen resolves, never "fix the files on disk". The winner's publication
// moves the fragment aside; a loser loading the unit in between found it
// missing (422 "do not load as one unit"). The loser queues behind it.
func TestSaveAUnitLoserWaitsForTheWinner(t *testing.T) {
	for _, tc := range []struct {
		name string
		seam string
	}{
		{name: "the winner has moved the fragment aside", seam: "publish"},
		{name: "the winner is past its checks, before its file locks", seam: treeAuditSeam},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes})
			s := &Server{cfg: Config{WorkDir: workdir}}
			fragment := filepath.Join(workdir, "demo", "lib", "nodes.bot")
			_, opened := openPath(t, s, "demo/main.bot")
			if opened.Unit == nil || opened.Unit.Revision == "" {
				t.Fatalf("the fixture opens as no unit: %+v", opened)
			}
			edited := editDocument(t, opened.Document, func(m map[string]any) {
				agentsOf(m)[0].(map[string]any)["model"] = "anthropic/claude-opus-5"
			})
			save := saveFileRequest{Path: "demo/main.bot", Document: edited, Revision: opened.Unit.Revision}
			c := newTreeChoreography(t)
			gate := c.hold(tc.seam, nil)
			winner := c.start(s.handleSaveFile, treeJSON(t, http.MethodPost, "/api/files/save", save))
			c.at(gate, "the winner at "+tc.seam)
			if tc.seam == "publish" {
				treeRequireAbsent(t, fragment, "the loser")
			}
			loser := c.start(s.handleSaveFile, treeJSON(t, http.MethodPost, "/api/files/save", save))
			outcome := c.first(loser, botTreeWriterEditorSave)
			gate.let()
			requireTreeAnswer(t, "loser", c.answer(loser, "the loser"), http.StatusConflict, "the files of demo/main.bot changed since the document was opened")
			requireTreeAnswer(t, "winner", c.answer(winner, "the winner"), http.StatusOK, "")
			requireTreeQueued(t, "the loser", outcome)
			if got := readFixture(t, workdir, "demo/lib/nodes.bot"); !strings.Contains(got, "anthropic/claude-opus-5") {
				t.Fatalf("the fragment is not the winner's:\n%s", got)
			}
		})
	}
}

// unitSkewOther is a sibling main of demo/main.bot importing its fragment
// lib/nodes.bot; unitSkewExtra a second fragment declaring a prompt helping.
const unitSkewOther = "import \"lib/nodes.bot\"\n\nworkflow other:\n  entry: worker\n  worker -> done\n"

const unitSkewExtra = "schema helped:\n  ok: bool\n\nprompt helping:\n  Help.\n\nagent helper:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: helping\n  output: helped\n"

// Finding (T50 round 2, saveUnit write skew): a save that rewrites a
// fragment checks that each sibling main importing it still compiles, then
// writes. Alice's save adds a prompt helping to lib/nodes.bot — other.bot
// compiles with it. Bob's save makes other.bot import lib/extra.bot, which
// declares helping too — other.bot compiles with the nodes.bot it read.
// Each is right alone; together other.bot declares helping twice. With
// Bob's save landing between Alice's sibling check and her write, both
// answered 200 and other.bot no longer compiled. Under the bot tree's write
// lock, held from the check through the write, the second save decides on
// what the first wrote: Bob's revision read the old fragment (409), Alice's
// check reads Bob's sibling (422).
func TestSaveAUnitSiblingCheckHoldsThroughItsWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		aliceFirst bool
	}{
		{name: "Alice checked the siblings and goes on to write", aliceFirst: true},
		{name: "Bob checked his unit and goes on to write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			writeUnitFixture(t, workdir, map[string]string{"demo/main.bot": unitFixtureMain, "demo/lib/nodes.bot": unitFixtureNodes, "demo/other.bot": unitSkewOther, "demo/lib/extra.bot": unitSkewExtra})
			s := &Server{cfg: Config{WorkDir: workdir}}
			applyIn := func(path, file, source string) saveFileRequest {
				_, opened := openPath(t, s, path)
				rec, applied := parseCall(t, s, map[string]any{"path": path, "file": file, "source": source})
				if rec.Code != http.StatusOK || applied.Unit == nil || opened.Unit == nil {
					t.Fatalf("apply of %s in %s: %d %s", file, path, rec.Code, rec.Body.String())
				}
				return saveFileRequest{Path: path, Document: applied.Document, Revision: opened.Unit.Revision, UnitFiles: applied.Unit.Files}
			}
			alice := applyIn("demo/main.bot", "lib/nodes.bot", unitFixtureNodes+"\nprompt helping:\n  Collide.\n")
			bobOther := "import \"lib/nodes.bot\"\nimport \"lib/extra.bot\"\n\nworkflow other:\n  entry: worker\n  worker -> helper\n  helper -> done\n"
			bob := applyIn("demo/other.bot", "other.bot", bobOther)

			c := newTreeChoreography(t)
			gate := c.hold(treeAuditSeam, nil)
			firstSave, secondSave, second := alice, bob, "Bob"
			if !tc.aliceFirst {
				firstSave, secondSave, second = bob, alice, "Alice"
			}
			first := c.start(s.handleSaveFile, treeJSON(t, http.MethodPost, "/api/files/save", firstSave))
			c.at(gate, "the first save past its checks")
			then := c.start(s.handleSaveFile, treeJSON(t, http.MethodPost, "/api/files/save", secondSave))
			outcome := c.first(then, botTreeWriterEditorSave)
			gate.let()
			firstRec, thenRec := c.answer(first, "the first save"), c.answer(then, second)
			// Whichever order: never two successes leaving the sibling broken.
			_, sibling := openPath(t, s, "demo/other.bot")
			if len(sibling.Diagnostics) != 0 || !sibling.Bindable {
				t.Fatalf("other.bot no longer opens clean on disk (first save %d, %s's %d): %v", firstRec.Code, second, thenRec.Code, sibling.Diagnostics)
			}
			if tc.aliceFirst {
				requireTreeAnswer(t, "Bob", thenRec, http.StatusConflict, "the files of demo/other.bot changed since the document was opened")
				requireTreeAnswer(t, "Alice", firstRec, http.StatusOK, "")
				if got := readFixture(t, workdir, "demo/other.bot"); got != unitSkewOther {
					t.Fatalf("Bob's refused save wrote other.bot:\n%s", got)
				}
			} else {
				requireTreeAnswer(t, "Alice", thenRec, http.StatusUnprocessableEntity, "saving would break other.bot")
				requireTreeAnswer(t, "Bob", firstRec, http.StatusOK, "")
				if got := readFixture(t, workdir, "demo/lib/nodes.bot"); got != unitFixtureNodes {
					t.Fatalf("Alice's refused save rewrote the fragment:\n%s", got)
				}
			}
			requireTreeQueued(t, second, outcome)
		})
	}
}

// metaBotFixture is a workspace with bots/meta-bot — and, when shadowed, a
// twin of the same name under examples/ that discovery shadows — served by a
// server with an org.
func metaBotFixture(t *testing.T, shadowed bool, extra map[string]string) (*Server, string) {
	t.Helper()
	work := t.TempDir()
	files := map[string]string{
		"bots/meta-bot/main.bot":      metaBakedBot,
		"bots/meta-bot/manifest.yaml": "name: meta-bot\nversion: 1.0.0\ndescription: start\nenabled: true\n",
	}
	if shadowed {
		files["examples/meta-bot/main.bot"] = metaBakedBot
		files["examples/meta-bot/manifest.yaml"] = treeTwinManifest
	}
	for rel, text := range extra {
		files[rel] = text
	}
	writeFiles(t, work, files)
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	return s, work
}

const treeTwinManifest = "name: meta-bot\nversion: 9.9.9\ndescription: shadowed\nenabled: true\n"

type treeEntry struct {
	Name    string `json:"name"`
	RelPath string `json:"rel_path"`
	Enabled bool   `json:"enabled"`
}

func treeEntryOf(t *testing.T, rec *httptest.ResponseRecorder) treeEntry {
	t.Helper()
	var entry treeEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatalf("answer %d is no entry: %s", rec.Code, rec.Body.String())
	}
	return entry
}

// Finding (T50 round 2, the settle chokepoint): the metadata edit and the
// workspace toggle resolve their bot through the catalog, which reads
// manifests without their locks. A writer replacing the bot's manifest moved
// it aside: the lookup listed no such bot (404 — after its write, for the
// toggle), or the shadowed twin that claims the name meanwhile (the edit
// patched the twin). A settle step that scanned the writers' locks once
// missed a writer whose lock was born after the scan, and decided in the
// middle of that writer's publication. Under the bot tree's write lock there
// is no scan: the request queues behind every writer ahead of it — whenever
// that writer's lock was born — and resolves the tree they left.
func TestBotMetadataEditsWaitForEveryWriterAhead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		overlay  bool
		shadowed bool
	}{
		{name: "the edit, its manifest moved aside"},
		{name: "the edit beside a shadowed twin", shadowed: true},
		{name: "the toggle, its manifest moved aside", overlay: true},
		{name: "the toggle beside a shadowed twin", overlay: true, shadowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, work := metaBotFixture(t, tc.shadowed, nil)
			manifest := filepath.Join(work, "bots", "meta-bot", "manifest.yaml")
			c := newTreeChoreography(t)
			gate := c.hold("publish", nil)
			writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace("bots/meta-bot/main.bot", "manifest.yaml", treeSnapshot(t, manifest), "description: start", "description: edited")))
			c.at(gate, "the writer at publish")
			treeRequireAbsent(t, manifest, "the lookup")
			route, target, name := s.handleBotsPut, "/api/v1/bots/meta-bot", botTreeWriterBotMetadata
			if tc.overlay {
				route, target, name = s.handleBotOverlay, "/api/v1/bots/meta-bot/overlay", botTreeWriterBotOverlay
			}
			edit := c.start(route, treeJSON(t, http.MethodPut, target, map[string]any{"enabled": false}, "name", "meta-bot"))
			outcome := c.first(edit, name)
			gate.let()
			rec := c.answer(edit, "the edit")
			requireTreeAnswer(t, "edit", rec, http.StatusOK, "")
			if entry := treeEntryOf(t, rec); entry.Name != "meta-bot" || entry.RelPath != "bots/meta-bot" || entry.Enabled {
				t.Fatalf("the edit answered %s; want bots/meta-bot, disabled", rec.Body.String())
			}
			requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
			if tc.shadowed {
				if got := readFixture(t, work, "examples/meta-bot/manifest.yaml"); got != treeTwinManifest {
					t.Fatalf("the edit wrote into the shadowed twin:\n%s", got)
				}
			}
			m, err := bundle.LoadManifest(manifest)
			if err != nil || m == nil || m.Description != "edited" || (!tc.overlay && m.IsEnabled()) {
				t.Fatalf("bots/meta-bot's manifest = %+v, %v; want the writer's description and, for the edit, the disable", m, err)
			}
			requireTreeQueued(t, "the edit", outcome)
		})
	}

	// The finding's own sequence: the edit queues behind one writer — here
	// another bot's edit — and a second writer, of the edited bot's
	// manifest, starts behind it with no lock of that bot born yet. The
	// edit decides once the first is done, before the second moves
	// anything; the second then answers the conflict its stale snapshot is.
	t.Run("a writer whose lock is born after the edit began waiting", func(t *testing.T) {
		s, work := metaBotFixture(t, false, map[string]string{
			"bots/other/main.bot":      metaBakedBot,
			"bots/other/manifest.yaml": "name: other\nversion: 1.0.0\ndescription: start\nenabled: true\n",
		})
		manifest := filepath.Join(work, "bots", "meta-bot", "manifest.yaml")
		snapshot := treeSnapshot(t, manifest)
		c := newTreeChoreography(t)
		ahead := c.hold("replace:staged", nil)
		otherEdit := c.start(s.handleBotsPut, treeJSON(t, http.MethodPut, "/api/v1/bots/other", map[string]any{"description": "other"}, "name", "other"))
		c.at(ahead, "the other bot's edit with its candidate staged")
		edit := c.start(s.handleBotsPut, treeJSON(t, http.MethodPut, "/api/v1/bots/meta-bot", map[string]any{"enabled": false}, "name", "meta-bot"))
		editOutcome := c.first(edit, botTreeWriterBotMetadata)
		if _, err := os.Lstat(filepath.Join(work, "bots", "meta-bot", ".iterion")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bots/meta-bot holds a lock before its writer starts (%v): its lock is born no later than the edit's wait", err)
		}
		behind := c.hold("publish", absentNow(manifest))
		writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace("bots/meta-bot/main.bot", "manifest.yaml", snapshot, "description: start", "description: edited")))
		writerOutcome := c.first(writer, botTreeWriterAuthoringCommit, behind)
		ahead.let()
		rec := c.answer(edit, "the edit")
		behind.let()
		requireTreeAnswer(t, "edit", rec, http.StatusOK, "")
		if entry := treeEntryOf(t, rec); entry.RelPath != "bots/meta-bot" || entry.Enabled {
			t.Fatalf("the edit answered %s; want bots/meta-bot, disabled", rec.Body.String())
		}
		requireTreeAnswer(t, "the other bot's edit", c.answer(otherEdit, "the other bot's edit"), http.StatusOK, "")
		requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusConflict, "bundle:manifest.yaml changed since the assistant snapshot")
		requireTreeQueued(t, "the edit", editOutcome)
		requireTreeQueued(t, "the writer", writerOutcome)
		m, err := bundle.LoadManifest(manifest)
		if err != nil || m == nil || m.IsEnabled() || m.Description != "start" {
			t.Fatalf("bots/meta-bot's manifest = %+v, %v; want the edit's disable alone", m, err)
		}
	})
}

// Finding (T50 round 2, the stale lookup): the metadata edit picked its
// bundle on the catalog, then took the manifest's file lock. An assistant
// commit renaming the bot in between left the edit writing enabled: false
// into a manifest that now named another bot — renamed-bot silently
// disabled — and answering 404 for the bot it was asked to edit. Under the
// bot tree's write lock the edit holds its lookup through its write: the
// rename queues behind it and answers the conflict, or goes first and the
// edit finds no such bot and writes nothing.
func TestBotMetadataEditWritesTheBotItResolved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		editFirst bool
	}{
		{name: "the edit resolved the bot, a rename comes in", editFirst: true},
		{name: "the rename moved the manifest aside, the edit comes in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, work := metaBotFixture(t, false, nil)
			manifest := filepath.Join(work, "bots", "meta-bot", "manifest.yaml")
			rename := treeReplace("bots/meta-bot/main.bot", "manifest.yaml", treeSnapshot(t, manifest), "name: meta-bot", "name: renamed-bot")
			editRequest := func() *http.Request {
				return treeJSON(t, http.MethodPut, "/api/v1/bots/meta-bot", map[string]any{"enabled": false}, "name", "meta-bot")
			}
			c := newTreeChoreography(t)
			if tc.editFirst {
				gate := c.hold(treeAuditSeam, nil)
				edit := c.start(s.handleBotsPut, editRequest())
				c.at(gate, "the edit past its lookup")
				renamer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", rename))
				outcome := c.first(renamer, botTreeWriterAuthoringCommit)
				gate.let()
				rec, renamed := c.answer(edit, "the edit"), c.answer(renamer, "the rename")
				if m, err := bundle.LoadManifest(manifest); err != nil || m == nil || m.Name != "meta-bot" || m.IsEnabled() {
					t.Fatalf("the manifest = %+v, %v (edit %d, rename %d); want meta-bot, disabled — never another bot disabled", m, err, rec.Code, renamed.Code)
				}
				requireTreeAnswer(t, "edit", rec, http.StatusOK, "")
				if entry := treeEntryOf(t, rec); entry.Name != "meta-bot" || entry.Enabled {
					t.Fatalf("the edit answered %s; want meta-bot, disabled", rec.Body.String())
				}
				requireTreeAnswer(t, "rename", renamed, http.StatusConflict, "bundle:manifest.yaml changed since the assistant snapshot")
				requireTreeQueued(t, "the rename", outcome)
				return
			}
			gate := c.hold("publish", nil)
			renamer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", rename))
			c.at(gate, "the rename at publish")
			treeRequireAbsent(t, manifest, "the edit")
			edit := c.start(s.handleBotsPut, editRequest())
			outcome := c.first(edit, botTreeWriterBotMetadata)
			gate.let()
			requireTreeAnswer(t, "edit", c.answer(edit, "the edit"), http.StatusNotFound, "bots: \"meta-bot\" not found")
			requireTreeAnswer(t, "rename", c.answer(renamer, "the rename"), http.StatusOK, "")
			requireTreeQueued(t, "the edit", outcome)
			m, err := bundle.LoadManifest(manifest)
			if err != nil || m == nil || m.Name != "renamed-bot" || !m.IsEnabled() {
				t.Fatalf("the manifest = %+v, %v; want renamed-bot, still enabled", m, err)
			}
		})
	}
}

// heldTriggerStore is a subscription store whose listing of a bot's
// subscriptions — the from-invocation route's duplicate check, after it
// resolved the bot and before it writes — passes through a seam.
type heldTriggerStore struct {
	trigger.SubscriptionStore
	c *treeChoreography
}

const treeTriggerSeam = "trigger list"

func (h heldTriggerStore) ListByBot(ctx context.Context, tenantID, botID string) ([]trigger.Subscription, error) {
	h.c.pass(treeTriggerSeam)
	return h.SubscriptionStore.ListByBot(ctx, tenantID, botID)
}

// Finding (T50 round 2, the triggers route): the bot home's "enable this
// trigger" derives a subscription from the bot's manifest, resolved through
// the catalog outside any hold. A writer replacing that manifest moved it
// aside: the route answered 404 for a bot that is there. Under the bot
// tree's write lock the route queues behind the writer and derives from the
// manifest it left; a writer coming in after the route resolved queues
// behind the route's write.
func TestTriggerFromInvocationWaitsForTheWriterOfItsBot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writerFirst bool
		cron        string
	}{
		{name: "the writer moved the manifest aside, the route comes in", writerFirst: true, cron: "0 8 * * 1"},
		{name: "the route resolved the bot, the writer comes in", cron: "0 7 * * 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			writeFiles(t, workdir, map[string]string{
				"bots/trig-bot/main.bot":      "workflow w:\n  agent a:\n    model: \"test\"\n  a -> done\n\nagent a:\n  model: \"test\"\n",
				"bots/trig-bot/manifest.yaml": "name: trig-bot\nschema_version: 1\ninvocations:\n  - kind: schedule\n    schedule:\n      suggested_cron: \"0 7 * * 1\"\n",
			})
			c := newTreeChoreography(t)
			s := New(Config{DisableAuth: true, WorkDir: workdir, TriggerStore: heldTriggerStore{SubscriptionStore: trigger.NewMemorySubscriptionStore(), c: c}}, iterlog.New(iterlog.LevelError, nil))
			manifest := filepath.Join(workdir, "bots", "trig-bot", "manifest.yaml")
			change := treeReplace("bots/trig-bot/main.bot", "manifest.yaml", treeSnapshot(t, manifest), "\"0 7 * * 1\"", "\"0 8 * * 1\"")
			enable := func() *http.Request {
				return treeJSON(t, http.MethodPost, "/api/v1/bots/trig-bot/triggers/from-invocation", map[string]any{"index": 0}, "name", "trig-bot")
			}
			var route, writer *treeRequest
			var outcome treeOutcome
			if tc.writerFirst {
				gate := c.hold("publish", nil)
				writer = c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", change))
				c.at(gate, "the writer at publish")
				treeRequireAbsent(t, manifest, "the route")
				route = c.start(s.handleTriggerFromInvocation, enable())
				outcome = c.first(route, botTreeWriterTriggerInvocation)
				gate.let()
			} else {
				gate := c.hold(treeTriggerSeam, nil)
				route = c.start(s.handleTriggerFromInvocation, enable())
				c.at(gate, "the route past its resolution")
				writer = c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", change))
				outcome = c.first(writer, botTreeWriterAuthoringCommit)
				gate.let()
			}
			rec := c.answer(route, "the route")
			requireTreeAnswer(t, "route", rec, http.StatusCreated, "")
			var sub trigger.Subscription
			if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil || sub.BotID != "trig-bot" || sub.Cron != tc.cron {
				t.Fatalf("the route answered %s (%v); want trig-bot's subscription at %q", rec.Body.String(), err, tc.cron)
			}
			requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
			requireTreeQueued(t, "the second request", outcome)
		})
	}
}

// A create decides whether its name is free on the catalog. A writer
// replacing the manifest of the bot holding that name moved it aside: the
// name read free, and the create scaffolded a duplicate that took the
// routing from the bot that is there (201). The create queues behind the
// writer and finds the name taken.
func TestBotCreateDecidesTheNameOnTheTreeTheWriterLeft(t *testing.T) {
	work := t.TempDir()
	writeFiles(t, work, map[string]string{
		"examples/meta-bot/main.bot":      metaBakedBot,
		"examples/meta-bot/manifest.yaml": "name: meta-bot\nversion: 1.0.0\ndescription: there\nenabled: true\n",
	})
	s := newOrgTestServer(t)
	s.cfg.WorkDir = work
	manifest := filepath.Join(work, "examples", "meta-bot", "manifest.yaml")
	c := newTreeChoreography(t)
	gate := c.hold("publish", nil)
	writer := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace("examples/meta-bot/main.bot", "manifest.yaml", treeSnapshot(t, manifest), "description: there", "description: edited")))
	c.at(gate, "the writer at publish")
	treeRequireAbsent(t, manifest, "the create")
	create := c.start(s.handleBotCreate, treeJSON(t, http.MethodPost, "/api/v1/bots", map[string]any{"slug": "meta-bot", "instructions": "Do the thing."}))
	outcome := c.first(create, botTreeWriterBotCreate)
	gate.let()
	requireTreeAnswer(t, "create", c.answer(create, "the create"), http.StatusConflict, "bot name already in use")
	requireTreeAnswer(t, "writer", c.answer(writer, "the writer"), http.StatusOK, "")
	requireTreeQueued(t, "the create", outcome)
	if _, err := os.Stat(filepath.Join(work, "bots", "meta-bot")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused create scaffolded bots/meta-bot (%v)", err)
	}
}

// Every handler that writes the bot tree, or resolves a bot and then writes,
// takes the tree's write lock before it resolves anything: with the lock
// held elsewhere it queues under its own name, and a request that ends while
// it waits is answered 503 bot_tree_busy with nothing written. A preview and a stored
// (botsource://) commit write nothing of the tree and take nothing.
func TestEveryBotTreeWriterTakesTheWriteLock(t *testing.T) {
	type writerCase struct {
		writer string
		// setup builds the server and the request; written reports what the
		// handler wrote that it must not have, "" when nothing.
		setup func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string)
	}
	// The handlers that read no request body.
	bodyless := map[string]bool{botTreeWriterMarketplace: true, botTreeWriterMarketplaceRemove: true}
	absent := func(paths ...string) func() string {
		return func() string {
			for _, p := range paths {
				if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
					return p
				}
			}
			return ""
		}
	}
	unchanged := func(t *testing.T, path string) func() string {
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return func() string {
			if now, _ := os.ReadFile(path); !bytes.Equal(now, before) {
				return path
			}
			return ""
		}
	}
	authoring := func(t *testing.T) (*Server, string, string) {
		s, editor := authoringFixture(t)
		return s, editor, filepath.Join(s.cfg.WorkDir, filepath.FromSlash(editor))
	}
	cases := []writerCase{
		{botTreeWriterAuthoringCommit, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, editor, path := authoring(t)
			return s, treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "main.bot", treeSnapshot(t, path), "workflow main:", "workflow changed:")), unchanged(t, path)
		}},
		{botTreeWriterAuthoringGitCommit, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, editor, _ := authoring(t)
			initAuthoringGit(t, s.cfg.WorkDir)
			work := filepath.Join(s.cfg.WorkDir, "project-bot", "subbots", "work.bot")
			writeFiles(t, s.cfg.WorkDir, map[string]string{"project-bot/subbots/work.bot": "workflow work:\n  entry: published\n"})
			head := filepath.Join(s.cfg.WorkDir, ".git", "HEAD")
			ref, err := os.ReadFile(head)
			if err != nil {
				t.Fatal(err)
			}
			refPath := filepath.Join(s.cfg.WorkDir, ".git", strings.TrimSpace(strings.TrimPrefix(string(ref), "ref:")))
			return s, s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{EditorPath: editor, Files: []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}}, Message: "chore(test): commit"}), unchanged(t, refPath)
		}},
		{botTreeWriterAuthoringGitPush, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, editor, _ := authoring(t)
			return s, s.handleAuthoringGitPublish, treeJSON(t, http.MethodPost, "/git-push", authoringGitPublishRequest{EditorPath: editor, Commit: "0123456789ab", Branch: "copi/test"}), func() string { return "" }
		}},
		{botTreeWriterEditorSave, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, editor, path := authoring(t)
			return s, s.handleSaveFile, treeJSON(t, http.MethodPost, "/api/files/save", saveFileRequest{Path: editor, Document: json.RawMessage(`{"workflows":[{"name":"main","entry":"done"}]}`), Digest: treeSnapshot(t, path)}), unchanged(t, path)
		}},
		{botTreeWriterRunFileSave, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, _, path := authoring(t)
			rs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run, err := rs.CreateRun(t.Context(), "tree-run", "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.WorkDir = filepath.Dir(path)
			if err := rs.SaveRun(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
			return s, s.handleSaveRunFileContent, treeJSON(t, http.MethodPut, "/api/runs/tree-run/files/content", saveRunFileRequest{Path: "main.bot", Content: "run content"}, "id", run.ID), unchanged(t, path)
		}},
		{botTreeWriterBotMetadata, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, work := metaBotFixture(t, false, nil)
			return s, s.handleBotsPut, treeJSON(t, http.MethodPut, "/api/v1/bots/meta-bot", map[string]any{"enabled": false}, "name", "meta-bot"), unchanged(t, filepath.Join(work, "bots", "meta-bot", "manifest.yaml"))
		}},
		{botTreeWriterBotOverlay, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, work := metaBotFixture(t, false, nil)
			return s, s.handleBotOverlay, treeJSON(t, http.MethodPut, "/api/v1/bots/meta-bot/overlay", map[string]any{"enabled": false}, "name", "meta-bot"), absent(filepath.Join(work, ".iterion", "bot-overrides.yaml"))
		}},
		{botTreeWriterTriggerInvocation, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s := newFromInvocationServer(t)
			subs := s.cfg.TriggerStore
			return s, s.handleTriggerFromInvocation, treeJSON(t, http.MethodPost, "/api/v1/bots/trig-bot/triggers/from-invocation", map[string]any{"index": 0}, "name", "trig-bot"), func() string {
				if listed, _ := subs.ListByBot(context.Background(), "", "trig-bot"); len(listed) != 0 {
					return "a subscription"
				}
				return ""
			}
		}},
		{botTreeWriterBotCreate, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, work := metaBotFixture(t, false, nil)
			return s, s.handleBotCreate, treeJSON(t, http.MethodPost, "/api/v1/bots", map[string]any{"slug": "fresh-bot", "instructions": "Do the thing."}), absent(filepath.Join(work, "bots", "fresh-bot"))
		}},
		{botTreeWriterBotImport, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, work := metaBotFixture(t, false, nil)
			return s, s.handleBotImport, treeJSON(t, http.MethodPost, "/api/v1/bots/import", botImportRequest{Source: importScriptJS}), absent(filepath.Join(work, "bots", "hello_flow.bot"))
		}},
		{botTreeWriterBotInstall, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			repo := t.TempDir()
			writeFixtureBundle(t, repo, "mybot")
			s, work := metaBotFixture(t, false, nil)
			return s, s.handleBotInstall, treeJSON(t, http.MethodPost, "/api/v1/bots/install", botInstallRequest{URL: repo}), absent(filepath.Join(work, ".botz"))
		}},
		{botTreeWriterBotUpload, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, work := metaBotFixture(t, false, nil)
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			part, err := form.CreateFormFile("file", "bot.botz")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write([]byte("not an archive")); err != nil {
				t.Fatal(err)
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/bots/upload", &body)
			r.Header.Set("Content-Type", form.FormDataContentType())
			return s, s.handleBotUpload, r, absent(filepath.Join(work, ".botz"))
		}},
		{botTreeWriterMarketplace, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			repo := t.TempDir()
			writeFixtureBundle(t, repo, "mybot")
			work := t.TempDir()
			s := newMarketplaceServer(t, work)
			if err := s.marketplace.Upsert(t.Context(), marketplace.Entry{Slug: "mybot", Name: "mybot", RepoURL: repo}); err != nil {
				t.Fatal(err)
			}
			return s, s.handleMarketplaceInstall, treeJSON(t, http.MethodPost, "/api/v1/marketplace/bots/mybot/install", nil, "slug", "mybot"), absent(filepath.Join(work, ".botz"))
		}},
		{botTreeWriterMarketplaceRemove, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			work := t.TempDir()
			writeFiles(t, work, map[string]string{".botz/mybot/main.bot": metaBakedBot, ".botz/mybot/manifest.yaml": "name: mybot\nversion: 1.0.0\n"})
			s := newMarketplaceServer(t, work)
			if err := s.marketplace.Upsert(t.Context(), marketplace.Entry{Slug: "mybot", Name: "mybot", RepoURL: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			return s, s.handleMarketplaceUninstall, treeJSON(t, http.MethodDelete, "/api/v1/marketplace/bots/mybot/install", nil, "slug", "mybot"), unchanged(t, filepath.Join(work, ".botz", "mybot", "manifest.yaml"))
		}},
		{botTreeWriterDependencyUpdate, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s := New(Config{DisableAuth: true, SkipProjectRegistration: true, WorkDir: t.TempDir()}, iterlog.Nop())
			return s, s.handleAssistantDependencyBotsUpdate, treeJSON(t, http.MethodPost, "/api/v1/assistant/dependencies/bots-update", assistantDependencyBotsUpdateRequest{Name: "shared", Ref: "0123456789abcdef0123456789abcdef01234567", Message: "bump shared"}), func() string { return "" }
		}},
		{botTreeWriterDependencyLocalize, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
			s, editor, _ := authoring(t)
			return s, s.handleAssistantDependencyBotsLocalize, treeJSON(t, http.MethodPost, "/api/v1/assistant/dependencies/bots-localize", assistantDependencyBotsLocalizeRequest{EditorPath: editor, Name: "shared", Message: "localize shared"}), func() string { return "" }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.writer, func(t *testing.T) {
			s, handler, r, written := tc.setup(t)
			release, err := s.holdBotTreeWrites(t.Context(), "the test")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			body := countRequestBody(t, r)
			c := newTreeChoreography(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := c.start(handler, r.WithContext(ctx))
			outcome := c.first(req, tc.writer)
			read := body.read.Load()
			cancel()
			rec := c.answer(req, tc.writer)
			if outcome != treeQueued {
				t.Fatalf("%s %s while the test held the bot tree's lock: %d %s", tc.writer, outcome, rec.Code, rec.Body.String())
			}
			// The body is read before the lock is taken: a client that
			// stalls its body never holds every writer of the tree.
			if !bodyless[tc.writer] && read != body.size {
				t.Fatalf("%s queued on the bot tree's lock having read %d of its %d body bytes: its client's pace holds the lock", tc.writer, read, body.size)
			}
			var busy struct {
				Code string `json:"error_code"`
			}
			if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &busy) != nil || busy.Code != botTreeBusyCode {
				t.Fatalf("%s = %d %s; want 503 %s", tc.writer, rec.Code, rec.Body.String(), botTreeBusyCode)
			}
			if what := written(); what != "" {
				t.Fatalf("%s wrote %s while it waited for the lock", tc.writer, what)
			}
		})
	}

	t.Run("the writers of the repository's index take its lock", func(t *testing.T) {
		var gitCommit func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string)
		for _, tc := range cases {
			if tc.writer == botTreeWriterAuthoringGitCommit {
				gitCommit = tc.setup
			}
		}
		for _, tc := range []struct {
			writer string
			setup  func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string)
		}{
			{botTreeWriterAuthoringGitCommit, gitCommit},
			{botTreeWriterDependencyUpdate, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
				s, ref := assistantDependencyFixture(t)
				lock := filepath.Join(s.cfg.WorkDir, "bots.lock")
				before := unchanged(t, lock)
				return s, s.handleAssistantDependencyBotsUpdate, treeJSON(t, http.MethodPost, "/api/v1/assistant/dependencies/bots-update", assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: ref, Message: "chore(bots): refresh shared planner"}), func() string {
					if what := before(); what != "" {
						return what
					}
					if left, _ := filepath.Glob(filepath.Join(s.cfg.WorkDir, ".git", "iterion-dependency-*")); len(left) != 0 {
						return left[0]
					}
					return ""
				}
			}},
			{botTreeWriterDependencyLocalize, func(t *testing.T) (*Server, http.HandlerFunc, *http.Request, func() string) {
				s, root, _ := assistantDependencyLocalizeFixture(t)
				return s, s.handleAssistantDependencyBotsLocalize, treeJSON(t, http.MethodPost, "/api/v1/assistant/dependencies/bots-localize", assistantDependencyBotsLocalizeRequest{EditorPath: "bots/planner/main.bot", Name: "shared-planner", Message: "localize shared planner"}), absent(filepath.Join(root, "bots", "planner", "plugins"))
			}},
		} {
			t.Run(tc.writer, func(t *testing.T) {
				s, handler, r, written := tc.setup(t)
				release, err := s.holdAuthoringGitWrites(t.Context(), "the test")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				c := newTreeChoreography(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := c.start(handler, r.WithContext(ctx))
				outcome := c.first(req, gitWaitKey(tc.writer))
				cancel()
				rec := c.answer(req, tc.writer)
				if outcome != treeQueued {
					t.Fatalf("%s %s while the test held the repository's lock: %d %s", tc.writer, outcome, rec.Code, rec.Body.String())
				}
				var busy struct {
					Code string `json:"error_code"`
				}
				if rec.Code != http.StatusServiceUnavailable || json.Unmarshal(rec.Body.Bytes(), &busy) != nil || busy.Code != botTreeBusyCode {
					t.Fatalf("%s = %d %s; want 503 %s", tc.writer, rec.Code, rec.Body.String(), botTreeBusyCode)
				}
				if what := written(); what != "" {
					t.Fatalf("%s wrote %s while it waited for the repository's lock", tc.writer, what)
				}
			})
		}
	})

	t.Run("a preview and a stored commit take nothing", func(t *testing.T) {
		s, editor, path := authoring(t)
		release, err := s.holdBotTreeWrites(t.Context(), "the test")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		c := newTreeChoreography(t)
		for _, tc := range []struct {
			name    string
			handler http.HandlerFunc
			change  authoringChangeRequest
			code    int
			want    string
		}{
			{"preview", s.handleAuthoringPreview, treeReplace(editor, "main.bot", treeSnapshot(t, path), "workflow main:", "workflow changed:"), http.StatusOK, ""},
			{"stored commit", treeCommit(s), treeReplace(botSourceEditorScheme+"team/slug/main.bot", "main.bot", strings.Repeat("0", 64), "a", "b"), http.StatusBadRequest, "bot editing is not enabled on this server"},
		} {
			ctx, cancel := context.WithCancel(t.Context())
			req := c.start(tc.handler, treeJSON(t, http.MethodPost, "/authoring", tc.change).WithContext(ctx))
			outcome := c.first(req, botTreeWriterAuthoringCommit)
			cancel()
			rec := c.answer(req, tc.name)
			if outcome != treeAnswered {
				t.Fatalf("the %s %s: %d %s", tc.name, outcome, rec.Code, rec.Body.String())
			}
			requireTreeAnswer(t, tc.name, rec, tc.code, tc.want)
		}
	})
}

// countedBody is a request body that counts the bytes its handler read.
type countedBody struct {
	r    io.Reader
	size int64
	read atomic.Int64
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countedBody) Close() error { return nil }

// countRequestBody replaces r's body with one that counts what is read of it.
func countRequestBody(t *testing.T, r *http.Request) *countedBody {
	t.Helper()
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := &countedBody{r: bytes.NewReader(payload), size: int64(len(payload))}
	r.Body = body
	return body
}
