package kubernetes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
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
// streams the tree, the host extracts it beneath dir.HostDir (drainTree).
// The pod must still be up — the engine calls this at teardown, before the
// sandbox is destroyed.
//
// A writer racing the archive makes tar warn and exit 1 exactly as it does
// for the workspace export beside which this drain runs (the same teardown
// window, the same measured race), so the drain carries the export's
// discipline: onlyTarRaceWarnings names the one failure a retry can cure,
// and the retry bounds are the export's (exportRetrySettings). A tree that
// keeps changing keeps its last cause. And because tar never deletes, a
// retry that lands after a raced attempt sweeps what only that attempt
// wrote (sweepDrain) — the landed set is the archive that finally
// succeeded, not the union of every attempt's. The same promise covers a
// type flip between attempts: a dir where the settled archive carries a
// file or a link, or the mirror, is replaced by the extract — only where
// the drain made what stands there (drainState.made); an entry it did not
// make is the host's, and the member is declined like every other member
// the host declines.
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
// Every write, judgment and sweep goes through the one directory drainRoot
// resolves, opened once for the whole drain (drainTree): a host dir reached
// through a link of its own is held where the writes land.
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
	t, err := openDrainTree(root)
	if err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: open host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	defer t.close()
	s := newDrainState(dir.Cap())
	defer r.sayQuietLines(s, dir)
	attempts, pause := exportRetrySettings(r.driver.logger)
	var last error
	var lastDeclined sandbox.ReadBackDeclinedSet
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, x, err := r.drainOnce(ctx, dir, t, s)
		if err == nil {
			if attempt > 1 {
				r.driver.logger.Warn("sandbox: run-files drain of %s succeeded on attempt %d/%d, after tar saw the tree change mid-archive", dir.ContainerPath, attempt, attempts)
				if err := r.sweepDrain(t, s, x.landed, "absent from the one that succeeded"); err != nil {
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

// drainRoot is the directory a declared dir's drain lands in: hostDir made
// absolute, every link on its way — its own leaf included — resolved, the
// operator's configuration and never the archive's. The drain opens it
// once (drainTree), and everything beneath it is reached by descriptor.
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

// drainTree is the host dir one declared dir's drain lands in, held open
// for the whole drain. Every member is written, every entry judged and
// every sweep walks beneath this one directory, one component at a time
// and never through a link (drainDir). So a member lands where its name
// spells or not at all: the member's name IS the place its write reaches —
// the one key the landed record, the meter, the sweeps and the declines
// share, and the key a walk of the tree meets the entry under. An entry
// standing on a member's path as anything but a directory — a link above
// all — can decline the member, never redirect it.
type drainTree struct {
	root *drainDir
}

// openDrainTree opens the drain's root, rootPath as drainRoot resolved it.
func openDrainTree(rootPath string) (*drainTree, error) {
	root, err := openDrainRoot(rootPath)
	if err != nil {
		return nil, err
	}
	return &drainTree{root: root}, nil
}

func (t *drainTree) close() { t.root.close() }

// release closes a directory parent or spot handed out — never the root,
// which the drain holds to its end.
func (t *drainTree) release(d *drainDir) {
	if d != nil && d != t.root {
		d.close()
	}
}

// at is where the entry at key stands on the host, for messages.
func (t *drainTree) at(key string) string {
	return filepath.Join(t.root.path, filepath.FromSlash(key))
}

// fits reports whether the entry at key has a path the host's own readers
// can name (drainPathMax, its terminating NUL counted).
func (t *drainTree) fits(key string) bool {
	return len(t.root.path)+1+len(key) < drainPathMax
}

// entryKind is what stands at a name in the drain's tree, read off the
// entry itself, never what a link at it points to.
type entryKind int

const (
	entryNone entryKind = iota
	entryDir
	entryFile
	entryLink
	entryOther
)

var (
	// errNotDrainDir is a name the drain would enter that stands as no
	// directory: a link, whatever it points at, a file, another kind.
	errNotDrainDir = errors.New("not a directory the drain enters")
	// errNotDrainFile is a name the drain would read a file at that
	// stands as no regular file: a link, a directory, another kind.
	errNotDrainFile = errors.New("not a regular file")
)

// join is where name under d stands on the host, for messages.
func (d *drainDir) join(name string) string {
	return filepath.Join(d.path, name)
}

// joinKey is the key of name in the directory at key ("" for the root).
func joinKey(key, name string) string {
	if key == "" {
		return name
	}
	return key + "/" + name
}

// parent opens the directory the entry at key stands in: each component of
// key's parent chain entered from the root in turn (drainDir.child), one
// missing made when create is set — and recorded as the drain's own
// (drainState.made). blocked is the key of the first component standing as
// no directory — a link, a file, another kind — or, without create, not
// standing at all: nothing under it is reachable where its name spells, and
// the drain reaches nothing any other way. The caller releases dir.
func (t *drainTree) parent(key string, create bool, s *drainState) (dir *drainDir, base, blocked string, err error) {
	parts := strings.Split(key, "/")
	dir = t.root
	for i, part := range parts[:len(parts)-1] {
		next, err := dir.child(part)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err = dir.mkdir(part); err == nil {
				s.made[strings.Join(parts[:i+1], "/")] = true
				next, err = dir.child(part)
			}
		}
		t.release(dir)
		if errors.Is(err, errNotDrainDir) || errors.Is(err, fs.ErrNotExist) {
			return nil, "", strings.Join(parts[:i+1], "/"), nil
		}
		if err != nil {
			return nil, "", "", err
		}
		dir = next
	}
	return dir, parts[len(parts)-1], "", nil
}

// drainSpot is where one member lands: the directory it is written in,
// held open, its name there, and what stands at that name now.
type drainSpot struct {
	dir  *drainDir
	base string
	kind entryKind
}

// spot readies the place the member at key lands, its parent chain made as
// it needs (parent), or the decline that place earns: a component standing
// as no directory (blockedDecline), or a name longer than the host's
// filesystem holds. Only the host's own failure to read or create is an
// error. The caller releases the spot's dir.
func (t *drainTree) spot(key string, s *drainState) (drainSpot, *drainDecline, error) {
	dir, base, blocked, err := t.parent(key, true, s)
	if err == nil && blocked != "" {
		return drainSpot{}, blockedDecline(key, blocked, s), nil
	}
	kind := entryNone
	if err == nil {
		if kind, err = dir.kind(base); err != nil {
			t.release(dir)
		}
	}
	if errors.Is(err, syscall.ENAMETOOLONG) {
		return drainSpot{}, declined(sandbox.DeclinedUnsafeName, "sandbox: drain refused %s: a name on its path is longer than the host's filesystem holds: %v", key, err), nil
	}
	if err != nil {
		return drainSpot{}, nil, lost(sandbox.ReadBackRefused, fmt.Errorf("reach %s: %w", t.at(key), err))
	}
	return drainSpot{dir: dir, base: base, kind: kind}, nil, nil
}

// blockedDecline is the decline of the member at key whose path meets, at
// blocked, an entry standing as no directory. The drain writes through no
// such entry. One the drain did not make is the host's, and the reason the
// member is declined is the host's entry (pre_drain_file). One the drain
// made — a link or a file an archive laid there, this one or a raced
// attempt's, where no dir member came first to replace it — leaves the
// member's name nowhere it can be written as it spells (unsafe_name).
func blockedDecline(key, blocked string, s *drainState) *drainDecline {
	if s.made[blocked] {
		return declined(sandbox.DeclinedUnsafeName, "sandbox: drain refused %s: its path crosses %s, which the pod's archive laid as no directory — the drain writes through no link", key, blocked)
	}
	return declined(sandbox.DeclinedPreDrainFile, "sandbox: drain skipped %s: %s predates the drain and stands where its path needs a directory — not the drain's to remove or write through", key, blocked)
}

// drainDecline is one member the host declines: why, the token the run's
// event carries, and line, what the run's log says of it.
type drainDecline struct {
	why  sandbox.ReadBackDeclineWhy
	line string
}

func declined(why sandbox.ReadBackDeclineWhy, format string, args ...any) *drainDecline {
	return &drainDecline{why: why, line: fmt.Sprintf(format, args...)}
}

// drainState is what one declared dir's drain carries across its attempts:
// whose each entry is, the meter of what the host holds against the dir's
// cap, and the run log's budget for the lines the drain writes per member.
//
// Whose: made is every key the drain created an entry at or replaced one
// at — a member it landed, a directory a member's path needed. An entry
// standing at a key made does not hold is not the drain's: the host held
// it before the drain (the store's, or the pod's own on a bind), and the
// drain never removes, replaces or writes through it — only a member that
// carries bytes overwrites a file or a link there, the overlay a resumed
// drain lands. An entry the drain overwrote is the drain's from then on,
// whoever held the name before: made records what the drain did, never
// what stood before it.
//
// The meter is the host's, never one archive's: tar never deletes, and a
// drain no attempt settles keeps every attempt's files (the union the
// stale mark reports), so the figure is asked of what stands in the host
// dir — each file the drain landed counted once, at the size its latest
// landing left, under its key. Every site that changes what the drain's
// files hold says so here — a landing (land), a file replaced by a link or
// a dir or removed with a raced tree or by a sweep (forget) — so the meter
// never counts less than the host holds.
type drainState struct {
	// capBytes is the dir's figure (sandbox.ReadBackDir.Cap).
	capBytes int64
	// made is every key the drain created or replaced an entry at.
	made map[string]bool
	// held is the bytes each file the drain landed holds, by key; total
	// their sum.
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
	return &drainState{capBytes: capBytes, made: map[string]bool{}, held: map[string]int64{}}
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
// directory, or removed with a raced tree or by a sweep — and holds
// nothing any more.
func (s *drainState) forget(key string) {
	s.total -= s.held[key]
	delete(s.held, key)
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
// it — the sweep a settled attempt runs (sweepDrain), run now, against
// what this attempt landed so far, because the cap needs the room before
// the attempt can settle. The newer archive is the better view of the
// pod's tree, and every file the meter holds is one the drain made, so
// those this attempt has not landed are exactly what a settled attempt
// would sweep. Nothing is swept when the member fits as it is, when this
// attempt's own stream has no room for it (attemptRoom), or when the
// leftovers' bytes would not make it fit: the meter refuses it then, and
// they stand — the union the stale mark reports. Reports whether it swept:
// a directory on the member's path may have gone with the leftovers, and
// the caller readies its place again.
func (r *Run) makeDrainRoom(t *drainTree, s *drainState, landed map[string]bool, key string, need, attemptRoom int64) (bool, error) {
	room := s.room(key)
	if need <= room || need > attemptRoom {
		return false, nil
	}
	var leftovers int64
	for k, n := range s.held {
		if k != key && !landed[k] {
			leftovers += n
		}
	}
	if leftovers == 0 || need > room+leftovers {
		return false, nil
	}
	why := fmt.Sprintf("swept before any attempt settled because %s, a later attempt's member, needed the room under the dir's %s cap", key, sandbox.ReadBackCapText(s.capBytes))
	if err := r.sweepDrain(t, s, landed, why); err != nil {
		return true, lost(sandbox.ReadBackRefused, fmt.Errorf("make room for %s: %w", key, err))
	}
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

// drainExtract is what one attempt's extract did: the bytes it metered,
// the key of each member it landed — the record a retried drain's sweep
// keeps against everything else a raced attempt may have left — and the
// members it declined, by name, counted in full and kept up to the bound
// the loss names them to.
type drainExtract struct {
	drained  int64
	landed   map[string]bool
	declined sandbox.ReadBackDeclinedSet
}

// drainOnce streams one archive of dir.ContainerPath and lands it in t, the
// host dir as drainRoot resolved it, held open. retryable reports a failure
// onlyTarRaceWarnings accepts as transient — the workspace export's own
// predicate; x is what the extract did, as far as it got. s is what the
// dir's attempts share (drainState).
func (r *Run) drainOnce(ctx context.Context, dir sandbox.ReadBackDir, t *drainTree, s *drainState) (retryable bool, x drainExtract, err error) {
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
	x, xerr := r.extractDrain(pipe, t, s)
	if xerr != nil {
		// The extract is done reading — the cap, the stream, the host
		// ended it, typed where it failed — so the archiver is stopped,
		// never read to its end for nothing; then the failure is reported.
		stopPodArchiver(stopArchiver, podTar)
		return false, x, fmt.Errorf("kubernetes: drain %s into %s: %w", dir.ContainerPath, dir.HostDir, xerr)
	}
	// The set landed: what the archiver still sends is tar's padding, read
	// so that its exit — the race warning it carries — can be waited for.
	if err := readArchiveTail(pipe); err != nil {
		stopPodArchiver(stopArchiver, podTar)
		return false, x, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: read the pod's archive past its end: %w", dir.ContainerPath, err))
	}
	if err := podTar.Wait(); err != nil {
		return onlyTarRaceWarnings(err, podErr.String()), x, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: in-pod tar: %w\n%s", dir.ContainerPath, err, strings.TrimSpace(podErr.String())))
	}
	r.driver.logger.Info("sandbox: drained %d bytes of run files from pod %s:%s into %s", x.drained, r.podName, dir.ContainerPath, dir.HostDir)
	return false, x, nil
}

// extractDrain writes the pod's archive beneath t and returns what it did
// (drainExtract) — the landed record is the successful attempt's own
// account of what it put there, which is what a retried drain's sweep
// keeps against everything else a raced attempt may have left. s is what
// the dir's attempts share: whose each entry is, the meter of what the
// host holds against the dir's cap (drainState), and the run log's budget
// for per-member lines.
//
// The host reads the archive itself, like the workspace export does: the
// member names are chosen in the sandbox, by an archiver the workflow's
// own image provides. Every member lands where its name spells beneath the
// host dir, written through no link (drainTree), so what each judgment
// reads is what the write reaches. Unlike the workspace export there are
// no name exclusions — a run file is the run's product whatever it is
// named — and every member the host declines (sandbox.ReadBackDeclineWhy:
// a name that cannot be written where it spells, a link that leaves the
// tree, a link the kernel refuses to create, a hardlink to no file this
// archive landed, a kind it does not write back, a pre-drain tree where
// the archive carries a file or a link, a pre-drain file or link where it
// carries a dir or another link, or on its path) is skipped, warned, and
// recorded for the run's event, and the members after it still land. Only
// the host's own failure to write — and the cap — end the extract.
func (r *Run) extractDrain(stream io.Reader, t *drainTree, s *drainState) (drainExtract, error) {
	x := drainExtract{landed: map[string]bool{}}
	// The regular files this attempt landed, by key: the only targets a
	// hardlink member may be landed from. The registry only grows; whether
	// the file still stands there is openLanded's to prove — a later
	// member may have replaced it, a raced tree's removal taken it.
	files := map[string]bool{}
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
		key, d, err := r.landDrainMember(t, s, &x, files, hdr, tr)
		if err != nil {
			return x, err
		}
		switch {
		case key == "":
		case d != nil:
			x.declined.Add(sandbox.ReadBackDeclinedMember{Name: key, Why: d.why})
			r.warnMember(s, "%s", d.line)
		default:
			// The one place the record the sweep keeps is written: a dir
			// is in it too, since an empty dir the settled archive carries
			// is content, not a raced leftover, and the sweep cannot tell
			// the two apart except through this record.
			x.landed[key] = true
		}
	}
	return x, nil
}

// landDrainMember lands one member of the archive and returns its key —
// its name, clean and slash-separated, which is also where it lands — or
// the decline it earned; an empty key is the archive's root, nothing to
// land. tr is the archive, positioned at the member's bytes.
func (r *Run) landDrainMember(t *drainTree, s *drainState, x *drainExtract, files map[string]bool, hdr *tar.Header, tr io.Reader) (string, *drainDecline, error) {
	if err := safepath.GuardName(hdr.Name); err != nil {
		return strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./"), declined(sandbox.DeclinedUnsafeName, "sandbox: drain refused a member of the pod's archive: %v", err), nil
	}
	key := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(hdr.Name)), "./")
	if key == "" || key == "." {
		return "", nil, nil
	}
	if !t.fits(key) {
		return key, declined(sandbox.DeclinedUnsafeName, "sandbox: drain refused %s: at %d bytes its path on the host is longer than the host's own readers can name", key, len(t.at(key))), nil
	}
	var d *drainDecline
	var err error
	switch hdr.Typeflag {
	case tar.TypeDir:
		d, err = r.landDrainDir(t, s, key)
	case tar.TypeReg:
		d, err = r.landDrainFile(t, s, x, files, key, hdr.Size, hdr.FileInfo().Mode(), tr)
	case tar.TypeLink:
		d, err = r.landDrainHardlink(t, s, x, files, key, hdr.Linkname)
	case tar.TypeSymlink:
		d, err = r.landDrainSymlink(t, s, key, hdr.Linkname)
	default:
		// A device, a fifo: each is a way to point a later write
		// somewhere it was never meant to reach, and none is a file a
		// tool produces as its product.
		d = declined(sandbox.DeclinedKind, "sandbox: drain skipped %s: the pod's archive carries it as type %q, which the host does not write back", key, string(hdr.Typeflag))
	}
	return key, d, err
}

// landDrainDir lands a dir member at key. A file or a link standing there
// is replaced only when the drain made it — a raced attempt's, the mirror
// of the file branch's own replace. One it did not make is the host's, and
// a dir member carries none of the set's bytes to trade for it: the member
// is declined, and every member under it with it, since none is written
// through that entry (blockedDecline).
func (r *Run) landDrainDir(t *drainTree, s *drainState, key string) (*drainDecline, error) {
	sp, d, err := t.spot(key, s)
	if d != nil || err != nil {
		return d, err
	}
	defer t.release(sp.dir)
	switch sp.kind {
	case entryDir:
		return nil, nil
	case entryNone:
	default:
		if !s.made[key] {
			return declined(sandbox.DeclinedPreDrainFile, "sandbox: drain skipped the dir %s: what stands there predates the drain — not the drain's to remove", key), nil
		}
		if err := sp.dir.remove(sp.base, false); err != nil {
			return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", t.at(key), err))
		}
		s.forget(key)
	}
	if err := sp.dir.mkdir(sp.base); err != nil {
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("create %s: %w", t.at(key), err))
	}
	s.made[key] = true
	return nil, nil
}

// landDrainFile lands a regular member at key — the archive's bytes, or a
// hardlink's copy — of need bytes and the given mode, read from src. A
// directory where the member carries a file is what a raced attempt's
// extract leaves behind; the rename cannot write over it, so it is
// removed when the drain made all of it (dropRacedDrainDir) and the member
// is declined when not — tr.Next skips its bytes. The member is metered
// against what the host holds (drainState): room made from what only
// earlier attempts left when that is what it needs (makeDrainRoom), the
// budget the smaller of what this attempt's own stream has left and what
// the set has room for at key.
func (r *Run) landDrainFile(t *drainTree, s *drainState, x *drainExtract, files map[string]bool, key string, need int64, mode os.FileMode, src io.Reader) (*drainDecline, error) {
	sp, d, err := t.spot(key, s)
	if d != nil || err != nil {
		return d, err
	}
	defer func() { t.release(sp.dir) }()
	kept, err := r.dropRacedDrainDir(t, sp, key, s)
	if err != nil {
		return nil, err
	}
	if kept != "" {
		return declined(sandbox.DeclinedPreDrainTree, "sandbox: drain skipped %s: the pod's archive carries it as a file, but %s predates the drain — the tree there is not the drain's to replace", key, kept), nil
	}
	attemptRoom := s.capBytes - x.drained
	swept, err := r.makeDrainRoom(t, s, x.landed, key, need, attemptRoom)
	if err != nil {
		return nil, err
	}
	if swept {
		// The sweep takes every empty directory only earlier attempts
		// made — one on this member's path among them: its place is
		// readied again.
		t.release(sp.dir)
		if sp, d, err = t.spot(key, s); d != nil || err != nil {
			return d, err
		}
	}
	budget := max(0, min(attemptRoom, s.room(key)))
	n, err := r.writeDrainFile(sp.dir, sp.base, key, s.capBytes, budget, mode, src)
	x.drained += n
	if err != nil {
		return nil, err
	}
	s.land(key, n)
	s.made[key] = true
	files[key] = true
	return nil, nil
}

// landDrainHardlink lands a hardlink member at key. A file with several
// names is archived once: tar carries every name but the first as a
// hardlink to that first member, which the same archive landed before it.
// The host lands the link as a copy of what that member landed — a tool
// that names one report twice gets both names in its set — metered like
// any file, since the upload carries both. A link to anything else (a name
// where this archive landed no file, one it declined, one outside the
// tree, one no longer standing as that file) is declined.
func (r *Run) landDrainHardlink(t *drainTree, s *drainState, x *drainExtract, files map[string]bool, key, linkname string) (*drainDecline, error) {
	target := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(linkname)), "./")
	if safepath.GuardName(linkname) != nil || target == "" || target == "." || !files[target] {
		return declined(sandbox.DeclinedHardlinkTarget, "sandbox: drain skipped the hardlink %s -> %s: its target is no file this drain landed", key, linkname), nil
	}
	src, err := t.openLanded(target, s)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return declined(sandbox.DeclinedHardlinkTarget, "sandbox: drain skipped the hardlink %s -> %s: its target no longer stands as a file this drain landed", key, linkname), nil
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("stat %s for the hardlink %s: %w", t.at(target), key, err))
	}
	return r.landDrainFile(t, s, x, files, key, info.Size(), info.Mode(), src)
}

