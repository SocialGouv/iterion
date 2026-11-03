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
// A tree that never settles is the one failure that leaves a STALE set:
// no attempt succeeded, so no sweep runs, and the host dir holds the union
// of every attempt's members — a superset of the pod's final tree. The
// unsettled error says so, because the panel will serve it.
//
// Every failure travels typed (sandbox.ReadBackLoss): the reason the
// runtime's event carries is read off the error, at the site that knows
// it, never off its text.
func (r *Run) drainRunFilesDir(ctx context.Context, dir sandbox.ReadBackDir) error {
	if err := os.MkdirAll(dir.HostDir, 0o775); err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: create host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	// The sweep's anchor: the host dir's state BEFORE any archive landed.
	// An entry it holds is the store's (or, on a bind, the pod's own) — a
	// raced attempt is never allowed to take it. The directories are
	// anchored beside the files: a raced extract builds dirs as readily as
	// it writes files, and a dir the host already had is as untouchable as
	// a file it already had.
	before, beforeDirs, err := readBackDirEntries(dir.HostDir)
	if err != nil {
		return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: list host dir %s: %w", dir.ContainerPath, dir.HostDir, err))
	}
	attempts, pause := exportRetrySettings(r.driver.logger)
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, landed, err := r.drainOnce(ctx, dir, before, beforeDirs)
		if err == nil {
			if attempt > 1 {
				r.driver.logger.Warn("sandbox: run-files drain of %s succeeded on attempt %d/%d, after tar saw the tree change mid-archive", dir.ContainerPath, attempt, attempts)
				if err := r.dropRacedDrainLeftovers(dir.HostDir, before, beforeDirs, landed); err != nil {
					return lost(sandbox.ReadBackRefused, fmt.Errorf("kubernetes: drain %s: %w", dir.ContainerPath, err))
				}
			}
			return nil
		}
		if !retryable {
			return err
		}
		last = err
		r.driver.logger.Warn("sandbox: run-files drain of %s attempt %d/%d: tar saw the tree change mid-archive, retrying", dir.ContainerPath, attempt, attempts)
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return lost(sandbox.ReadBackUnsettled, fmt.Errorf("kubernetes: drain %s: retry abandoned (%v): %w", dir.ContainerPath, ctx.Err(), last))
			case <-time.After(pause):
			}
			pause *= 2
		}
	}
	return lost(sandbox.ReadBackUnsettled, fmt.Errorf("kubernetes: drain %s: the pod's run files kept changing under the drain after %d attempts — what the attempts landed stays and uploads as the union of every attempt's members, a stale superset of the pod's final tree (a file the run removed between attempts may be among them): %w", dir.ContainerPath, attempts, last))
}

// lost types one drain failure with the reason the runtime's event carries;
// sandbox.ReadBackLossReasonOf reads it back through every wrap above.
func lost(reason sandbox.ReadBackLossReason, err error) error {
	return &sandbox.ReadBackLoss{Reason: reason, Err: err}
}

// drainOnce streams one archive of dir.ContainerPath and lands it under
// dir.HostDir. retryable reports a failure onlyTarRaceWarnings accepts as
// transient — the workspace export's own predicate; landed names the members
// the extract wrote, HostDir-relative and slash-separated. before and
// beforeDirs are the two halves of the host dir's pre-drain state — files
// and links, and the directories beside them — the proof a wrong-kind
// occupant is judged on.
func (r *Run) drainOnce(ctx context.Context, dir sandbox.ReadBackDir, before, beforeDirs map[string]bool) (retryable bool, landed map[string]bool, err error) {
	kubectlArgs := []string{"--namespace", r.namespace,
		"exec", r.podName, "--container", "workload", "--",
		"env", "LC_ALL=" + sandbox.TarLocale, "tar", "-C", dir.ContainerPath, "-cf", "-", "."}
	podTar := r.driver.kubectl.cmdContext(ctx, kubectlArgs...)

	pipe, err := podTar.StdoutPipe()
	if err != nil {
		return false, nil, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: pod tar stdout pipe: %w", dir.ContainerPath, err))
	}
	var podErr bytes.Buffer
	podTar.Stderr = &podErr

	if err := podTar.Start(); err != nil {
		return false, nil, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: start the pod's archiver: %w", dir.ContainerPath, err))
	}
	drained, landed, xerr := r.extractDrain(pipe, dir.HostDir, before, beforeDirs, dir.Cap())
	if xerr != nil {
		// Drain the pipe so the pod's archiver sees its reader go away
		// rather than block on a full one, then report the extraction —
		// typed where it failed (the cap, the stream, the host).
		_, _ = io.Copy(io.Discard, pipe)
		_ = podTar.Wait()
		return false, nil, fmt.Errorf("kubernetes: drain %s into %s: %w", dir.ContainerPath, dir.HostDir, xerr)
	}
	if err := podTar.Wait(); err != nil {
		return onlyTarRaceWarnings(err, podErr.String()), nil, lost(sandbox.ReadBackArchiver, fmt.Errorf("kubernetes: drain %s: in-pod tar: %w\n%s", dir.ContainerPath, err, strings.TrimSpace(podErr.String())))
	}
	r.driver.logger.Info("sandbox: drained %d bytes of run files from pod %s:%s into %s", drained, r.podName, dir.ContainerPath, dir.HostDir)
	return false, landed, nil
}

