package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// The server orders its writers on two process locks, both an orderedLock.
//
// The bot tree's write lock (Server.botTreeWrites) orders every handler that
// writes a file of a bot, its workspace overlay, a subscription copied from
// its manifest, or resolves a bot and then writes it. A writer takes it
// BEFORE it resolves anything and holds it through resolve, validate and
// write: the bot a write acts on, the bundle its manifest perimeter grants,
// the siblings its check reads and the catalog entry it answers are all read
// in a tree no other server writer is in the middle of. Without it a
// resolution read the tree mid-write — a publication moves the live file
// aside before it moves its successor in, a rename moves a name to another
// directory — and the write that followed answered a request error for the
// loser of a race, or acted on another bot. Under it, the loser waits and
// decides on the tree the winner left: the conflict (409) its stale snapshot
// is, or the success its independent change is.
//
// The key is the whole tree, not a bundle: a name maps to a directory only
// through the catalog, which a rename re-points, so no per-bot key exists
// before the resolution the lock must precede. What a holder does under it
// is local work on the tree: the network and the repository's own programs
// run outside it — a clone or a fetch before the lock is taken, a push after
// it is released, an authoring commit's hooks and signature under the
// repository's lock alone. The two dependency actions are the exception: from
// the moment one installs into the workspace to the moment it publishes or
// restores, the tree is provisional, so the commit between (its hooks and
// signature included) runs under the tree's lock.
//
// The repository's lock (Server.authoringGitWrites) orders the server's
// writers of the repository's index — the authoring git commit and the two
// dependency actions — through their commit. Git refuses a second writer of
// the index outright (index.lock); in the process, the second waits instead.
// It is taken before the tree's lock, never while holding it.
//
// Both locks are the process's: the per-file authoring locks
// (acquireAuthoringLocalLocks) keep ordering each file's writers across
// processes, and every holder takes them after these, so none of them wait
// on each other in a cycle. Reads take nothing.
//
// A waiter waits as long as its request: one whose request ends first
// answers 503 with nothing resolved and nothing written, and its retry goes
// through once the holder is done.

// orderedLock is a lock its waiters take one at a time, in the order they
// arrived, each waiting until its context ends. The zero value is ready to
// use.
type orderedLock struct {
	mu    sync.Mutex
	held  bool
	queue []chan struct{}
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

// acquire takes the lock for writer, waiting in arrival order behind its
// holder until ctx ends; event names the lock to botTreeWriteHook.
func (l *orderedLock) acquire(ctx context.Context, event, writer string) error {
	l.mu.Lock()
	if !l.held {
		l.held = true
		l.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	l.queue = append(l.queue, turn)
	l.mu.Unlock()
	botTreeWriteWindow(event, writer)
	select {
	case <-turn:
		return nil
	case <-ctx.Done():
	}
	l.mu.Lock()
	for i, queued := range l.queue {
		if queued == turn {
			l.queue = append(l.queue[:i:i], l.queue[i+1:]...)
			l.mu.Unlock()
			return ctx.Err()
		}
	}
	l.mu.Unlock()
	// The turn was handed over as the request ended: pass it on.
	l.release()
	return ctx.Err()
}

// release hands the lock to the first writer in line, or frees it.
func (l *orderedLock) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		l.held = false
		return
	}
	next := l.queue[0]
	l.queue = l.queue[1:]
	close(next)
}

// hold takes l for writer and returns its release, idempotent.
func (l *orderedLock) hold(ctx context.Context, event, writer, what string) (func(), error) {
	if err := l.acquire(ctx, event, writer); err != nil {
		return nil, botTreeWaitError{writer: writer, what: what, cause: err}
	}
	var once sync.Once
	return func() { once.Do(l.release) }, nil
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

// holdBotTreeWrites takes the bot tree's write lock for writer — before the
// handler resolves anything — and returns its release, idempotent.
func (s *Server) holdBotTreeWrites(ctx context.Context, writer string) (func(), error) {
	return s.botTreeWrites.hold(ctx, botTreeWaitEvent, writer, "the bot tree")
}

// holdAuthoringGitWrites takes the repository's lock for writer — before the
// bot tree's — and returns its release, idempotent.
func (s *Server) holdAuthoringGitWrites(ctx context.Context, writer string) (func(), error) {
	return s.authoringGitWrites.hold(ctx, authoringGitWaitEvent, writer, "the repository's index")
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

// lockAuthoringGit is holdAuthoringGitWrites for a handler, answered as
// lockBotTree answers.
func (s *Server) lockAuthoringGit(w http.ResponseWriter, r *http.Request, writer string) (release func(), ok bool) {
	release, err := s.holdAuthoringGitWrites(r.Context(), writer)
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

// lockAuthoringGitForEditor is lockAuthoringGit for an authoring request
// naming its editor file, skipped for a stored bot source as
// lockBotTreeForEditor skips it.
func (s *Server) lockAuthoringGitForEditor(w http.ResponseWriter, r *http.Request, writer, editorPath string) (release func(), ok bool) {
	if isBotSourceEditorPath(editorPath) {
		return func() {}, true
	}
	return s.lockAuthoringGit(w, r, writer)
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
)
