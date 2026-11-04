//go:build linux || darwin

package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

// programGate holds a program git runs — an upload-pack, a fake ssh, a hook —
// at its first run, until the test lets it go: the moment a writer is at its
// remote or in its hooks, decided by a signal and never by a clock. Both
// FIFOs are held open read-write by the test, so neither the program's
// signal nor its wait ever blocks on the test opening them.
type programGate struct {
	first      string
	sig, rel   *os.File
	sigN, relN string
}

func newProgramGate(t *testing.T) *programGate {
	t.Helper()
	dir := t.TempDir()
	g := &programGate{first: filepath.Join(dir, "first"), sigN: filepath.Join(dir, "reached"), relN: filepath.Join(dir, "let")}
	for _, fifo := range []string{g.sigN, g.relN} {
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	if g.sig, err = os.OpenFile(g.sigN, os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	}
	if g.rel, err = os.OpenFile(g.relN, os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	}
	// Closing the release FIFO's only writer ends the program's wait: a
	// test that fails before letting it go never leaves it blocked.
	t.Cleanup(func() { _ = g.rel.Close(); _ = g.sig.Close() })
	return g
}

// prelude is the shell the program runs first: its first run signals, then
// waits to be let go.
func (g *programGate) prelude() string {
	return fmt.Sprintf("if mkdir %s 2>/dev/null; then printf x > %s; read _ < %s; fi\n", shellWord(g.first), shellWord(g.sigN), shellWord(g.relN))
}

// reached waits for the program's signal; a request that answers first is a
// broken choreography.
func (g *programGate) reached(t *testing.T, req *treeRequest, what string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		_, err := g.sig.Read(make([]byte, 1))
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-req.done:
		_ = g.sig.Close()
		<-got
		t.Fatalf("%s answered %d %s before it reached the gated program", what, req.rec.Code, req.rec.Body.String())
	}
}

// let lets the program go on.
func (g *programGate) let(t *testing.T) {
	t.Helper()
	if _, err := g.rel.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writeProgram(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// probe reports what a writer named name finds on a lock now: it takes it at
// once (treeAnswered, released at once), or it queues behind a holder
// (treeQueued). key is the name the choreography knows the wait by.
func (c *treeChoreography) probe(name, key string, hold func(context.Context, string) (func(), error)) treeOutcome {
	c.t.Helper()
	ctx, cancel := context.WithCancel(c.t.Context())
	defer cancel()
	c.mu.Lock()
	waited := c.waitChan(key)
	c.mu.Unlock()
	took := make(chan error, 1)
	go func() {
		release, err := hold(ctx, name)
		if err == nil {
			release()
		}
		took <- err
	}()
	select {
	case err := <-took:
		if err != nil {
			c.t.Fatalf("%s: %v", name, err)
		}
		return treeAnswered
	case <-waited:
		cancel()
		<-took
		return treeQueued
	}
}

// probeOutcome says what a probe found, whichever lock it probed.
func probeOutcome(o treeOutcome) string {
	if o == treeQueued {
		return "queued behind its holder"
	}
	return "took the lock at once"
}

func (c *treeChoreography) probeTree(s *Server, name string) treeOutcome {
	return c.probe(name, name, s.holdBotTreeWrites)
}

func (c *treeChoreography) probeGit(s *Server, name string) treeOutcome {
	return c.probe(name, gitWaitKey(name), s.holdAuthoringGitWrites)
}

// fakeSSHRemote serves scp-like URLs "fakehost:<dir>" from the directories
// under base through a fake ssh that runs git's server side locally, its
// first connection held at gate.
func fakeSSHRemote(t *testing.T, base string, gate *programGate) {
	t.Helper()
	ssh := writeProgram(t, filepath.Join(t.TempDir(), "ssh"), gate.prelude()+"cd "+shellWord(base)+" || exit 1\nexec sh -c \"$2\"\n")
	t.Setenv("GIT_SSH_COMMAND", ssh)
	t.Setenv("GIT_SSH_VARIANT", "simple")
}

// A publication reaches its remote — ls-remote, push, ls-remote — with the
// bot tree's lock released: it pinned the commit it pushes under the lock,
// and nothing it does at the remote reads the tree. A writer of the tree
// that comes in while the remote answers slowly goes through instead of
// waiting the whole round trip (and answering 503 once its client gives
// up); a git commit landing then moves HEAD, never what the publication
// pushes.
func TestAuthoringGitPublicationReachesItsRemoteOutsideTheTreeLock(t *testing.T) {
	s, editor := authoringFixture(t)
	root := s.cfg.WorkDir
	initAuthoringGit(t, root)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitTestRead(t, root, "clone", "-q", "--bare", root, origin)
	gitTestRead(t, root, "remote", "add", "origin", origin)
	gate := newProgramGate(t)
	uploadPack := writeProgram(t, filepath.Join(t.TempDir(), "upload-pack"), gate.prelude()+"exec git upload-pack \"$@\"\n")
	gitTestRead(t, root, "config", "remote.origin.uploadpack", uploadPack)
	pinned := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))

	c := newTreeChoreography(t)
	publication := c.start(s.handleAuthoringGitPublish, treeJSON(t, http.MethodPost, "/git-push", authoringGitPublishRequest{EditorPath: editor, Commit: pinned, Branch: "copi/published"}))
	gate.reached(t, publication, "the publication")

	if outcome := c.probeTree(s, "a writer of the tree"); outcome != treeAnswered {
		gate.let(t)
		t.Fatalf("a writer of the tree %s while the publication was at its remote", probeOutcome(outcome))
	}
	work := filepath.Join(root, "project-bot", "subbots", "work.bot")
	commit := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "subbots/work.bot", treeSnapshot(t, work), "workflow work:", "workflow moved:")))
	if outcome := c.first(commit, botTreeWriterAuthoringCommit); outcome != treeAnswered {
		gate.let(t)
		t.Fatalf("an assistant commit %s while the publication was at its remote", outcome)
	}
	requireTreeAnswer(t, "assistant commit", c.answer(commit, "the assistant commit"), http.StatusOK, "")
	gitCommit := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}},
		Message:    "chore(test): move HEAD under the publication",
	}))
	if outcome := c.first(gitCommit, botTreeWriterAuthoringGitCommit); outcome != treeAnswered {
		gate.let(t)
		t.Fatalf("a git commit %s while the publication was at its remote", outcome)
	}
	requireTreeAnswer(t, "git commit", c.answer(gitCommit, "the git commit"), http.StatusOK, "")
	if moved := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD")); moved == pinned {
		t.Fatalf("the git commit left HEAD at %s", pinned)
	}

	gate.let(t)
	requireTreeAnswer(t, "publication", c.answer(publication, "the publication"), http.StatusOK, "")
	if pushed := strings.TrimSpace(gitTestRead(t, origin, "rev-parse", "refs/heads/copi/published")); pushed != pinned {
		t.Fatalf("the publication pushed %s; it pinned %s", pushed, pinned)
	}
}