// extractDrain writes the pod's archive under hostDst and returns the
// bytes it landed and the members it wrote, HostDir-relative and
// slash-separated — the successful attempt's own record of what it put
// there, which is what a retried drain's sweep keeps against everything
// else a raced attempt may have left. before and beforeDirs are the two
// halves of the host dir's pre-drain state — files and links, and the
// directories beside them: the proof a wrong-kind occupant at a member's
// path is judged on (the drain's to replace when it wrote it, the store's
// or the pod's when it did not). capBytes is the figure this dir's set is
// metered against (sandbox.ReadBackDir.Cap).
//
// The host reads the archive itself, like the workspace export does: the
// member names are chosen in the sandbox, by an archiver the workflow's
// own image provides, so the decision is made on the path a write lands
// on after every existing symlink in its parent chain is resolved. Unlike
// the workspace export there are no name exclusions — a run file is the
// run's product whatever it is named — and every refusal (a member that
// cannot be made safe, a type the host does not write back, a pre-drain
// tree where the archive carries a file or a link) is skipped and
// reported rather than written, and the members after it still land. Only
// the host's own failure to write — and the cap — end the extract.
func (r *Run) extractDrain(stream io.Reader, hostDst string, before, beforeDirs map[string]bool, capBytes int64) (int64, map[string]bool, error) {
	root, err := filepath.Abs(hostDst)
	if err != nil {
		return 0, nil, fmt.Errorf("resolve drain destination %s: %w", hostDst, err)
	}
	root = filepath.Clean(root)
	var drained int64
	landed := map[string]bool{}
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return drained, nil, lost(sandbox.ReadBackArchiver, fmt.Errorf("read the pod's archive: %w", err))
		}
		if err := safepath.GuardName(hdr.Name); err != nil {
			r.driver.logger.Warn("sandbox: drain refused a member of the pod's archive: %v", err)
			continue
		}
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(hdr.Name)), "./")
		if clean == "" || clean == "." {
			continue
		}
		abs, err := safepath.Join(root, clean)
		if err != nil {
			r.driver.logger.Warn("sandbox: drain refused %q from the pod's archive: %v", hdr.Name, err)
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
				if err := os.Remove(abs); err != nil {
					return drained, landed, lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", abs, err))
				}
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return drained, landed, lost(sandbox.ReadBackRefused, fmt.Errorf("create %s: %w", abs, err))
			}
			// A dir member is a member the extract wrote: the sweep keeps
			// it — an empty dir the settled archive carries is content,
			// not a raced leftover, and the sweep cannot tell the two
			// apart except through this record.
			landed[clean] = true
		case tar.TypeReg:
			// A directory where the archive carries a file is what a raced
			// attempt's extract leaves behind; the file branch cannot write
			// over it. Judged on the before snapshot, never taken blindly:
			// a pre-drain tree there is not the drain's, and the member is
			// skipped — tr.Next skips its bytes — with the rest still to land.
			kept, err := r.dropRacedDrainDir(root, before, beforeDirs, abs)
			if err != nil {
				return drained, landed, err
			}
			if kept != "" {
				r.driver.logger.Warn("sandbox: drain skipped %s: the pod's archive carries it as a file, but %s predates the drain — the tree there is not the drain's to replace", clean, kept)
				continue
			}
			n, err := r.writeDrainFile(abs, clean, capBytes, capBytes-drained, hdr, tr)
			drained += n
			if err != nil {
				return drained, landed, err
			}
			landed[clean] = true
		case tar.TypeSymlink:
			// A link carries no bytes of the set — only the files it points
			// into do, when their own member lands — so neither a landed
			// link nor a refused one moves the meter: a refused link (its
			// target leaves the tree) is a member dropped with its reason,
			// and a landed one adds nothing to count. The refusal is judged
			// first: a member that lands nothing removes nothing.
			if _, inside := symlinkLanding(root, abs, hdr.Linkname); !inside {
				r.driver.logger.Warn("sandbox: drain skipped the link %s -> %s: it leaves the run-files tree", abs, hdr.Linkname)
				continue
			}
			// A directory where the archive carries a link is the same
			// raced occupant the file branch cures — the link cannot be
			// written over it — judged on the same before snapshot, and
			// skipped on the same witness.
			kept, err := r.dropRacedDrainDir(root, before, beforeDirs, abs)
			if err != nil {
				return drained, landed, err
			}
			if kept != "" {
				r.driver.logger.Warn("sandbox: drain skipped the link %s -> %s: %s predates the drain — the tree there is not the drain's to replace", clean, hdr.Linkname, kept)
				continue
			}
			wrote, err := r.writeExportSymlink(root, abs, hdr)
			if err != nil {
				return drained, landed, lost(sandbox.ReadBackRefused, err)
			}
			if wrote {
				landed[clean] = true
			}
		default:
			// A hardlink, a device, a fifo: each is a way to point a later
			// write somewhere it was never meant to reach, and none is a
			// file a tool produces as its product.
			r.driver.logger.Warn("sandbox: drain skipped %s: the pod's archive carries it as type %q, which the host does not write back", abs, string(hdr.Typeflag))
			continue
		}
	}
	return drained, landed, nil
}

