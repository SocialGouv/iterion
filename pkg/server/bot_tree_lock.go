package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// The process orders the server's writers on two directory locks, both a
// dirLocks of writeLocks: every Server of the process — the one per project
// `iterion studio --workspace` runs included — takes them from one registry,
// so two projects in one repository, or with one root inside the other, are
// ordered like two writers of one project.
//
// The bot tree's lock (writeLocks.trees) orders the writers named by the
// botTreeWriter* constants below, and no other: the sixteen authoring and
// bot-tree handlers, the server's run merge routes (merge, conflict
// resolution by hand and by the agent, finalize, abort) and the project
// switch. For them it covers what reads or writes the bot files of a working
// tree to decide or make a write: resolution, validation, the write, and the
// hooks git runs in that tree. A handler takes it on its project's root
// (holdBotTreeWrites) BEFORE it resolves anything and holds it through
// resolve, validate and write: the bot a write acts on, the bundle its
// manifest perimeter grants, the siblings its check reads and the catalog
// entry it answers are all read in a tree none of those writers is in the
// middle of; the run merge routes take it around their git writes. Without
// it a resolution read the tree mid-write — a publication moves the live
// file aside before it moves its successor in, a rename moves a name to
// another directory — and the write that followed answered a request error
// for the loser of a race, or acted on another bot. Under it, the loser
// waits and decides on the tree the winner left: the conflict (409) its
// stale snapshot is, or the success its independent change is.
//
// The key is a directory, not a bundle: a name maps to a directory only
// through the catalog, which a rename re-points, so no per-bot key exists
// before the resolution the lock must precede. A git writer takes it on the
// repository's whole working tree (holdRepository): git's hooks read and
// write any file of it, and every project inside it is excluded meanwhile.
// Outside the lock run only the network and git objects: a clone or a fetch
// into a staging directory, an ls-remote or a push of a pinned commit, the
// signature commit-tree puts over a tree already built. A git writer lets
// the tree go around those and takes it again for what follows; the two
// dependency actions keep it from their install to their publication — the
// tree is provisional in between — their signature included.
//
// The repository's lock (writeLocks.repositories) orders the server's writers
// of a repository's index — the authoring git commit, the two dependency
// actions and the server's run merge routes — through their commit, keyed by
// the repository's common git directory. Git refuses a second writer of the
// index outright (index.lock); in the process, the second waits instead. It
// is taken before the tree's lock, never while holding it.
//
// Neither lock orders these, each a window docs/authoring-lock.md names with
// its consequence: the engine's own merges into the operator's checkout (the
// review-gate merge, performGateMerge; the run-end auto-merge,
// finalizeWorktree → trySquashMerge), the rewind of a run that executed in
// place, bot discovery paths outside the project root (a workspace
// project's bots paths), the pipeline card create and the run launch, the
// run-start catalogue refresh, and every writer in another process.
//
// Both locks are the process's: the per-file authoring locks
// (acquireAuthoringLocalLocks) keep ordering each file's writers across
// processes, and every holder takes them after these, so none of them wait
// on each other in a cycle. Reads take nothing.
//
// A waiter waits as long as its request: one whose request ends first
// answers 503 with nothing resolved and nothing written, and its retry goes
// through once the holder is done.

// writeLocks is a process's two write locks.
type writeLocks struct {
	trees        dirLocks
	repositories dirLocks
}

// processWriteLocks is the registry every Server of the process takes its
// write locks from.
var processWriteLocks writeLocks

// locks is the registry s takes its write locks from: the process's, unless
// s.writeLocks names another — a test's model of another process.
func (s *Server) locks() *writeLocks {
	if s.writeLocks != nil {
		return s.writeLocks
	}
	return &processWriteLocks
}

// dirLocks is a lock over directories its waiters take in the order they
// arrived, each waiting until its context ends: two holds exclude each other
// when their directories are one, or one contains the other. The zero value
// is ready to use.
type dirLocks struct {
	mu      sync.Mutex
	held    []*dirTurn
	waiting []*dirTurn
}

// dirTurn is one writer's place on a dirLocks: in line, then holding.
type dirTurn struct {
	dir  string
	turn chan struct{}
}