// An authoring git commit stages the attested files under the bot tree's
// lock and lets it go: the repository's hooks, the signature and the
// publication read nothing of the tree, and a hook (a linter, a pinentry
// behind the signature) can take as long as it likes. A writer of the tree
// goes through meanwhile; a second writer of the repository's index waits
// on the repository's lock — never failing on git's index.lock — and lands
// once the first is done.
func TestAuthoringGitCommitRunsItsHooksUnderTheRepositoryLockAlone(t *testing.T) {
	s, editor := authoringFixture(t)
	root := s.cfg.WorkDir
	initAuthoringGit(t, root)
	gate := newProgramGate(t)
	writeProgram(t, strings.TrimSpace(gitTestRead(t, root, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-commit")), gate.prelude()+"exit 0\n")
	work := filepath.Join(root, "project-bot", "subbots", "work.bot")
	writeFiles(t, root, map[string]string{"project-bot/subbots/work.bot": "workflow work:\n  entry: hooked\n"})
	helper := filepath.Join(root, "scripts", "helper.py")
	writeFiles(t, root, map[string]string{"scripts/helper.py": "def answer():\n    return 42\n"})
	head := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))

	c := newTreeChoreography(t)
	first := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}},
		Message:    "chore(test): the hooked commit",
	}))
	gate.reached(t, first, "the git commit")

	main := filepath.Join(root, filepath.FromSlash(editor))
	local := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "main.bot", treeSnapshot(t, main), "workflow main:", "workflow edited:")))
	if outcome := c.first(local, botTreeWriterAuthoringCommit); outcome != treeAnswered {
		gate.let(t)
		t.Fatalf("an assistant commit %s while a git commit ran its hooks", outcome)
	}
	requireTreeAnswer(t, "assistant commit", c.answer(local, "the assistant commit"), http.StatusOK, "")
	second := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: treeSnapshot(t, helper)}},
		Message:    "chore(test): the queued commit",
	}))
	outcome := c.first(second, gitWaitKey(botTreeWriterAuthoringGitCommit))
	gate.let(t)
	requireTreeAnswer(t, "first git commit", c.answer(first, "the first git commit"), http.StatusOK, "")
	requireTreeAnswer(t, "second git commit", c.answer(second, "the second git commit"), http.StatusOK, "")
	if outcome != treeQueued {
		t.Fatalf("a second git commit %s while the first held the repository's index", outcome)
	}
	if got, want := strings.TrimSpace(gitTestRead(t, root, "log", "--format=%s", head+"..HEAD")), "chore(test): the queued commit\nchore(test): the hooked commit"; got != want {
		t.Fatalf("commits after %s: %q; want %q", head, got, want)
	}
}

