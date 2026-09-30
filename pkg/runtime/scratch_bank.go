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
	"reflect"
	"strings"
	"sync"
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

// recordRetryPauseDefault is the first pause between tries at writing a
// record a resume decides from (emitRecord); it doubles up to
// recordRetryPauseMax, within the write's budget.
const (
	recordRetryPauseDefault = time.Second
	recordRetryPauseMax     = 8 * time.Second
)

// scratchBankAttempts bounds the teardown's tries at banking the scratch, a
// failure another try may cure (scratchBanked.retry) apart. The pause before
// each next try starts at scratchBankRetryPauseDefault and doubles.
// scratchTarAttempts bounds, within one try, the archives of a scratch that
// changes while tar reads it.
const (
	scratchBankAttempts          = 3
	scratchBankRetryPauseDefault = time.Second
	scratchTarAttempts           = 3
	// scratchUploadAttempts bounds the uploads of one archive; the pause
	// before each next one is scratchUploadPause times the tries so far.
	scratchUploadAttempts = 3
	scratchUploadPause    = 250 * time.Millisecond
)

// scratchRacedNamed bounds the raced members a record names.
const scratchRacedNamed = 20

// scratchNamespaceGuard refuses, before anything is signalled (exit
// scratchQuiesceRefused), a shell that does not run in a process namespace
// of its own read through its own /proc: the host's initial namespace —
// whose inode is a kernel constant — a namespace that cannot be read, and a
// /proc that is not this namespace's process table (hidden, replaced,
// another namespace's), whatever the Run declared.
const scratchNamespaceGuard = `ns=$(readlink /proc/self/ns/pid 2>/dev/null) || ns=
case "$ns" in
""|"pid:[4026531836]") echo "the sandbox's process namespace is the host's initial one, or cannot be read: nothing is signalled" >&2; exit 3 ;;
esac
me=
read -r me _ </proc/self/stat 2>/dev/null
if [ "$me" != "$$" ]; then echo "/proc is not this sandbox's process table: nothing is signalled" >&2; exit 3; fi
`

// scratchQuiesceScript stops every process of the sandbox but its first and
// the script itself, then checks that nothing else still runs, stopping
// again what started since: it exits 0 when all stopped, and
// scratchQuiescePartial naming what it could not stop (another user's) or
// could not check (a process whose stat it cannot read, a /proc that hides
// other users' processes). A process's stat is read whole and cut after its
// name's last parenthesis: a name holds anything, a newline included.
const scratchQuiesceScript = scratchNamespaceGuard + `hidden=
if grep -Eq '^[^ ]+ /proc proc [^ ]*hidepid=([1-9]|invisible|noaccess|ptraceable)' /proc/mounts 2>/dev/null; then
  hidden=" (/proc hides other users' processes: they cannot be checked)"
fi
tries=0
while :; do
  kill -STOP -1 2>/dev/null
  left=
  for d in /proc/[0-9]*; do
    p=${d#/proc/}
    case "$p" in 1|$$) continue ;; esac
    s=$(cat "$d/stat" 2>/dev/null)
    if [ -z "$s" ]; then
      [ -d "$d" ] && left="$left $p(unreadable)"
      continue
    fi
    s=${s##*) }
    set -- $s
    [ "${2:-}" = "$$" ] && continue
    case "${1:-}" in T|t|Z|X|x) ;; *) left="$left $p($(tr -d '\n' <"$d/comm" 2>/dev/null))" ;; esac
  done
  if [ -z "$left" ]; then
    [ -z "$hidden" ] && exit 0
    echo "not verified$hidden" >&2
    exit 4
  fi
  tries=$((tries+1))
  if [ "$tries" -ge 5 ]; then echo "not stopped:$left$hidden" >&2; exit 4; fi
  sleep 0.2 2>/dev/null || sleep 1
done`

// scratchQuiesceRefused is the scripts' exit when scratchNamespaceGuard
// refused them: nothing was signalled. scratchQuiescePartial is
// scratchQuiesceScript's exit when some processes still run, or could not be
// checked.
const (
	scratchQuiesceRefused = 3
	scratchQuiescePartial = 4
)

// scratchResumeBudget bounds the resume of the processes a quiesce stopped,
// on a budget of its own: the teardown's may be spent by then.
const scratchResumeBudget = 30 * time.Second

