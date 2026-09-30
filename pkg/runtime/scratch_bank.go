package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// scratchBankMaxBytes caps a run's scratch bank, compressed (ADR-106). Past
// it the teardown banks nothing, says why, and a resume that would need the
// scratch is refused before it claims the run.
const scratchBankMaxBytes int64 = 256 << 20

// scratchBankTimeout bounds the teardown's tar and upload of the scratch, and
// a resume's download and extraction of it.
const scratchBankTimeout = 5 * time.Minute

// scratchBanked is what one teardown did with a container-local scratch.
type scratchBanked struct {
	banked bool
	empty  bool
	bytes  int64
	reason string // why a scratch that held something was not banked
}

func (b scratchBanked) event() map[string]any {
	data := map[string]any{"banked": b.banked, "empty": b.empty}
	if b.banked {
		data["bytes"] = b.bytes
	}
	if b.reason != "" {
		data["reason"] = b.reason
	}
	return data
}

// bankScratchOnCleanup keeps a run's container-local scratch across a park
// (ADR-106). The sandbox dies at teardown and a resume starts a new one —
// kubernetes always does, docker without the host bind too — so what the
// nodes left under ${PROJECT_SCRATCH_DIR} (a verify.sh an agent wrote, a
// source clone, a measurement a later node reads) is streamed out as a
// gzip'd tar into the run's scratch bank, and restoreBankedScratch puts it
// back before the resumed run's first node. A finished run keeps nothing: it
// will not resume. A failed one banks like a park: a rewind brings it back.
func (e *Engine) bankScratchOnCleanup(runID string, active *activeSandbox, emit func(store.EventType, map[string]any) error) {
	if active == nil || active.run == nil || active.scratchHostDir != "" || e.scratchBankHeld {
		return
	}
	bs := store.AsScratchBankStore(e.store)
	ctx, cancel := context.WithTimeout(context.Background(), scratchBankTimeout)
	defer cancel()
	if r, err := e.store.LoadRun(ctx, runID); err == nil && r.Status.IsFinalSuccess() {
		if bs != nil {
			if err := bs.DeleteScratchBank(ctx, runID); err != nil && e.logger != nil {
				e.logger.Warn("runtime: the scratch bank of finished run %s could not be deleted: %v", runID, err)
			}
		}
		return
	}
	got := bankScratch(ctx, active.run, sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes)
	if e.logger != nil {
		switch {
		case got.banked:
			e.logger.Info("runtime: sandbox scratch banked for resume (%d bytes)", got.bytes)
		case got.reason != "":
			e.logger.Warn("runtime: the sandbox scratch was NOT banked — a resume of run %s will be refused SCRATCH_NOT_PORTABLE: %s", runID, got.reason)
		}
	}
	if emit != nil {
		if err := emit(store.EventSandboxScratchBanked, got.event()); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchBanked, err)
		}
	}
}

