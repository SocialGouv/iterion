package forge

import (
	"context"
	"errors"
)

// FileRef is a single file's decoded content plus its blob SHA at a ref. The
// SHA is the optimistic-concurrency token: a subsequent PutFile must echo it
// as PrevSHA so a stale write is rejected (ErrFileConflict) instead of
// clobbering a concurrent edit.
type FileRef struct {
	Path    string
	Content []byte // decoded (NOT base64)
	SHA     string // blob SHA — required as PrevSHA for a later PutFile
	Ref     string // the branch/ref the content was read at
}

// PutFile is a single-file atomic write over a forge's contents API. Every
// commit-shaping field is set by the SERVER, never from a request body: the
// config-share editor supplies only file content, and the caller derives
// Message / Branch / Author from the pinned share record.
type PutFile struct {
	Path    string
	Content []byte // raw bytes; the client base64-encodes for the wire
	Message string // commit message (server-controlled template)
	Branch  string // target branch/ref (pinned; never a request-body value)
	// PrevSHA is the blob SHA the edit is based on. REQUIRED to update an
	// existing file: the forge rejects the write (ErrFileConflict) if the
	// blob moved, so a stale editor never silently overwrites a concurrent
	// change. Empty creates a new file.
	PrevSHA string
	// AuthorName/AuthorEmail stamp a FIXED bot identity on the commit (never a
	// real user), so a share edit can't forge attribution. Optional; the
	// forge defaults to the token's app identity when empty.
	AuthorName  string
	AuthorEmail string
}

// FileClient is a minimal single-file read/write over a forge's contents API
// (GitHub Contents, GitLab repository files, Forgejo contents). It is the seam
// the config-share editor writes through: an atomic if-match PUT with no
// clone/worktree, so it never races a bot's concurrent state push. `repo` is
// the provider-native slug, "owner/name".
type FileClient interface {
	GetFile(ctx context.Context, repo, path, ref string) (FileRef, error)
	PutFile(ctx context.Context, repo string, in PutFile) (FileRef, error)
}

// ErrFileNotFound is returned by GetFile when the path does not exist (404).
var ErrFileNotFound = errors.New("forge: file not found")

// ErrFileConflict is returned by PutFile when the forge rejects the write for
// a stale PrevSHA (the blob moved since it was read) — the caller re-reads
// and surfaces a diff rather than overwriting.
var ErrFileConflict = errors.New("forge: file changed since read (stale sha)")