// scratchResumeScript lets the stopped processes go on, under the same
// guard as the quiesce.
const scratchResumeScript = scratchNamespaceGuard + `kill -CONT -1 2>/dev/null
exit 0`

// scratchBanked is what one teardown did with a container-local scratch.
type scratchBanked struct {
	banked bool
	empty  bool
	// unknown: the scratch could not be listed — most often the sandbox is
	// already gone (an OOM kill, an eviction). Whether it held anything is
	// not known, so a resume is not refused over it; a bank recorded before
	// it still stands (lastScratchPark).
	unknown bool
	bytes   int64
	reason  string // why the scratch was not banked
	// retry: the failure is a transport's or the store's — a blip on the
	// exec into the sandbox, tar caught mid-write, an upload that failed —
	// and another try may bank the scratch.
	retry bool
	// raced: the members tar caught changing on its every archive of the
	// banked scratch (sandbox.TarRace).
	raced []string
	// unquiesced: why the sandbox's processes were not stopped before tar
	// read the scratch — a write tar reports nothing of may have torn it.
	unquiesced string
	// resumeFailed: why the processes the quiesce stopped could not be let
	// go on — logged, not recorded: the bank does not depend on it.
	resumeFailed string
}

// knows ranks what a try saw of the scratch: nothing (unknown), an empty
// scratch, or files.
func (b scratchBanked) knows() int {
	switch {
	case b.unknown:
		return 0
	case b.empty:
		return 1
	}
	return 2
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
	if len(b.raced) > 0 {
		data["raced"] = b.raced[:min(len(b.raced), scratchRacedNamed)]
		data["raced_count"] = len(b.raced)
	}
	if b.unquiesced != "" {
		data["unquiesced"] = b.unquiesced
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
// A failure another try may cure is tried again, within the same budget.
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
	pause := e.scratchBankRetryPause
	if pause <= 0 {
		pause = scratchBankRetryPauseDefault
	}
	bank := func() scratchBanked {
		b := bankScratch(bctx, active.run, sandboxScratchContainerPath, bs, runID, scratchBankMaxBytes)
		if b.resumeFailed != "" && e.logger != nil {
			e.logger.Warn("runtime: run %s: %s — the sandbox's shutdown may wait its grace period", runID, b.resumeFailed)
		}
		return b
	}
	got := bank()
	for attempt := 1; got.retry && attempt < scratchBankAttempts; attempt++ {
		if e.logger != nil {
			e.logger.Warn("runtime: banking the scratch of run %s failed (%s) — trying again in %s", runID, got.reason, pause)
		}
		select {
		case <-bctx.Done():
		case <-time.After(pause):
		}
		if bctx.Err() != nil {
			break
		}
		pause *= 2
		next := bank()
		if next.knows() < got.knows() {
			// What an earlier try saw stands: a sandbox killed between two
			// tries cannot be listed any more, but the files it held were
			// not banked.
			break
		}
		got = next
	}
	if e.logger != nil {
		switch {
		case got.banked && len(got.raced) > 0:
			e.logger.Warn("runtime: sandbox scratch banked for resume (%d bytes), but tar caught %d member(s) changing on every archive — they are banked as it caught them: %s", got.bytes, len(got.raced), strings.Join(got.raced[:min(len(got.raced), scratchRacedNamed)], ", "))
		case got.banked:
			e.logger.Info("runtime: sandbox scratch banked for resume (%d bytes)", got.bytes)
		case got.unknown:
			e.logger.Warn("runtime: the sandbox scratch could not be read at teardown — a resume of run %s starts without it: %s", runID, got.reason)
		case got.reason != "":
			e.logger.Warn("runtime: the sandbox scratch was NOT banked — a resume of run %s will be refused SCRATCH_NOT_PORTABLE: %s", runID, got.reason)
		}
	}
	// The record is what a resume decides from: a store that fails its write
	// — a blip, a failover — does not leave an exact bank behind an older
	// record while the budget lasts.
	wctx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), e.recordWriteBudget())
	defer cancelWrite()
	if err := e.emitRecord(wctx, runID, store.EventSandboxScratchBanked, got.event()); err != nil && e.logger != nil {
		e.logger.Warn("runtime: emit %s: %v — a resume of run %s will not know what this teardown banked", store.EventSandboxScratchBanked, err, runID)
	}
}