// bankScratch streams dir, inside run, into runID's scratch bank: a gzip'd
// tar through a host temporary file, so the cap is enforced before anything
// is uploaded and nothing sits in memory. An empty scratch drops a previous
// bank, which would otherwise restore a state the run has moved past. A nil
// bs keeps no bank: a scratch that holds something is recorded as not
// banked, so the resume refuses rather than lose it.
func bankScratch(ctx context.Context, run sandbox.Run, dir string, bs store.ScratchBankStore, runID string, limit int64) scratchBanked {
	res, err := run.Exec(ctx, []string{"sh", "-c", `if [ -d "$1" ]; then find "$1" -mindepth 1 -print -quit; fi`, "sh", dir}, sandbox.ExecOpts{})
	if err != nil {
		return scratchBanked{reason: "the scratch could not be listed: " + err.Error()}
	}
	if res.ExitCode != 0 {
		return scratchBanked{reason: fmt.Sprintf("listing the scratch exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))}
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		if bs == nil {
			return scratchBanked{empty: true}
		}
		if err := bs.DeleteScratchBank(ctx, runID); err != nil {
			return scratchBanked{empty: true, reason: "the scratch is empty, but a previous bank could not be dropped: " + err.Error()}
		}
		return scratchBanked{empty: true}
	}
	if bs == nil {
		return scratchBanked{reason: "this store keeps no scratch bank"}
	}
	tmp, err := os.CreateTemp("", "iterion-scratch-bank-*.tgz")
	if err != nil {
		return scratchBanked{reason: "no host temporary file for the bank: " + err.Error()}
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	capped := &cappedWriter{w: tmp, left: limit}
	var stderr bytes.Buffer
	res, err = run.Exec(ctx, []string{"tar", "-C", dir, "-czf", "-", "."}, sandbox.ExecOpts{Stdout: capped, Stderr: &stderr})
	switch {
	case capped.over:
		return scratchBanked{reason: fmt.Sprintf("the scratch compresses past the %d MiB cap", limit>>20)}
	case err != nil:
		return scratchBanked{reason: "the scratch could not be archived: " + err.Error()}
	case res.ExitCode == 1 && onlyFileChangedWarnings(stderr.String()):
		// GNU tar exits 1 when a file changed while it was read: the
		// archive is complete, the file is the state it caught.
	case res.ExitCode != 0:
		return scratchBanked{reason: fmt.Sprintf("tar exited %d archiving the scratch: %s", res.ExitCode, strings.TrimSpace(stderr.String()))}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return scratchBanked{reason: "the archived scratch could not be re-read: " + err.Error()}
	}
	if err := bs.PutScratchBank(ctx, runID, tmp, capped.n); err != nil {
		return scratchBanked{reason: "the scratch bank could not be stored: " + err.Error()}
	}
	return scratchBanked{banked: true, bytes: capped.n}
}

// cappedWriter refuses the byte past its budget, which ends the stream it
// is copied from.
type cappedWriter struct {
	w    io.Writer
	left int64
	n    int64
	over bool
}

var errScratchOverCap = errors.New("the scratch bank is over its cap")

func (c *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		c.over = true
		return 0, errScratchOverCap
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.left -= int64(n)
	return n, err
}

// onlyFileChangedWarnings reports that every line tar wrote to stderr is its
// "file changed as we read it" notice.
func onlyFileChangedWarnings(stderr string) bool {
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !strings.HasSuffix(line, "file changed as we read it") {
			return false
		}
		seen = true
	}
	return seen
}

// scratchPark is what the run's last teardown recorded about its scratch.
type scratchPark struct {
	recorded bool
	banked   bool
	empty    bool
	reason   string
}

// lastScratchPark reads the last sandbox_scratch_banked event of runID. A run
// with none — parked before banking existed, or whose scratch never lived in
// a sandbox that dies — recorded nothing. Events that cannot be read are an
// error, never "nothing recorded": that reading would skip a restore or a
// refusal the run needs.
func (e *Engine) lastScratchPark(ctx context.Context, runID string) (scratchPark, error) {
	evs, err := e.store.LoadEvents(ctx, runID)
	if err != nil {
		return scratchPark{}, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type != store.EventSandboxScratchBanked {
			continue
		}
		p := scratchPark{recorded: true}
		p.banked, _ = evs[i].Data["banked"].(bool)
		p.empty, _ = evs[i].Data["empty"].(bool)
		p.reason, _ = evs[i].Data["reason"].(string)
		return p, nil
	}
	return scratchPark{}, nil
}

// refuseResumeLosingScratch refuses, before the resume claims the run, a run
// whose last teardown left a scratch it could not bank: resumed, its next
// nodes would find ${PROJECT_SCRATCH_DIR} empty and fail far from the cause.
// --force resumes it anyway, without the scratch, and says so.
func (e *Engine) refuseResumeLosingScratch(ctx context.Context, r *store.Run) error {
	if r == nil {
		return nil
	}
	p, err := e.lastScratchPark(ctx, r.ID)
	if err != nil {
		return fmt.Errorf("runtime: resume run %q: its last teardown's record of the scratch cannot be read: %w", r.ID, err)
	}
	if !p.recorded || p.banked || p.empty {
		return nil
	}
	if e.forceResume {
		if e.logger != nil {
			e.logger.Warn("runtime: run %s resumed with --force WITHOUT the scratch its last teardown could not bank (%s)", r.ID, p.reason)
		}
		return nil
	}
	return &RuntimeError{
		Code:    ErrCodeScratchNotPortable,
		Message: fmt.Sprintf("run %s left files under ${PROJECT_SCRATCH_DIR} in a sandbox that is gone, and its teardown could not bank them (%s): resumed, its next nodes would find the scratch empty", r.ID, p.reason),
		Hint:    "relaunch the run fresh; or resume it with --force to continue without its scratch",
	}
}

// restoreBankedScratch gives a resumed run's new sandbox the scratch its last
// teardown banked, before the first node runs. A bank the teardown recorded
// that cannot be read or extracted fails the resume by name: continuing
// would run the nodes on an empty scratch. The failure also holds the bank,
// so this sandbox's teardown does not replace it with a partial scratch.
// --force continues without it, as it does past the pre-claim refusal.
func (e *Engine) restoreBankedScratch(ctx context.Context, runID string) error {
	if e.activeShare == nil || e.activeShare.Run == nil {
		return nil
	}
	fail := func(what string, err error) error {
		if e.forceResume {
			reason := fmt.Sprintf("%s: %v", what, err)
			if e.logger != nil {
				e.logger.Warn("runtime: run %s resumed with --force WITHOUT its scratch: %s", runID, reason)
			}
			if eerr := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "forced": true, "reason": reason}); eerr != nil && e.logger != nil {
				e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, eerr)
			}
			return nil
		}
		e.scratchBankHeld = true
		return &RuntimeError{
			Code:    ErrCodeScratchNotPortable,
			Message: fmt.Sprintf("run %s: %s: %v", runID, what, err),
			Hint:    "resume again: the bank is kept; or resume with --force to continue without the scratch; or relaunch the run fresh",
		}
	}
	p, err := e.lastScratchPark(ctx, runID)
	if err != nil {
		return fail("the last teardown's record of the scratch cannot be read", err)
	}
	if !p.banked {
		return nil
	}
	bs := store.AsScratchBankStore(e.store)
	if bs == nil {
		return fail("the scratch banked at the last teardown cannot be read", errors.New("this store keeps no scratch bank"))
	}
	rctx, cancel := context.WithTimeout(ctx, scratchBankTimeout)
	defer cancel()
	body, err := bs.OpenScratchBank(rctx, runID)
	if err != nil {
		return fail("the scratch banked at the last teardown cannot be read", err)
	}
	defer body.Close()
	counted := &countingReader{r: body}
	if err := restoreScratch(rctx, e.activeShare.Run, sandboxScratchContainerPath, counted); err != nil {
		return fail("the scratch banked at the last teardown could not be restored", err)
	}
	if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": true, "bytes": counted.n}); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
	}
	return nil
}

// restoreScratch extracts a bank into dir, inside run.
func restoreScratch(ctx context.Context, run sandbox.Run, dir string, body io.Reader) error {
	var stderr bytes.Buffer
	res, err := run.Exec(ctx, []string{"sh", "-c", `mkdir -p "$1" && tar -C "$1" -xzf -`, "sh", dir}, sandbox.ExecOpts{Stdin: body, Stderr: &stderr})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tar exited %d: %s", res.ExitCode, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
