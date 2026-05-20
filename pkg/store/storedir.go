package store

import (
	"os"
	"path/filepath"
	"strings"
)

// StoreDirName is the conventional directory name for an iterion run
// store, both as the project-local opt-in marker and as the leaf of
// the global iterion data root (~/.iterion).
const StoreDirName = ".iterion"

// ResolveStoreDir picks the run-store directory shared by the CLI and
// the studio.
//
// Resolution order:
//  1. Explicit override (--store-dir, cfg.StoreDir) wins.
//  2. Project-local opt-in: if <start>/.iterion exists as a directory,
//     use it. Created either by an explicit `iterion init` or by hand
//     when the operator wants the run state versioned with (or merely
//     adjacent to) the repo.
//  3. Default: a per-project subdir of the user's global iterion data
//     dir — ~/.iterion/projects/<workdir-key>/, where <workdir-key>
//     is a deterministic encoding of the absolute workdir path. This
//     mirrors Claude Code's ~/.claude/projects/<key>/ layout: every
//     workdir gets an isolated slot, no cross-project leakage, no
//     pollution inside repos by default.
//
// Why the global default (rather than the legacy walk-up): a stray
// .iterion in any ancestor (typically ~/.iterion left over from
// running iterion once from $HOME) used to be picked up by every
// project nested under it, silently sharing run state across
// unrelated projects. The keyed layout removes that footgun by
// design.
//
// Backward compatibility: users who already have <repo>/.iterion
// keep the project-local store via the opt-in branch (step 2). Users
// who relied on the legacy walk-up to a shared parent .iterion need
// to either move/copy that directory under each project or set
// --store-dir / cfg.StoreDir explicitly.
//
// $ITERION_HOME overrides the global root for operators who want
// runs to live somewhere other than ~/.iterion (e.g. on a different
// disk, or under XDG_DATA_HOME).
func ResolveStoreDir(start, override string) string {
	if override != "" {
		return override
	}
	if start == "" {
		return StoreDirName
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		return filepath.Join(start, StoreDirName)
	}

	// Project-local opt-in. To distinguish an iterion-managed store
	// from a stray `.iterion/` that some workspace tool created (e.g.
	// the whats-next bot's emit_action writing `.iterion/plans/*.md`),
	// require a marker subdir / sentinel file that the RunStore itself
	// would have written. Without this check, an LLM-created
	// `.iterion/plans/` hijacks every subsequent CLI/daemon call and
	// the operator sees an empty board / no runs. F-NEW-10.
	candidate := filepath.Join(abs, StoreDirName)
	if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
		if isManagedStore(candidate) {
			return candidate
		}
	}

	// Default: per-project slot under the global iterion data dir.
	return globalProjectStoreDir(abs)
}

// isManagedStore reports whether candidate looks like an
// iterion-managed run store, as opposed to a stray `.iterion/` left
// behind by a workspace tool. Recognises:
//   - <candidate>/runs/         (created by RunStore on first run)
//   - <candidate>/dispatcher/   (created by NativeStore on first board op)
//   - <candidate>/.iterion-store (sentinel file, write-on-init for forward
//     compatibility — RunStore can touch this on init to opt-in even when
//     no runs exist yet)
//
// Any of the three is enough; the OR semantics keep legacy stores
// (which only have runs/) working alongside dispatcher-only stores
// (which have dispatcher/ but no runs yet).
func isManagedStore(candidate string) bool {
	for _, marker := range []string{"runs", "dispatcher", ".iterion-store"} {
		if _, err := os.Stat(filepath.Join(candidate, marker)); err == nil {
			return true
		}
	}
	return false
}

// globalProjectStoreDir returns the per-project subdir of the global
// iterion data dir for absWorkDir. Layout: <iterionHome>/projects/<key>/.
func globalProjectStoreDir(absWorkDir string) string {
	return filepath.Join(GlobalIterionDataDir(), "projects", EncodeWorkDirKey(absWorkDir))
}

// GlobalIterionDataDir locates the user's iterion data dir, checked
// in this order:
//  1. $ITERION_HOME — operator escape hatch
//  2. ~/.iterion    — matches the convention iterion already uses
//  3. <tmp>/iterion-data — last-resort fallback when the user has no
//     home dir (CI containers without HOME, etc.)
//
// Trailing path separators are normalised away so callers can join
// safely.
func GlobalIterionDataDir() string {
	if dir := strings.TrimRight(os.Getenv("ITERION_HOME"), string(filepath.Separator)); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, StoreDirName)
	}
	return filepath.Join(os.TempDir(), "iterion-data")
}

// EncodeWorkDirKey produces a deterministic, filesystem-safe key
// from an absolute workdir path. Path separators are replaced with
// "-"; on Windows the drive ":" is also collapsed; the result always
// starts with "-" so a relative-looking input still produces a
// distinct key (and so the leading separator on Unix is preserved
// rather than dropped silently).
//
//	Unix:    "/home/jo/lab/devthefuture/modjo"
//	      -> "-home-jo-lab-devthefuture-modjo"
//
//	Windows: "C:\\foo\\bar"
//	      -> "-C-foo-bar"
//
// Different absolute paths therefore yield different keys, including
// when one project is a clone of another at a different location.
func EncodeWorkDirKey(absPath string) string {
	p := filepath.ToSlash(absPath)
	// Replace both separator flavours regardless of the runtime OS
	// so the key for a given path is the same whether iterion sees
	// it via a Windows-style or Unix-style spelling. ToSlash already
	// handles the host-OS case; the explicit backslash sweep covers
	// Unix hosts that happen to be passed a Windows path string
	// (rare, but cheap to be defensive about — keys are forever).
	p = strings.ReplaceAll(p, `\`, "-")
	p = strings.ReplaceAll(p, ":", "-")
	p = strings.ReplaceAll(p, "/", "-")
	if !strings.HasPrefix(p, "-") {
		p = "-" + p
	}
	return p
}
