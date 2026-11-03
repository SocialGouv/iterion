package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/safepath"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The read-back drain (ADR-143 §3): the driver streams a declared
// read-back path's content out of the pod at teardown and lands it in the
// host directory the run file store owns. The drain is the clause that
// makes naming the path legal on a pod — the variable is set *because* the
// content is recovered, not merely because a directory exists.

// ExportRunFiles implements [sandbox.WorkspaceExporter]: it drains every
// declared read-back dir ([sandbox.RunInfo.ReadBackDirs]) into the host
// directory each entry names. No-op when nothing was declared — a run that
// named no read-back path collects nothing by construction, and the no-op
// is honest because the variable naming the path is equally absent. Every
// declared dir is attempted and the failures travel joined, so one lost
// tree does not mute the report on the others.
func (r *Run) ExportRunFiles(ctx context.Context) error {
	if len(r.info.ReadBackDirs) == 0 {
		return nil
	}
	var lost []error
	for _, dir := range r.info.ReadBackDirs {
		if err := r.drainRunFilesDir(ctx, dir); err != nil {
			lost = append(lost, err)
		}
	}
	return errors.Join(lost...)
}

// drainRunFilesDir drains one declared read-back dir: the pod's archiver
// streams the tree, the host extracts it under dir.HostDir with the path
// guards the workspace export uses. The pod must still be up — the engine
// calls this at teardown, before the sandbox is destroyed.
//
// A writer racing the archive makes tar warn and exit 1 exactly as it does
// for the workspace export beside which this drain runs (the same teardown
// window, the same measured race), so the drain carries the export's
// discipline: onlyTarRaceWarnings names the one failure a retry can cure,
// and the retry bounds are the export's (exportRetrySettings). A tree that
// keeps changing keeps its last cause. And because tar never deletes, a
// retry that lands after a raced attempt sweeps what only that attempt
// wrote (dropRacedDrainLeftovers) — the landed set is the archive that
// finally succeeded, not the union of every attempt's. The same promise
// covers a type flip between attempts: a dir where the settled archive
// carries a file or a link, or the mirror, is replaced by the extract — the
// dir only where the before snapshot proves the drain wrote it
// (dropRacedDrainDir); a pre-drain tree there is not the drain's, and the
// member is skipped like every other member the host declines.
//
// A tree that never settles across more than one attempt leaves a STALE
// set: no attempt succeeded, so no sweep runs, and the host dir holds the
// union of every attempt's members — a superset of the pod's final tree.
// So does a failure no retry cures when it follows a raced attempt: it ends
// the drain before any sweep, and what the raced attempt landed stands
// beside what the failing one did. Both say so — in the error, because the
// panel will serve the set, and on the loss (sandbox.ReadBackLoss.Stale),
// because the event carries it. A single raced attempt — the whole retry
// budget, or the only one before the drain's context ended the wait for the
// next — leaves its own archive and nothing beside it: unsettled, and no
// union to call stale (settleUnsettledDrain).
//
// Every judgment the drain makes of the host dir — the pre-drain snapshot,
// each attempt's extract, the sweeps — reads the one directory drainRoot
// resolves, once: a host dir reached through a link of its own is walked
// where the writes land, never as the link a walk stops at.
//
// The cap meters that union, never one attempt's archive alone
// (drainState): what uploads is what the host holds, so every attempt's
// files count together, each where it landed once, at the size its latest
// landing left — a tool still writing at teardown under rotating names
// cannot land the cap once per attempt. A member of a later attempt that
// needs the room only earlier attempts' leftovers hold sweeps them first
// (makeDrainRoom): the sweep a settled attempt would have run, run early,
// so an archive that settles is never refused for what a raced one left.
//
// Every failure travels typed (sandbox.ReadBackLoss): the reason the
// runtime's event carries is read off the error, at the site that knows
// it, never off its text. So do the members the host declined: a drain
// whose set landed short of one is a loss on the run too (reason
// declined), never a clean drain with a log line beside it — counted in
// full, named up to sandbox.ReadBackDeclinedListed, and warned in the
// run's log up to the same bound (drainMemberLines).
func (r *Run) drainRunFilesDir(ctx context.Context, dir sandbox.ReadBackDir) error {
	if err := os.MkdirAll(dir.HostDir, 0o775); err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: create host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	root, err := drainRoot(dir.HostDir)
	if err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: resolve host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	s := newDrainState(dir.Cap())
	defer r.sayQuietLines(s, dir)
	// The sweep's anchor: the host dir's state BEFORE any archive landed.
	// An entry it holds is the store's (or, on a bind, the pod's own) — a
	// raced attempt is never allowed to take it. The directories are
	// anchored beside the files: a raced extract builds dirs as readily as
	// it writes files, and a dir the host already had is as untouchable as
	// a file it already had.
	before, beforeDirs, err := readBackDirEntries(root)
	if err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: list host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	attempts, pause := exportRetrySettings(r.driver.logger)
	var last error
	var lastDeclined sandbox.ReadBackDeclinedSet
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, x, err := r.drainOnce(ctx, dir, root, before, beforeDirs, s)
		if err == nil {
			if attempt > 1 {
				r.driver.logger.Warn("sandbox: run-files drain of %s succeeded on attempt %d/%d, after tar saw the tree change mid-archive", dir.ContainerPath, attempt, attempts)
				if err := r.dropRacedDrainLeftovers(root, before, beforeDirs, x.landed, s, "absent from the one that succeeded"); err != nil {
					// The sweep stopped short: what a raced attempt alone
					// landed may still stand beside the settled set.
					return settleDrainLoss(dir, lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: %w", dir.ContainerPath, err)), &x.declined, true)
				}
			}
			return settleDrainLoss(dir, nil, &x.declined, false)
		}
		if !retryable {
			// A cause no retry cures ends the drain where it stands — before
			// any sweep: after a raced attempt, what that attempt landed
			// and this one did not replace stays beside this one's own.
			return settleDrainLoss(dir, err, &x.declined, attempt > 1)
		}
		last, lastDeclined = err, x.declined
		r.driver.logger.Warn("sandbox: run-files drain of %s attempt %d/%d: tar saw the tree change mid-archive, retrying", dir.ContainerPath, attempt, attempts)
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return settleUnsettledDrain(dir, attempt, fmt.Sprintf("retry abandoned (%v)", ctx.Err()), last, &lastDeclined)
			case <-time.After(pause):
			}
			pause *= 2
		}
	}
	return settleUnsettledDrain(dir, attempts, fmt.Sprintf("the pod's run files kept changing under the drain through the %d attempt(s) its retry budget allows", attempts), last, &lastDeclined)
}

