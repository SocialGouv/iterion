package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mirrorBundleSkills copies every top-level entry from bundle.SkillsDir
// into <workDir>/.claude/skills/, preserving file mode but skipping
// names that already exist on disk (workspace wins on collision, per
// the docs/bundles.md collision rule).
//
// Symlinks would be lighter than a copy but they break inside the
// sandbox bind-mount: the in-container view sees /workspace and any
// symlink target outside that mount returns ENOENT.
//
// No-op when bundle is nil or carries no skills directory.
func mirrorBundleSkills(workDir string, b *bundle.Bundle, logger *iterlog.Logger) error {
	if b == nil || b.SkillsDir == "" || workDir == "" {
		return nil
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir %s: %w", dest, err)
	}
	entries, err := os.ReadDir(b.SkillsDir)
	if err != nil {
		return fmt.Errorf("runtime/bundle: read skills dir %s: %w", b.SkillsDir, err)
	}
	mirrored, shadowed := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		destPath := filepath.Join(dest, name)
		if _, err := os.Stat(destPath); err == nil {
			shadowed++
			if logger != nil {
				logger.Warn("bundle skill %q shadowed by existing workspace entry at %s", name, destPath)
			}
			continue
		}
		srcPath := filepath.Join(b.SkillsDir, name)
		if entry.IsDir() {
			if err := copyDir(srcPath, destPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, destPath); err != nil {
				return err
			}
		}
		mirrored++
	}
	if logger != nil && mirrored > 0 {
		logger.Info("bundle: mirrored %d skill(s) into %s (shadowed=%d)", mirrored, dest, shadowed)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("runtime/bundle: open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("runtime/bundle: stat %s: %w", src, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("runtime/bundle: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("runtime/bundle: copy %s → %s: %w", src, dst, err)
	}
	return out.Close()
}

// promoteBundleAttachmentDefaults reads every attachment declared in
// the bundle's manifest.yaml `attachments:` map and persists it as a
// run attachment via store.WriteAttachment. Runs before the host-side
// attachmentPromote callback so runtime uploads (Launch modal, cloud)
// can override bundle defaults by re-writing the same attachment name.
//
// Only attachments declared in both the bundle manifest AND the
// workflow's `attachments:` block are promoted — others are warned
// and skipped (the workflow would not be able to reference them
// anyway).
//
// No-op when bundle, manifest, or attachments map are absent.
func promoteBundleAttachmentDefaults(
	ctx context.Context,
	s store.RunStore,
	runID string,
	wf *ir.Workflow,
	b *bundle.Bundle,
	logger *iterlog.Logger,
) error {
	if b == nil || b.Manifest == nil || len(b.Manifest.Attachments) == 0 || b.AttachmentsDir == "" {
		return nil
	}
	for name, relPath := range b.Manifest.Attachments {
		if wf != nil {
			if _, declared := wf.Attachments[name]; !declared {
				if logger != nil {
					logger.Warn("bundle manifest declares attachment %q but workflow does not — skipping", name)
				}
				continue
			}
		}
		srcPath := filepath.Join(b.AttachmentsDir, relPath)
		f, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("runtime/bundle: open attachment %s: %w", srcPath, err)
		}
		// Sniff MIME from the first 512 bytes; reset the file before
		// passing it to WriteAttachment so the stream starts at zero.
		head := make([]byte, 512)
		n, _ := f.Read(head)
		mime := http.DetectContentType(head[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("runtime/bundle: rewind %s: %w", srcPath, err)
		}
		rec := store.AttachmentRecord{
			Name:             name,
			OriginalFilename: filepath.Base(relPath),
			MIME:             mime,
		}
		writeErr := s.WriteAttachment(ctx, runID, rec, f)
		f.Close()
		if writeErr != nil {
			return fmt.Errorf("runtime/bundle: write attachment %q: %w", name, writeErr)
		}
		if logger != nil {
			logger.Info("bundle: promoted default attachment %q (file=%s, mime=%s)", name, relPath, mime)
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("runtime/bundle: stat %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir %s: %w", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("runtime/bundle: read %s: %w", src, err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d); err != nil {
			// EEXIST inside a recursive copy means a deeper file already
			// existed; treat as a benign collision rather than aborting
			// the whole bundle setup.
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
	}
	return nil
}
