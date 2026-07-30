package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
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
	if info.IsDir() {
		destPath := filepath.Join(dest, name)
		if _, statErr := os.Stat(destPath); statErr == nil {
			return nil
		}
		return copyDir(srcPath, destPath)
	}
	// File skill → directory form (native discovery) + flat alias (prompt
	// Reads). Shared with mirrorBundleSkills via mirrorFileSkill.
	_, err = mirrorFileSkill(dest, markerDir, srcPath, name, logger)
	return err
}

// skillDestDirForm computes the destination for a FLAT source skill file so it
// lands in the DIRECTORY form Claude Code's Skill tool requires:
// <dest>/<stem>/SKILL.md. A flat <dest>/<name>.md is NOT discovered as a skill
// by claude_code — only the directory form is (Agent Skills spec); claw's
// skill_manager discovers BOTH the flat and directory forms, so the directory
// form satisfies both backends. stem drops a trailing ".md" ("whats-next.md" →
// dir "whats-next"); the marker keys on "<stem>.SKILL.md.sha256" (same scheme
// as mirrorLibrarySkills). The caller mkdirs the returned skillDir before
// reconciling into destPath.
func skillDestDirForm(dest, markerDir, srcName string) (skillDir, destPath, markerPath string, err error) {
	stem := strings.TrimSuffix(srcName, ".md")
	if stem == "" || stem == "." || stem == ".." || strings.ContainsAny(stem, "/\\") {
		return "", "", "", fmt.Errorf("runtime/bundle: invalid skill file name %q", srcName)
	}
	skillDir = filepath.Join(dest, stem)
	destPath = filepath.Join(skillDir, "SKILL.md")
	markerPath = filepath.Join(markerDir, stem+".SKILL.md.sha256")
	return skillDir, destPath, markerPath, nil
}

// mirrorFileSkill mirrors one flat "<stem>.md" source skill into BOTH forms
// under <dest>:
//   - directory form <stem>/SKILL.md — what the claude_code (and claw) Skill
//     TOOLS discover natively (Agent Skills spec);
//   - flat alias <stem>.md — what a bot prompt that Reads the skill by PATH
//     ("READ .claude/skills/<stem>.md FIRST", the pattern most catalog bots
//     use for upfront context) resolves.
//
// The mirror historically wrote only the flat file, then moved to the
// directory form for native discovery — which silently broke every prompt
// still Reading the flat path (the agent then wastes turns re-finding the file,
// observed dogfooding Vetty on a dependency PR). Writing both keeps native
// discovery AND explicit Reads working. Each form goes through
// reconcileSkillFile so the workspace-wins collision policy holds independently;
// the returned outcome is the directory-form result (the one callers tally).
func mirrorFileSkill(dest, markerDir, srcPath, name string, logger *iterlog.Logger) (skillReconcileOutcome, error) {
	skillDir, skillDest, markerPath, err := skillDestDirForm(dest, markerDir, name)
	if err != nil {
		return skillOutcomeShadowed, err
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return skillOutcomeShadowed, fmt.Errorf("runtime/bundle: mkdir %s: %w", skillDir, err)
	}
	outcome, err := reconcileSkillFile(srcPath, skillDest, markerPath, logger)
	if err != nil {
		return outcome, err
	}
	flatDest := filepath.Join(dest, name)
	flatMarker := filepath.Join(markerDir, name+".sha256")
	if _, ferr := reconcileSkillFile(srcPath, flatDest, flatMarker, logger); ferr != nil {
		return outcome, ferr
	}
	return outcome, nil
}

