package cli

import (
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/store"
)

// storeAnchorDir returns the directory a command should hand to
// store.ResolveStoreDir.
//
// The working directory wins. A .bot that lives inside the project it drives
// is the common case, and anchoring on the bot's own directory keyed the store
// on that subdirectory: `iterion run project/bots/x/main.bot` wrote to
// ~/.iterion/projects/<project-bots-x-key>/ while resume, inspect, issue,
// dispatch and the studio all resolved <project>/.iterion. Launching worked
// and every follow-up reported "run not found".
//
// The bot's directory stays as the fallback for the case the working
// directory is genuinely unavailable, and for an empty iterFile the caller
// still gets a usable anchor.
func storeAnchorDir(iterFile string) string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	if iterFile == "" {
		return ""
	}
	return filepath.Dir(iterFile)
}

// runStoreDir resolves the store a run is launched into, resumed from, and
// gated against. Every one of those three must agree: when the schedule gate
// listed one store while the tick wrote to another, overlap protection silently
// stopped working. Going through one function makes that structural instead of
// three call sites that happen to match.
func runStoreDir(iterFile, override string) string {
	return store.ResolveStoreDir(storeAnchorDir(iterFile), override)
}

// legacyRunStoreDir returns the pre-cwd-anchoring store that holds runID, or ""
// when there is nothing to fall back to.
//
// Runs launched before the anchor moved live under the store keyed on the
// .bot's directory. Resume must keep finding them, otherwise upgrading orphans
// every paused / failed_resumable run in flight and the operator gets exactly
// the "run not found" this change set out to remove. Only consulted when the
// caller passed no explicit --store-dir and the current store does not hold the
// run.
func legacyRunStoreDir(iterFile, override, resolved, runID string) string {
	if override != "" || iterFile == "" || runID == "" {
		return ""
	}
	legacy := store.ResolveStoreDir(filepath.Dir(iterFile), "")
	if legacy == resolved || !runStoreHas(legacy, runID) {
		return ""
	}
	return legacy
}

func runStoreHas(storeDir, runID string) bool {
	if storeDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(storeDir, "runs", runID, "run.json"))
	return err == nil
}