// openLanded opens the regular file standing at key for reading — reached
// as every member is, never through a link, and opened without following
// one at its own name — or nil when no regular file stands there: the
// file opened is the file judged.
func (t *drainTree) openLanded(key string, s *drainState) (*os.File, error) {
	dir, base, blocked, err := t.parent(key, false, s)
	if err != nil {
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("reach %s: %w", t.at(key), err))
	}
	if blocked != "" {
		return nil, nil
	}
	defer t.release(dir)
	f, err := dir.openFile(base)
	if errors.Is(err, errNotDrainFile) || errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("open %s: %w", t.at(key), err))
	}
	return f, nil
}

// landDrainSymlink lands a link member at key pointing at target. A link
// carries no bytes of the set — only the files it points into do, when
// their own member lands — so neither a landed link nor a refused one
// moves the meter, and a link member never takes what the host holds: a
// file or another link the drain did not make stands, and the member is
// declined. The link already standing there, target for target, IS the
// member landed — the host already holds the pod's view of that name.
// Every refusal is judged before anything is removed: a member that lands
// nothing removes nothing, and the link is created beside its path and
// renamed over it only once the place is judged.
func (r *Run) landDrainSymlink(t *drainTree, s *drainState, key, target string) (*drainDecline, error) {
	if target == "" {
		return declined(sandbox.DeclinedUnwritableLink, "sandbox: drain skipped the link %s -> %q: its target is empty", key, target), nil
	}
	if _, inside := linkResolution(t.root.path, t.at(path.Dir(key)), target); !inside {
		return declined(sandbox.DeclinedLinkLeavesTree, "sandbox: drain skipped the link %s -> %s: its target leaves the run-files tree or can come to", key, target), nil
	}
	sp, d, err := t.spot(key, s)
	if d != nil || err != nil {
		return d, err
	}
	defer t.release(sp.dir)
	if sp.kind == entryLink {
		standing, err := sp.dir.readlink(sp.base)
		if err != nil {
			return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("read the link %s: %w", t.at(key), err))
		}
		if standing == target {
			return nil, nil
		}
	}
	if sp.kind != entryNone && sp.kind != entryDir && !s.made[key] {
		return declined(sandbox.DeclinedPreDrainFile, "sandbox: drain skipped the link %s -> %s: what stands there predates the drain — not the drain's to remove", key, target), nil
	}
	staged, err := sp.dir.symlinkStaged(target)
	if errors.Is(err, syscall.ENAMETOOLONG) {
		// The staged name is short whatever the member's: only the
		// target's own text is longer than the host holds.
		return declined(sandbox.DeclinedUnwritableLink, "sandbox: drain skipped the link %s: the kernel refuses its target: %v", key, err), nil
	}
	if err != nil {
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("stage the link %s -> %s: %w", t.at(key), target, err))
	}
	// A directory where the archive carries a link is the same raced
	// occupant the file branch cures — the link cannot be renamed over it —
	// judged on the same record, and skipped on the same witness.
	kept, err := r.dropRacedDrainDir(t, sp, key, s)
	if err != nil {
		_ = sp.dir.remove(staged, false)
		return nil, err
	}
	if kept != "" {
		if err := sp.dir.remove(staged, false); err != nil {
			return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("drop the staged link for %s: %w", t.at(key), err))
		}
		return declined(sandbox.DeclinedPreDrainTree, "sandbox: drain skipped the link %s -> %s: %s predates the drain — the tree there is not the drain's to replace", key, target, kept), nil
	}
	// The link replaces the file or link the drain landed at its path: a
	// file's bytes leave the set with it.
	if err := sp.dir.rename(staged, sp.base); err != nil {
		_ = sp.dir.remove(staged, false)
		return nil, lost(sandbox.ReadBackRefused, fmt.Errorf("land the link %s -> %s: %w", t.at(key), target, err))
	}
	s.forget(key)
	s.made[key] = true
	return nil, nil
}

