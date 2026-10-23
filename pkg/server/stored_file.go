package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// storedFile is a stored file as one reading of it found it: its bytes, or
// its absence.
type storedFile struct {
	bytes  []byte
	exists bool
	// readWhole says a reading of this request read these very bytes whole
	// (a unit load that answered no unit.ShortError): checkStoredFile holds
	// such a write to the bytes it read, and asks no digest of it — the
	// bytes are the decision.
	readWhole bool
}

// storedCheck is what a writer decided its write on: the stored file's name
// as its messages say it, the bytes (or the absence) its decision was taken
// on, and the digest of the stored text the write presents — the digest the
// whole read that seeded it issued (the digest rule)."" for a write decided
// from bytes read in this request (decided holds them).
type storedCheck struct {
	name      string
	decided   storedFile
	presented string
}

// storedAt is a stored file present with bytes b.
func storedAt(b []byte) storedFile { return storedFile{bytes: b, exists: true} }

// storedFileRefusal is a write the stored-file check refused: the status it
// answers — 409 when the stored file is not the one the write was decided
// on, 422 when the reader refuses the file as stored — and why.
type storedFileRefusal struct {
	status int
	msg    string
}

func (e *storedFileRefusal) Error() string { return e.msg }

// checkStoredFile is the one check every server writer that replaces a
// stored bot file it decided on makes — the studio's saves, the unit save,
// the assistant's commits, the run file editor, the manifest edit, every
// bot-source update — on the stored bytes it reads under its lock: the
// authoring lock of a file on disk (writeEditorFile, prepareAuthoringLocal,
// replaceFile), the version a cloud write is pinned to (storeBotSource) —
// before it writes a byte. Writers that decide nothing from a reading of the
// file they replace are not held to it: the generated catalogue, an
// installed package (.botz), git's operations on a run's worktree, the
// CLI's own writers, a whole bot's deletion.
//
//   - the stored file is the one the write was decided on (decided: the
//     bytes its route, its revision or its raw edit was taken from). Another
//     — a writer that held the lock meanwhile, an edit on disk, a file
//     created since its absence was read — is a 409: the decision was taken
//     on bytes the write would not replace.
//   - THE DIGEST RULE — what an open decided, the write carries: a write of
//     an existing WORKFLOW file whose decision came from an earlier whole
//     read (an open's, a bundle fetch's) presents the digest that read
//     issued (presented, sha256 hex), and the stored file must still hash
//     to it, or the write is a 409 naming what changed. An open that did
//     not read a file whole — the read budget, the size cap, a file that is
//     no text the reader reads (not UTF-8) — issues no digest for it, so no
//     write decided from such an open can carry one: nothing is written
//     over what nobody read. A write decided from bytes read IN THIS
//     REQUEST under the write's own lock (a unit save's files, a manifest
//     edit) presents none — decided holds those bytes, readWhole their
//     strict reading here.
//
// name is the file's name in the messages; its extension decides whether it
// is a workflow file.
func checkStoredFile(name string, stored, decided storedFile, presented string) error {
	if stored.exists != decided.exists {
		if !decided.exists {
			return createdSinceRefusal(name)
		}
		return &storedFileRefusal{http.StatusConflict, fmt.Sprintf("%s changed since this write was decided on it — deleted on disk, or by another writer: reload it and redo the edit", name)}
	}
	if presented != "" {
		if !stored.exists || presented != contentSHA256(string(stored.bytes)) {
			return &storedFileRefusal{http.StatusConflict, fmt.Sprintf("%s as stored is not the text this write replaces (its digest differs): reload it and redo the edit", name)}
		}
		return nil
	}
	if len(decided.bytes) > 0 && !bytes.Equal(stored.bytes, decided.bytes) {
		return &storedFileRefusal{http.StatusConflict, fmt.Sprintf("%s changed since this write was decided on it — on disk, or by another writer: reopen it and redo the edit", name)}
	}
	if !stored.exists || !workflowfile.IsWorkflowFile(name) || decided.readWhole {
		return nil
	}
	return &storedFileRefusal{http.StatusConflict, fmt.Sprintf("%s carries no digest of the read it replaces: a save of an existing workflow file presents the digest a whole read of it issued (the open response's), and none came — reopen the file, read whole, and redo the edit", name)}
}

