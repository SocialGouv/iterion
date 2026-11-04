package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// botTreeWriteLock orders the server's writes of the bot tree — every
// handler that writes a file of a bot, its workspace overlay, a
// subscription derived from its manifest, or resolves a bot and then writes
// — one at a time, in the order they arrived.
//
// A writer takes it BEFORE it resolves anything and holds it through
// resolve, validate and write: the bot a write acts on, the bundle its
// manifest perimeter grants, the siblings its check reads and the catalog
// entry it answers are all read in a tree no other server writer is in the
// middle of. Without it a resolution read the tree mid-write — a publication
// moves the live file aside before it moves its successor in, a rename moves
// a name to another directory — and the write that followed answered a
// request error for the loser of a race, or acted on another bot. Under it,
// the loser waits and decides on the tree the winner left: the conflict
// (409) its stale snapshot is, or the success its independent change is.
//
// The key is the whole tree, not a bundle: a name maps to a directory only
// through the catalog, which a rename re-points, so no per-bot key exists
// before the resolution the lock must precede. Writes are human-driven and
// rare; serializing them costs a wait, never a refusal. Reads take nothing.
//
// The lock is the process's: the per-file authoring locks
// (acquireAuthoringLocalLocks) keep ordering each file's writers across
// processes, and every holder of this lock takes them after it, so the two
// never wait on each other in a cycle. The zero value is ready to use.
type botTreeWriteLock struct {
	mu    sync.Mutex
	held  bool
	queue []chan struct{}
}

// botTreeWriteHook is a test seam: called with "wait" and the writer's name
// once a writer that found the lock taken has queued behind its holder, just
// before it blocks — the moment a test can let the holder go knowing the
// waiter is in line. Nil outside tests.
var botTreeWriteHook func(event, writer string)

func botTreeWriteWindow(event, writer string) {
	if botTreeWriteHook != nil {
		botTreeWriteHook(event, writer)
	}
}

// acquire takes the lock for writer, waiting in arrival order behind its
// holder until ctx ends.
func (l *botTreeWriteLock) acquire(ctx context.Context, writer string) error {
	l.mu.Lock()
	if !l.held {
		l.held = true
		l.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	l.queue = append(l.queue, turn)
	l.mu.Unlock()
	botTreeWriteWindow("wait", writer)
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
func (l *botTreeWriteLock) release() {
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

// botTreeWaitError is a writer whose request ended while it waited for the
// bot tree's write lock: nothing was resolved and nothing written, and a
// retry once the holder is done goes through.
type botTreeWaitError struct {
	writer string
	cause  error
}

func (e botTreeWaitError) Error() string {
	return fmt.Sprintf("%s: another write of the bot tree held its write lock until this request ended (%v); nothing was written — retry", e.writer, e.cause)
}

func (e botTreeWaitError) Unwrap() error { return e.cause }

// holdBotTreeWrites takes the bot tree's write lock for writer — before the
// handler resolves anything — and returns its release, idempotent.
func (s *Server) holdBotTreeWrites(ctx context.Context, writer string) (func(), error) {
	if err := s.botTreeWrites.acquire(ctx, writer); err != nil {
		return nil, botTreeWaitError{writer: writer, cause: err}
	}
	var once sync.Once
	return func() { once.Do(s.botTreeWrites.release) }, nil
}

// botTreeBusyCode is the error_code of a write whose request ended while it
// waited for the bot tree's write lock: a 5xx body carries no message, and
// the code is what tells a client the write can be retried as it is.
const botTreeBusyCode = "bot_tree_busy"

// lockBotTree is holdBotTreeWrites for a handler: a wait the request did not
// outlive is answered 503 bot_tree_busy here, and ok is false.
func (s *Server) lockBotTree(w http.ResponseWriter, r *http.Request, writer string) (release func(), ok bool) {
	release, err := s.holdBotTreeWrites(r.Context(), writer)
	if err != nil {
		s.httpErrorCode(w, r, http.StatusServiceUnavailable, botTreeBusyCode, "%v", err)
		return nil, false
	}
	return release, true
}

// lockBotTreeForEditor is lockBotTree for an authoring request naming its
// editor file: a local file's write takes the lock, a stored bot source's
// (botsource://) does not — the store's version CAS orders those, and the
// tree holds none of their files.
func (s *Server) lockBotTreeForEditor(w http.ResponseWriter, r *http.Request, writer, editorPath string) (release func(), ok bool) {
	if strings.HasPrefix(strings.TrimSpace(editorPath), botSourceEditorScheme) {
		return func() {}, true
	}
	return s.lockBotTree(w, r, writer)
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
