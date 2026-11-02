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
func (r *Run) drainRunFilesDir(ctx context.Context, dir sandbox.ReadBackDir) error {
	if err := os.MkdirAll(dir.HostDir, 0o775); err != nil {
		return fmt.Errorf("kubernetes: drain %s: create host dir %s: %w", dir.ContainerPath, dir.HostDir, err)
	}
	kubectlArgs := []string{"--namespace", r.namespace,
		"exec", r.podName, "--container", "workload", "--",
		"env", "LC_ALL=" + sandbox.TarLocale, "tar", "-C", dir.ContainerPath, "-cf", "-", "."}
	podTar := r.driver.kubectl.cmdContext(ctx, kubectlArgs...)

	pipe, err := podTar.StdoutPipe()
	if err != nil {
		return fmt.Errorf("kubernetes: drain %s: pod tar stdout pipe: %w", dir.ContainerPath, err)
	}
	var podErr bytes.Buffer
	podTar.Stderr = &podErr

	if err := podTar.Start(); err != nil {
		return fmt.Errorf("kubernetes: drain %s: start the pod's archiver: %w", dir.ContainerPath, err)
	}
	drained, xerr := r.extractDrain(pipe, dir.HostDir)
	if xerr != nil {
		// Drain the pipe so the pod's archiver sees its reader go away
		// rather than block on a full one, then report the extraction.
		_, _ = io.Copy(io.Discard, pipe)
		_ = podTar.Wait()
		return fmt.Errorf("kubernetes: drain %s into %s: %w", dir.ContainerPath, dir.HostDir, xerr)
	}
	if err := podTar.Wait(); err != nil {
		return fmt.Errorf("kubernetes: drain %s: in-pod tar: %w\n%s", dir.ContainerPath, err, strings.TrimSpace(podErr.String()))
	}
	r.driver.logger.Info("sandbox: drained %d bytes of run files from pod %s:%s into %s", drained, r.podName, dir.ContainerPath, dir.HostDir)
	return nil
}

// extractDrain writes the pod's archive under hostDst and returns the
// bytes it landed.
//
// The host reads the archive itself, like the workspace export does: the
// member names are chosen in the sandbox, by an archiver the workflow's
// own image provides, so the decision is made on the path a write lands
// on after every existing symlink in its parent chain is resolved. Unlike
// the workspace export there are no name exclusions — a run file is the
// run's product whatever it is named — and every refusal (a member that
// cannot be made safe, a type the host does not write back) is skipped
// and reported rather than written.
func (r *Run) extractDrain(stream io.Reader, hostDst string) (int64, error) {
	root, err := filepath.Abs(hostDst)
	if err != nil {
		return 0, fmt.Errorf("resolve drain destination %s: %w", hostDst, err)
	}
	root = filepath.Clean(root)
	var drained int64
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return drained, fmt.Errorf("read the pod's archive: %w", err)
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
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return drained, fmt.Errorf("create %s: %w", abs, err)
			}
		case tar.TypeReg:
			n, err := r.writeDrainFile(abs, sandbox.ReadBackCapBytes-drained, hdr, tr)
			drained += n
			if err != nil {
				return drained, err
			}
			if drained > sandbox.ReadBackCapBytes {
				return drained, fmt.Errorf("the run's read-back set is over the %d MiB cap (ADR-143); the rest stays in the pod", sandbox.ReadBackCapBytes>>20)
			}
		case tar.TypeSymlink:
			// The write is counted only when the link landed: a refused
			// link (its target leaves the tree) is a member dropped with
			// its reason, not a byte of the set.
			if _, err := r.writeExportSymlink(root, abs, hdr); err != nil {
				return drained, err
			}
		default:
			// A hardlink, a device, a fifo: each is a way to point a later
			// write somewhere it was never meant to reach, and none is a
			// file a tool produces as its product.
			r.driver.logger.Warn("sandbox: drain skipped %s: the pod's archive carries it as type %q, which the host does not write back", abs, string(hdr.Typeflag))
			continue
		}
	}
	return drained, nil
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
