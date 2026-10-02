package delegate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// codexInstructionFiles are the operator-level instruction files codex reads
// from CODEX_HOME, whatever the working directory.
var codexInstructionFiles = []string{"AGENTS.md", "AGENTS.override.md"}

// codexOverlayDirName holds the per-spawn homes codexHomeWithoutInstructions
// builds inside the operator's own CODEX_HOME. Inside, so that a file codex
// replaces by rename (a refreshed auth.json) can be carried back with an
// atomic rename on the same filesystem.
const codexOverlayDirName = ".iterion-ambient"

// codexAmbientConfig is the `-c` part of the node's ambient-context policy
// (ADR-119): without the workspace, codex stops reading the repository's
// AGENTS.md files (measured on codex-cli 0.156.1).
func codexAmbientConfig(task Task) map[string]string {
	if task.AmbientContext.IncludesWorkspace() {
		return nil
	}
	return map[string]string{"project_doc_max_bytes": "0"}
}

// codexConfig is the one `-c` map of a spawn. codexsdk.WithConfig replaces
// rather than merges, so every key the engine sets is composed here.
func codexConfig(task Task, webSearch string) map[string]string {
	cfg := map[string]string{"web_search": webSearch}
	maps.Copy(cfg, codexAmbientConfig(task))
	return cfg
}

// codexSpawnEnv is the environment of a node's codex spawns: the credential
// environment, and — when the policy keeps the operator out — a CODEX_HOME
// that serves everything of the effective home except its instruction files.
// codex's project_doc_max_bytes only governs the repository's AGENTS.md; the
// home's own AGENTS.md loads regardless, so withholding it means moving the
// home. release must run once both passes are done.
func codexSpawnEnv(ctx context.Context, task Task) (env map[string]string, release func() error, err error) {
	env = codexCredEnvForCLI(ctx)
	noop := func() error { return nil }
	if task.AmbientContext.IncludesOperator() {
		return env, noop, nil
	}
	home := env["CODEX_HOME"]
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, herr := os.UserHomeDir()
		if herr != nil {
			return nil, nil, fmt.Errorf("delegate: codex: locate the operator's codex home: %w", herr)
		}
		home = filepath.Join(userHome, ".codex")
	}
	dir, release, err := codexHomeWithoutInstructions(home)
	if err != nil {
		return nil, nil, err
	}
	if dir == home {
		return env, release, nil
	}
	out := maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	out["CODEX_HOME"] = dir
	return out, release, nil
}

// codexHomeWithoutInstructions returns a directory that serves every entry of
// home through a symlink except codex's instruction files. When home holds no
// instruction file it returns home itself and builds nothing.
//
// Two of codex's writes need care. The sessions directory is created in home
// first: one codex created inside the overlay would vanish with it, and with
// it the session the formatting pass resumes. And a file codex replaces by
// rename (a refreshed auth.json) or creates becomes a regular entry of the
// overlay: release renames it back into home before removing the overlay, so
// a rotated refresh token is never lost.
func codexHomeWithoutInstructions(home string) (dir string, release func() error, err error) {
	noop := func() error { return nil }
	entries, err := os.ReadDir(home)
	if errors.Is(err, os.ErrNotExist) {
		return home, noop, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("delegate: codex: read the codex home %s: %w", home, err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return slices.Contains(codexInstructionFiles, e.Name()) }) {
		return home, noop, nil
	}
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		return "", nil, fmt.Errorf("delegate: codex: prepare the sessions directory: %w", err)
	}
	// Re-read: the listing above predates sessions/, which must be linked.
	if entries, err = os.ReadDir(home); err != nil {
		return "", nil, fmt.Errorf("delegate: codex: read the codex home %s: %w", home, err)
	}
	root := filepath.Join(home, codexOverlayDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, fmt.Errorf("delegate: codex: prepare the overlay root: %w", err)
	}
	// A process killed between the spawn and its release leaves its overlay
	// behind; sweep the stale siblings (older than a day — no live node runs
	// that long) with the same carry-back as a normal release, so a token a
	// crashed node refreshed is not lost with them.
	sweepStaleCodexOverlays(root)
	dir, err = os.MkdirTemp(root, "home-")
	if err != nil {
		return "", nil, fmt.Errorf("delegate: codex: create the overlay home: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == codexOverlayDirName || slices.Contains(codexInstructionFiles, name) {
			continue
		}
		if err := os.Symlink(filepath.Join(home, name), filepath.Join(dir, name)); err != nil {
			return "", nil, errors.Join(fmt.Errorf("delegate: codex: link %s into the overlay home: %w", name, err), releaseCodexOverlay(home, dir))
		}
	}
	return dir, func() error { return releaseCodexOverlay(home, dir) }, nil
}

// codexOverlayStaleAfter is the age at which a leftover overlay home counts
// as orphaned by a dead process and gets swept.
const codexOverlayStaleAfter = 24 * time.Hour

// sweepStaleCodexOverlays releases every overlay home under root whose
// directory mtime is older than codexOverlayStaleAfter — the residue of a
// process killed before its release. Each one goes through the same
// carry-back as a normal release, best-effort: a node that died left nothing
// to report the error to, and the directories it cannot carry stay for a
// human rather than being deleted in silence.
func sweepStaleCodexOverlays(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < codexOverlayStaleAfter {
			continue
		}
		_ = releaseCodexOverlay(filepath.Dir(root), dir)
	}
}

// releaseCodexOverlay carries back into home every entry codex created or
// replaced in the overlay, then removes the overlay, which by then holds only
// symlinks. A directory that already exists in home is left where it is and
// reported, rather than merged or lost in silence.
func releaseCodexOverlay(home, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("delegate: codex: read the overlay home %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		src, dst := filepath.Join(dir, e.Name()), filepath.Join(home, e.Name())
		if e.IsDir() {
			if _, statErr := os.Lstat(dst); statErr == nil {
				errs = append(errs, fmt.Errorf("delegate: codex: %s was created in the overlay home but already exists in %s; left in %s", e.Name(), home, src))
				continue
			}
		}
		if err := os.Rename(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("delegate: codex: carry %s back into %s: %w", e.Name(), home, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delegate: codex: remove the overlay home %s: %w", dir, err)
	}
	_ = os.Remove(filepath.Join(home, codexOverlayDirName)) // only succeeds once empty
	return nil
}