// recordWriteBudget bounds the write of a record a resume decides from.
func (e *Engine) recordWriteBudget() time.Duration {
	if e.recordWriteLimit > 0 {
		return e.recordWriteLimit
	}
	return scratchBankRecordBudget
}

// emitRecord writes an event a later resume decides from. A store that fails
// the write is tried again, the pause doubling, until ctx's budget runs out:
// a failure is never taken for "nothing to record".
func (e *Engine) emitRecord(ctx context.Context, runID string, t store.EventType, data map[string]any) error {
	pause := e.recordRetryPause
	if pause <= 0 {
		pause = recordRetryPauseDefault
	}
	for {
		err := e.emit(ctx, runID, t, "", data)
		if err == nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		pause = min(pause*2, recordRetryPauseMax)
	}
}

// bankScratch streams dir, inside run, into runID's scratch bank: a gzip'd
// tar through a host temporary file, so the cap is enforced before anything
// is uploaded and nothing sits in memory. An empty scratch drops a previous
// bank, which would otherwise restore a state the run has moved past. A nil
// bs keeps no bank: a scratch that holds something is recorded as not
// banked, so the resume refuses rather than lose it.
func bankScratch(ctx context.Context, run sandbox.Run, dir string, bs store.ScratchBankStore, runID string, limit int64) (got scratchBanked) {
	// "$1/": a scratch that is a symlink to a directory is listed as tar
	// archives it, through the link.
	res, err := run.Exec(ctx, []string{"sh", "-c", `if [ -d "$1" ]; then find "$1/" -mindepth 1 -print -quit; fi`, "sh", dir}, sandbox.ExecOpts{})
	if err != nil {
		return scratchBanked{unknown: true, retry: true, reason: "the scratch could not be listed: " + err.Error()}
	}
	if res.ExitCode != 0 {
		return scratchBanked{unknown: true, retry: true, reason: fmt.Sprintf("listing the scratch exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))}
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		if bs == nil {
			return scratchBanked{empty: true}
		}
		if err := bs.DeleteScratchBank(ctx, runID); err != nil {
			return scratchBanked{empty: true, retry: true, reason: "the scratch is empty, but a previous bank could not be dropped: " + err.Error()}
		}
		return scratchBanked{empty: true}
	}
	if bs == nil {
		return scratchBanked{reason: "this store keeps no scratch bank"}
	}
	// Nothing else writes the scratch while tar reads it: this sandbox dies
	// after its bank, and its export already ran, so every process in it
	// but its first and this shell is stopped. A write tar reports nothing
	// of — a file moved between two directories, a page written through a
	// shared mapping — would otherwise leave an archive torn in silence.
	// Only in a sandbox whose commands run in a process namespace of their
	// own: the same signal from a host shell would stop the host's.
	var unquiesced, resumeFailed string
	resume := func() {}
	// Runs last: what the resume of the stopped processes met, whichever
	// return ends the banking.
	defer func() { got.resumeFailed = resumeFailed }()
	if pi, ok := run.(sandbox.ProcessIsolated); !ok || !pi.ProcessIsolated() {
		unquiesced = "the sandbox is not known to run in a process namespace of its own: its processes were not stopped"
	} else {
		q, err := run.Exec(ctx, []string{"sh", "-c", scratchQuiesceScript}, sandbox.ExecOpts{})
		switch {
		case err != nil:
			unquiesced = "the sandbox's processes could not be stopped: " + err.Error()
		case q.ExitCode == 0:
		case q.ExitCode == scratchQuiescePartial:
			unquiesced = "some of the sandbox's processes could not be stopped: " + strings.TrimSpace(string(q.Stderr))
		default:
			unquiesced = fmt.Sprintf("stopping the sandbox's processes exited %d: %s", q.ExitCode, strings.TrimSpace(string(q.Stderr)))
		}
		if err != nil || q.ExitCode != scratchQuiesceRefused {
			// Whatever the script got to before it ended, the processes it
			// may have stopped go on once the archive is taken — or when
			// anything cuts the banking short: held stopped, they keep the
			// sandbox's shutdown waiting its grace period (an entrypoint
			// such as tini waits on a stopped child).
			resume = sync.OnceFunc(func() {
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scratchResumeBudget)
				defer cancel()
				r, err := run.Exec(rctx, []string{"sh", "-c", scratchResumeScript}, sandbox.ExecOpts{})
				switch {
				case err != nil:
					resumeFailed = "the stopped processes could not be resumed: " + err.Error()
				case r.ExitCode != 0:
					resumeFailed = fmt.Sprintf("resuming the stopped processes exited %d: %s", r.ExitCode, strings.TrimSpace(string(r.Stderr)))
				}
			})
			defer resume()
		}
	}
	// GNU tar exits 1 when a member changed or vanished while it read it
	// (sandbox.TarRace): the archive then holds what it caught, which may be
	// no state the scratch was ever in — a file torn between two writes, a
	// file renamed into place missing. tar runs again while it races, into
	// the other of two host files: the last complete archive that raced
	// stays aside, and is banked with what raced recorded when no later try
	// does better — a clean archive — before a failure or the budget ends
	// the tries.
	var files [2]*os.File
	for i := range files {
		f, err := os.CreateTemp("", "iterion-scratch-bank-*.tgz")
		if err != nil {
			return scratchBanked{reason: "no host temporary file for the bank: " + err.Error()}
		}
		defer func() {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}()
		files[i] = f
	}
	cur, spare := files[0], files[1]
	var (
		capped    *cappedWriter
		stderr    bytes.Buffer
		kept      *os.File
		keptBytes int64
		keptRaced []string
	)
	for try := 1; try <= scratchTarAttempts; try++ {
		if try > 1 && ctx.Err() != nil {
			break
		}
		if _, err := cur.Seek(0, io.SeekStart); err != nil {
			return scratchBanked{reason: "the host temporary file for the bank could not be rewound: " + err.Error()}
		}
		if err := cur.Truncate(0); err != nil {
			return scratchBanked{reason: "the host temporary file for the bank could not be emptied: " + err.Error()}
		}
		capped = &cappedWriter{w: cur, left: limit}
		stderr.Reset()
		started := time.Now()
		res, err = run.Exec(ctx, []string{"tar", "-C", dir, "-czf", "-", "."}, sandbox.ExecOpts{Stdout: capped, Stderr: &stderr, Env: map[string]string{"LC_ALL": sandbox.TarLocale}})
		if err != nil || capped.over || res.ExitCode != 1 {
			break
		}
		members, only := sandbox.TarRace(stderr.String())
		if !only {
			break
		}
		kept, keptBytes, keptRaced = cur, capped.n, members
		cur, spare = spare, cur
		// Another archive, and its upload, must fit what is left of the
		// budget: the raced one is banked rather than lost to it.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 3*time.Since(started) {
			break
		}
	}
	// The archive is taken: the stopped processes go on before the upload.
	resume()
	archive, size, raced := cur, capped.n, []string(nil)
	switch {
	case err == nil && !capped.over && res.ExitCode == 0:
	case kept != nil:
		archive, size, raced = kept, keptBytes, keptRaced
	case capped.over:
		return scratchBanked{reason: fmt.Sprintf("the scratch compresses past the %d MiB cap", limit>>20)}
	case err != nil:
		return scratchBanked{retry: true, reason: "the scratch could not be archived: " + err.Error()}
	default:
		return scratchBanked{retry: true, reason: fmt.Sprintf("tar exited %d archiving the scratch: %s", res.ExitCode, strings.TrimSpace(stderr.String()))}
	}
	// The upload is tried again on the same archive: the store's blip is
	// not a reason to archive the scratch again.
	var perr error
	for try := 1; try <= scratchUploadAttempts; try++ {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return scratchBanked{reason: "the archived scratch could not be re-read: " + err.Error()}
		}
		if perr = bs.PutScratchBank(ctx, runID, archive, size); perr == nil || ctx.Err() != nil || try == scratchUploadAttempts {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(try) * scratchUploadPause):
		}
	}
	if perr != nil {
		return scratchBanked{retry: true, reason: "the scratch bank could not be stored: " + perr.Error(), unquiesced: unquiesced}
	}
	return scratchBanked{banked: true, bytes: size, raced: raced, unquiesced: unquiesced}
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

