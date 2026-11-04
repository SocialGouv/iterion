//go:build linux || darwin

package server

import (
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
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/store"
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

// gitHookPath is where root's repository looks for its hook name.
func gitHookPath(t *testing.T, root, name string) string {
	t.Helper()
	return strings.TrimSpace(gitTestRead(t, root, "rev-parse", "--path-format=absolute", "--git-path", "hooks/"+name))
}

// An authoring git commit holds the bot tree's lock through its hooks: a
// hook sees the real worktree — a validator, a linter, a formatter that
// rewrites what it reads — so a writer of the tree that comes in while it
// runs waits, and the hook never reads a file another writer has moved
// aside to publish its successor (the git commit answering 400 for a race
// it lost). A second writer of the repository's index waits on the
// repository's lock — never failing on git's index.lock — and lands once
// the first is done.
func TestAuthoringGitCommitRunsItsHooksUnderTheTreeLock(t *testing.T) {
	s, editor := authoringFixture(t)
	root := s.cfg.WorkDir
	initAuthoringGit(t, root)
	main := filepath.Join(root, filepath.FromSlash(editor))
	gate := newProgramGate(t)
	writeProgram(t, gitHookPath(t, root, "pre-commit"), gate.prelude()+"test -f "+shellWord(main)+" || { echo 'pre-commit: project-bot/main.bot is missing from the worktree' >&2; exit 1; }\n")
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

	// An assistant commit of the file the hook reads, held where it has
	// moved the live file aside.
	publish := c.hold("publish", absentNow(main))
	local := c.start(treeCommit(s), treeJSON(t, http.MethodPost, "/commit", treeReplace(editor, "main.bot", treeSnapshot(t, main), "workflow main:", "workflow edited:")))
	localOutcome := c.first(local, botTreeWriterAuthoringCommit, publish)
	second := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: treeSnapshot(t, helper)}},
		Message:    "chore(test): the queued commit",
	}))
	secondOutcome := c.first(second, gitWaitKey(botTreeWriterAuthoringGitCommit))
	gate.let(t)
	firstRec := c.answer(first, "the first git commit")
	publish.let()
	localRec := c.answer(local, "the assistant commit")
	secondRec := c.answer(second, "the second git commit")
	if localOutcome != treeQueued {
		t.Fatalf("an assistant commit %s while a git commit ran its hooks; the git commit answered %d %s", localOutcome, firstRec.Code, firstRec.Body.String())
	}
	if secondOutcome != treeQueued {
		t.Fatalf("a second git commit %s while the first held the repository's index: %d %s", secondOutcome, secondRec.Code, secondRec.Body.String())
	}
	requireTreeAnswer(t, "first git commit", firstRec, http.StatusOK, "")
	requireTreeAnswer(t, "assistant commit", localRec, http.StatusOK, "")
	requireTreeAnswer(t, "second git commit", secondRec, http.StatusOK, "")
	if got, want := strings.TrimSpace(gitTestRead(t, root, "log", "--format=%s", head+"..HEAD")), "chore(test): the queued commit\nchore(test): the hooked commit"; got != want {
		t.Fatalf("commits after %s: %q; want %q", head, got, want)
	}
}

// An authoring git commit lets the bot tree go around its signature alone:
// commit-tree -S reads the tree object the commit built, never the
// worktree, and a pinentry may wait on a person as long as it likes — a
// writer of the tree goes through meanwhile. What follows runs in the
// worktree again — the reference-transaction hooks of the publication, the
// index, the post-commit hook — so the commit takes the tree back first.
func TestAuthoringGitCommitSignsOutsideTheTreeLock(t *testing.T) {
	s, editor := authoringFixture(t)
	root := s.cfg.WorkDir
	initAuthoringGit(t, root)
	sign := newProgramGate(t)
	gpg := writeProgram(t, filepath.Join(t.TempDir(), "gpg"), sign.prelude()+"cat >/dev/null\nprintf '\\n[GNUPG:] SIG_CREATED D 1 8 00 0 FAKE\\n' >&2\nprintf -- '-----BEGIN PGP SIGNATURE-----\\n\\nZmFrZQ==\\n-----END PGP SIGNATURE-----\\n'\n")
	gitTestRead(t, root, "config", "gpg.program", gpg)
	gitTestRead(t, root, "config", "commit.gpgsign", "true")
	post := newProgramGate(t)
	writeProgram(t, gitHookPath(t, root, "post-commit"), post.prelude()+"exit 0\n")
	work := filepath.Join(root, "project-bot", "subbots", "work.bot")
	writeFiles(t, root, map[string]string{"project-bot/subbots/work.bot": "workflow work:\n  entry: signed\n"})

	c := newTreeChoreography(t)
	commit := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}},
		Message:    "chore(test): the signed commit",
	}))
	sign.reached(t, commit, "the git commit's signature")
	signing := c.probeTree(s, "a writer of the tree during the signature")
	sign.let(t)
	post.reached(t, commit, "the git commit's post-commit hook")
	hooked := c.probeTree(s, "a writer of the tree during the post-commit hook")
	post.let(t)
	requireTreeAnswer(t, "git commit", c.answer(commit, "the git commit"), http.StatusOK, "")
	if signing != treeAnswered {
		t.Fatalf("a writer of the tree %s while the git commit was signing", probeOutcome(signing))
	}
	if hooked != treeQueued {
		t.Fatalf("a writer of the tree %s while the git commit ran its post-commit hook", probeOutcome(hooked))
	}
	if body := gitTestRead(t, root, "cat-file", "commit", "HEAD"); !strings.Contains(body, "gpgsig -----BEGIN PGP SIGNATURE-----") {
		t.Fatalf("HEAD carries no signature:\n%s", body)
	}
}

