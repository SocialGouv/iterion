package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
)

// ErrBotFileChanged is the cause of a refused replacement: the file on disk
// is not the one the writer read, so writing would drop bytes the writer
// never saw. It is the CLI's half of the refusal the server answers with a
// 409 — the studio, the assistant and the unit editor all replace a stored
// bot file only over the bytes their write was decided on.
var ErrBotFileChanged = errors.New("the file changed since it was read")

// ErrBotFileNotTaken is the cause of a refusal where NOTHING was written and
// the file is whole: the authoring lock was not taken, so another writer's
// save is in flight on bytes nobody can compare yet.
var ErrBotFileNotTaken = errors.New("the file's authoring lock was not taken")

// botFileLeftAsItIs reports whether err is one of the two refusals that
// leave the file exactly as it was — the outcome a per-file report calls
// "left as it is" — as opposed to a write that broke halfway, which a
// command stops on. One predicate, because two guards that each cover one
// cause leave the next caller covering neither.
func botFileLeftAsItIs(err error) bool {
	return errors.Is(err, ErrBotFileChanged) || errors.Is(err, ErrBotFileNotTaken)
}

// botFileLockWait bounds how long a CLI writer waits for a save of the same
// file to finish. These commands carry no context of their own (RunFmt,
// RunFix, MigrateDSL, RunImport take none), and a save takes milliseconds:
// the bound lives here, named, rather than as an unbounded wait inside a
// one-shot command. A caller that has a context of its own calls
// replaceBotFile with it.
var botFileLockWait = 15 * time.Second

// replaceBotFile replaces path with data under the authoring lock, over the
// bytes the caller decided on and no others: a save by the studio, the
// assistant or the unit editor that landed since the caller read the file
// is a refusal naming it (ErrBotFileChanged), never a replacement. A nil
// decidedOn means the caller read an ABSENCE — the write is a creation, and
// anything there now refuses the same way.
//
// The lock is the one pkg/server's editors take for the same file
// (pkg/authoringlock owns the identity and the control-directory contract,
// and an agreement test in pkg/server holds the two takes to one lock), so
// the read-back below cannot race a save: it happens under the lock, and
// the save that follows re-reads and refuses on its own check.
func replaceBotFile(ctx context.Context, path string, decidedOn, data []byte, perm os.FileMode) error {
	held := false
	err := authoringlock.Hold(ctx, path, func() error {
		held = true
		current, readErr := os.ReadFile(path)
		switch {
		case readErr == nil && decidedOn == nil:
			return fmt.Errorf("%s: %w — another writer created it since it was read as absent; left as it is", path, ErrBotFileChanged)
		case readErr == nil && !bytes.Equal(current, decidedOn):
			return fmt.Errorf("%s: %w — another writer replaced it since it was read; left as it is", path, ErrBotFileChanged)
		case errors.Is(readErr, fs.ErrNotExist) && decidedOn != nil:
			return fmt.Errorf("%s: %w — another writer deleted it since it was read; nothing is written back", path, ErrBotFileChanged)
		case readErr != nil && !errors.Is(readErr, fs.ErrNotExist):
			return fmt.Errorf("read back %s: %w", path, readErr)
		}
		return writeFileAtomic(path, data, perm)
	})
	switch {
	case err != nil && !held:
		// Nothing was written and the file is whole: the take is the only
		// thing that failed, and it is the one failure a caller may want to
		// tell its user to retry.
		return fmt.Errorf("%s: %w (an editor is saving it, or the %s this writer waits ended): %w", path, ErrBotFileNotTaken, botFileLockWait, err)
	case err != nil:
		return err
	}
	return nil
}

// replaceBotFileNow is replaceBotFile with this package's own bound, for
// the commands that carry no context. Threading the command's context
// (cobra cancels it on SIGINT) through RunFmt, RunFix, MigrateDSL and
// RunImport is a change of their signatures, filed as debt rather than
// smuggled in here.
func replaceBotFileNow(path string, decidedOn, data []byte, perm os.FileMode) error {
	ctx, cancel := context.WithTimeout(context.Background(), botFileLockWait)
	defer cancel()
	return replaceBotFile(ctx, path, decidedOn, data, perm)
}