// scratchPark is what the run's last teardown recorded about its scratch.
type scratchPark struct {
	recorded bool
	banked   bool
	empty    bool
	unknown  bool
	// forsaken: a resume went on without the bank (--force past a bank that
	// is gone, or with no sandbox to restore it into). The run's scratch no
	// longer starts from it: nothing is restored or refused over it.
	forsaken bool
	// raced: tar caught members of the banked scratch changing on its every
	// archive; the record names them.
	raced bool
	// unquiesced: the sandbox's processes were not all stopped while tar
	// read the scratch; the record says why.
	unquiesced bool
	reason     string
	// advanced: a node finished in the sandbox after the bank was recorded,
	// and no teardown banked again — the attempt that ran it lost its sandbox
	// without one (an OOM kill, a lost node). A bank is then older than the
	// run's scratch. Only a node that ran there and succeeded counts
	// (ranInSandbox): one that failed re-runs from the checkpoint; a rewind
	// that dropped it takes it back; a stale bank --force restored is the
	// run's scratch again.
	advanced bool
	// superseded: an execution started after that record and wrote none of
	// its own. Its teardown may still be banking — which only the holder of
	// the run's lock knows — or its sandbox was lost without one.
	superseded bool
	// lossShown: a resume since the bank met a loss only a restore finds
	// (restoreBankedScratch's gone) and was refused over it, lossReason
	// saying why. The operator has been shown it: a --force given since
	// accepts it, and one of the bank itself (lostForGood) is refused before
	// the claim from then on.
	lossShown  string
	lossReason string
}

