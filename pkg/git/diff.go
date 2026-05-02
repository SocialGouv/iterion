package git

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// Diff returns the HEAD content (Before) and current working-tree content
// (After) of relPath inside dir, suitable for feeding into Monaco's
// DiffEditor. Either side is nil when the file does not exist there:
//
//   - Before == nil: untracked or freshly added (no HEAD blob)
//   - After  == nil: deleted from the worktree
//
// When the content is binary (NUL byte present in either side), Before
// and After are both nil and Binary is true so the editor can surface a
// "binary file not shown" placeholder instead of feeding raw bytes into
// a text editor.
//
// relPath must already have passed ValidateRelPath. dir must be inside a
// git working tree (else ErrNotGitRepo).
func Diff(dir, relPath string) (DiffPayload, error) {
	if !isGitDir(dir) {
		return DiffPayload{}, ErrNotGitRepo
	}

	payload := DiffPayload{Path: relPath}

	// HEAD-side content via `git show HEAD:path`. A non-zero exit usually
	// means the path is untracked or freshly added — distinguishable from
	// real failures by the stderr message ("does not exist" / "exists on
	// disk, but not in 'HEAD'"), but for our purposes any "no such object"
	// outcome is rendered as Before == nil.
	headOut, headErr := showHead(dir, relPath)
	switch {
	case headErr == nil:
		s := string(headOut)
		payload.Before = &s
	case errors.Is(headErr, errNotInHead):
		// nil Before
	default:
		return DiffPayload{}, headErr
	}

	// Worktree-side content via direct file read. We don't pipe through
	// git here because `git show :path` would mirror the index, not the
	// dirty working copy that the user actually sees on disk.
	abs := filepath.Join(dir, filepath.FromSlash(relPath))
	wtOut, wtErr := os.ReadFile(abs)
	switch {
	case wtErr == nil:
		s := string(wtOut)
		payload.After = &s
	case os.IsNotExist(wtErr):
		// nil After (deleted)
	default:
		return DiffPayload{}, wtErr
	}

	// Binary detection: a NUL byte in either side is the conventional
	// signal git itself uses (see diff.c:buffer_is_binary). We keep the
	// payload metadata but blank the contents so the editor doesn't try
	// to render bytes that aren't valid UTF-8.
	if (payload.Before != nil && bytes.IndexByte([]byte(*payload.Before), 0) >= 0) ||
		(payload.After != nil && bytes.IndexByte([]byte(*payload.After), 0) >= 0) {
		payload.Binary = true
		payload.Before = nil
		payload.After = nil
	}

	return payload, nil
}

// errNotInHead is the sentinel for "this path doesn't exist on HEAD"
// (untracked or newly added). Callers map it to a nil `Before` rather
// than propagating it as a hard error.
var errNotInHead = errors.New("git: path not in HEAD")

// showHead runs `git show HEAD:relPath` and disambiguates the "missing
// from HEAD" exit (which we treat as a nil `Before`) from real failures
// (process spawn errors, other non-zero exits with unfamiliar stderr).
func showHead(dir, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "show", "HEAD:"+relPath)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	msg := stderr.String()
	// `git show` reports several phrasings depending on whether HEAD
	// exists and whether the path was ever tracked. Treat them all as
	// "no HEAD-side content" rather than hard errors.
	if bytes.Contains([]byte(msg), []byte("does not exist")) ||
		bytes.Contains([]byte(msg), []byte("exists on disk, but not in")) ||
		bytes.Contains([]byte(msg), []byte("unknown revision")) ||
		bytes.Contains([]byte(msg), []byte("bad revision")) {
		return nil, errNotInHead
	}
	return nil, err
}