// writeDrainFile writes one regular member as base under dir — member is
// its key, mode the mode it carries — and returns the bytes it copied,
// metered as it goes against budget, the room its caller's meter leaves it
// (drainState): an archive that declares one size and carries another is
// billed for what actually arrived, and a member that takes the set past
// the cap (capBytes, the dir's own figure, named in the refusal) is the
// failure, named.
//
// The member is staged beside its path and renamed over it only once it is
// whole and within the budget: a write the cap or the pod's stream cuts
// short never stands at the member's path, where the upload would carry it
// as a file that looks entire. The rename replaces a file or a link the
// path holds — the host writes the file the member names, never through a
// link standing there — and a dir there was judged before
// (dropRacedDrainDir). The staged name is short and named from dir, never
// built on the member's: a member the host can hold is a member the drain
// can stage. On every failure the staged bytes are removed, so the bytes
// returned are the meter's, never a file's.
func (r *Run) writeDrainFile(dir *drainDir, base, member string, capBytes, budget int64, mode os.FileMode, src io.Reader) (int64, error) {
	at := dir.join(base)
	staged, name, err := dir.createStaged()
	if err != nil {
		return 0, lost(sandbox.ReadBackRefused, fmt.Errorf("stage %s: %w", at, err))
	}
	drop := func() { _ = dir.remove(name, false) }
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
			return n, lost(sandbox.ReadBackRefused, fmt.Errorf("write %s: %w", at, copyErr))
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
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("chmod %s: %w", at, err))
	}
	if err := staged.Close(); err != nil {
		drop()
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("close %s: %w", at, err))
	}
	if err := dir.rename(name, base); err != nil {
		drop()
		return n, lost(sandbox.ReadBackRefused, fmt.Errorf("land %s: %w", at, err))
	}
	return n, nil
}