// nodeFinishedAnswered marks the node_finished a resume emits for the node
// its answer finishes: nothing ran in a sandbox for it.
const nodeFinishedAnswered = "answered"

// nodeFinishedInSandbox marks, on every node_finished, whether the workflow
// that executed the node runs it in the sandbox (nodeMayWriteScratch) — the
// fact a resume ages the scratch bank from, whatever the source says by
// then. Metadata: an underscore key, outside the node's output.
const nodeFinishedInSandbox = "_in_sandbox"

// nodeFinishedOnCycle marks, on every node_finished, whether the node lies on
// a cycle of the workflow that executed it (onCycle): a rewind that drops it
// cannot take back the passes it ran there, whatever the source says by then.
const nodeFinishedOnCycle = "_on_cycle"

// lastScratchPark reads the last sandbox_scratch_banked event of runID. A run
// with none — parked before banking existed, or whose scratch never lived in
// a sandbox that dies — recorded nothing. Events that cannot be read are an
// error, never "nothing recorded": that reading would skip a restore or a
// refusal the run needs.
//
// A record the teardown could not read the scratch for (unknown) does not
// replace a bank before it: the sandbox that teardown lost started from that
// bank, which is still stored and, unless a node finished since, still the
// state the run resumes from. What a sandbox started from is read from
// sandbox_scratch_restored.
func (e *Engine) lastScratchPark(ctx context.Context, runID string) (scratchPark, error) {
	return lastScratchPark(ctx, e.store, e.workflow, runID)
}

