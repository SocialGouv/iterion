package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// bundleMirrorMarkerDir is the sidecar directory under
// <workDir>/.claude/skills/ where iterion stores per-skill content
// hashes of the last mirror operation. The marker file
// <markerDir>/<name>.sha256 contains the hex sha256 of what we last
// wrote at <skills>/<name>. We use it to distinguish two collision
// cases that the v0.1.0 unconditional-shadow rule conflated:
//
//   - User-customized: workspace file's hash != marker → preserve.
//   - Stale previous mirror: workspace file's hash == marker → safe
//     to refresh with the bundle's current content (the upgrade
//     case — a v0.2.0 bot run after a v0.1.0 would otherwise see
//     v0.1.0's skill files indefinitely).
const bundleMirrorMarkerDir = ".iterion-managed"

// skillReconcileOutcome enumerates the four collision-policy results
// for a single file-skill mirror: what reconcileSkillFile observed
// and acted upon. The caller logs aggregate counts.
type skillReconcileOutcome int

const (
	skillOutcomeMirrored  skillReconcileOutcome = iota // new file copied + marker written
	skillOutcomeUpToDate                               // dest matched source verbatim
	skillOutcomeRefreshed                              // marker matched dest → safe overwrite
	skillOutcomeShadowed                               // dest exists and diverged → leave alone
)

// reconcileSkillFile applies the 4-branch collision policy to one
// file skill: copy / no-op / refresh / shadow. Shared by
// MirrorSingleSkill (called per-skill on chatbox attach) and
// mirrorBundleSkills (called once per skill at run start) so the
// rules stay in lockstep. The caller has already prepared
// markerDir/dest and resolved srcPath.
func reconcileSkillFile(srcPath, destPath, markerPath string, logger *iterlog.Logger) (skillReconcileOutcome, error) {
	srcHash, err := hashFile(srcPath)
	if err != nil {
		return skillOutcomeShadowed, err
	}
	destInfo, destErr := os.Stat(destPath)
	switch {
	case errors.Is(destErr, os.ErrNotExist):
		if err := copyFile(srcPath, destPath); err != nil {
			return skillOutcomeShadowed, err
		}
		if err := writeMarker(markerPath, srcHash); err != nil {
			return skillOutcomeShadowed, err
		}
		return skillOutcomeMirrored, nil
	case destErr != nil:
		return skillOutcomeShadowed, fmt.Errorf("runtime/bundle: stat %s: %w", destPath, destErr)
	}
	destHash, err := hashFile(destPath)
	if err != nil {
		return skillOutcomeShadowed, err
	}
	if destHash == srcHash {
		if err := writeMarker(markerPath, srcHash); err != nil {
			return skillOutcomeShadowed, err
		}
		return skillOutcomeUpToDate, nil
	}
	if markerHash := readMarker(markerPath); markerHash != "" && markerHash == destHash {
		_ = os.Chmod(destPath, destInfo.Mode().Perm())
		if err := overwriteFile(srcPath, destPath); err != nil {
			return skillOutcomeShadowed, err
		}
		if err := writeMarker(markerPath, srcHash); err != nil {
			return skillOutcomeShadowed, err
		}
		return skillOutcomeRefreshed, nil
	}
	if logger != nil {
		logger.Warn("bundle skill %q shadowed by existing workspace entry at %s (workspace differs from both source and previous-mirror marker)", filepath.Base(srcPath), destPath)
	}
	return skillOutcomeShadowed, nil
}

// MirrorSingleSkill mirrors one bundle skill by name into the run's
// .claude/skills/ directory, applying the same collision policy as
// mirrorBundleSkills. Used by the chatbox skill-attachment path: when
// an operator queues a message with skill refs, the drain logic calls
// this once per ref before injecting the message into the agent's
// conversation.
//
// No-op when the bundle is nil, has no SkillsDir, or the skill name
// doesn't resolve to a file/dir under SkillsDir. Returns an error
// only when the copy/marker write itself fails — a missing skill
// silently no-ops (the agent simply sees the text message without
// the skill loaded; the studio surfaces the discrepancy via the
// catalog endpoint).
func MirrorSingleSkill(workDir string, b *bundle.Bundle, name string, logger *iterlog.Logger) error {
	if b == nil || b.SkillsDir == "" || workDir == "" || name == "" {
		return nil
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("runtime/bundle: invalid skill name %q", name)
	}
	srcPath := filepath.Join(b.SkillsDir, name)
	info, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			if logger != nil {
				logger.Warn("queued-message skill %q not found in bundle — skipping", name)
			}
			return nil
		}
		return fmt.Errorf("runtime/bundle: stat skill %s: %w", srcPath, err)
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir %s: %w", dest, err)
	}
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir markers %s: %w", markerDir, err)
	}
	destPath := filepath.Join(dest, name)
	if info.IsDir() {
		if _, statErr := os.Stat(destPath); statErr == nil {
			return nil
		}
		return copyDir(srcPath, destPath)
	}
	_, err = reconcileSkillFile(srcPath, destPath, filepath.Join(markerDir, name+".sha256"), logger)
	return err
}

