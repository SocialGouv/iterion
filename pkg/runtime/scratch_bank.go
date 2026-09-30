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

// scratchBankRecordBudget bounds the write of the teardown's record, on its
// own budget: a slow upload must not leave the record no time to land.
const scratchBankRecordBudget = 30 * time.Second

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
//
// ctx is the run's: its identity scopes every store call (the cloud store
// refuses a tenant-less query), and its cancellation — a drain, a lost
// lease, an operator's cancel, all of which reach the teardown first — is
// dropped, so the bank and its record still land.
func (e *Engine) bankScratchOnCleanup(ctx context.Context, runID string, active *activeSandbox) {
	if active == nil || active.run == nil || active.scratchHostDir != "" || e.scratchBankHeld {
		return
	}
	bs := store.AsScratchBankStore(e.store)
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scratchBankTimeout)
	defer cancel()
	if r, err := e.store.LoadRun(bctx, runID); err == nil && r.Status.IsFinalSuccess() {
		if bs != nil {
			if err := bs.DeleteScratchBank(bctx, runID); err != nil && e.logger != nil {
				e.logger.Warn("runtime: the scratch bank of finished run %s could not be deleted: %v", runID, err)
			}
		}
		return
	}
	got := bankScratch(bctx, active.run, sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes)
	if e.logger != nil {
		switch {
		case got.banked:
			e.logger.Info("runtime: sandbox scratch banked for resume (%d bytes)", got.bytes)
		case got.reason != "":
			e.logger.Warn("runtime: the sandbox scratch was NOT banked — a resume of run %s will be refused SCRATCH_NOT_PORTABLE: %s", runID, got.reason)
		}
	}
	wctx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), scratchBankRecordBudget)
	defer cancelWrite()
	if err := e.emit(wctx, runID, store.EventSandboxScratchBanked, "", got.event()); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit %s: %v — a resume of run %s will not know what this teardown banked", store.EventSandboxScratchBanked, err, runID)
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
	// advanced: a node finished after that record, and no teardown banked
	// again — the attempt that ran it lost its sandbox without one (an
	// OOM kill, a lost node). A bank is then older than the run's state.
	advanced bool
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
	advanced := false
	for i := len(evs) - 1; i >= 0; i-- {
		switch evs[i].Type {
		case store.EventNodeFinished:
			advanced = true
			continue
		case store.EventSandboxScratchBanked:
		default:
			continue
		}
		p := scratchPark{recorded: true, advanced: advanced}
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
// It refuses a bank the run has moved past, too: restored, it would revert
// in silence what the nodes finished since wrote there. --force resumes it
// anyway — without the scratch, or with the older bank — and says so.
func (e *Engine) refuseResumeLosingScratch(ctx context.Context, r *store.Run) error {
	if r == nil {
		return nil
	}
	p, err := e.lastScratchPark(ctx, r.ID)
	if err != nil {
		return fmt.Errorf("runtime: resume run %q: its last teardown's record of the scratch cannot be read: %w", r.ID, err)
	}
	var cause string
	switch {
	case !p.recorded || p.empty:
		return nil
	case p.banked && !p.advanced:
		return nil
	case p.banked:
		cause = "nodes finished after its last teardown banked the scratch, in a sandbox lost without a teardown: restored, the bank would revert what they wrote there"
	default:
		cause = fmt.Sprintf("its teardown could not bank the files it left under ${PROJECT_SCRATCH_DIR} in a sandbox that is gone (%s): resumed, its next nodes would find the scratch empty", p.reason)
	}
	if e.forceResume {
		if e.logger != nil {
			e.logger.Warn("runtime: run %s resumed with --force although %s", r.ID, cause)
		}
		return nil
	}
	return &RuntimeError{
		Code:    ErrCodeScratchNotPortable,
		Message: fmt.Sprintf("run %s: %s", r.ID, cause),
		Hint:    "relaunch the run fresh; or resume it with --force to continue as it stands",
	}
}

// restoreBankedScratch gives a resumed run's new sandbox the scratch its last
// teardown banked, before the first node runs. Every failure holds the bank,
// so this sandbox's teardown does not replace it with a partial scratch. A
// bank that is gone or does not extract fails the resume by name
// (SCRATCH_NOT_PORTABLE): continuing would run the nodes on an empty
// scratch, and --force does, as it does past the pre-claim refusal. A read
// that failed on the way — the event log, the bank's store, the exec stream
// — fails it without a code, so the resume is retried, --force or not.
func (e *Engine) restoreBankedScratch(ctx context.Context, runID string) error {
	if e.activeShare == nil || e.activeShare.Run == nil {
		return nil
	}
	gone := func(what string, err error) error {
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
			Message: fmt.Sprintf("run %s: %s", runID, what),
			Hint:    "relaunch the run fresh; or resume it with --force to continue without the scratch",
			Cause:   err,
		}
	}
	unread := func(what string, err error) error {
		e.scratchBankHeld = true
		return fmt.Errorf("runtime: run %s: %s (the bank is kept; a later resume retries it): %w", runID, what, err)
	}
	p, err := e.lastScratchPark(ctx, runID)
	if err != nil {
		return unread("the last teardown's record of the scratch cannot be read", err)
	}
	if !p.banked {
		return nil
	}
	bs := store.AsScratchBankStore(e.store)
	if bs == nil {
		return gone("the scratch banked at the last teardown cannot be read", errors.New("this store keeps no scratch bank"))
	}
	rctx, cancel := context.WithTimeout(ctx, scratchBankTimeout)
	defer cancel()
	body, err := bs.OpenScratchBank(rctx, runID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return gone("the scratch banked at the last teardown is gone", err)
	case err != nil:
		return unread("the scratch banked at the last teardown could not be read", err)
	}
	defer body.Close()
	counted := &countingReader{r: body}
	err = restoreScratch(rctx, e.activeShare.Run, sandboxScratchContainerPath, counted)
	switch {
	case errors.Is(err, errBankDoesNotExtract):
		return gone("the scratch banked at the last teardown does not extract", err)
	case err != nil:
		return unread("the scratch banked at the last teardown could not be streamed into the sandbox", err)
	}
	data := map[string]any{"restored": true, "bytes": counted.n}
	if p.advanced {
		// Only --force gets here: the pre-claim refusal stopped the rest.
		data["stale"] = true
	}
	if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", data); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
	}
	return nil
}

// errBankDoesNotExtract is a bank tar refused: truncated, corrupt, not a
// gzip'd tar. Retrying reads the same bytes.
var errBankDoesNotExtract = errors.New("the bank does not extract")

// restoreScratch extracts a bank into dir, inside run.
func restoreScratch(ctx context.Context, run sandbox.Run, dir string, body io.Reader) error {
	var stderr bytes.Buffer
	res, err := run.Exec(ctx, []string{"sh", "-c", `mkdir -p "$1" && tar -C "$1" -xzf -`, "sh", dir}, sandbox.ExecOpts{Stdin: body, Stderr: &stderr})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w: tar exited %d: %s", errBankDoesNotExtract, res.ExitCode, strings.TrimSpace(stderr.String()))
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