// writeDrainFile writes one regular member — member is its name in the set,
// HostDir-relative — and returns the bytes it copied, metered against the
// remaining budget as it goes: an archive that declares one size and
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
func (r *Run) writeDrainFile(abs, member string, capBytes, budget int64, hdr *tar.Header, src io.Reader) (int64, error) {
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
	if err := staged.Chmod(exportFileMode(hdr.FileInfo().Mode())); err != nil {
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
// Only the host's own failure to read or remove is an error.
func (r *Run) dropRacedDrainDir(hostDir string, before, beforeDirs map[string]bool, abs string) (kept string, err error) {
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
		return nil
	})
	if walkErr != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("read %s: %w", abs, walkErr))
	}
	if kept != "" {
		return kept, nil
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", lost(sandbox.ReadBackRefused, fmt.Errorf("replace %s: %w", abs, err))
	}
	r.driver.logger.Warn("sandbox: removed %s — a directory a raced drain attempt landed where the settled archive carries a file or a link", abs)
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
func (r *Run) dropRacedDrainLeftovers(hostDir string, before, beforeDirs, landed map[string]bool) error {
	nowFiles, nowDirs, err := readBackDirEntries(hostDir)
	if err != nil {
		return fmt.Errorf("list host dir after drain: %w", err)
	}
	for rel := range nowFiles {
		if before[rel] || landed[rel] {
			continue
		}
		path := filepath.Join(hostDir, filepath.FromSlash(rel))
		// Containment by filepath.Rel, matching the walks above: Rel
		// cleans both of its sides, so a hostDir carrying a trailing
		// separator still reads as the base — a prefix compare against
		// the raw dir would refuse every candidate and fail a drain
		// that landed — and a candidate climbing out of hostDir still
		// reads as outside.
		if back, backErr := filepath.Rel(hostDir, path); backErr != nil || back == ".." || strings.HasPrefix(back, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to remove %q: outside %s", path, hostDir)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove raced leftover %s: %w", path, err)
		}
		r.driver.logger.Warn("sandbox: removed %s — landed by a raced drain attempt, absent from the one that succeeded", path)
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
		r.driver.logger.Warn("sandbox: removed %s — a directory a raced drain attempt built, absent from the one that succeeded", path)
	}
	return nil
}