// drainRoot is the directory a declared dir's drain lands in and judges
// everything against: hostDir made absolute, every link on its way — its
// own leaf included — resolved. Where a write lands is read through the
// links in its chain (safepath.Landing), so a host dir reached through a
// link would put every landing outside a root left unresolved; and the
// pre-drain snapshot, each attempt's extract and both sweeps read this one
// directory, resolved once per drain: a walk handed the link stops at it,
// sees nothing the host held, and would take every pre-drain entry for the
// drain's own.
func drainRoot(hostDir string) (string, error) {
	abs, err := filepath.Abs(hostDir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// settleUnsettledDrain is the verdict of a drain no attempt settled: ran
// attempts each raced a writer, why says what ended the retries, last is
// the final attempt's failure and declined what it declined. The stale mark
// and the text follow ran, and agree: more than one attempt leaves the
// union of their members on the host — tar never deletes, and no settled
// attempt swept — a stale superset of the pod's final tree; a single one
// leaves its own archive and nothing beside it, which no reader may be told
// is a union.
func settleUnsettledDrain(dir sandbox.ReadBackDir, ran int, why string, last error, declined *sandbox.ReadBackDeclinedSet) error {
	stale := ran > 1
	what := "what its one attempt landed stays and uploads — the archive tar saw the tree change under, and no other attempt's members beside it"
	if stale {
		what = fmt.Sprintf("what the %d attempts landed stays and uploads as the union of every attempt's members the dir's cap holds, a stale superset of the pod's final tree (a file the run removed between attempts may be among them)", ran)
	}
	return settleDrainLoss(dir, lost(sandbox.ReadBackUnsettled, fmt.Errorf("kubernetes: drain %s: %s — %s: %w", dir.ContainerPath, why, what, last)), declined, stale)
}

// drainState is what one declared dir's drain carries across its attempts:
// the meter of what the host holds against the dir's cap, and the run log's
// budget for the lines the drain writes per member.
//
// The meter is the host's, never one archive's: tar never deletes, and a
// drain no attempt settles keeps every attempt's files (the union the
// stale mark reports), so the figure is asked of what stands in the host
// dir — each file the drain landed counted once, at the size its latest
// landing left, under the place the write landed (safepath.Landing,
// HostDir-relative). The place, not the member's name: a link an earlier
// member laid can send a name elsewhere, and a removal releases exactly
// the bytes of what it removed only when both are keyed where the bytes
// are. Every site that changes what the drain's files hold says so here —
// a landing (land), a file replaced by a link or a dir or removed with a
// raced tree (forget, file by file, as the removal's walk met them), a
// sweep (keepOnly) — so the meter never counts less than the host holds.
type drainState struct {
	// capBytes is the dir's figure (sandbox.ReadBackDir.Cap).
	capBytes int64
	// held is the bytes each file the drain landed holds, by landing;
	// total their sum.
	held  map[string]int64
	total int64
	// warned is how many per-member lines reached the run's log, quiet how
	// many were held back past drainMemberLines.
	warned, quiet int
}

// drainMemberLines bounds the per-member lines one dir's drain writes to
// the run's log — a declined member, a raced leftover swept, a raced dir
// replaced. The driver's logger is the run's, teed into the store in cloud
// (flow-sandbox-isolation.md), and an archive is the pod's to fill: past
// the bound the lines are counted, and one line says how many
// (sayQuietLines). The bound is the one the drain's loss names declined
// members up to, so the log never names more members than the run's event
// can.
const drainMemberLines = sandbox.ReadBackDeclinedListed

func newDrainState(capBytes int64) *drainState {
	return &drainState{capBytes: capBytes, held: map[string]int64{}}
}

// room is what the file landing at key may hold with the set still within
// the cap: the cap less every other file's bytes — what key holds now is
// what the landing replaces, never billed twice.
func (s *drainState) room(key string) int64 {
	return s.capBytes - (s.total - s.held[key])
}

// land records the file at key holding n bytes.
func (s *drainState) land(key string, n int64) {
	s.total += n - s.held[key]
	s.held[key] = n
}

// forget records that the file at key is gone — replaced by a link or a
// directory, or removed with a raced tree — and holds nothing any more.
func (s *drainState) forget(key string) {
	s.total -= s.held[key]
	delete(s.held, key)
}

// keepOnly forgets every file keep does not hold — what a sweep removed.
func (s *drainState) keepOnly(keep func(key string) bool) {
	for k, n := range s.held {
		if !keep(k) {
			s.total -= n
			delete(s.held, k)
		}
	}
}

// warnMember writes one per-member line to the run's log while the dir's
// budget lasts (drainMemberLines), and counts it past that.
func (r *Run) warnMember(s *drainState, format string, args ...any) {
	if s.warned < drainMemberLines {
		s.warned++
		r.driver.logger.Warn(format, args...)
		return
	}
	s.quiet++
}

// sayQuietLines closes a dir's drain in the run's log: the per-member lines
// held back past the bound are counted in one, so the log says how much it
// left unsaid and where the full count lives.
func (r *Run) sayQuietLines(s *drainState, dir sandbox.ReadBackDir) {
	if s.quiet > 0 {
		r.driver.logger.Warn("sandbox: run-files drain of %s: %d more per-member line(s) held back past the first %d — the drain's verdict counts every member it declined", dir.ContainerPath, s.quiet, drainMemberLines)
	}
}

// makeDrainRoom answers a member that needs need bytes at key when the set
// has no room left for it (drainState.room): if what only earlier, raced
// attempts landed holds the bytes that would make it fit, the drain sweeps
// it — the sweep a settled attempt runs (dropRacedDrainLeftovers), run
// now, against what this attempt landed so far, because the cap needs
// the room before the attempt can settle. The newer archive is the better
// view of the pod's tree, and those leftovers are exactly what a settled
// attempt would sweep. Nothing is swept when the member fits as it is,
// when this attempt's own stream has no room for it (attemptRoom), or when
// the leftovers' bytes would not make it fit: the meter refuses it then,
// and they stand — the union the stale mark reports. Reports whether it
// swept: a link in the member's path may have gone with the leftovers, and
// the caller re-reads where the write lands.
func (r *Run) makeDrainRoom(root string, before, beforeDirs, landed map[string]bool, s *drainState, key string, need, attemptRoom int64) (bool, error) {
	room := s.room(key)
	if need <= room || need > attemptRoom {
		return false, nil
	}
	earlier := func(k string) bool { return !landed[k] && !before[k] }
	var leftovers int64
	for k, n := range s.held {
		if k != key && earlier(k) {
			leftovers += n
		}
	}
	if leftovers == 0 || need > room+leftovers {
		return false, nil
	}
	why := fmt.Sprintf("swept before any attempt settled because %s, a later attempt's member, needed the room under the dir's %s cap", key, sandbox.ReadBackCapText(s.capBytes))
	if err := r.dropRacedDrainLeftovers(root, before, beforeDirs, landed, s, why); err != nil {
		return true, lost(sandbox.ReadBackRefused, fmt.Errorf("make room for %s: %w", key, err))
	}
	s.keepOnly(func(k string) bool { return !earlier(k) })
	return true, nil
}

// lost types one drain failure with the reason the runtime's event carries;
// sandbox.ReadBackLossReasonOf reads it back through every wrap above.
func lost(reason sandbox.ReadBackLossReason, err error) error {
	return &sandbox.ReadBackLoss{Reason: reason, Err: err}
}

// drainDeclinedNamed bounds the declined members a drain's error text names
// one by one; the loss counts every one and names the first
// sandbox.ReadBackDeclinedListed (sandbox.ReadBackLoss.Declined), the event
// lists those, and the runner's log warned the first ones as the drain met
// them (drainMemberLines).
const drainDeclinedNamed = 5

// settleDrainLoss is the dir's verdict once an attempt ended its drain: err
// is that attempt's failure (nil when it landed its set), declined the
// members it declined, stale whether a raced attempt's members may stand
// beside its own. A clean set is nil; a set short only of declined members
// is a ReadBackDeclined loss naming them; a failure keeps its own reason —
// the outermost typed loss still says what ended the drain — and carries
// the declined members and the stale mark beside it, its text saying both.
func settleDrainLoss(dir sandbox.ReadBackDir, err error, declined *sandbox.ReadBackDeclinedSet, stale bool) error {
	listed, count := declined.List(), declined.Count()
	for i := range listed {
		listed[i].Dir = dir.ContainerPath
	}
	if err == nil {
		if count == 0 {
			return nil
		}
		return &sandbox.ReadBackLoss{
			Reason:        sandbox.ReadBackDeclined,
			Err:           fmt.Errorf("kubernetes: drain %s: the rest of the set landed, but the host declined %s", dir.ContainerPath, declinedText(listed, count)),
			Declined:      listed,
			DeclinedCount: count,
		}
	}
	if count == 0 && !stale {
		return err
	}
	reason := sandbox.ReadBackLossReasonOf(err)
	text := err
	if stale && reason != sandbox.ReadBackUnsettled {
		text = fmt.Errorf("%w — and an earlier attempt that raced a writer landed members this one never replaced: they stay and upload with what it landed, a stale superset of the pod's final tree", text)
	}
	if count > 0 {
		text = fmt.Errorf("%w; the host also declined %s", text, declinedText(listed, count))
	}
	return &sandbox.ReadBackLoss{Reason: reason, Err: text, Declined: listed, DeclinedCount: count, Stale: stale}
}

// declinedText names declined members for an error: the count of all, then
// the first drainDeclinedNamed of listed — the set's own order, dir then
// name then why — each with its reason.
func declinedText(listed []sandbox.ReadBackDeclinedMember, count int) string {
	named := make([]string, 0, min(len(listed), drainDeclinedNamed))
	for _, m := range listed[:min(len(listed), drainDeclinedNamed)] {
		named = append(named, fmt.Sprintf("%s (%s)", m.Name, m.Why))
	}
	text := fmt.Sprintf("%d member(s) of the pod's archive: %s", count, strings.Join(named, ", "))
	if more := count - len(named); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}

// drainExtract is what one attempt's extract did: the bytes it metered, the
// members it wrote — HostDir-relative and slash-separated, the record a
// retried drain's sweep keeps against everything else a raced attempt may
// have left — and the members it declined, counted in full and kept up to
// the bound the loss names them to.
type drainExtract struct {
	drained  int64
	landed   map[string]bool
	declined sandbox.ReadBackDeclinedSet
}

// drainOnce streams one archive of dir.ContainerPath and lands it under
// root, dir.HostDir as drainRoot resolved it. retryable reports a failure
// onlyTarRaceWarnings accepts as transient — the workspace export's own
// predicate; x is what the extract did, as far as it got. before and
// beforeDirs are the two halves of root's pre-drain state — files and
// links, and the directories beside them — the proof a wrong-kind occupant
// is judged on; s is what the dir's attempts share (drainState).
func (r *Run) drainOnce(ctx context.Context, dir sandbox.ReadBackDir, root string, before, beforeDirs map[string]bool, s *drainState) (retryable bool, x drainExtract, err error) {
	kubectlArgs := []string{"--namespace", r.namespace,
		"exec", r.podName, "--container", "workload", "--",
		"env", "LC_ALL=" + sandbox.TarLocale, "tar", "-C", dir.ContainerPath, "-cf", "-", "."}
	// The archiver's own cancel: an extract that stops reading stops it
	// (stopPodArchiver).
	archiverCtx, stopArchiver := context.WithCancel(ctx)
	defer stopArchiver()
	podTar := r.driver.kubectl.cmdContext(archiverCtx, kubectlArgs...)

	pipe, err := podTar.StdoutPipe()
	if err != nil {
		return false, x, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: pod tar stdout pipe: %w", dir.ContainerPath, err))
	}
	var podErr bytes.Buffer
	podTar.Stderr = &podErr

	if err := podTar.Start(); err != nil {
		return false, x, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: start the pod's archiver: %w", dir.ContainerPath, err))
	}
	x, xerr := r.extractDrain(pipe, root, before, beforeDirs, s)
	if xerr != nil {
		// The extract is done reading — the cap, the stream, the host
		// ended it, typed where it failed — so the archiver is stopped,
		// never read to its end for nothing; then the failure is reported.
		stopPodArchiver(stopArchiver, podTar)
		return false, x, fmt.Errorf("kubernetes: drain %s into %s: %w", dir.ContainerPath, dir.HostDir, xerr)
	}
	if err := podTar.Wait(); err != nil {
		return onlyTarRaceWarnings(err, podErr.String()), x, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: in-pod tar: %w\n%s", dir.ContainerPath, err, strings.TrimSpace(podErr.String())))
	}
	r.driver.logger.Info("sandbox: drained %d bytes of run files from pod %s:%s into %s", x.drained, r.podName, dir.ContainerPath, dir.HostDir)
	return false, x, nil
}

// extractDrain writes the pod's archive under root and returns what it did
// (drainExtract) — the landed record is the successful attempt's own
// account of what it put there, which is what a retried drain's sweep
// keeps against everything else a raced attempt may have left. root is the
// host dir as drainRoot resolved it — absolute, clean, every link on its
// way resolved — and every path below is joined to it and judged against
// it. before and beforeDirs are the two halves of root's pre-drain state —
// files and links, and the directories beside them: the proof a wrong-kind
// occupant at a member's path is judged on (the drain's to replace when it
// wrote it, the store's or the pod's when it did not). s is what the dir's
// attempts share: the meter of what the host holds against the dir's cap
// (drainState), and the run log's budget for per-member lines.
//
// The host reads the archive itself, like the workspace export does: the
// member names are chosen in the sandbox, by an archiver the workflow's
// own image provides, so the decision is made on the path a write lands
// on after every existing symlink in its parent chain is resolved. Unlike
// the workspace export there are no name exclusions — a run file is the
// run's product whatever it is named — and every member the host declines
// (sandbox.ReadBackDeclineWhy: a name that cannot be made safe, a link
// that leaves the tree, a hardlink to no file this archive landed, a kind
// it does not write back, a pre-drain tree where the archive carries a file
// or a link) is skipped, warned, and recorded for the run's event, and the
// members after it still land. Only the host's own failure to write — and
// the cap — end the extract.
func (r *Run) extractDrain(stream io.Reader, root string, before, beforeDirs map[string]bool, s *drainState) (drainExtract, error) {
	x := drainExtract{landed: map[string]bool{}}
	// The regular files this attempt landed that still stand as such: the
	// only targets a hardlink member may be landed from.
	files := map[string]bool{}
	decline := func(name string, why sandbox.ReadBackDeclineWhy, format string, args ...any) {
		x.declined.Add(sandbox.ReadBackDeclinedMember{Name: name, Why: why})
		r.warnMember(s, format, args...)
	}
	// landing is where a write to abs lands, HostDir-relative: the key the
	// meter holds the file's bytes under (drainState).
	landing := func(abs string) (string, error) {
		key, err := safepath.Landing(root, abs)
		if err != nil {
			return "", lost(sandbox.ReadBackRefused, fmt.Errorf("resolve where %s lands: %w", abs, err))
		}
		return key, nil
	}
	// meterFile readies a regular member of need bytes about to land at abs:
	// room made from what only earlier attempts left when that is what it
	// needs (makeDrainRoom), then where the write lands — read after the
	// sweep, which may have taken a link in its path — and the budget it is
	// metered against: the smaller of what this attempt's own stream has
	// left and what the set has room for there.
	meterFile := func(abs string, need int64) (key string, budget int64, err error) {
		if key, err = landing(abs); err != nil {
			return "", 0, err
		}
		attemptRoom := s.capBytes - x.drained
		swept, err := r.makeDrainRoom(root, before, beforeDirs, x.landed, s, key, need, attemptRoom)
		if err != nil {
			return "", 0, err
		}
		if swept {
			if key, err = landing(abs); err != nil {
				return "", 0, err
			}
		}
		return key, max(0, min(attemptRoom, s.room(key))), nil
	}
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return x, lost(sandbox.ReadBackArchiver, fmt.Errorf("read the pod's archive: %w", err))
		}
		// A pax global header is archive metadata, not a member: nothing of
		// the set is in it, and nothing of the set is missing for it.
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := safepath.GuardName(hdr.Name); err != nil {
			decline(strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./"), sandbox.DeclinedUnsafeName, "sandbox: drain refused a member of the pod's archive: %v", err)
			continue
		}
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(hdr.Name)), "./")
		if clean == "" || clean == "." {
			continue
		}
		abs, err := safepath.Join(root, clean)
		if err != nil {
			decline(clean, sandbox.DeclinedUnsafeName, "sandbox: drain refused %q from the pod's archive: %v", hdr.Name, err)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// A file or link where the archive carries a dir is replaced —
			// the mirror of the file branch's own replace: the pod's view of
			// a path it names wins over the single non-dir member standing
			// there, a raced attempt's or the pre-drain state's alike, as
			// writeDrainFile's overwrite of a pre-drain file does. Only a
			// TREE is never taken on the archive's word (dropRacedDrainDir).
			if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
				key, err := landing(abs)
				if err != nil {
					return x, err
				}
				if err := os.Remove(abs); err != nil {
					return x, lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", abs, err))
				}
				s.forget(key)
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return x, lost(sandbox.ReadBackRefused, fmt.Errorf("create %s: %w", abs, err))
			}
			// A dir member is a member the extract wrote: the sweep keeps
			// it — an empty dir the settled archive carries is content,
			// not a raced leftover, and the sweep cannot tell the two
			// apart except through this record.
			x.landed[clean] = true
			delete(files, clean)
		case tar.TypeReg:
			// A directory where the archive carries a file is what a raced
			// attempt's extract leaves behind; the file branch cannot write
			// over it. Judged on the before snapshot, never taken blindly:
			// a pre-drain tree there is not the drain's, and the member is
			// skipped — tr.Next skips its bytes — with the rest still to land.
			kept, err := r.dropRacedDrainDir(root, before, beforeDirs, abs, s)
			if err != nil {
				return x, err
			}
			if kept != "" {
				decline(clean, sandbox.DeclinedPreDrainTree, "sandbox: drain skipped %s: the pod's archive carries it as a file, but %s predates the drain — the tree there is not the drain's to replace", clean, kept)
				continue
			}
			key, budget, err := meterFile(abs, hdr.Size)
			if err != nil {
				return x, err
			}
			n, err := r.writeDrainFile(abs, clean, s.capBytes, budget, hdr.FileInfo().Mode(), tr)
			x.drained += n
			if err != nil {
				return x, err
			}
			s.land(key, n)
			x.landed[clean] = true
			files[clean] = true
		case tar.TypeLink:
			// A file with several names is archived once: tar carries every
			// name but the first as a hardlink to that first member, which
			// the same archive landed before it. The host lands the link as
			// a copy of what that member landed — a tool that names one
			// report twice gets both names in its set — metered like any
			// file, since the upload carries both. A link to anything else
			// (a name the archive never landed as a file here, one it
			// declined, one outside the tree) is declined.
			target := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(hdr.Linkname)), "./")
			if safepath.GuardName(hdr.Linkname) != nil || !files[target] {
				decline(clean, sandbox.DeclinedHardlinkTarget, "sandbox: drain skipped the hardlink %s -> %s: its target is no file this drain landed", clean, hdr.Linkname)
				continue
			}
			src, err := safepath.Join(root, target)
			if err != nil {
				decline(clean, sandbox.DeclinedHardlinkTarget, "sandbox: drain skipped the hardlink %s -> %s: %v", clean, hdr.Linkname, err)
				continue
			}
			srcInfo, err := os.Lstat(src)
			if err != nil || !srcInfo.Mode().IsRegular() {
				decline(clean, sandbox.DeclinedHardlinkTarget, "sandbox: drain skipped the hardlink %s -> %s: its target no longer stands as a file this drain landed", clean, hdr.Linkname)
				continue
			}
			kept, err := r.dropRacedDrainDir(root, before, beforeDirs, abs, s)
			if err != nil {
				return x, err
			}
			if kept != "" {
				decline(clean, sandbox.DeclinedPreDrainTree, "sandbox: drain skipped the hardlink %s -> %s: %s predates the drain — the tree there is not the drain's to replace", clean, hdr.Linkname, kept)
				continue
			}
			key, budget, err := meterFile(abs, srcInfo.Size())
			if err != nil {
				return x, err
			}
			n, err := r.copyDrainFile(abs, clean, src, srcInfo, s.capBytes, budget)
			x.drained += n
			if err != nil {
				return x, err
			}
			s.land(key, n)
			x.landed[clean] = true
			files[clean] = true
		case tar.TypeSymlink:
			// A link carries no bytes of the set — only the files it points
			// into do, when their own member lands — so neither a landed
			// link nor a refused one moves the meter: a refused link (its
			// target leaves the tree) is a member dropped with its reason,
			// and a landed one adds nothing to count. The refusal is judged
			// first: a member that lands nothing removes nothing.
			if _, inside := symlinkLanding(root, abs, hdr.Linkname); !inside {
				decline(clean, sandbox.DeclinedLinkLeavesTree, "sandbox: drain skipped the link %s -> %s: it leaves the run-files tree", abs, hdr.Linkname)
				continue
			}
			// A directory where the archive carries a link is the same
			// raced occupant the file branch cures — the link cannot be
			// written over it — judged on the same before snapshot, and
			// skipped on the same witness.
			kept, err := r.dropRacedDrainDir(root, before, beforeDirs, abs, s)
			if err != nil {
				return x, err
			}
			if kept != "" {
				decline(clean, sandbox.DeclinedPreDrainTree, "sandbox: drain skipped the link %s -> %s: %s predates the drain — the tree there is not the drain's to replace", clean, hdr.Linkname, kept)
				continue
			}
			// The link replaces whatever file stands at its path: that
			// file's bytes leave the set with it.
			key, err := landing(abs)
			if err != nil {
				return x, err
			}
			wrote, err := r.writeExportSymlink(root, abs, hdr)
			if err != nil {
				return x, lost(sandbox.ReadBackRefused, err)
			}
			if wrote {
				s.forget(key)
				x.landed[clean] = true
				delete(files, clean)
			}
		default:
			// A device, a fifo: each is a way to point a later write
			// somewhere it was never meant to reach, and none is a file a
			// tool produces as its product.
			decline(clean, sandbox.DeclinedKind, "sandbox: drain skipped %s: the pod's archive carries it as type %q, which the host does not write back", abs, string(hdr.Typeflag))
		}
	}
	return x, nil
}

