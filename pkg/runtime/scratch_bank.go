package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
	// unknown: the scratch could not be listed — most often the sandbox is
	// already gone (an OOM kill, an eviction). Whether it held anything is
	// not known, so a resume is not refused over it.
	unknown bool
	bytes   int64
	reason  string // why the scratch was not banked
}

func (b scratchBanked) event() map[string]any {
	data := map[string]any{"banked": b.banked, "empty": b.empty}
	if b.banked {
		data["bytes"] = b.bytes
	}
	if b.unknown {
		data["unknown"] = true
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
		case got.unknown:
			e.logger.Warn("runtime: the sandbox scratch could not be read at teardown — a resume of run %s starts without it: %s", runID, got.reason)
		case got.reason != "":
			e.logger.Warn("runtime: the sandbox scratch was NOT banked — a resume of run %s will be refused SCRATCH_NOT_PORTABLE: %s", runID, got.reason)
		}
	}
	// The record is what a resume decides from: retried within its budget,
	// so a store blip does not leave an exact bank behind an older record.
	wctx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), scratchBankRecordBudget)
	defer cancelWrite()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = e.emit(wctx, runID, store.EventSandboxScratchBanked, "", got.event()); err == nil || wctx.Err() != nil {
			break
		}
		select {
		case <-wctx.Done():
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err != nil && e.logger != nil {
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
		return scratchBanked{unknown: true, reason: "the scratch could not be listed: " + err.Error()}
	}
	if res.ExitCode != 0 {
		return scratchBanked{unknown: true, reason: fmt.Sprintf("listing the scratch exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))}
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

// onlyFileChangedWarnings reports that every line tar wrote to stderr is one
// of its notices for a file a node changed or removed while it was read:
// GNU tar exits 1 on those, with a complete archive of what it caught.
func onlyFileChangedWarnings(stderr string) bool {
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(stderr), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !strings.HasSuffix(line, "file changed as we read it") && !strings.HasSuffix(line, "File removed before we read it") {
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
	unknown  bool
	reason   string
	// advanced: a node that runs in the sandbox finished after that record,
	// and no teardown banked again — the attempt that ran it lost its
	// sandbox without one (an OOM kill, a lost node). A bank is then older
	// than the run's scratch. The engine-side kinds (the human node a resume
	// records as answered, a router, a compute) never wrote there.
	advanced bool
}

// lastScratchPark reads the last sandbox_scratch_banked event of runID. A run
// with none — parked before banking existed, or whose scratch never lived in
// a sandbox that dies — recorded nothing. Events that cannot be read are an
// error, never "nothing recorded": that reading would skip a restore or a
// refusal the run needs.
func (e *Engine) lastScratchPark(ctx context.Context, runID string) (scratchPark, error) {
	return lastScratchPark(ctx, e.store, e.workflow, runID)
}

func lastScratchPark(ctx context.Context, st store.RunStore, wf *ir.Workflow, runID string) (scratchPark, error) {
	var p scratchPark
	err := st.ScanEvents(ctx, runID, func(ev *store.Event) bool {
		switch {
		case ev.Type == store.EventSandboxScratchBanked:
			p = scratchPark{recorded: true}
			p.banked, _ = ev.Data["banked"].(bool)
			p.empty, _ = ev.Data["empty"].(bool)
			p.unknown, _ = ev.Data["unknown"].(bool)
			p.reason, _ = ev.Data["reason"].(string)
		case ev.Type == store.EventNodeFinished && p.recorded && nodeMayWriteScratch(wf, ev.NodeID):
			p.advanced = true
		}
		return true
	})
	if err != nil {
		return scratchPark{}, err
	}
	return p, nil
}

// nodeMayWriteScratch reports whether node id executes in the sandbox, where
// ${PROJECT_SCRATCH_DIR} lives: an agent, a judge, a tool or a subbot does;
// the engine-side kinds never do. A node this workflow does not declare may
// be anything: it counts.
func nodeMayWriteScratch(wf *ir.Workflow, id string) bool {
	if wf == nil {
		return true
	}
	switch wf.Nodes[id].(type) {
	case *ir.HumanNode, *ir.RouterNode, *ir.ComputeNode, *ir.EmitNode, *ir.WaitNode, *ir.AwaitAnswersNode, *ir.DoneNode, *ir.FailNode:
		return false
	}
	return true
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
	cause, err := scratchRefusal(ctx, e.store, e.workflow, r.ID)
	if err != nil || cause == "" {
		return err
	}
	if e.forceResume {
		if e.logger != nil {
			e.logger.Warn("runtime: run %s resumed with --force although %s", r.ID, cause)
		}
		return nil
	}
	return scratchNotPortable(r.ID, cause)
}

// ValidateResumeScratch is the engine's pre-claim scratch refusal, run by
// the resume surface before anything moves the run: an operator hears it
// synchronously, and a cloud resume is refused before the publisher flips
// the run to queued. force waives it, as it does the engine's.
func ValidateResumeScratch(ctx context.Context, st store.RunStore, r *store.Run, wf *ir.Workflow, force bool) error {
	if r == nil || force {
		return nil
	}
	cause, err := scratchRefusal(ctx, st, wf, r.ID)
	if err != nil || cause == "" {
		return err
	}
	return scratchNotPortable(r.ID, cause)
}

// scratchRefusal names why a resume of runID would lose or revert its
// scratch, or returns "" when it would not.
func scratchRefusal(ctx context.Context, st store.RunStore, wf *ir.Workflow, runID string) (string, error) {
	p, err := lastScratchPark(ctx, st, wf, runID)
	if err != nil {
		return "", fmt.Errorf("runtime: resume run %q: its last teardown's record of the scratch cannot be read: %w", runID, err)
	}
	switch {
	case !p.recorded || p.empty || p.unknown:
		return "", nil
	case p.banked && !p.advanced:
		return "", nil
	case p.banked:
		return "nodes finished after its last teardown banked the scratch, in a sandbox lost without a teardown: restored, the bank would revert what they wrote there", nil
	}
	return fmt.Sprintf("its teardown could not bank the files it left under ${PROJECT_SCRATCH_DIR} in a sandbox that is gone (%s): resumed, its next nodes would find the scratch empty", p.reason), nil
}

func scratchNotPortable(runID, cause string) error {
	return &RuntimeError{
		Code:    ErrCodeScratchNotPortable,
		Message: fmt.Sprintf("run %s: %s", runID, cause),
		Hint:    "relaunch the run fresh; or resume it with --force to continue as it stands",
	}
}

// restoreBankedScratch gives a resumed run's new sandbox the scratch its last
// teardown banked, before the first node runs. Every failure holds the bank,
// so this sandbox's teardown does not replace it. A bank that is gone, that
// does not extract, or a resume that runs without a sandbox fails it by name
// (SCRATCH_NOT_PORTABLE): continuing would run the nodes without their
// scratch, and --force does, as it does past the pre-claim refusal. The bank
// is checked on the host before anything reaches the sandbox, so every later
// failure — a read that stops on the way, the stream into the sandbox, its
// tar — is a transport's: the resume fails without a code and is retried,
// --force or not.
func (e *Engine) restoreBankedScratch(ctx context.Context, runID string) error {
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
	if p.unknown {
		// Not refused — the scratch may have held nothing — but said.
		if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "reason": "the last teardown could not read the scratch: " + p.reason}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
		}
		return nil
	}
	if !p.banked {
		return nil
	}
	if e.activeShare == nil || e.activeShare.Run == nil {
		return gone("the run banked its scratch in a sandbox, and this resume runs without one", errResumedWithoutSandbox)
	}
	bs := store.AsScratchBankStore(e.store)
	if bs == nil {
		return gone("the scratch banked at the last teardown cannot be read", errors.New("this store keeps no scratch bank"))
	}
	rctx, cancel := context.WithTimeout(ctx, scratchBankTimeout)
	defer cancel()
	bank, err := fetchScratchBank(rctx, bs, runID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return gone("the scratch banked at the last teardown is gone", err)
	case errors.Is(err, errBankDoesNotExtract):
		return gone("the scratch banked at the last teardown does not extract", err)
	case err != nil:
		return unread("the scratch banked at the last teardown could not be read", err)
	}
	defer bank.release()
	if err := restoreScratch(rctx, e.activeShare.Run, sandboxScratchContainerPath, bank.file); err != nil {
		return unread("the scratch banked at the last teardown could not be streamed into the sandbox", err)
	}
	data := map[string]any{"restored": true, "bytes": bank.size}
	if p.advanced {
		// Only --force gets here: the pre-claim refusal stopped the rest.
		data["stale"] = true
	}
	if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", data); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
	}
	return nil
}

// errResumedWithoutSandbox is a resume of a run that banked its scratch, on
// a path that starts no sandbox (--sandbox none, an unsandboxed runner):
// there is nowhere to restore the bank to.
var errResumedWithoutSandbox = errors.New("no sandbox to restore the scratch into")

// errBankDoesNotExtract is a bank the host refused: truncated, corrupt, not
// a gzip'd tar, larger than the cap. Retrying reads the same bytes.
var errBankDoesNotExtract = errors.New("the bank does not extract")

// fetchedBank is a bank read whole onto the host and checked to extract.
type fetchedBank struct {
	file *os.File
	size int64
}

func (b *fetchedBank) release() {
	_ = b.file.Close()
	_ = os.Remove(b.file.Name())
}

// fetchScratchBank reads runID's bank into a host temporary file and checks
// there that it extracts: a bank that does not is refused before anything
// reaches the sandbox, where a tar failure cannot tell a corrupt archive from
// a broken stream. A read that stops on the way — a reset connection, the
// deadline — is an error of its own.
func fetchScratchBank(ctx context.Context, bs store.ScratchBankStore, runID string) (*fetchedBank, error) {
	body, err := bs.OpenScratchBank(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	tmp, err := os.CreateTemp("", "iterion-scratch-restore-*.tgz")
	if err != nil {
		return nil, fmt.Errorf("no host temporary file for the bank: %w", err)
	}
	bank := &fetchedBank{file: tmp}
	n, err := io.Copy(tmp, io.LimitReader(body, scratchBankMaxBytes+1))
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && n > scratchBankMaxBytes {
		err = fmt.Errorf("%w: it is larger than the %d MiB cap", errBankDoesNotExtract, scratchBankMaxBytes>>20)
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err == nil {
		if xerr := checkBankExtracts(tmp); xerr != nil {
			err = fmt.Errorf("%w: %v", errBankDoesNotExtract, xerr)
		}
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		bank.release()
		return nil, err
	}
	bank.size = n
	return bank, nil
}

// checkBankExtracts reads the gzip'd tar r to its end, checksum included.
func checkBankExtracts(r io.Reader) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		if _, err := tr.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return err
	}
	return zr.Close()
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