// dropRacedDrainDir removes a directory standing at the member's spot
// where the archive carries a regular file or a link — the one occupant
// neither writeDrainFile's rename nor the link branch's takes, and what a
// raced attempt's extract leaves behind on a type flip. The removal is
// proven, never blind: the drain must have made every entry of the tree,
// the directory itself included (drainState.made), or the tree holds
// something the host had before the drain — the store's, or the pod's own
// on a bind — and the removal is declined with that entry's key returned
// as kept: the caller skips the member, as it skips every member the host
// declines, and the rest of the set still lands. The tree's files leave the
// meter with it. Only the host's own failure to read or remove is an error.
func (r *Run) dropRacedDrainDir(t *drainTree, sp drainSpot, key string, s *drainState) (kept string, err error) {
	if sp.kind != entryDir {
		return "", nil
	}
	if !s.made[key] {
		return key, nil
	}
	tree, err := sp.dir.child(sp.base)
	if err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("read %s: %w", t.at(key), err))
	}
	defer tree.close()
	if kept, err = firstNotMade(tree, key, s); err != nil || kept != "" {
		if err != nil {
			return "", lost(sandbox.ReadBackRefused, fmt.Errorf("read %s: %w", t.at(key), err))
		}
		return kept, nil
	}
	if err := removeDrainTree(tree, key, s); err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", t.at(key), err))
	}
	if err := sp.dir.remove(sp.base, true); err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", t.at(key), err))
	}
	r.warnMember(s, "sandbox: removed %s — a directory a raced drain attempt landed where the settled archive carries a file or a link", t.at(key))
	return "", nil
}