// copyDrainFile lands a hardlink member at abs as a copy of src, the
// regular file this attempt landed under the link's target (srcInfo, its
// Lstat). The file opened must be the one judged: a path swapped between
// the Lstat and the open is no file this drain landed, and the link is
// refused rather than landed from it. The copy goes through
// writeDrainFile — staged, metered against the same budget, renamed over
// abs only once whole.
func (r *Run) copyDrainFile(abs, member, src string, srcInfo os.FileInfo, capBytes, budget int64) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("open %s for the hardlink %s: %w", src, member, err))
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("stat %s for the hardlink %s: %w", src, member, err))
	}
	if !os.SameFile(srcInfo, opened) {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("the hardlink %s: %s changed between its check and its open", member, src))
	}
	return r.writeDrainFile(abs, member, capBytes, budget, srcInfo.Mode(), f)
}

// writeDrainFile writes one regular member — member is its name in the set,
// HostDir-relative, mode the mode the member carries — and returns the
// bytes it copied, metered as it goes against budget, the room its caller's
// meter leaves it (drainState): an archive that declares one size and
// carries another is billed for what actually arrived, and a member that
// takes the set past the cap (capBytes, the dir's own figure, named in the
// refusal) is the failure, named.
//
// The member is staged beside its path and renamed over it only once it is
// whole and within the budget: a write the cap or the pod's stream cuts
// short never stands at the member's path, where the upload would carry it
// as a file that looks entire. The rename replaces a file or a link the
// path holds — the host writes the file the member names, never through a
// link the archive left there first — and a dir there was judged before
// (dropRacedDrainDir). On every failure the staged bytes are removed, so
// the bytes returned are the meter's, never a file's.
func (r *Run) writeDrainFile(abs, member string, capBytes, budget int64, mode os.FileMode, src io.Reader) (int64, error) {
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("create %s: %w", parent, err))
	}
	staged, err := os.CreateTemp(parent, "."+filepath.Base(abs)+".drain-*")
	if err != nil {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("stage %s: %w", abs, err))
	}
	drop := func() { _ = os.Remove(staged.Name()) }
	// One byte past the remaining budget, so an archive that carries more
	// than it declared is billed for the evidence of being over instead of
	// stopping short at exactly the cap.
	n, copyErr := io.Copy(staged, io.LimitReader(src, budget+1))
	if copyErr != nil {
		_ = staged.Close()
		drop()
		// The host's own write failing is a refusal; the pod's stream
		// ending short of the member it declared is the archiver's.
		var pathErr *os.PathError
		if errors.As(copyErr, &pathErr) {
			return n, lost(sandbox.ReadBackRefused, fmt.Errorf("write %s: %w", abs, copyErr))
		}
		return n, lost(sandbox.ReadBackArchiver, fmt.Errorf("read %s from the pod's archive: %w", member, copyErr))
	}
	if n > budget {
		_ = staged.Close()
		drop()
		return n, lost(sandbox.ReadBackOverCap, fmt.Errorf("the run's read-back set is over its %s cap (ADR-143; the serving mount's size= or %s raises it) at %s: that member is dropped, what landed before it stays, and the rest stays in the pod", sandbox.ReadBackCapText(capBytes), sandbox.ReadBackCapEnv, member))
	}
	if err := staged.Chmod(exportFileMode(mode)); err != nil {
		_ = staged.Close()
		drop()
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("chmod %s: %w", abs, err))
	}
	if err := staged.Close(); err != nil {
		drop()
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("close %s: %w", abs, err))
	}
	if err := os.Rename(staged.Name(), abs); err != nil {
		drop()
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("land %s: %w", abs, err))
	}
	return n, nil
}

