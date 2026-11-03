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
// carries a file, or the mirror, is replaced by the extract — the dir only
// where the before snapshot proves the drain wrote it (dropRacedDrainDir).
func (r *Run) drainRunFilesDir(ctx context.Context, dir sandbox.ReadBackDir) error {
	if err := os.MkdirAll(dir.HostDir, 0o775); err != nil {
		return fmt.Errorf("kubernetes: drain %s: create host dir %s: %w", dir.ContainerPath, dir.HostDir, err)
	}
	// The sweep's anchor: the host dir's state BEFORE any archive landed.
	// An entry it holds is the store's (or, on a bind, the pod's own) — a
	// raced attempt is never allowed to take it.
	before, err := readBackDirEntries(dir.HostDir)
	if err != nil {
		return fmt.Errorf("kubernetes: drain %s: list host dir %s: %w", dir.ContainerPath, dir.HostDir, err)
	}
	attempts, pause := exportRetrySettings(r.driver.logger)
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		retryable, landed, err := r.drainOnce(ctx, dir, before)
		if err == nil {
			if attempt > 1 {
				r.driver.logger.Warn("sandbox: run-files drain of %s succeeded on attempt %d/%d, after tar saw the tree change mid-archive", dir.ContainerPath, attempt, attempts)
				if err := r.dropRacedDrainLeftovers(dir.HostDir, before, landed); err != nil {
					return err
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
				return fmt.Errorf("kubernetes: drain %s: retry abandoned (%v): %w", dir.ContainerPath, ctx.Err(), last)
			case <-time.After(pause):
			}
			pause *= 2
		}
	}
	return fmt.Errorf("kubernetes: drain %s: the pod's run files kept changing under the drain after %d attempts: %w", dir.ContainerPath, attempts, last)
}

// drainOnce streams one archive of dir.ContainerPath and lands it under
// dir.HostDir. retryable reports a failure onlyTarRaceWarnings accepts as
// transient — the workspace export's own predicate; landed names the members
// the extract wrote, HostDir-relative and slash-separated. before is the
// host dir's pre-drain state, the proof a wrong-kind occupant is judged on.
func (r *Run) drainOnce(ctx context.Context, dir sandbox.ReadBackDir, before map[string]bool) (retryable bool, landed map[string]bool, err error) {
	kubectlArgs := []string{"--namespace", r.namespace,
		"exec", r.podName, "--container", "workload", "--",
		"env", "LC_ALL=" + sandbox.TarLocale, "tar", "-C", dir.ContainerPath, "-cf", "-", "."}
	podTar := r.driver.kubectl.cmdContext(ctx, kubectlArgs...)

	pipe, err := podTar.StdoutPipe()
	if err != nil {
		return false, nil, fmt.Errorf("kubernetes: drain %s: pod tar stdout pipe: %w", dir.ContainerPath, err)
	}
	var podErr bytes.Buffer
	podTar.Stderr = &podErr

	if err := podTar.Start(); err != nil {
		return false, nil, fmt.Errorf("kubernetes: drain %s: start the pod's archiver: %w", dir.ContainerPath, err)
	}
	drained, landed, xerr := r.extractDrain(pipe, dir.HostDir, before)
	if xerr != nil {
		// Drain the pipe so the pod's archiver sees its reader go away
		// rather than block on a full one, then report the extraction.
		_, _ = io.Copy(io.Discard, pipe)
		_ = podTar.Wait()
		return false, nil, fmt.Errorf("kubernetes: drain %s into %s: %w", dir.ContainerPath, dir.HostDir, xerr)
	}
	if err := podTar.Wait(); err != nil {
		return onlyTarRaceWarnings(err, podErr.String()), nil, fmt.Errorf("kubernetes: drain %s: in-pod tar: %w\n%s", dir.ContainerPath, err, strings.TrimSpace(podErr.String()))
	}
	r.driver.logger.Info("sandbox: drained %d bytes of run files from pod %s:%s into %s", drained, r.podName, dir.ContainerPath, dir.HostDir)
	return false, landed, nil
}