// firstNotMade is the key of the first entry under d (at key), in name
// order and depth first, the drain did not make — "" when it made them
// all.
func firstNotMade(d *drainDir, key string, s *drainState) (string, error) {
	names, err := d.names()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		k := joinKey(key, name)
		if !s.made[k] {
			return k, nil
		}
		kind, err := d.kind(name)
		if err != nil {
			return "", err
		}
		if kind != entryDir {
			continue
		}
		sub, err := d.child(name)
		if err != nil {
			return "", err
		}
		kept, err := firstNotMade(sub, k, s)
		sub.close()
		if err != nil || kept != "" {
			return kept, err
		}
	}
	return "", nil
}

// removeDrainTree removes every entry under d (at key), deepest first, each
// file leaving the meter (s) where the walk met it: the cost is the tree's,
// never a scan of everything the meter holds.
func removeDrainTree(d *drainDir, key string, s *drainState) error {
	names, err := d.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		kind, err := d.kind(name)
		if err != nil {
			return err
		}
		if kind == entryDir {
			sub, err := d.child(name)
			if err != nil {
				return err
			}
			err = removeDrainTree(sub, joinKey(key, name), s)
			sub.close()
			if err != nil {
				return err
			}
		}
		if err := d.remove(name, kind == entryDir); err != nil {
			return err
		}
		if kind == entryFile {
			s.forget(joinKey(key, name))
		}
	}
	return nil
}