// dropRacedDrainDir removes a directory standing where the settled archive
// carries a regular file or a link — the one occupant neither
// writeDrainFile's landing nor writeExportSymlink's replace takes, and what
// a raced attempt's extract leaves behind on a type flip. The removal is
// proven, never blind: neither half of the before snapshot may hold an
// entry under the tree — no file or link, and no directory either, a dir
// the host already had being as untouchable as a file — or the tree
// predates the drain (the store's, or the pod's own on a bind) and the
// removal is declined with that entry returned as kept, HostDir-relative:
// the caller skips the member, as it skips every member the host declines,
// and the rest of the set still lands — a raced attempt never takes a
// pre-drain tree, and one such collision never costs the members after it.
// The tree's files leave the meter with it (s). hostDir is the drain's root
// (drainRoot), the one directory the before snapshot was taken of. Only the
// host's own failure to read or remove is an error.
func (r *Run) dropRacedDrainDir(hostDir string, before, beforeDirs map[string]bool, abs string, s *drainState) (kept string, err error) {
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("read %s: %w", abs, err))
	}
	if !info.IsDir() {
		return "", nil // the file and link branches replace a non-dir occupant themselves
	}
	// The tree's files, by their path under it: the walk never follows a
	// link, so each stands at <where the tree stands>/<that path>.
	var files []string
	walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		anchored := before
		if d.IsDir() {
			anchored = beforeDirs
		} else if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return nil // not a kind the drain writes or anchors
		}
		rel, relErr := filepath.Rel(hostDir, path)
		if relErr != nil {
			return relErr
		}
		if anchored[filepath.ToSlash(rel)] {
			kept = filepath.ToSlash(rel)
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			under, relErr := filepath.Rel(abs, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, filepath.ToSlash(under))
		}
		return nil
	})
	if walkErr != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("read %s: %w", abs, walkErr))
	}
	if kept != "" {
		return kept, nil
	}
	// Where the tree stands, HostDir-relative: the meter holds each of its
	// files under that place (drainState).
	key, err := safepath.Landing(hostDir, abs)
	if err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("resolve where %s lands: %w", abs, err))
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", abs, err))
	}
	// The tree's files leave the meter with it, each released where the
	// walk met it: the cost is the tree's, never a scan of everything the
	// meter holds for every tree a crafted archive flips.
	for _, f := range files {
		s.forget(key + "/" + f)
	}
	r.warnMember(s, "sandbox: removed %s — a directory a raced drain attempt landed where the settled archive carries a file or a link", abs)
	return "", nil
}