// botTreeWriteHook is a test seam: called with the lock's wait event — "wait"
// for the bot tree's, "git wait" for the repository's — and the writer's name
// once a writer that found the lock taken has queued behind its holder, just
// before it blocks: the moment a test can let the holder go knowing the
// waiter is in line. Nil outside tests.
var botTreeWriteHook func(event, writer string)

func botTreeWriteWindow(event, writer string) {
	if botTreeWriteHook != nil {
		botTreeWriteHook(event, writer)
	}
}

// The wait events of the two locks (botTreeWriteHook).
const (
	botTreeWaitEvent      = "wait"
	authoringGitWaitEvent = "git wait"
)

// acquire takes dir for writer, waiting in arrival order behind every holder
// and earlier waiter it overlaps until ctx ends; event names the lock to
// botTreeWriteHook.
func (l *dirLocks) acquire(ctx context.Context, dir, event, writer string) (*dirTurn, error) {
	t := &dirTurn{dir: dir, turn: make(chan struct{})}
	l.mu.Lock()
	if !l.excludedLocked(t, len(l.waiting)) {
		l.held = append(l.held, t)
		l.mu.Unlock()
		return t, nil
	}
	l.waiting = append(l.waiting, t)
	l.mu.Unlock()
	botTreeWriteWindow(event, writer)
	select {
	case <-t.turn:
		return t, nil
	case <-ctx.Done():
	}
	l.mu.Lock()
	if i := slices.Index(l.waiting, t); i >= 0 {
		l.waiting = slices.Delete(l.waiting, i, i+1)
		// A writer behind this one may have waited on it alone.
		l.grantLocked()
		l.mu.Unlock()
		return nil, ctx.Err()
	}
	l.mu.Unlock()
	// The turn was handed over as the request ended: pass it on.
	l.release(t)
	return nil, ctx.Err()
}

// release lets t go and hands the lock to every waiter it frees.
func (l *dirLocks) release(t *dirTurn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := slices.Index(l.held, t); i >= 0 {
		l.held = slices.Delete(l.held, i, i+1)
	}
	l.grantLocked()
}

// excludedLocked reports whether t must wait: a holder overlaps it, or a
// writer in line before position n does. l.mu is held.
func (l *dirLocks) excludedLocked(t *dirTurn, n int) bool {
	for _, h := range l.held {
		if dirsOverlap(h.dir, t.dir) {
			return true
		}
	}
	for _, w := range l.waiting[:n] {
		if dirsOverlap(w.dir, t.dir) {
			return true
		}
	}
	return false
}

// grantLocked hands the lock, in arrival order, to every waiter that no
// holder and no writer ahead of it excludes. l.mu is held.
func (l *dirLocks) grantLocked() {
	for i := 0; i < len(l.waiting); {
		w := l.waiting[i]
		if l.excludedLocked(w, i) {
			i++
			continue
		}
		l.waiting = slices.Delete(l.waiting, i, i+1)
		l.held = append(l.held, w)
		close(w.turn)
	}
}

func dirsOverlap(a, b string) bool { return dirWithin(a, b) || dirWithin(b, a) }