// A publication's pre-push hook reads the worktree, so it runs under the bot
// tree's lock, taken again once the remote said the branch is free, and the
// push runs with --no-verify once it passed: a writer of the tree that comes
// in while the hook runs waits. The hook gets what git's own push gives it:
// the remote's name and URL as its arguments, the update on its stdin.
func TestAuthoringGitPublicationRunsItsPrePushHookUnderTheTreeLock(t *testing.T) {
	s, editor := authoringFixture(t)
	root := s.cfg.WorkDir
	initAuthoringGit(t, root)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitTestRead(t, root, "clone", "-q", "--bare", root, origin)
	gitTestRead(t, root, "remote", "add", "origin", origin)
	gate := newProgramGate(t)
	record := filepath.Join(t.TempDir(), "pre-push")
	writeProgram(t, gitHookPath(t, root, "pre-push"), gate.prelude()+`{ printf '%s\n' "$*"; cat; } >> `+shellWord(record)+"\n")
	pinned := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))

	c := newTreeChoreography(t)
	publication := c.start(s.handleAuthoringGitPublish, treeJSON(t, http.MethodPost, "/git-push", authoringGitPublishRequest{EditorPath: editor, Commit: pinned, Branch: "copi/published"}))
	gate.reached(t, publication, "the publication's pre-push hook")
	outcome := c.probeTree(s, "a writer of the tree")
	gate.let(t)
	requireTreeAnswer(t, "publication", c.answer(publication, "the publication"), http.StatusOK, "")
	if outcome != treeQueued {
		t.Fatalf("a writer of the tree %s while the publication ran its pre-push hook", probeOutcome(outcome))
	}
	if pushed := strings.TrimSpace(gitTestRead(t, origin, "rev-parse", "refs/heads/copi/published")); pushed != pinned {
		t.Fatalf("the publication pushed %s; it pinned %s", pushed, pinned)
	}
	ran, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	gitTestRead(t, root, "push", "-q", "origin", pinned+":refs/heads/copi/by-git")
	both, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	byGit := strings.TrimPrefix(string(both), string(ran))
	if want := strings.ReplaceAll(byGit, "refs/heads/copi/by-git", "refs/heads/copi/published"); string(ran) != want {
		t.Fatalf("the publication ran its pre-push hook as\n%s\ngit's own push runs it once, as\n%s", ran, want)
	}
}

// Every Server of the process takes its write locks from one registry: two
// projects of `iterion studio --workspace` in one repository — here one root
// inside the other — are ordered as two writers of one project are. A git
// commit of the inner project waits on the repository's lock while the
// outer one holds its index, instead of failing on git's index.lock, and a
// writer of the inner project's tree waits while the outer one's hooks run
// over the whole working tree.
func TestServersOfOneRepositoryShareItsWriteLocks(t *testing.T) {
	outer, editor := authoringFixture(t)
	root := outer.cfg.WorkDir
	initAuthoringGit(t, root)
	inner := &Server{cfg: Config{WorkDir: filepath.Join(root, "project-bot")}}
	gate := newProgramGate(t)
	writeProgram(t, gitHookPath(t, root, "pre-commit"), gate.prelude()+"exit 0\n")
	helper := filepath.Join(root, "scripts", "helper.py")
	writeFiles(t, root, map[string]string{"scripts/helper.py": "def answer():\n    return 42\n"})
	work := filepath.Join(root, "project-bot", "subbots", "work.bot")
	writeFiles(t, root, map[string]string{"project-bot/subbots/work.bot": "workflow work:\n  entry: inner\n"})

	c := newTreeChoreography(t)
	outerCommit := c.start(outer.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: treeSnapshot(t, helper)}},
		Message:    "chore(test): the outer project's commit",
	}))
	gate.reached(t, outerCommit, "the outer project's git commit")
	innerTree := c.probeTree(inner, "a writer of the inner project's tree")
	innerCommit := c.start(inner.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: "main.bot",
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}},
		Message:    "chore(test): the inner project's commit",
	}))
	innerOutcome := c.first(innerCommit, gitWaitKey(botTreeWriterAuthoringGitCommit))
	gate.let(t)
	outerRec := c.answer(outerCommit, "the outer project's git commit")
	innerRec := c.answer(innerCommit, "the inner project's git commit")
	if innerOutcome != treeQueued {
		t.Fatalf("the inner project's git commit %s while the outer one held the repository's index: %d %s", innerOutcome, innerRec.Code, innerRec.Body.String())
	}
	if innerTree != treeQueued {
		t.Fatalf("a writer of the inner project's tree %s while the outer project's git commit ran its hooks", probeOutcome(innerTree))
	}
	requireTreeAnswer(t, "outer project's git commit", outerRec, http.StatusOK, "")
	requireTreeAnswer(t, "inner project's git commit", innerRec, http.StatusOK, "")
}