// A dependency update fetches a remote source with no lock held: its
// snapshot is taken under the bot tree's lock and verified again under both
// locks before it installs anything, so the network sits between them. While
// the remote answers, neither a writer of the tree nor a writer of the
// repository's index waits on it.
func TestDependencyUpdateFetchesARemoteSourceUnderNoLock(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "shared")
	bundleDir := filepath.Join(source, "bots", "shared-planner")
	writeAssistantDependencyBundle(t, bundleDir, "0.1.0", "workflow shared {}\n")
	initAuthoringGit(t, source)
	gitTestRead(t, source, "config", "uploadpack.allowAnySHA1InWant", "true")
	oldRef := strings.TrimSpace(gitTestRead(t, source, "rev-parse", "HEAD"))
	oldHash, err := bundle.ContentHashDir(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		botlock.FileName: fmt.Sprintf("version: 1\ndependencies:\n  shared-planner:\n    source: fakehost:shared\n    ref: %s\n    path: bots/shared-planner\n    bundle_sha256: %s\n", oldRef, oldHash),
		".gitignore":     ".botz/\n",
	})
	initAuthoringGit(t, root)
	writeAssistantDependencyBundle(t, bundleDir, "0.2.0", "workflow refreshed {}\n")
	gitTestRead(t, source, "add", "--", "bots/shared-planner")
	gitTestRead(t, source, "commit", "-qm", "refresh shared planner")
	ref := strings.TrimSpace(gitTestRead(t, source, "rev-parse", "HEAD"))
	gate := newProgramGate(t)
	fakeSSHRemote(t, base, gate)
	s := &Server{cfg: Config{WorkDir: root}}

	c := newTreeChoreography(t)
	update := c.start(s.handleAssistantDependencyBotsUpdate, treeJSON(t, http.MethodPost, "/api/v1/assistant/dependencies/bots-update", assistantDependencyBotsUpdateRequest{Name: "shared-planner", Ref: ref, Message: "chore(bots): refresh shared planner"}))
	gate.reached(t, update, "the dependency update")
	tree := c.probeTree(s, "a writer of the tree")
	git := c.probeGit(s, "a writer of the repository's index")
	gate.let(t)
	requireTreeAnswer(t, "dependency update", c.answer(update, "the dependency update"), http.StatusOK, "")
	if tree != treeAnswered || git != treeAnswered {
		t.Fatalf("while the dependency update fetched its source, a writer of the tree %s and a writer of the repository's index %s", probeOutcome(tree), probeOutcome(git))
	}
	lock, err := botlock.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := lock.Dependencies["shared-planner"].Ref; got != ref {
		t.Fatalf("bots.lock pins %s; want %s", got, ref)
	}
}

// An install from a git URL clones it before it takes the bot tree's lock:
// the clone is the network and writes nothing of the workspace, and the
// install of the checkout is the write the lock orders. A writer of the tree
// goes through while the remote answers. The install records and reports the
// URL it was given, never the temp checkout it installed from.
func TestBotInstallsCloneOutsideTheTreeLock(t *testing.T) {
	for _, tc := range []struct {
		writer string
		start  func(t *testing.T, c *treeChoreography, work, url string) (*Server, *treeRequest)
		code   int
	}{
		{botTreeWriterBotInstall, func(t *testing.T, c *treeChoreography, work, url string) (*Server, *treeRequest) {
			s := newMarketplaceServer(t, work)
			return s, c.start(s.handleBotInstall, treeJSON(t, http.MethodPost, "/api/v1/bots/install", botInstallRequest{URL: url}))
		}, http.StatusOK},
		{botTreeWriterMarketplace, func(t *testing.T, c *treeChoreography, work, url string) (*Server, *treeRequest) {
			s := newMarketplaceServer(t, work)
			if err := s.marketplace.Upsert(t.Context(), marketplace.Entry{Slug: "mybot", Name: "mybot", RepoURL: url}); err != nil {
				t.Fatal(err)
			}
			return s, c.start(s.handleMarketplaceInstall, treeJSON(t, http.MethodPost, "/api/v1/marketplace/bots/mybot/install", nil, "slug", "mybot"))
		}, http.StatusOK},
	} {
		t.Run(tc.writer, func(t *testing.T) {
			base := t.TempDir()
			writeFixtureBundle(t, filepath.Join(base, "mybot"), "mybot")
			initAuthoringGit(t, filepath.Join(base, "mybot"))
			gate := newProgramGate(t)
			fakeSSHRemote(t, base, gate)
			work := t.TempDir()
			url := "fakehost:mybot"
			c := newTreeChoreography(t)
			s, install := tc.start(t, c, work, url)
			gate.reached(t, install, "the "+tc.writer)
			outcome := c.probeTree(s, "a writer of the tree")
			gate.let(t)
			requireTreeAnswer(t, tc.writer, c.answer(install, "the "+tc.writer), tc.code, "")
			if outcome != treeAnswered {
				t.Fatalf("a writer of the tree %s while the %s cloned its source", probeOutcome(outcome), tc.writer)
			}
			origin, err := botinstall.ReadOrigin(filepath.Join(work, ".botz", "mybot"))
			if err != nil {
				t.Fatal(err)
			}
			if origin.Source != url {
				t.Fatalf("the install recorded its source as %q; it was given %q", origin.Source, url)
			}
			if !strings.Contains(install.rec.Body.String(), `"source":"`+url+`"`) {
				t.Fatalf("the %s answered %s; want its source reported as %q", tc.writer, install.rec.Body.String(), url)
			}
		})
	}
}
