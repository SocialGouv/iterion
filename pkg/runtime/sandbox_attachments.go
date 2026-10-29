package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/store"
)

// attachmentNamesIn lists the attachments a run's attachments directory
// holds — one subdirectory per attachment name, the file beneath it — or
// nothing when the directory is absent, empty or unreadable. It answers one
// question for the sandbox wiring: does this run carry attachments a driver
// has to deliver? A directory that cannot be read answers "no" here and
// fails where it is read for real.
func attachmentNamesIn(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// attachmentsUndeliverable is the typed refusal of a run that holds
// attachments on a driver that can neither bind-mount their directory into
// the container nor copy it in. It carries the names so the record says
// what was given and not delivered.
func attachmentsUndeliverable(driver string, names []string) *RuntimeError {
	return &RuntimeError{
		Code:    ErrCodeAttachmentsUndeliverable,
		Message: fmt.Sprintf("the run's attachments (%s) cannot reach the %s sandbox: the driver has no host filesystem to bind their directory from and does not copy it in, so no node could open them", strings.Join(names, ", "), driver),
		Hint:    "use a sandbox driver that hands files to the container (one with a host filesystem, or one that copies directories in), or launch the run without attachments",
	}
}

// materializeAttachments fetches the attachments a run holds on a store
// with no filesystem root into a directory of the run's own —
// <name>/<filename> under it, files read-only — and returns it, or "" when
// the run holds none. It is the directory the sandbox binds or seeds, and
// the one nodes open when the run executes in this process. A fetch that
// fails is the run's failure, typed: a run that went on without what it
// was given would read as a run that never had it.
func (e *Engine) materializeAttachments(ctx context.Context, runID string) (string, error) {
	list, err := e.store.ListAttachments(ctx, runID)
	if err != nil {
		return "", attachmentsUnfetchable(runID, "list the run's attachments", err)
	}
	if len(list) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "iterion-attachments-")
	if err != nil {
		return "", attachmentsUnfetchable(runID, "create the attachments directory", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	for _, rec := range list {
		if err := store.SanitizePathComponent("attachment name", rec.Name); err != nil {
			return "", attachmentsUnfetchable(runID, "name attachment "+rec.Name, err)
		}
		file := attachmentFilename(rec)
		if err := store.SanitizePathComponent("attachment filename", file); err != nil {
			return "", attachmentsUnfetchable(runID, "name attachment "+rec.Name, err)
		}
		if err := os.MkdirAll(filepath.Join(dir, rec.Name), 0o755); err != nil {
			return "", attachmentsUnfetchable(runID, "create the directory of attachment "+rec.Name, err)
		}
		if err := e.fetchAttachment(ctx, runID, rec.Name, filepath.Join(dir, rec.Name, file)); err != nil {
			return "", attachmentsUnfetchable(runID, "fetch attachment "+rec.Name, err)
		}
	}
	keep = true
	return dir, nil
}

// fetchAttachment streams one attachment from the store into dst, a file
// nobody can write to once closed.
func (e *Engine) fetchAttachment(ctx context.Context, runID, name, dst string) error {
	rc, _, err := e.store.OpenAttachment(ctx, runID, name)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// attachmentsUnfetchable is the typed failure of a run whose attachments
// the store could not hand over before the sandbox.
func attachmentsUnfetchable(runID, step string, cause error) *RuntimeError {
	return &RuntimeError{
		Code:    ErrCodeAttachmentsUndeliverable,
		Message: fmt.Sprintf("the run's attachments could not be materialised for the sandbox (%s): %v", step, cause),
		Hint:    "the run store did not serve the attachments the run holds; launch again once it answers, or launch without attachments",
		Cause:   cause,
	}
}
