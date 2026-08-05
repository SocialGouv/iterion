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
	if len(paths) == 0 {
		return r.FilePath
	}
	// BotID first when present, then the bot name carried by the path itself.
	// A studio launch persists file_path WITHOUT bot_id, so keying only on
	// BotID silently no-ops for exactly the runs this exists to fix.
	for _, name := range []string{r.BotID, botNameFromPath(r.FilePath)} {
		if name == "" {
			continue
		}
		if main, err := botregistry.ResolveBotPath(name, paths); err == nil {
			return main
		}
	}
	return r.FilePath
}

// botNameFromPath extracts the bot name from a catalog-relative workflow path
// ("bots/app-dev/main.bot" -> "app-dev"), i.e. the parent directory of the
// workflow file. Returns "" when the path has no parent directory to name.
func botNameFromPath(p string) string {
	dir := filepath.Dir(filepath.ToSlash(p))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	return filepath.Base(dir)
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

// BotIDForRun resolves the bot identity a run's bot-scoped memory should be
// keyed on, matching what the LAUNCH used.
//
// The identity a run carries depends on how it was launched, and only cloud
// persists it:
//   - cloud stamps BotID on the run document (CreateQueuedRun);
//   - a studio launch passes the catalog bot id to the executor but persists
//     nothing;
//   - `iterion run` passes nothing, and the executor falls back to the
//     workflow's own name.
//
// A resume rebuilds the executor from scratch, so without this it inherits the
// workflow-name fallback — and for a bundle whose bot id and workflow name
// differ (`whats-next` vs `whats_next`, the usual shape) the resumed nodes read
// an EMPTY memory and write their notes into a second space nothing will read
// again.
//
// The bundle layout is what closes the gap: `<id>/main.bot` is the shape
// botregistry discovers, and `<id>` is precisely the id the studio launched
// with. A standalone `.bot` deliberately resolves to "" so the executor keeps
// its workflow-name fallback — which is what a CLI launch of that same file
// used, so launch and resume still agree.
func BotIDForRun(r *store.Run) string {
	if r == nil {
		return ""
	}
	return ResolveBotID(r.BotID, r.BundleName, r.FilePath)
}

// ResolveBotID is THE rule, and every surface that starts or resumes a run
// applies it: an explicit id first, then the bundle's own declared name, then
// the bundle directory a path names, then nothing — which lets the executor
// fall back to the workflow's name.
//
// The bundle NAME has to come before the path, because a `.botz` archive is
// extracted into a cache slot named after its CONTENT HASH. Deriving the
// identity from that path keys the bot's memory on the hash: the same bot
// answers to a different name than in its directory form, and every edit to
// the bundle — a comment, a version bump — mints a fresh hash and orphans
// everything it had learned. `manifest.yaml`'s `name` is the id that does not
// move, and the run already persists it as BundleName.
func ResolveBotID(explicitID, bundleName, filePath string) string {
	if explicitID != "" {
		return explicitID
	}
	if bundleName != "" {
		return bundleName
	}
	return BotIDForPath(filePath)
}

// BotIDForPath derives a bot identity from a workflow path, or "" when the
// path does not identify one.
//
// `<id>/main.bot` is the bundle layout botregistry discovers, and `<id>` is
// exactly the catalog id the studio launches with — so deriving it here makes
// the CLI, a detached subprocess and an in-process studio launch key the same
// bot's memory identically, instead of the first two falling back to the
// workflow's own name (`whats_next` where the studio said `whats-next`).
//
// A standalone `.bot` returns "" on purpose. It has no identity beyond its
// workflow name, and inventing one from its parent directory would key the
// memory on wherever the file happens to sit.
func BotIDForPath(filePath string) string {
	if !strings.EqualFold(filepath.Base(filePath), "main.bot") {
		return ""
	}
	return botNameFromPath(filePath)
}