// The run merge family writes the operator's index and working tree: a
// squash merge started while an authoring git commit holds them waits on
// the repository's lock, and lands once the commit is done, instead of
// failing on git's index.lock or on the commit's half-staged tree.
func TestRunMergeWaitsForTheRepositoryItWrites(t *testing.T) {
	fixture, editor := authoringFixture(t)
	root := fixture.cfg.WorkDir
	writeFiles(t, root, map[string]string{".gitignore": ".iterion/\n"})
	initAuthoringGit(t, root)
	base := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))
	branch := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "--abbrev-ref", "HEAD"))
	gitTestRead(t, root, "checkout", "-qb", "iterion/run/held")
	writeFiles(t, root, map[string]string{"run.txt": "the run's work\n"})
	gitTestRead(t, root, "add", "--", "run.txt")
	gitTestRead(t, root, "commit", "-qm", "the run's work")
	final := strings.TrimSpace(gitTestRead(t, root, "rev-parse", "HEAD"))
	gitTestRead(t, root, "checkout", "-q", branch)

	s := New(Config{DisableAuth: true, SkipProjectRegistration: true, WorkDir: root, StoreDir: filepath.Join(t.TempDir(), "store")}, iterlog.Nop())
	stopRunviewOnCleanup(t, s.runs)
	run, err := s.runs.RunStore().CreateRun(t.Context(), "held-merge", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Worktree, run.RepoRoot, run.WorkDir = true, root, root
	run.BaseCommit, run.FinalCommit, run.FinalBranch = base, final, "iterion/run/held"
	run.Status, run.MergeStrategy = store.RunStatusFinished, store.MergeStrategySquash
	if err := s.runs.RunStore().SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	gate := newProgramGate(t)
	writeProgram(t, gitHookPath(t, root, "pre-commit"), gate.prelude()+"exit 0\n")
	work := filepath.Join(root, "project-bot", "subbots", "work.bot")
	writeFiles(t, root, map[string]string{"project-bot/subbots/work.bot": "workflow work:\n  entry: before the merge\n"})

	c := newTreeChoreography(t)
	commit := c.start(s.handleAuthoringGitCommit, treeJSON(t, http.MethodPost, "/git-commit", authoringGitCommitRequest{
		EditorPath: editor,
		Files:      []authoringGitCommitFile{{Scope: "bundle", Path: "subbots/work.bot", ExpectedSHA256: treeSnapshot(t, work)}},
		Message:    "chore(test): the commit the merge waits for",
	}))
	gate.reached(t, commit, "the git commit")
	merge := c.start(s.handleMergeRun, treeJSON(t, http.MethodPost, "/api/runs/held-merge/merge", mergeRunRequest{}, "id", run.ID))
	outcome := c.first(merge, gitWaitKey(botTreeWriterRunMerge))
	gate.let(t)
	commitRec := c.answer(commit, "the git commit")
	mergeRec := c.answer(merge, "the merge")
	if outcome != treeQueued {
		t.Fatalf("a run merge %s while a git commit held the repository: %d %s", outcome, mergeRec.Code, mergeRec.Body.String())
	}
	requireTreeAnswer(t, "git commit", commitRec, http.StatusOK, "")
	requireTreeAnswer(t, "merge", mergeRec, http.StatusOK, "")
	if got, want := strings.TrimSpace(gitTestRead(t, root, "show", "HEAD:run.txt")), "the run's work"; got != want {
		t.Fatalf("the merged HEAD carries run.txt = %q; want %q", got, want)
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