// dirWithin reports whether dir is base or lies beneath it.
func dirWithin(dir, base string) bool {
	rel, err := filepath.Rel(base, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// lockDir is dir as the locks key it: absolute, clean and with its symlinks
// resolved, so one directory under two spellings is one key. A directory
// that does not resolve keys by its absolute path: nothing exists there for
// another spelling to reach.
func lockDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// treeHold is one writer's hold of the bot tree's lock on a directory, which
// it may let go around work that reads git objects or the network and take
// again. A nil *treeHold is a tree its caller holds throughout: leave and take
// do nothing.
type treeHold struct {
	locks  *dirLocks
	dir    string
	writer string
	turn   *dirTurn
}

// take takes the tree again if the holder let it go.
func (h *treeHold) take(ctx context.Context) error {
	if h == nil || h.turn != nil {
		return nil
	}
	turn, err := h.locks.acquire(ctx, h.dir, botTreeWaitEvent, h.writer)
	if err != nil {
		return botTreeWaitError{writer: h.writer, what: "the bot tree", cause: err}
	}
	h.turn = turn
	return nil
}

// leave lets the tree go if the holder holds it.
func (h *treeHold) leave() {
	if h == nil || h.turn == nil {
		return
	}
	h.locks.release(h.turn)
	h.turn = nil
}

// botTreeWaitError is a writer whose request ended while it waited for one
// of the server's write locks: nothing was resolved and nothing written, and
// a retry once the holder is done goes through.
type botTreeWaitError struct {
	writer string
	what   string
	cause  error
}

func (e botTreeWaitError) Error() string {
	return fmt.Sprintf("%s: another write of %s held its write lock until this request ended (%v); nothing was written — retry", e.writer, e.what, e.cause)
}

func (e botTreeWaitError) Unwrap() error { return e.cause }

// holdBotTreeWrites takes, for writer, the bot tree's lock on s's project
// root — before the handler resolves anything — and returns its release,
// idempotent. A project switch that landed while the writer waited sends it
// to the tree it now writes: once it holds, the root it holds stays s's
// until it lets go, since a switch takes this lock first (swapWorkDir).
func (s *Server) holdBotTreeWrites(ctx context.Context, writer string) (func(), error) {
	for {
		dir := s.currentWorkDir()
		hold := &treeHold{locks: &s.locks().trees, dir: lockDir(dir), writer: writer}
		if err := hold.take(ctx); err != nil {
			return nil, err
		}
		if s.currentWorkDir() == dir {
			var once sync.Once
			return func() { once.Do(hold.leave) }, nil
		}
		hold.leave()
	}
}

// gitRepository is a repository as its writers lock it: worktree, the top of
// the working tree its files and hooks are in, which the bot tree's lock
// takes whole; commonDir, the git directory every worktree of it shares,
// which keys the repository's lock — empty outside any repository.
type gitRepository struct {
	worktree, commonDir string
}

// identifyGitRepository is the repository dir is in. A dir in none is its
// own working tree with no index to lock: the writer's own git calls refuse
// it, with git's word.
func identifyGitRepository(ctx context.Context, dir string) (gitRepository, error) {
	out, err := runAuthoringGit(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return gitRepository{worktree: lockDir(dir)}, nil
		}
		return gitRepository{}, fmt.Errorf("identify the repository of %s: %w", dir, err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return gitRepository{}, fmt.Errorf("identify the repository of %s: git rev-parse answered %q; want its top level and its common git directory", dir, out)
	}
	return gitRepository{worktree: lockDir(lines[0]), commonDir: lockDir(lines[1])}, nil
}

// holdRepositoryIndex takes, for writer, the lock of repo's index and
// returns its release, idempotent; a repo outside any repository has none.
func (s *Server) holdRepositoryIndex(ctx context.Context, repo gitRepository, writer string) (func(), error) {
	if repo.commonDir == "" {
		return func() {}, nil
	}
	locks := &s.locks().repositories
	turn, err := locks.acquire(ctx, repo.commonDir, authoringGitWaitEvent, writer)
	if err != nil {
		return nil, botTreeWaitError{writer: writer, what: "the repository's index", cause: err}
	}
	var once sync.Once
	return func() { once.Do(func() { locks.release(turn) }) }, nil
}

// holdRepository takes, for writer, what a git writer of an operator's
// checkout holds: the lock of repo's index when index is set, then the bot
// tree's lock on its whole working tree. tree is the tree's hold, which the
// writer may let go and take again; release lets everything go, idempotent.
func (s *Server) holdRepository(ctx context.Context, repo gitRepository, writer string, index bool) (tree *treeHold, release func(), err error) {
	releaseIndex := func() {}
	if index {
		if releaseIndex, err = s.holdRepositoryIndex(ctx, repo, writer); err != nil {
			return nil, nil, err
		}
	}
	tree = &treeHold{locks: &s.locks().trees, dir: repo.worktree, writer: writer}
	if err := tree.take(ctx); err != nil {
		releaseIndex()
		return nil, nil, err
	}
	var once sync.Once
	return tree, func() {
		once.Do(func() {
			tree.leave()
			releaseIndex()
		})
	}, nil
}

// holdWorkDirRepository is holdRepository on the repository of s's project
// root, taken again when a project switch landed while the writer waited.
func (s *Server) holdWorkDirRepository(ctx context.Context, writer string, index bool) (*treeHold, func(), error) {
	for {
		dir := s.currentWorkDir()
		repo, err := identifyGitRepository(ctx, dir)
		if err != nil {
			return nil, nil, err
		}
		tree, release, err := s.holdRepository(ctx, repo, writer, index)
		if err != nil {
			return nil, nil, err
		}
		if s.currentWorkDir() == dir {
			return tree, release, nil
		}
		release()
	}
}

// holdRunMergeRepository is the hold the server's run merge routes write
// repoRoot under (runview.WithRepositoryWrites): the repository's index, then
// its whole working tree — a merge writes the operator's files, bots
// included. The engine's own merges (the review gate's, a run end's
// auto-merge) do not take it.
func (s *Server) holdRunMergeRepository(ctx context.Context, repoRoot string) (func(), error) {
	repo, err := identifyGitRepository(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	_, release, err := s.holdRepository(ctx, repo, botTreeWriterRunMerge, true)
	return release, err
}

// botTreeBusyCode is the error_code of a write whose request ended while it
// waited for one of the server's write locks: a 5xx body carries no message,
// and the code is what tells a client the write can be retried as it is.
const botTreeBusyCode = "bot_tree_busy"

// writeBotTreeWait answers err with 503 bot_tree_busy when it is a wait the
// request did not outlive, and reports whether it did.
func (s *Server) writeBotTreeWait(w http.ResponseWriter, r *http.Request, err error) bool {
	var wait botTreeWaitError
	if !errors.As(err, &wait) {
		return false
	}
	s.httpErrorCode(w, r, http.StatusServiceUnavailable, botTreeBusyCode, "%v", wait)
	return true
}

// lockBotTree is holdBotTreeWrites for a handler: a wait the request did not
// outlive is answered 503 bot_tree_busy here, and ok is false.
func (s *Server) lockBotTree(w http.ResponseWriter, r *http.Request, writer string) (release func(), ok bool) {
	release, err := s.holdBotTreeWrites(r.Context(), writer)
	if err != nil {
		s.writeBotTreeWait(w, r, err)
		return nil, false
	}
	return release, true
}

// lockBotTreeForEditor is lockBotTree for an authoring request naming its
// editor file: a local file's write takes the lock, a stored bot source's
// (botsource://) does not — the store's version CAS orders those, and the
// tree holds none of their files.
func (s *Server) lockBotTreeForEditor(w http.ResponseWriter, r *http.Request, writer, editorPath string) (release func(), ok bool) {
	if isBotSourceEditorPath(editorPath) {
		return func() {}, true
	}
	return s.lockBotTree(w, r, writer)
}

// lockRepositoryForEditor is holdWorkDirRepository for an authoring git
// request naming its editor file, skipped for a stored bot source as
// lockBotTreeForEditor skips it (tree nil). A failure is answered here, and
// ok is false.
func (s *Server) lockRepositoryForEditor(w http.ResponseWriter, r *http.Request, writer, editorPath string, index bool) (tree *treeHold, release func(), ok bool) {
	if isBotSourceEditorPath(editorPath) {
		return nil, func() {}, true
	}
	tree, release, err := s.holdWorkDirRepository(r.Context(), writer, index)
	if err != nil {
		s.authoringError(w, r, err)
		return nil, nil, false
	}
	return tree, release, true
}

func isBotSourceEditorPath(editorPath string) bool {
	return strings.HasPrefix(strings.TrimSpace(editorPath), botSourceEditorScheme)
}

// The names the writers of the bot tree take its lock under.
const (
	botTreeWriterAuthoringCommit    = "authoring commit"
	botTreeWriterAuthoringGitCommit = "authoring git commit"
	botTreeWriterAuthoringGitPush   = "authoring git publication"
	botTreeWriterEditorSave         = "editor save"
	botTreeWriterRunFileSave        = "run file save"
	botTreeWriterBotMetadata        = "bot metadata edit"
	botTreeWriterBotOverlay         = "bot workspace toggle"
	botTreeWriterTriggerInvocation  = "trigger from invocation"
	botTreeWriterBotCreate          = "bot create"
	botTreeWriterBotImport          = "bot import"
	botTreeWriterBotInstall         = "bot install"
	botTreeWriterBotUpload          = "bot upload"
	botTreeWriterMarketplace        = "marketplace bot install"
	botTreeWriterMarketplaceRemove  = "marketplace bot uninstall"
	botTreeWriterDependencyUpdate   = "dependency update"
	botTreeWriterDependencyLocalize = "dependency localization"
	botTreeWriterRunMerge           = "run merge"
	botTreeWriterProjectSwitch      = "project switch"
)