// mirrorBundleSkills copies every top-level entry from bundle.SkillsDir
// into <workDir>/.claude/skills/.
//
// Collision policy (v2 of docs/bundles.md "workspace wins" rule):
//   - File doesn't exist → copy, record marker.
//   - File exists & content == source → no-op (already current).
//   - File exists & content == previous mirror marker → refresh (we
//     wrote it last, user hasn't touched it).
//   - File exists & content differs from both source and marker →
//     SHADOW (user customized OR a different bundle owns the name).
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
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return fmt.Errorf("runtime/bundle: mkdir markers %s: %w", markerDir, err)
	}
	entries, err := os.ReadDir(b.SkillsDir)
	if err != nil {
		return fmt.Errorf("runtime/bundle: read skills dir %s: %w", b.SkillsDir, err)
	}
	mirrored, refreshed, shadowed, uptodate := 0, 0, 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if name == bundleMirrorMarkerDir {
			continue // never mirror our own marker dir
		}
		destPath := filepath.Join(dest, name)
		srcPath := filepath.Join(b.SkillsDir, name)
		if entry.IsDir() {
			// Directory skills bypass the marker logic — we keep the
			// original "copy missing, skip existing" behaviour. The
			// per-file marker would need to walk every nested file
			// and that's more complex than current use justifies.
			if _, err := os.Stat(destPath); err == nil {
				shadowed++
				if logger != nil {
					logger.Warn("bundle skill %q shadowed by existing workspace entry at %s", name, destPath)
				}
				continue
			}
			if err := copyDir(srcPath, destPath); err != nil {
				return err
			}
			mirrored++
			continue
		}
		// File skill: shared reconciliation with MirrorSingleSkill.
		outcome, err := reconcileSkillFile(srcPath, destPath, filepath.Join(markerDir, name+".sha256"), logger)
		if err != nil {
			return err
		}
		switch outcome {
		case skillOutcomeMirrored:
			mirrored++
		case skillOutcomeUpToDate:
			uptodate++
		case skillOutcomeRefreshed:
			refreshed++
		case skillOutcomeShadowed:
			shadowed++
		}
	}
	if logger != nil && (mirrored > 0 || refreshed > 0 || uptodate > 0) {
		logger.Info("bundle: skills mirrored=%d refreshed=%d up-to-date=%d shadowed=%d at %s", mirrored, refreshed, uptodate, shadowed, dest)
	}
	return nil
}

// hashFile returns the hex sha256 of path's content.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("runtime/bundle: hash open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("runtime/bundle: hash read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readMarker returns the sha256 hex stored in path, or "" on any error
// (missing file, unreadable, empty). Marker absence is a benign signal
// — we treat the existing skill as user-owned and shadow.
func readMarker(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeMarker records hash at path. Best-effort: failures don't abort
// the mirror (we've already copied the content; missing marker just
// means the next run shadows instead of refreshes).
func writeMarker(path, hash string) error {
	if err := os.WriteFile(path, []byte(hash), 0o644); err != nil {
		return fmt.Errorf("runtime/bundle: write marker %s: %w", path, err)
	}
	return nil
}

// overwriteFile replaces dst's content with src's content atomically
// via a sibling temp + rename. Unlike copyFile which uses O_EXCL,
// this is intended for the refresh path where we've confirmed it's
// safe to clobber.
func overwriteFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("runtime/bundle: open %s: %w", src, err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("runtime/bundle: stat %s: %w", src, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("runtime/bundle: tempfile %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("runtime/bundle: copy %s → %s: %w", src, tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("runtime/bundle: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("runtime/bundle: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("runtime/bundle: rename %s → %s: %w", tmpName, dst, err)
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