// createdSinceRefusal is the 409 of a write decided on a file's absence
// that meets the file: another writer created it since.
func createdSinceRefusal(name string) error {
	return &storedFileRefusal{http.StatusConflict, fmt.Sprintf("file already exists: %s was written since this write was decided on its absence — reopen it and redo the edit", name)}
}

// checkStoredBotFiles is checkStoredFile for a write of a bot source's files
// (storeBotSource), on the bundle stored at the version the write is pinned
// to (stored) and the bundle it writes (next). EVERY file the write replaces
// — or drops: a whole-bundle push that omits a stored file removes it, a
// replacement by nothing — is held to the digest rule, whatever its
// extension: the write presents the digest of the file AS STORED AT THE
// PINNED VERSION (presented, by path) — what a whole read of that version
// issued, the bundle fetch whose version the write presents as its if-match;
// it issues a digest for every file it answers (botSourceView.FileSHA256),
// so a changed or dropped path without one is a 409 naming it, never a
// silent deletion or a digest-less replacement. A file kept byte for byte
// asks nothing; a creation decides on the bot's absence; removedByName — the
// file a per-file DELETE names — is removed by name, which no reading
// decided.
func checkStoredBotFiles(stored, next, presented map[string]string, removedByName string) error {
	var changed []string
	dropped := map[string]bool{}
	for path, before := range stored {
		after, kept := next[path]
		switch {
		case kept && after == before:
			continue
		case !kept && path == removedByName:
			continue
		case !kept:
			dropped[path] = true
		}
		changed = append(changed, path)
	}
	sort.Strings(changed) // a stable name in the refusal, whatever the map order
	for _, path := range changed {
		// The bundle read issues a digest for every file it answers, so a
		// changed-or-dropped path presented without one is a write decided on
		// no read of it — refused, whatever the file's extension (the
		// workflow-file rule below is checkStoredFile's own, the disk doors').
		if presented[path] == "" {
			err := &storedFileRefusal{http.StatusConflict, fmt.Sprintf("%s carries no digest of the read it replaces: a bot-source write presents, for every file it replaces or omits, the digest the bundle read issued (the response's file_sha256), and none came — reload the bot and redo the edit", path)}
			if dropped[path] {
				// The omission deletes; the remedy rides the refusal's own
				// message, the boundary's wording (writeStoredFileRefusal).
				return &storedFileRefusal{err.status, err.msg + droppedRemedy(path)}
			}
			return err
		}
		at := storedAt([]byte(stored[path]))
		if err := checkStoredFile(path, at, at, presented[path]); err != nil {
			if dropped[path] {
				// Rebuilt, not wrapped: the boundary answers the refusal's
				// own message (writeStoredFileRefusal), and the remedy the
				// omission asks for must ride it.
				var refused *storedFileRefusal
				if errors.As(err, &refused) {
					return &storedFileRefusal{refused.status, refused.msg + droppedRemedy(path)}
				}
			}
			return err
		}
	}
	return nil
}

// droppedRemedy is what a write that omits a stored file is told: the
// omission would delete it, and the deletion it may mean is made by name.
func droppedRemedy(path string) string {
	return fmt.Sprintf(": a write that omits it deletes it — delete it by name (DELETE …/files/%s) if that is the edit, or push it with its digest", path)
}

// writeStoredFileRefusal answers a refusal of checkStoredFile with its
// status; false for any other error, which the caller answers.
func (s *Server) writeStoredFileRefusal(w http.ResponseWriter, r *http.Request, err error) bool {
	var refused *storedFileRefusal
	if !errors.As(err, &refused) {
		return false
	}
	s.httpErrorFor(w, r, refused.status, "%s", refused.msg)
	return true
}
