package kubernetes

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/safepath"
)

// extractExport writes the pod's archive under hostDst and returns the
// members it wrote, workspace-relative and slash-separated.
//
// The host reads the archive itself rather than handing the stream to `tar
// -x --exclude=…`, because the member names are chosen in the sandbox — by an
// archiver the workflow's own `sandbox:` image provides. A tar exclude
// pattern answers for the TEXT of a name, and the same file has many names:
// `.git/config`, `./.git/./config` and a member sent through a symlink all
// reach the host clone's config while matching no pattern. Here the decision
// is made on the path the write lands on, after every existing symlink in its
// parent chain is resolved, which is the thing the exclusions are about.
//
// Entries that cannot be made safe are skipped and reported, never written:
// the export is a best-effort overlay of the run's work, and a refusal costs
// a file, while obeying would cost the host.
func (r *Run) extractExport(stream io.Reader, hostDst string) (map[string]bool, error) {
	root, err := filepath.Abs(hostDst)
	if err != nil {
		return nil, fmt.Errorf("resolve export destination %s: %w", hostDst, err)
	}
	root = filepath.Clean(root)
	extracted := map[string]bool{}
	dirTimes := map[string]time.Time{}
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read the pod's archive: %w", err)
		}
		rel, abs, skip, err := r.exportTarget(root, hdr.Name)
		if err != nil {
			return nil, err
		}
		if skip || rel == "." {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", abs, err)
			}
			dirTimes[abs] = hdr.ModTime
		case tar.TypeReg:
			if err := r.writeExportFile(abs, hdr, tr); err != nil {
				return nil, err
			}
		case tar.TypeSymlink:
			wrote, err := r.writeExportSymlink(root, abs, hdr)
			if err != nil {
				return nil, err
			}
			if !wrote {
				continue
			}
		default:
			// A hardlink, a device, a fifo: nothing a workspace needs back
			// from a run, and each is a way to point a later write somewhere
			// it was never meant to reach.
			r.driver.logger.Warn("sandbox: export skipped %s: the pod's archive carries it as type %q, which the host does not write back", rel, string(hdr.Typeflag))
			continue
		}
		extracted[rel] = true
	}
	// Directory times last: writing a child bumps its parent's mtime.
	for dir, mod := range dirTimes {
		if mod.IsZero() {
			continue
		}
		if err := os.Chtimes(dir, mod, mod); err != nil && !os.IsNotExist(err) {
			r.driver.logger.Warn("sandbox: export could not restore the time of %s: %v", dir, err)
		}
	}
	return extracted, nil
}

// exportTarget turns one member name into the path to write, or says to skip
// it. rel is workspace-relative, slash-separated, and is what the EXCLUSIONS
// are tested on — after symlink resolution, so the answer is about the file
// the write reaches.
func (r *Run) exportTarget(root, name string) (rel, abs string, skip bool, err error) {
	if err := safepath.GuardName(name); err != nil {
		r.driver.logger.Warn("sandbox: export refused a member of the pod's archive: %v", err)
		return "", "", true, nil
	}
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "./")
	if clean == "" || clean == "." {
		return ".", "", true, nil
	}
	abs, err = safepath.Join(root, clean)
	if err != nil {
		r.driver.logger.Warn("sandbox: export refused %q from the pod's archive: %v", name, err)
		return "", "", true, nil
	}
	landing, err := safepath.Landing(root, abs)
	if err != nil {
		r.driver.logger.Warn("sandbox: export refused %q from the pod's archive: %v", name, err)
		return "", "", true, nil
	}
	if exportExcluded(landing) {
		return landing, abs, true, nil
	}
	return landing, abs, false, nil
}

// writeExportFile writes one regular member. An existing symlink at the path
// is replaced rather than followed: the host writes the file the member
// names, never through a link the archive left there first.
func (r *Run) writeExportFile(abs string, hdr *tar.Header, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(abs), err)
	}
	// Replace rather than reopen: the path may hold a symlink the archive
	// left there (which O_WRONLY would follow), or a read-only file — git
	// writes its objects 0444, and the export carries them back.
	if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("replace %s: %w", abs, err)
		}
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, exportFileMode(hdr.FileInfo().Mode()))
	if err != nil {
		return fmt.Errorf("create %s: %w", abs, err)
	}
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", abs, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", abs, err)
	}
	// The mode of an existing file is not changed by O_CREATE.
	if err := os.Chmod(abs, exportFileMode(hdr.FileInfo().Mode())); err != nil {
		return fmt.Errorf("chmod %s: %w", abs, err)
	}
	if !hdr.ModTime.IsZero() {
		// git reads mtime to decide what changed: a file restored with
		// today's time reads as modified in a clone that is identical.
		if err := os.Chtimes(abs, hdr.ModTime, hdr.ModTime); err != nil {
			return fmt.Errorf("set the time of %s: %w", abs, err)
		}
	}
	return nil
}

// writeExportSymlink recreates a symlink the run's work left in the
// workspace — a repository may legitimately hold some — but only where the
// link resolves inside the exported tree. A link pointing out of it is the
// member that turns a later write into a write anywhere. Reports whether the
// link was written.
func (r *Run) writeExportSymlink(root, abs string, hdr *tar.Header) (bool, error) {
	target := hdr.Linkname
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(abs), target)
	}
	resolved = filepath.Clean(resolved)
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		r.driver.logger.Warn("sandbox: export skipped the link %s -> %s: it leaves the exported workspace", abs, target)
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", filepath.Dir(abs), err)
	}
	if info, err := os.Lstat(abs); err == nil && !info.IsDir() {
		if err := os.Remove(abs); err != nil {
			return false, fmt.Errorf("replace %s: %w", abs, err)
		}
	}
	if err := os.Symlink(target, abs); err != nil {
		return false, fmt.Errorf("link %s -> %s: %w", abs, target, err)
	}
	return true, nil
}

// exportFileMode is the permission bits of a member and nothing else.
// FileMode.Perm() is what drops setuid, setgid and sticky — they live in
// FileMode's own bits, not in the low nine — and none of them is something
// the host needs back from a run.
func exportFileMode(mode os.FileMode) os.FileMode {
	return mode.Perm()
}
