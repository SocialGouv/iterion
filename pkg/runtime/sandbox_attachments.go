package runtime

import (
	"context"
	"errors"
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
		return "", fetchFailure(ctx, runID, "list the run's attachments", err)
	}
	if len(list) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "iterion-attachments-")
	if err != nil {
		return "", fmt.Errorf("the attachments of run %s were not materialised (%s): %w", runID, "create the attachments directory", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	materialised := 0
	for _, rec := range list {
		file := attachmentFilename(rec)
		if store.SanitizePathComponent("attachment name", rec.Name) != nil || store.SanitizePathComponent("attachment filename", file) != nil {
			// A record no name reaches: the empty entry a dotted name left in
			// a store that wrote the attachments map through a path (a
			// <node>.<field> gate attachment, before names were stored as
			// keys). Nothing can reference it and nothing can be copied under
			// it; a redelivery would meet it again, so it is no fetch blip.
			if e.logger != nil {
				e.logger.Warn("runtime: run %s: an attachment record with no usable name (name %q, file %q) is skipped — it cannot be referenced or materialised", runID, rec.Name, file)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Join(dir, rec.Name), 0o755); err != nil {
			return "", fmt.Errorf("the attachments of run %s were not materialised (%s): %w", runID, "create the directory of attachment "+rec.Name, err)
		}
		if err := e.fetchAttachment(ctx, runID, rec.Name, filepath.Join(dir, rec.Name, file)); err != nil {
			return "", fetchFailure(ctx, runID, "fetch attachment "+rec.Name, err)
		}
		materialised++
	}
	if materialised == 0 {
		return "", nil
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

// fetchFailure is how a failed FETCH reaches the setup — a store's list or
// read, never a local step (a directory the runner could not create is its
// own plain failure, not the store's). A store that did
// not hand the attachments over under a LIVE context is
// ATTACHMENTS_UNFETCHABLE — resumable, a redelivery fetches again. A fetch
// the run's own context cut short (an operator's cancel, a drain, the
// run's deadline) is the context's outcome: markFailedBestEffort's
// decision (setupFailureStatus) and its mapping to the returned error
// (setupErrFrom) settle it, and it carries no code a runner would
// re-offer the run under.
func fetchFailure(ctx context.Context, runID, step string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(cause, ctxErr) {
			return fmt.Errorf("the attachments of run %s were not materialised (%s): %w", runID, step, cause)
		}
		return fmt.Errorf("the attachments of run %s were not materialised (%s): %v: %w", runID, step, cause, ctxErr)
	}
	return attachmentsUnfetchable(runID, step, cause)
}

// attachmentsUnfetchable is the typed failure of a run whose attachments
// the store could not hand over before the sandbox: nothing of the run
// executed, so it parks resumable (setupFailureStatus) and a redelivery
// fetches again. The cause rides as Cause, not in the message — Error()
// prints it once.
func attachmentsUnfetchable(runID, step string, cause error) *RuntimeError {
	return &RuntimeError{
		Code:    ErrCodeAttachmentsUnfetchable,
		Message: fmt.Sprintf("the run's attachments could not be materialised for the sandbox of run %s (%s)", runID, step),
		Hint:    "the run store did not hand the attachments over; nothing of this attempt executed — resume it once the store answers",
		Cause:   cause,
	}
}