// mirrorBundleSkills copies every top-level entry from bundle.SkillsDir
// into <workDir>/.claude/skills/. A flat "<name>.md" source is mirrored as the
// directory form "<name>/SKILL.md" (see skillDestDirForm); a source that is
// already a directory is copied through unchanged.
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
// It returns the directories under <workDir>/.claude/skills/ that iterion
// OWNS — the ones it wrote or refreshed this run. A shadowed entry (the
// workspace's own file won) is deliberately absent: that is the target repo's
// content, and a backend deciding what it may hand an agent needs to tell the
// two apart. The workspace is a checkout of an untrusted repository, so nothing
// read back from it can establish that distinction; only the mirror knows.
func mirrorBundleSkills(workDir string, b *bundle.Bundle, logger *iterlog.Logger) ([]string, error) {
	if b == nil || b.SkillsDir == "" || workDir == "" {
		return nil, nil
	}
	dest := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("runtime/bundle: mkdir %s: %w", dest, err)
	}
	markerDir := filepath.Join(dest, bundleMirrorMarkerDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return nil, fmt.Errorf("runtime/bundle: mkdir markers %s: %w", markerDir, err)
	}
	entries, err := os.ReadDir(b.SkillsDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/bundle: read skills dir %s: %w", b.SkillsDir, err)
	}
	var owned []string
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
				return nil, err
			}
			owned = append(owned, destPath)
			mirrored++
			continue
		}
		// Only .md files are skills. Anything else at the top level
		// (.gitkeep placeholders, editor droppings) must be skipped:
		// a non-.md name keeps its full basename as the stem, so its
		// directory form and flat alias collide on the SAME path and
		// the mirror errors with "is a directory".
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		// File skill → both the directory form (native Skill-tool discovery)
		// and the flat <name>.md alias (prompt Reads). Shared with
		// MirrorSingleSkill via mirrorFileSkill.
		outcome, err := mirrorFileSkill(dest, markerDir, srcPath, name, logger)
		if err != nil {
			return nil, err
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
		if outcome != skillOutcomeShadowed {
			// The directory form is the canonical one for skill discovery; the
			// flat alias beside it is the same content under another name.
			owned = append(owned, filepath.Join(dest, strings.TrimSuffix(name, ".md")))
		}
	}
	if logger != nil && (mirrored > 0 || refreshed > 0 || uptodate > 0) {
		logger.Info("bundle: skills mirrored=%d refreshed=%d up-to-date=%d shadowed=%d at %s", mirrored, refreshed, uptodate, shadowed, dest)
	}
	return owned, nil
}

// MergeBundlePresets folds a bundle's file-based presets
// (presets/<name>.md) into the compiled workflow's preset set. A file
// preset OVERWRITES an in-source `presets:` entry of the same name — the
// explicit, richer artifact wins. Best-effort: a malformed preset file is
// logged and skipped, never failing the merge. Idempotent, so it can run
// both at compile (CLI/dispatcher paths that pass the bundle to the
// compiler) and again as an engine backstop at run start (studio / cloud
// paths that compiled without the bundle but attached it via WithBundle).
//
// Var-only file presets behave exactly like an in-source preset; the extra
// Prompt/Skills/DisplayName/Description dimensions are what the inline
// block can't express (a launch-time "## Focus" bias + skill hints).
func MergeBundlePresets(wf *ir.Workflow, b *bundle.Bundle, logger *iterlog.Logger) {
	if wf == nil || b == nil || b.PresetsDir == "" {
		return
	}
	specs, errs := bundle.LoadPresets(b.PresetsDir)
	for _, err := range errs {
		if logger != nil {
			logger.Warn("runtime: bundle preset: %v", err)
		}
	}
	if len(specs) == 0 {
		return
	}
	if wf.Presets == nil {
		wf.Presets = make(map[string]ir.Preset, len(specs))
	}
	for _, ps := range specs {
		wf.Presets[ps.Name] = presetSpecToIR(ps)
	}
}

// presetSpecToIR converts a bundle's on-disk preset into the runtime IR
// form. The bundle package stays decoupled from pkg/dsl/ir, so the bridge
// lives here.
func presetSpecToIR(ps bundle.PresetSpec) ir.Preset {
	return ir.Preset{
		Name:        ps.Name,
		Values:      maps.Clone(ps.Vars), // nil-safe; engine coerces values to var types
		DisplayName: ps.DisplayName,
		Description: ps.Description,
		Prompt:      ps.Prompt,
		Skills:      ps.Skills,
	}
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

// overwriteFile replaces dst's content with src's content via a durable
// atomic write (store.WriteFileAtomic: write-temp → fsync → rename →
// dir-fsync). Unlike copyFile which uses O_EXCL, this is intended for the
// refresh path where we've confirmed it's safe to clobber.
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
	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("runtime/bundle: read %s: %w", src, err)
	}
	if err := store.WriteFileAtomic(dst, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("runtime/bundle: write %s → %s: %w", src, dst, err)
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
