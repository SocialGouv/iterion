package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// skillTier identifies which mirror source owns a written skill, recorded
// in the marker alongside the content hash. The refresh branch refuses a
// LOWER-ranked source overwriting a higher-ranked incumbent — without it,
// mirror ORDER decided the winner: bundle skills are written first, so a
// same-named plugin/library skill mirrored later would match the fresh
// marker and "refresh" the bundle's copy away, inverting the documented
// bundle > plugin > library precedence.
type skillTier string

const (
	skillTierBundle  skillTier = "bundle"
	skillTierPlugin  skillTier = "plugin"
	skillTierLibrary skillTier = "library"
)

// tierRank orders the tiers; the empty tier (a legacy hash-only marker)
// ranks lowest so pre-tier markers keep their historical refresh behaviour.
func tierRank(t skillTier) int {
	switch t {
	case skillTierBundle:
		return 3
	case skillTierPlugin:
		return 2
	case skillTierLibrary:
		return 1
	default:
		return 0
	}
}

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
func reconcileSkillFile(srcPath, destPath, markerPath string, tier skillTier, logger *iterlog.Logger) (skillReconcileOutcome, error) {
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
		// Mark ownership BEFORE writing the refresh marker. Failure
		// ordering under SIGKILL then trends toward safety (a file with
		// an .iterion-wrote sidecar but no .sha256 marker is skipped by
		// the pruner — no data loss) rather than unsafety (marker written
		// without the sidecar means a future UpToDate adoption cannot
		// tell iterion's own file from an operator's identical copy —
		// R2-F2 of the round-2 adversarial).
		markIterionWrote(markerPath)
		if err := writeMarker(markerPath, srcHash, tier); err != nil {
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
	markerHash, markerTier := readMarker(markerPath)
	if destHash == srcHash {
		// Same content: keep the HIGHEST-ranked owner on the marker, so a
		// later lower-tier pass over identical bytes can't demote ownership.
		if tierRank(markerTier) > tierRank(tier) {
			tier = markerTier
		}
		if err := writeMarker(markerPath, srcHash, tier); err != nil {
			return skillOutcomeShadowed, err
		}
		return skillOutcomeUpToDate, nil
	}
	if markerHash != "" && markerHash == destHash {
		if tierRank(markerTier) > tierRank(tier) {
			// The incumbent was written by a HIGHER-priority source this
			// run (bundle > plugin > library): the collision resolves to
			// the incumbent, not to whoever mirrors last.
			if logger != nil {
				logger.Warn("skill %q: %s-tier copy kept over the %s-tier one (precedence, not mirror order)", filepath.Base(destPath), markerTier, tier)
			}
			return skillOutcomeShadowed, nil
		}
		_ = os.Chmod(destPath, destInfo.Mode().Perm())
		if err := overwriteFile(srcPath, destPath); err != nil {
			return skillOutcomeShadowed, err
		}
		// Same ordering rationale as the copyFile branch: mark ownership
		// before the refresh marker so a SIGKILL between the two leaves a
		// safe state (marker missing → pruner skips) instead of an unsafe
		// one (marker present without provenance → next UpToDate cannot
		// tell iterion's file from an identical operator copy).
		markIterionWrote(markerPath)
		if err := writeMarker(markerPath, srcHash, tier); err != nil {
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
	_, err = mirrorFileSkill(dest, markerDir, srcPath, name, skillTierBundle, logger)
	return err
}

// skillDestDirForm computes the destination for a FLAT source skill file so it
// lands in the DIRECTORY form Claude Code's Skill tool requires:
// <dest>/<stem>/SKILL.md. A flat <dest>/<name>.md is NOT discovered as a skill
// by claude_code — only the directory form is (Agent Skills spec); claw's
// skill_manager discovers BOTH the flat and directory forms, so the directory
// form satisfies both backends. stem drops a trailing ".md" case-insensitively
// ("whats-next.md" and "Whats-Next.MD" both stem "whats-next" for the dir form
// and "Whats-Next" for the flat alias — collectSkillFiles ACCEPTS .MD via
// EqualFold and this function refused to disambiguate before, letting stem ==
// name make the flat alias collide with the dir on the same path and error
// with "is a directory"). The marker keys on "<stem>.SKILL.md.sha256" (same
// scheme as mirrorLibrarySkills). A name that does not end in .md at all is
// refused — the caller must skip that entry rather than let its flat alias and
// dir form resolve to the same path.
func skillDestDirForm(dest, markerDir, srcName string) (skillDir, destPath, markerPath string, err error) {
	if !hasMarkdownSuffix(srcName) {
		return "", "", "", &invalidSkillNameError{name: srcName, reason: "expected an .md extension (case-insensitive)"}
	}
	stem := srcName[:len(srcName)-len(".md")]
	if stem == "" || stem == "." || stem == ".." || strings.ContainsAny(stem, "/\\") {
		return "", "", "", &invalidSkillNameError{name: srcName, reason: "empty or path-escape stem"}
	}
	skillDir = filepath.Join(dest, stem)
	destPath = filepath.Join(skillDir, "SKILL.md")
	markerPath = filepath.Join(markerDir, stem+".SKILL.md.sha256")
	return skillDir, destPath, markerPath, nil
}

// invalidSkillNameError is the sentinel skillDestDirForm returns when a
// contribution name is structurally unfit for the two-form mirror — no `.md`
// extension (case-insensitive), or a stem that would escape the skills dir
// or collide the flat alias with the directory form. It is a VALIDATION
// error, deliberately distinct from an I/O error (which the mirror pass
// must NOT swallow — see isSkillValidationError):
//
//   - validation: soft. The one bad contribution is skipped with a WARN
//     naming it; the pass continues so an unrelated plugin's skills still
//     land. This is the round-2 medium (R6178bd/Rda9e59) `Deploy.MD` was
//     opening — one malformed manifest entry used to abort every OTHER
//     plugin's contribution.
//
//   - I/O (ENOSPC / EACCES / ENOTDIR when the target checkout planted a
//     plain file where the skill dir goes / a copyFile that refused / a
//     writeMarker that failed): FATAL. A run whose `.bot` explicitly
//     declares a skill it cannot mirror must NOT report success without
//     it. That is the house doctrine — no silent fallback — and is why
//     the two classes need to stay separate.
type invalidSkillNameError struct {
	name   string
	reason string
}

func (e *invalidSkillNameError) Error() string {
	return fmt.Sprintf("runtime/bundle: invalid skill file name %q: %s", e.name, e.reason)
}

// isSkillValidationError reports whether err (or any wrapped err) is the
// validation sentinel. Callers use this to split soft (skip) from fatal
// (return): the sole shared predicate, so the five mirror sites cannot
// drift on which error class they treat as which.
func isSkillValidationError(err error) bool {
	var v *invalidSkillNameError
	return errors.As(err, &v)
}

// hasMarkdownSuffix reports whether srcName ends in ".md" case-insensitively.
// collectSkillFiles accepts either casing (via strings.EqualFold on filepath.Ext),
// so every mirror site that names a file by its base must accept both here or
// leak a "is a directory" error when the dir and flat forms collide on stem ==
// name.
func hasMarkdownSuffix(srcName string) bool {
	if len(srcName) < len(".md") {
		return false
	}
	return strings.EqualFold(srcName[len(srcName)-len(".md"):], ".md")
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
func mirrorFileSkill(dest, markerDir, srcPath, name string, tier skillTier, logger *iterlog.Logger) (skillReconcileOutcome, error) {
	skillDir, skillDest, markerPath, err := skillDestDirForm(dest, markerDir, name)
	if err != nil {
		return skillOutcomeShadowed, err
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return skillOutcomeShadowed, fmt.Errorf("runtime/bundle: mkdir %s: %w", skillDir, err)
	}
	outcome, err := reconcileSkillFile(srcPath, skillDest, markerPath, tier, logger)
	if err != nil {
		return outcome, err
	}
	flatDest := filepath.Join(dest, name)
	flatMarker := filepath.Join(markerDir, name+".sha256")
	if _, ferr := reconcileSkillFile(srcPath, flatDest, flatMarker, tier, logger); ferr != nil {
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
			// Directory skills carry no marker, so ownership is decided by
			// CONTENT: an existing destination identical to the source is ours
			// (we wrote it on an earlier pass — this runs again on every
			// resume against the same worktree), anything else is the
			// workspace's own and shadows.
			//
			// Content is the right oracle rather than a marker precisely
			// because the workspace is untrusted: the only way a repo can be
			// reported as owned is by shipping a byte-identical copy of
			// iterion's own skill, which is not an attack.
			if _, err := os.Stat(destPath); err == nil {
				same, cmpErr := sameTree(srcPath, destPath)
				if cmpErr == nil && same {
					owned = append(owned, destPath)
					uptodate++
					continue
				}
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
		// the mirror errors with "is a directory". Case-insensitive to
		// match collectSkillFiles' EqualFold on `.md` — Deploy.MD is a
		// skill too.
		if !hasMarkdownSuffix(name) {
			continue
		}
		// File skill → both the directory form (native Skill-tool discovery)
		// and the flat <name>.md alias (prompt Reads). Shared with
		// MirrorSingleSkill via mirrorFileSkill.
		outcome, err := mirrorFileSkill(dest, markerDir, srcPath, name, skillTierBundle, logger)
		if err != nil {
			// Split soft vs fatal: a validation error (a name
			// skillDestDirForm refuses) skips ONE entry so the rest of
			// the bundle still lands; an I/O failure (ENOSPC / ENOTDIR
			// when the checkout planted a plain file at the skill dir
			// path / a copyFile that refused) is FATAL — a run whose
			// bot declares a skill that could not be mirrored must not
			// silently proceed without it. Same predicate everywhere:
			// isSkillValidationError.
			if isSkillValidationError(err) {
				if logger != nil {
					logger.Warn("runtime/bundle: skipping skill %q (validation): %v", name, err)
				}
				shadowed++
				continue
			}
			return nil, fmt.Errorf("runtime/bundle: skill %q: %w", name, err)
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
			// The FILE, not its directory. A flat source writes exactly
			// <stem>/SKILL.md, and MkdirAll happily succeeds on a directory the
			// checkout already shipped — so claiming <stem>/ would report a
			// directory the target repo pre-populated, and any .md it planted
			// there would ride along wherever this list is trusted. Naming the
			// one file we wrote cannot carry a sibling. Compute the stem
			// through skillDestDirForm so its case-insensitive strip and the
			// owned path agree — a bare TrimSuffix leaves "Deploy.MD" ->
			// "Deploy.MD" while mirrorFileSkill actually wrote under
			// "Deploy/SKILL.md", handing the executor a path that never
			// existed on disk.
			_, ownedPath, _, dfErr := skillDestDirForm(dest, markerDir, name)
			if dfErr == nil {
				owned = append(owned, ownedPath)
			}
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

// tierSidecarSuffix is the per-skill sidecar recording which mirror TIER
// wrote the marker. A SIDECAR, deliberately: the marker file itself stays a
// bare sha256 hex so an OLDER binary (a rolled-back deploy) still reads it
// and its refresh logic keeps working — encoding the tier into the marker's
// own grammar was a one-way door that turned every mirrored skill into a
// permanent shadow after a rollback.
//
// Sidecars are wiped at the START of each run's mirror pass
// (ClearMirroredTierMarkers): the tier arbitrates collisions WITHIN one pass
// (bundle > plugin > library regardless of mirror order), never across
// runs — a persisted tier would let a bundle that no longer ships a skill
// lock a library skill of the same name out forever.
//
// The wipe also doubles as the pruner's freshness signal: every touched
// mirror rewrites the sidecar via writeMarker, so a `.sha256` marker whose
// tier sidecar is MISSING at pass end names a destination no source
// produced this run — a candidate orphan (pruneWorkspaceMirror confirms it
// is iterion's before removing it via the iterion-wrote sidecar and a
// content hash match).
const tierSidecarSuffix = ".tier"

// iterionWroteSidecarSuffix records that iterion ACTIVELY WROTE the mirrored
// file at some run — Mirrored via copyFile, or Refreshed via overwriteFile.
// It is NOT created by the UpToDate branch that adopts an operator's byte-
// identical file: hash(dest) == hash(src) on a first mirror is entirely
// consistent with an operator copy-pasting an example verbatim, and the
// tier marker alone (which UpToDate rewrites) does not distinguish "iterion
// wrote it" from "iterion adopted an identical operator file".
//
// The pruner needs the stronger fact — "iterion put those bytes there" —
// before it deletes anything, so a coincidence-adopted file whose source is
// later removed does not silently disappear. Persistent across runs (unlike
// the tier sidecar), because the "we wrote this once" fact must survive the
// per-pass wipe.
const iterionWroteSidecarSuffix = ".iterion-wrote"

// readMarker returns the sha256 hex + owning tier for path, or ("", "") on
// any error (missing file, unreadable, empty). Marker absence is a benign
// signal — we treat the existing skill as user-owned and shadow. A marker
// without a sidecar (legacy, or a pre-wipe run) reads with an empty tier
// (lowest rank — the historical refresh behaviour).
func readMarker(path string) (hash string, tier skillTier) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	// Cut tolerates a transitional "hash tier" single-file form; sha256 hex
	// never contains a space, so a bare legacy marker passes through whole.
	h, inline, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	tier = skillTier(inline)
	if tb, terr := os.ReadFile(path + tierSidecarSuffix); terr == nil {
		tier = skillTier(strings.TrimSpace(string(tb)))
	}
	return h, tier
}

// writeMarker records the bare hash at path and the owning tier in the
// sidecar. Best-effort: failures don't abort the mirror (we've already
// copied the content; a missing marker just means the next run shadows
// instead of refreshes).
func writeMarker(path, hash string, tier skillTier) error {
	if err := os.WriteFile(path, []byte(hash), 0o644); err != nil {
		return fmt.Errorf("runtime/bundle: write marker %s: %w", path, err)
	}
	if tier != "" {
		if err := os.WriteFile(path+tierSidecarSuffix, []byte(tier), 0o644); err != nil {
			return fmt.Errorf("runtime/bundle: write tier sidecar %s: %w", path, err)
		}
	} else {
		_ = os.Remove(path + tierSidecarSuffix)
	}
	return nil
}

// markIterionWrote drops the sticky "iterion actively wrote this file"
// sidecar next to the marker. Best-effort — losing the sidecar only
// downgrades the pruner's certainty about this destination on some future
// run (which is exactly what safety requires), never fails the mirror.
// Called from reconcileSkillFile's Mirrored / Refreshed branches. NOT
// called from UpToDate: an adopted-identical-operator-file must not be
// pruneable, ever.
func markIterionWrote(markerPath string) {
	_ = os.WriteFile(markerPath+iterionWroteSidecarSuffix, nil, 0o644)
}

// iterionWroteFile reports whether the marker at path was actually written
// by iterion (Mirrored/Refreshed) at some past run. Bare stat — the
// sidecar's content is deliberately empty; presence is the whole signal.
func iterionWroteFile(markerPath string) bool {
	if _, err := os.Stat(markerPath + iterionWroteSidecarSuffix); err != nil {
		return false
	}
	return true
}

// mirrorKindDirs enumerates the three .claude/ leaf directories the runtime
// mirrors write into. Kept alongside the marker code because both the
// tier-sidecar wipe (ClearMirroredTierMarkers) and the orphan pruner
// (pruneWorkspaceMirror) walk them in lockstep — one canonical list keeps a
// new kind from being added to one function and forgotten in the other.
var mirrorKindDirs = []string{"skills", "commands", "agents"}

// ClearMirroredTierMarkers removes every tier sidecar under each of the
// workspace's mirror marker dirs (skills / commands / agents) — called once
// at the start of a run's mirror sequence so tier precedence is scoped to
// THAT pass, and so the pruner can tell a fresh mirror (sidecar rewritten
// by writeMarker) from an orphan (sidecar still absent at pass end).
// Best-effort; a missing dir is simply a workspace that was never mirrored.
func ClearMirroredTierMarkers(workDir string) {
	for _, kind := range mirrorKindDirs {
		markerDir := filepath.Join(workDir, ".claude", kind, bundleMirrorMarkerDir)
		entries, err := os.ReadDir(markerDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), tierSidecarSuffix) {
				_ = os.Remove(filepath.Join(markerDir, e.Name()))
			}
		}
	}
}

// pruneWorkspaceMirror removes orphan mirror files — those iterion wrote
// during a previous pass and whose source no longer produces them (renamed,
// removed, or disabled upstream). The safe predicate is the existing marker
// scheme:
//
//  1. Post-mirror, every touched marker has a fresh .tier sidecar (writeMarker
//     writes both, and ClearMirroredTierMarkers cleared the sidecars at the
//     start of the pass). A .sha256 marker whose sidecar is MISSING at prune
//     time names a destination no source produced this run.
//  2. A file whose content still hashes to its .sha256 marker was written by
//     iterion and has not been edited by the operator — same test
//     reconcileSkillFile uses to decide refresh-vs-shadow.
//
// Combining the two: an orphan is a marker with no fresh sidecar AND a
// destination file whose hash matches the marker. Those are the only files
// pruned; files with no marker, or diverged content, are the operator's and
// stay.
//
// isWorktreeOwned gates the sweep: pruning runs by default only for a
// run-owned worktree (`worktree: auto`), where a stale mirror file has no
// operator interpretation. In-place runs against the operator's own checkout
// keep every mirror file — an orphan there costs one unused file; a false
// positive costs an operator's edit. The escape hatch is a load-bearing
// limit, greppable and opt-in: ITERION_PRUNE_MIRROR_IN_CHECKOUT=1 (see
// CLAUDE.md philosophy #1).
//
// Best-effort throughout: a prune failure is logged but never fails a run.
// The sweep walks all three mirror kind dirs (skills / commands / agents).
func pruneWorkspaceMirror(workDir string, isWorktreeOwned bool, logger *iterlog.Logger) {
	if workDir == "" {
		return
	}
	if !isWorktreeOwned && !envOptIn(os.Getenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT")) {
		return
	}
	pruned := 0
	for _, kind := range mirrorKindDirs {
		kindDir := filepath.Join(workDir, ".claude", kind)
		markerDir := filepath.Join(kindDir, bundleMirrorMarkerDir)
		entries, err := os.ReadDir(markerDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".sha256") {
				continue
			}
			markerPath := filepath.Join(markerDir, name)
			// Freshness signal: touched this pass iff its .tier sidecar exists.
			// ClearMirroredTierMarkers wiped every sidecar at pass start, and
			// writeMarker rewrites it on every touch — mirrored, up-to-date,
			// refreshed — but not on shadow, which is where a stale marker
			// against an operator-edited file also lands.
			if _, err := os.Stat(markerPath + tierSidecarSuffix); err == nil {
				continue
			}
			destPath := destPathFromMarkerName(kindDir, name)
			if destPath == "" {
				continue
			}
			destInfo, statErr := os.Stat(destPath)
			if os.IsNotExist(statErr) {
				// Destination is gone already (operator deleted it, or a
				// prior prune ran). The marker is bare bookkeeping — drop
				// it and any companion sidecars so `.iterion-managed/`
				// does not accumulate forever.
				_ = os.Remove(markerPath)
				_ = os.Remove(markerPath + iterionWroteSidecarSuffix)
				continue
			}
			if statErr != nil {
				continue
			}
			if destInfo.IsDir() {
				// Only files carry the mirror contract; a directory here is
				// not something the mirror shipped through reconcileSkillFile.
				continue
			}
			// Refuse to prune anything iterion did not ACTIVELY WRITE. The
			// tier sidecar's presence alone would count an UpToDate
			// adoption as ours: reconcileSkillFile's UpToDate branch
			// rewrites the marker to update the tier when hash(dest) ==
			// hash(src), which is entirely consistent with an operator
			// copy-pasting the source into their workspace before iterion
			// ever ran there. The iterion-wrote sidecar is created only
			// by copyFile / overwriteFile paths (Mirrored / Refreshed);
			// its absence is a hard "not ours".
			if !iterionWroteFile(markerPath) {
				continue
			}
			markerHash, _ := readMarker(markerPath)
			if markerHash == "" {
				continue
			}
			destHash, err := hashFile(destPath)
			if err != nil {
				continue
			}
			if destHash != markerHash {
				// Operator-edited: the shadow policy owns this file, leave
				// it alone. The marker stays too — pruning it would flip
				// next-run's semantics for the case where an operator
				// restores iterion's original content (marker match →
				// refresh → the restore is respected as still-ours, which
				// is the intended behaviour).
				continue
			}
			// Orphan confirmed: iterion actively wrote it (iterion-wrote
			// sidecar present), this pass didn't refresh it (tier sidecar
			// missing), the file still matches the marker (operator did
			// not edit). Prune all three.
			if err := os.Remove(destPath); err == nil {
				_ = os.Remove(markerPath)
				_ = os.Remove(markerPath + iterionWroteSidecarSuffix)
				// The prior directory-form of a skill leaves an empty
				// <stem>/ around after we drop SKILL.md. It was ours to
				// begin with — drop it too, best-effort (RemoveEmpty style).
				if strings.HasSuffix(destPath, string(filepath.Separator)+"SKILL.md") {
					_ = os.Remove(filepath.Dir(destPath))
				}
				pruned++
			}
		}
	}
	if logger != nil && pruned > 0 {
		logger.Info("workspace mirror: pruned %d orphan file(s) from %s", pruned, filepath.Join(workDir, ".claude"))
	}
}

// destPathFromMarkerName inverts writeMarker's naming for the pruner. Two
// shapes the pruner encounters:
//
//   - "<name>.md.sha256" → "<name>.md" — the FLAT alias every kind uses,
//     and the ONE shape a command/agent takes.
//   - "<stem>.SKILL.md.sha256" → "<stem>/SKILL.md" — the DIRECTORY form
//     used by skill tiers (the shape claude_code's Skill tool discovers).
//
// The two grammars overlap on a source file literally named "foo.SKILL.md":
// its FLAT-alias marker is "foo.SKILL.md.sha256", which the DIRECTORY-form
// pattern also matches. We disambiguate by checking the workspace: if the
// directory-form path exists on disk we return it; otherwise we fall back
// to the flat interpretation (the source was called foo.SKILL.md, its
// flat alias is at kindDir/foo.SKILL.md). A marker name that fits neither
// grammar is skipped (unknown grammar — leave it).
func destPathFromMarkerName(kindDir, markerName string) string {
	if strings.HasSuffix(markerName, ".SKILL.md.sha256") {
		stem := strings.TrimSuffix(markerName, ".SKILL.md.sha256")
		if stem == "" || stem == "." || stem == ".." || strings.ContainsAny(stem, "/\\") {
			// Fall through to the flat interpretation — a marker whose
			// stem is invalid for the directory form may still be a legit
			// flat marker for a name ending in `.SKILL.md`.
		} else {
			dirForm := filepath.Join(kindDir, stem, "SKILL.md")
			if _, err := os.Stat(dirForm); err == nil {
				return dirForm
			}
			// Directory form is not on disk — was the source a file
			// literally named "<stem>.SKILL.md" whose flat alias lives at
			// kindDir/<stem>.SKILL.md? If so, its marker is the same as
			// the directory-form marker for kindDir/<name-without-.md>.
			flat := filepath.Join(kindDir, strings.TrimSuffix(markerName, ".sha256"))
			if _, err := os.Stat(flat); err == nil {
				return flat
			}
			// Neither exists on disk. Prefer the directory-form path for
			// stale-marker cleanup (kills the more common ancestor —
			// `<stem>/SKILL.md` — first; a stray "<stem>.SKILL.md" flat
			// alias with no dest already dropped by the missing-dest
			// branch).
			return dirForm
		}
	}
	if strings.HasSuffix(markerName, ".md.sha256") {
		name := strings.TrimSuffix(markerName, ".sha256")
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			return ""
		}
		return filepath.Join(kindDir, name)
	}
	return ""
}

// envOptIn parses an operator-facing boolean env value. Accepts the common
// forms so an operator setting ITERION_PRUNE_MIRROR_IN_CHECKOUT to "true"
// or "yes" is not silently ignored. Anything else — empty, "0", "no",
// arbitrary text — reads as opt-OUT.
func envOptIn(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
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

// sameTree reports whether dst holds exactly the files of src, with identical
// contents. It is how a directory-form skill's ownership is decided on a
// re-mirror, since those carry no marker: dst is allowed to be a superset in
// no direction — an extra file on either side means the workspace has its own
// version of this skill.
//
// Files only, and no symlink following: the mirror writes plain files (symlinks
// break under the sandbox bind-mount), so anything else here is not ours.
func sameTree(src, dst string) (bool, error) {
	srcFiles, err := treeFiles(src)
	if err != nil {
		return false, err
	}
	dstFiles, err := treeFiles(dst)
	if err != nil {
		return false, err
	}
	if len(srcFiles) != len(dstFiles) {
		return false, nil
	}
	for rel, srcPath := range srcFiles {
		dstPath, ok := dstFiles[rel]
		if !ok {
			return false, nil
		}
		// An empty path marks an irregular entry (see treeFiles): not something
		// the mirror writes, so the tree is not ours.
		if srcPath == "" || dstPath == "" {
			return false, nil
		}
		a, err := os.ReadFile(srcPath) // #nosec G304 — walked from the bundle's own skills dir.
		if err != nil {
			return false, err
		}
		b, err := os.ReadFile(dstPath) // #nosec G304 — walked from the mirror destination.
		if err != nil {
			return false, err
		}
		if !bytes.Equal(a, b) {
			return false, nil
		}
	}
	return true, nil
}

// treeFiles maps each file under root to its full path, keyed by the path
// relative to root. An IRREGULAR entry is recorded with an empty path so it
// still COUNTS toward the comparison.
//
// That distinction is the point. The mirror only ever writes plain files
// (copyDir dereferences), so a symlink here means the tree is not ours — and
// simply skipping irregular entries made the invariant falsifiable by exactly
// the untrusted input it guards against: WalkDir does not descend into a
// symlinked directory, so a checkout could add symlinks beside a byte-identical
// SKILL.md and still be reported as iterion-owned.
func treeFiles(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			out[rel] = ""
			return nil
		}
		out[rel] = path
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