func lastScratchPark(ctx context.Context, st store.RunStore, wf *ir.Workflow, runID string) (scratchPark, error) {
	var p scratchPark
	// aging: the nodes that finished in the sandbox since the record, less
	// those a rewind dropped since, each with whether it finished on a cycle.
	aging := map[string]bool{}
	err := st.ScanEvents(ctx, runID, func(ev *store.Event) bool {
		switch ev.Type {
		case store.EventSandboxScratchBanked:
			if unknown, _ := ev.Data["unknown"].(bool); unknown && p.banked {
				p.superseded = false
				return true
			}
			p = scratchPark{recorded: true}
			clear(aging)
			p.banked, _ = ev.Data["banked"].(bool)
			p.empty, _ = ev.Data["empty"].(bool)
			p.unknown, _ = ev.Data["unknown"].(bool)
			p.reason, _ = ev.Data["reason"].(string)
			_, p.raced = ev.Data["raced"]
			_, p.unquiesced = ev.Data["unquiesced"]
		case store.EventSandboxScratchRestored:
			restored, _ := ev.Data["restored"].(bool)
			forced, _ := ev.Data["forced"].(bool)
			refused, _ := ev.Data["refused"].(bool)
			stale, _ := ev.Data["stale"].(bool)
			hostBacked, _ := ev.Data["host_backed"].(bool)
			switch {
			case restored && hostBacked:
				// Restored into a host directory that keeps the scratch from
				// then on: no bank decides what a later resume finds there.
				p = scratchPark{}
				clear(aging)
			case !restored && forced && p.banked:
				p.banked, p.forsaken = false, true
				clear(aging)
			case !restored && refused && p.banked:
				p.lossShown, _ = ev.Data["loss"].(string)
				p.lossReason, _ = ev.Data["reason"].(string)
				// The refusal held the bank: that execution banks nothing
				// more, and its record is the one to decide from.
				p.superseded = false
			case restored && stale:
				clear(aging)
			}
		case store.EventNodeFinished:
			if p.banked && ranInSandbox(wf, ev) {
				aging[ev.NodeID] = aging[ev.NodeID] || finishedOnCycle(wf, ev)
			}
		case store.EventRunRewound:
			for _, id := range droppedNodes(ev.Data["dropped_nodes"]) {
				// A node on a cycle may have run passes the rewind does not
				// replay — it keeps the loop's counter and replays the pass
				// it lands on — and their writes are not in the bank.
				if cyclic, ok := aging[id]; ok && !cyclic {
					delete(aging, id)
				}
			}
		case store.EventRunStarted, store.EventRunResumed:
			p.superseded = p.recorded
		}
		return true
	})
	if err != nil {
		return scratchPark{}, err
	}
	p.advanced = len(aging) > 0
	return p, nil
}

// ranInSandbox reports that ev, a node_finished, closes a node that ran in
// the sandbox and succeeded. A node that failed — killed with its sandbox,
// or abandoned with its branch — re-runs from the checkpoint, and the node
// a resume records as finished by its answer ran nowhere. Where the node ran
// is what the workflow that executed it said, carried by the finish
// (nodeFinishedInSandbox): an edited source may since have changed the
// node's kind or renamed it. A finish written before that fact travelled is
// read from the node's kind in wf.
func ranInSandbox(wf *ir.Workflow, ev *store.Event) bool {
	if ev.Data[nodeFinishedAnswered] == true || ev.Data["error"] != nil {
		return false
	}
	if in, ok := ev.Data[nodeFinishedInSandbox].(bool); ok {
		return in
	}
	return nodeMayWriteScratch(wf, ev.NodeID)
}

// finishedOnCycle reports that ev, a node_finished, closes a node that lies
// on a cycle of the workflow that executed it (nodeFinishedOnCycle). A finish
// written before that fact travelled is read from wf.
func finishedOnCycle(wf *ir.Workflow, ev *store.Event) bool {
	if c, ok := ev.Data[nodeFinishedOnCycle].(bool); ok {
		return c
	}
	return onCycle(wf, ev.NodeID)
}

// onCycle reports that node id lies on a cycle of wf's graph — a loop or a
// foreach body — and so may have run more than once. A workflow not known
// may loop anywhere.
func onCycle(wf *ir.Workflow, id string) bool {
	if wf == nil {
		return true
	}
	next := make(map[string][]string, len(wf.Edges))
	for _, e := range wf.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	seen := map[string]bool{}
	stack := append([]string(nil), next[id]...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == id {
			return true
		}
		if !seen[n] {
			seen[n] = true
			stack = append(stack, next[n]...)
		}
	}
	return false
}