// readBackDirEntries lists the regular files and symlinks under dir,
// relative to it and slash-separated — the two kinds the drain writes —
// and, beside them, the directories under it (never dir itself): a raced
// extract builds directories as readily as it writes files, and the sweep
// judges both kinds against the same pre-drain state. The drain never
// sweeps what it cannot prove it wrote and never takes a tree it cannot,
// so this walk is the host dir's state BEFORE any archive landed; an entry
// it already holds is never a sweep candidate, whoever wrote it. A single
// member the archive carries is another matter: the pod's view of a path
// it names wins over whatever non-dir stood there, pre-drain included, in
// every branch — the file branch's overwrite and the dir branch's replace
// alike.
//
// dir is the drain's root (drainRoot): the walk never follows a link, so
// handed one it would list the link alone and nothing behind it.
func readBackDirEntries(dir string) (files, dirs map[string]bool, err error) {
	files = map[string]bool{}
	dirs = map[string]bool{}
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return filepath.SkipDir
			}
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			dirs[filepath.ToSlash(rel)] = true
			return nil
		}
		if d.Type().IsRegular() || d.Type()&os.ModeSymlink != 0 {
			files[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return files, dirs, walkErr
}

// dropRacedDrainLeftovers removes, after a retried drain, every regular
// file or symlink under hostDir that was not there before the drain and
// that the successful archive did not land — and every directory under
// hostDir that neither the pre-drain state held nor the successful archive
// carries, once it is empty: only a raced attempt can have built it, and
// an empty shell of an attempt that failed is not the settled archive's
// content. tar never deletes, so without this sweep a file or dir the pod
// dropped between two attempts would survive in the run's collected set.
// Candidates come from walking hostDir itself — never from names the pod
// sent — and an entry the host already had is never touched. A candidate
// dir that is not empty keeps what the settled archive (or the pre-drain
// state) put under it and stands.
//
// The same sweep runs early, mid-attempt, when a member of a later attempt
// needs the room an earlier attempt's leftovers hold under the cap
// (makeDrainRoom): landed is then what the running attempt landed so far,
// and why — the clause each removal line ends on — says which of the two
// asked. Each removal is a per-member line, within the dir's budget (s).
// hostDir is the drain's root (drainRoot), the one directory before and
// beforeDirs were read from: a sweep of any other spelling of it would
// judge one directory's entries against another's snapshot.
func (r *Run) dropRacedDrainLeftovers(hostDir string, before, beforeDirs, landed map[string]bool, s *drainState, why string) error {
	nowFiles, nowDirs, err := readBackDirEntries(hostDir)
	if err != nil {
		return fmt.Errorf("list host dir: %w", err)
	}
	for rel := range nowFiles {
		if before[rel] || landed[rel] {
			continue
		}
		path := filepath.Join(hostDir, filepath.FromSlash(rel))
		// Containment by filepath.Rel, matching the walks above: Rel
		// cleans both of its sides, so the judgment reads paths, never
		// how hostDir is spelled, and a candidate climbing out of
		// hostDir reads as outside.
		if back, backErr := filepath.Rel(hostDir, path); backErr != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to remove %q: outside %s", path, hostDir)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove raced leftover %s: %w", path, err)
		}
		r.warnMember(s, "sandbox: removed %s — landed by a raced drain attempt, %s", path, why)
	}
	// A child path sorts after its parent (the parent is a proper prefix),
	// so the descending order removes nested raced dirs before the parents
	// they empty — a whole raced subtree collapses in one pass.
	racedDirs := make([]string, 0, len(nowDirs))
	for rel := range nowDirs {
		if beforeDirs[rel] || landed[rel] {
			continue
		}
		racedDirs = append(racedDirs, rel)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(racedDirs)))
	for _, rel := range racedDirs {
		path := filepath.Join(hostDir, filepath.FromSlash(rel))
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return fmt.Errorf("read raced dir %s: %w", path, readErr)
		}
		if len(entries) > 0 {
			continue // holds landed or pre-drain content — not an empty shell
		}
		if back, backErr := filepath.Rel(hostDir, path); backErr != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to remove %q: outside %s", path, hostDir)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove raced dir %s: %w", path, err)
		}
		r.warnMember(s, "sandbox: removed %s — a directory a raced drain attempt built, %s", path, why)
	}
	return nil
}
