package runview

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveWorkflowPath returns the path to compile a run's workflow from,
// falling back to the bot catalog when the persisted FilePath is not readable
// as-is.
//
// Why: a catalog-bot run persists a REPOSITORY-RELATIVE FilePath
// ("bots/app-dev/main.bot") — the path the launcher used. That resolves fine
// for a local run started from the repo root, but a cloud server pod has a
// different working directory and ships the catalog baked at
// $ITERION_BOTS_PATH (/opt/iterion/bots in the official image). The run itself
// is unaffected (it executes from the IR carried on the queue message), but the
// studio's workflow/diagram view compiles from this path and returned
// "cannot read file: open bots/app-dev/main.bot: no such file or directory"
// for every catalog-bot run in cloud mode.
//
// Resolution order: the persisted path when it exists (unchanged behaviour,
// including absolute paths and local runs), then the bot catalog keyed by the
// run's BotID. Returns the original path when nothing better is found, so the
// caller still surfaces the real compile error rather than a masked one.
func resolveWorkflowPath(r *store.Run) string {
	if r == nil || r.FilePath == "" {
		return ""
	}
	if _, err := os.Stat(r.FilePath); err == nil {
		return r.FilePath
	}
	paths := botsPathsFromEnv()
	if r.BotID == "" || len(paths) == 0 {
		return r.FilePath
	}
	if main, err := botregistry.ResolveBotPath(r.BotID, paths); err == nil {
		return main
	}
	return r.FilePath
}

// botsPathsFromEnv reads ITERION_BOTS_PATH — the colon-separated list of bot
// catalog roots the server/runner images set (mirrors cmd/iterion's helper,
// duplicated here so runview stays importable without the CLI).
func botsPathsFromEnv() []string {
	raw := os.Getenv("ITERION_BOTS_PATH")
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, string(os.PathListSeparator)) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.Clean(p))
		}
	}
	return out
}