// droppedNodes reads a run_rewound event's dropped_nodes, whatever slice
// type the store decoded it into.
func droppedNodes(v any) []string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil
	}
	ids := make([]string, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		if id, ok := rv.Index(i).Interface().(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// nodeMayWriteScratch reports whether node id executes in the sandbox, where
// ${PROJECT_SCRATCH_DIR} lives: an agent, a judge, a tool, a subbot or an
// LLM router (a delegate with the sandbox's tools) does; the engine-side
// kinds never do. A node this workflow does not declare may be anything: it
// counts.
func nodeMayWriteScratch(wf *ir.Workflow, id string) bool {
	if wf == nil {
		return true
	}
	switch n := wf.Nodes[id].(type) {
	case *ir.RouterNode:
		return n.RouterMode == ir.RouterLLM
	case *ir.HumanNode, *ir.ComputeNode, *ir.EmitNode, *ir.WaitNode, *ir.AwaitAnswersNode, *ir.DoneNode, *ir.FailNode:
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
// the run to queued. It decides from a record the run's latest execution
// wrote: an execution that wrote none may still be banking, and the
// engine's check decides that run, under the run's lock, once the teardown
// is over. force waives it, as it does the engine's.
func ValidateResumeScratch(ctx context.Context, st store.RunStore, r *store.Run, wf *ir.Workflow, force bool) error {
	if r == nil || force {
		return nil
	}
	p, err := readScratchPark(ctx, st, wf, r.ID)
	if err != nil || p.superseded {
		return err
	}
	if cause := p.refusal(); cause != "" {
		return scratchNotPortable(r.ID, cause)
	}
	return nil
}

// scratchRefusal names why a resume of runID would lose or revert its
// scratch, or returns "" when it would not.
func scratchRefusal(ctx context.Context, st store.RunStore, wf *ir.Workflow, runID string) (string, error) {
	p, err := readScratchPark(ctx, st, wf, runID)
	if err != nil {
		return "", err
	}
	return p.refusal(), nil
}

// readScratchPark is lastScratchPark for a resume's check: a timeline that
// cannot be read refuses the resume.
func readScratchPark(ctx context.Context, st store.RunStore, wf *ir.Workflow, runID string) (scratchPark, error) {
	p, err := lastScratchPark(ctx, st, wf, runID)
	if err != nil {
		return scratchPark{}, fmt.Errorf("runtime: resume run %q: its last teardown's record of the scratch cannot be read: %w", runID, err)
	}
	return p, nil
}

// refusal names why a resume from this park would lose or revert the
// scratch, or returns "" when it would not.
func (p scratchPark) refusal() string {
	switch {
	case !p.recorded || p.empty || p.unknown || p.forsaken:
		return ""
	case p.banked && lostForGood(p.lossShown):
		return p.lossReason
	case p.banked && !p.advanced:
		return ""
	case p.banked:
		return "nodes finished after its last teardown banked the scratch, in a sandbox lost without a teardown: restored, the bank would revert what they wrote there"
	}
	return fmt.Sprintf("its teardown could not bank the files it left under ${PROJECT_SCRATCH_DIR} in a sandbox that is gone (%s): resumed, its next nodes would find the scratch empty", p.reason)
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
// (SCRATCH_NOT_PORTABLE), recorded: continuing would run the nodes without
// their scratch, and --force does once that loss was shown — a --force given
// before it was is refused. The bank
// is checked on the host before anything reaches the sandbox, so every later
// failure — a read that stops on the way, the stream into the sandbox, its
// tar — is a transport's: the resume fails without a code and is retried,
// --force or not.
func (e *Engine) restoreBankedScratch(ctx context.Context, runID string) error {
	unread := func(what string, err error) error {
		e.scratchBankHeld = true
		return fmt.Errorf("runtime: run %s: %s (the bank is kept; a later resume retries it): %w", runID, what, err)
	}
	p, err := e.lastScratchPark(ctx, runID)
	if err != nil {
		return unread("the last teardown's record of the scratch cannot be read", err)
	}
	// gone is a loss only a restore finds. --force goes on without the
	// scratch past one the operator was shown — a resume refused over it
	// since the bank; a --force given before — for an edited source — was
	// given not knowing it: refused, and recorded, so the next --force
	// accepts it knowing it.
	gone := func(loss, what string, err error) error {
		reason := fmt.Sprintf("%s: %v", what, err)
		if e.forceResume && p.lossShown == loss {
			if e.logger != nil {
				e.logger.Warn("runtime: run %s resumed with --force WITHOUT its scratch: %s", runID, reason)
			}
			if eerr := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "forced": true, "reason": reason}); eerr != nil && e.logger != nil {
				e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, eerr)
			}
			return nil
		}
		e.scratchBankHeld = true
		if eerr := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "refused": true, "loss": loss, "reason": reason}); eerr != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, eerr)
		}
		hint := "relaunch the run fresh; or resume it with --force to continue without the scratch"
		if e.forceResume {
			hint = "relaunch the run fresh; or resume it with --force again, now knowing this loss, to continue without the scratch"
		}
		return &RuntimeError{
			Code:    ErrCodeScratchNotPortable,
			Message: fmt.Sprintf("run %s: %s", runID, what),
			Hint:    hint,
			Cause:   err,
		}
	}
	if p.unknown {
		// Not refused — the scratch may have held nothing — but said.
		if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "reason": "the last teardown could not read the scratch: " + p.reason}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
		}
		return nil
	}
	if !p.banked {
		if cause := p.refusal(); cause != "" && e.forceResume {
			// Only --force gets here past a scratch its teardown could not
			// bank: said, as a stale bank's restore is.
			if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", map[string]any{"restored": false, "forced": true, "reason": cause}); err != nil && e.logger != nil {
				e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
			}
		}
		return nil
	}
	if e.activeShare == nil || e.activeShare.Run == nil {
		return gone(lossNoSandbox, "the run banked its scratch in a sandbox, and this resume runs without one", errResumedWithoutSandbox)
	}
	bs := store.AsScratchBankStore(e.store)
	if bs == nil {
		return gone(lossNoBankStore, "the scratch banked at the last teardown cannot be read", errors.New("this store keeps no scratch bank"))
	}
	rctx, cancel := context.WithTimeout(ctx, scratchBankTimeout)
	defer cancel()
	bank, err := fetchScratchBank(rctx, bs, runID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return gone(lossBankGone, "the scratch banked at the last teardown is gone", err)
	case errors.Is(err, errBankDoesNotExtract):
		return gone(lossBankCorrupt, "the scratch banked at the last teardown does not extract", err)
	case err != nil:
		return unread("the scratch banked at the last teardown could not be read", err)
	}
	defer bank.release()
	if err := restoreScratch(rctx, e.activeShare.Run, sandboxScratchContainerPath, bank.file); err != nil {
		return unread("the scratch banked at the last teardown could not be streamed into the sandbox", err)
	}
	data := map[string]any{"restored": true, "bytes": bank.size}
	if p.raced {
		// The bank holds members as tar caught them changing: its record
		// names them.
		data["raced"] = true
	}
	if p.unquiesced {
		// Something may have written the scratch while tar read it: the
		// record says why the sandbox was not stopped.
		data["unquiesced"] = true
	}
	decides := false
	if p.advanced {
		// Only --force gets here: the pre-claim refusal stopped the rest.
		data["stale"] = true
		decides = true
	}
	if !e.activeShare.ScratchContainerLocal {
		// This sandbox's scratch is a host directory, which keeps it from
		// here on: what the bank held lives there now, and the bank stops
		// deciding what a later resume finds (lastScratchPark).
		data["host_backed"] = true
		decides = true
	}
	if !decides {
		// A later resume reads nothing from this record: said, as the rest
		// are.
		if err := e.emit(ctx, runID, store.EventSandboxScratchRestored, "", data); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit %s: %v", store.EventSandboxScratchRestored, err)
		}
		return nil
	}
	// A later resume decides from this record — a host directory's handover,
	// a stale bank made the run's scratch again: written within its budget,
	// or this resume fails and the next one restores the bank again.
	wctx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), e.recordWriteBudget())
	defer cancelWrite()
	if err := e.emitRecord(wctx, runID, store.EventSandboxScratchRestored, data); err != nil {
		return unread("the record of the scratch's restore could not be written", err)
	}
	return nil
}

// The losses only a restore finds: the bank's own, which every later restore
// finds too (lostForGood), and this resume's, which a resume that starts a
// sandbox on a store that keeps banks does not meet.
const (
	lossBankGone    = "gone"
	lossBankCorrupt = "does_not_extract"
	lossNoSandbox   = "no_sandbox"
	lossNoBankStore = "no_bank_store"
)

// lostForGood reports a loss of the bank itself.
func lostForGood(loss string) bool {
	return loss == lossBankGone || loss == lossBankCorrupt
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
		// %v: the host's missing temporary directory is not the bank's.
		return nil, fmt.Errorf("no host temporary file for the bank: %v", err)
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