// extractDrain writes the pod's archive under hostDst and returns the
// bytes it landed and the members it wrote, HostDir-relative and
// slash-separated — the successful attempt's own record of what it put
// there, which is what a retried drain's sweep keeps against everything
// else a raced attempt may have left. before is the host dir's pre-drain
// state: the proof a wrong-kind occupant at a member's path is judged on
// (the drain's to replace when it wrote it, the store's or the pod's when
// it did not).
//
// The host reads the archive itself, like the workspace export does: the
// member names are chosen in the sandbox, by an archiver the workflow's
// own image provides, so the decision is made on the path a write lands
// on after every existing symlink in its parent chain is resolved. Unlike
// the workspace export there are no name exclusions — a run file is the
// run's product whatever it is named — and every refusal (a member that
// cannot be made safe, a type the host does not write back) is skipped
// and reported rather than written.
func (r *Run) extractDrain(stream io.Reader, hostDst string, before map[string]bool) (int64, map[string]bool, error) {
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
			return drained, nil, fmt.Errorf("read the pod's archive: %w", err)
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
			// the mirror of the file branch's own replace. A raced attempt
			// (or the pre-drain state, on a flip) must not leave a member
			// standing where the settled archive builds a directory.
			if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
				if err := os.Remove(abs); err != nil {
					return drained, landed, fmt.Errorf("replace %s: %w", abs, err)
				}
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return drained, landed, fmt.Errorf("create %s: %w", abs, err)
			}
		case tar.TypeReg:
			// A directory where the archive carries a file is what a raced
			// attempt's extract leaves behind; the file branch cannot write
			// over it. Judged on the before snapshot, never taken blindly.
			if err := r.dropRacedDrainDir(root, before, abs); err != nil {
				return drained, landed, err
			}
			n, err := r.writeDrainFile(abs, sandbox.ReadBackCapBytes-drained, hdr, tr)
			drained += n
			if err != nil {
				return drained, landed, err
			}
			if drained > sandbox.ReadBackCapBytes {
				return drained, landed, fmt.Errorf("the run's read-back set is over the %d MiB cap (ADR-143); the rest stays in the pod", sandbox.ReadBackCapBytes>>20)
			}
			landed[clean] = true
		case tar.TypeSymlink:
			// A link carries no bytes of the set — only the files it points
			// into do, when their own member lands — so neither a landed
			// link nor a refused one moves the meter: a refused link (its
			// target leaves the tree) is a member dropped with its reason,
			// and a landed one adds nothing to count.
			wrote, err := r.writeExportSymlink(root, abs, hdr)
			if err != nil {
				return drained, landed, err
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

// writeDrainFile writes one regular member and returns the bytes it
// copied, metered against the read-back cap as it goes: an archive that
// declares one size and carries another is billed for what actually
// arrived. An existing symlink at the path is replaced rather than
// followed — the host writes the file the member names, never through a
// link the archive left there first.
func (r *Run) writeDrainFile(abs string, budget int64, hdr *tar.Header, src io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(abs), err)
	}
	if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
		if err := os.Remove(abs); err != nil {
			return 0, fmt.Errorf("replace %s: %w", abs, err)
		}
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, exportFileMode(hdr.FileInfo().Mode()))
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", abs, err)
	}
	// One byte past the remaining budget, so an archive that carries more
	// than it declared is billed for the evidence of being over instead of
	// stopping short at exactly the cap.
	n, copyErr := io.Copy(f, io.LimitReader(src, budget+1))
	closeErr := f.Close()
	if copyErr != nil {
		return n, fmt.Errorf("write %s: %w", abs, copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("close %s: %w", abs, closeErr)
	}
	return n, nil
}

// dropRacedDrainDir removes a directory standing where the settled archive
// carries a regular file — the one occupant writeDrainFile's replace does
// not take, and what a raced attempt's extract leaves behind on a type
// flip. The removal is proven, never blind: the before snapshot must hold
// no file or link under the tree, or the tree predates the drain (the
// store's, or the pod's own on a bind) and the removal is refused with
// that entry named — a raced attempt never takes pre-drain state.
func (r *Run) dropRacedDrainDir(hostDir string, before map[string]bool, abs string) error {
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil // writeDrainFile replaces a non-dir occupant itself
	}
	var kept string
	walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(hostDir, path)
		if relErr != nil {
			return relErr
		}
		if before[filepath.ToSlash(rel)] {
			kept = filepath.ToSlash(rel)
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("read %s: %w", abs, walkErr)
	}
	if kept != "" {
		return fmt.Errorf("the settled archive carries %s as a file, but %s under it predates the drain — not the drain's to remove", abs, kept)
	}
	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("replace %s: %w", abs, err)
	}
	r.driver.logger.Warn("sandbox: removed %s — a directory a raced drain attempt landed where the settled archive carries a file", abs)
	return nil
}

// readBackDirEntries lists the regular files and symlinks under dir,
// relative to it and slash-separated — the two kinds the drain writes. The
// drain only ever removes what it can prove it wrote, so this walk is the
// host dir's state BEFORE any archive landed; an entry it already holds is
// never a sweep candidate, whoever wrote it.
func readBackDirEntries(dir string) (map[string]bool, error) {
	entries := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return filepath.SkipDir
			}
			return err
		}
		if d.Type().IsRegular() || d.Type()&os.ModeSymlink != 0 {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				return relErr
			}
			entries[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return entries, err
}

// dropRacedDrainLeftovers removes, after a retried drain, every regular
// file or symlink under hostDir that was not there before the drain and
// that the successful archive did not land: only a raced attempt can have
// written it. tar never deletes, so without this sweep a file the pod
// dropped between two attempts would survive in the run's collected set.
// Candidates come from walking hostDir itself — never from names the pod
// sent — and an entry the host already had is never touched.
func (r *Run) dropRacedDrainLeftovers(hostDir string, before, landed map[string]bool) error {
	now, err := readBackDirEntries(hostDir)
	if err != nil {
		return fmt.Errorf("list host dir after drain: %w", err)
	}
	for rel := range now {
		if before[rel] || landed[rel] {
			continue
		}
		path := filepath.Join(hostDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(path, hostDir+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to remove %q: outside %s", path, hostDir)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove raced leftover %s: %w", path, err)
		}
		r.driver.logger.Warn("sandbox: removed %s — landed by a raced drain attempt, absent from the one that succeeded", path)
	}
	return nil
}