// sweepDrain removes, after a retried drain, every entry the drain made
// that the successful archive did not land: a file or a link outright, a
// directory once it is empty — only a raced attempt can have made it, and
// an empty shell of an attempt that failed is not the settled archive's
// content. tar never deletes, so without this sweep a file or dir the pod
// dropped between two attempts would survive in the run's collected set.
// Candidates come from walking the host dir itself, beneath its root and
// through no link (drainTree) — never from names the pod sent — and an
// entry the drain did not make is never touched. A candidate dir that is
// not empty keeps what the settled archive or the host put under it, and
// stands. Each removed file leaves the meter (s).
//
// The same sweep runs early, mid-attempt, when a member of a later attempt
// needs the room an earlier attempt's leftovers hold under the cap
// (makeDrainRoom): landed is then what the running attempt landed so far,
// and why — the clause each removal line ends on — says which of the two
// asked. Each removal is a per-member line, within the dir's budget (s).
func (r *Run) sweepDrain(t *drainTree, s *drainState, landed map[string]bool, why string) error {
	_, err := r.sweepDrainDir(t, t.root, "", s, landed, why)
	return err
}

// sweepDrainDir sweeps the directory d at key (sweepDrain) and reports
// whether it holds nothing once swept.
func (r *Run) sweepDrainDir(t *drainTree, d *drainDir, key string, s *drainState, landed map[string]bool, why string) (empty bool, err error) {
	names, err := d.names()
	if err != nil {
		return false, fmt.Errorf("list %s: %w", t.at(key), err)
	}
	left := 0
	for _, name := range names {
		k := joinKey(key, name)
		kind, err := d.kind(name)
		if err != nil {
			return false, err
		}
		raced := s.made[k] && !landed[k]
		switch kind {
		case entryNone:
			continue
		case entryDir:
			sub, err := d.child(name)
			if err != nil {
				return false, fmt.Errorf("read %s: %w", t.at(k), err)
			}
			subEmpty, err := r.sweepDrainDir(t, sub, k, s, landed, why)
			sub.close()
			if err != nil {
				return false, err
			}
			if !raced || !subEmpty {
				left++
				continue
			}
			if err := d.remove(name, true); err != nil {
				return false, fmt.Errorf("remove raced dir %s: %w", t.at(k), err)
			}
			r.warnMember(s, "sandbox: removed %s — a directory a raced drain attempt built, %s", t.at(k), why)
		default:
			if !raced {
				left++
				continue
			}
			if err := d.remove(name, false); err != nil {
				return false, fmt.Errorf("remove raced leftover %s: %w", t.at(k), err)
			}
			if kind == entryFile {
				s.forget(k)
			}
			r.warnMember(s, "sandbox: removed %s — landed by a raced drain attempt, %s", t.at(k), why)
		}
	}
	return left == 0, nil
}
