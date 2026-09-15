package cli

import (
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/bots"
)

// ResolveRecipePath returns a real on-disk path for `path`, transparently
// falling back to the recipes shipped embedded in the binary when the
// requested path does not exist on disk. This makes commands like
//
//	iterion run feature-dev/main.bot
//	iterion run bots/whole-improve-loop/main.bot
//
// work from any working directory — the user does not have to
// explicitly point at `<repo>/examples/...`. Bundle directories
// (`examples/<name>/main.bot`) and `.botz` archives are NOT in the
// embed glob; resolve them by explicit path or by `iterion run
// <name>.botz` when packed adjacent to the source.
//
// Resolution order:
//  1. If the path exists on disk, return it as-is.
//  2. Otherwise, look up `path` in the embedded recipe FS. If found,
//     materialise the bot — its main and, for a bot in several files,
//     the fragments the main imports — into a stable per-user cache
//     directory and return the main's path there.
//  3. Otherwise, return the original `path` so callers surface the
//     usual "no such file" error to the user.
//
// We materialise to a real file rather than reading from embed.FS at
// each call site because the engine, parser, and several runtime
// helpers operate on real paths (worktree relative locations,
// file-watcher, sandbox bind-mounts). Materialisation keeps that
// contract intact at the cost of a tiny one-time write.
func ResolveRecipePath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	dst, err := bots.Materialize(embeddedRecipeCacheDir(), path)
	if err != nil {
		return path
	}
	return dst
}

// embeddedRecipeCacheDir is the stable per-user directory that holds
// materialised embedded recipes: the OS-defined user cache dir, so
// repeated runs hit the same path (idempotency, the engine's resume /
// worktree assumptions), or the temp dir when none is available. Nothing
// is created here — Materialize creates what it writes, so a name the
// binary does not carry leaves no directory behind.
func embeddedRecipeCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "iterion", "embedded-recipes")
}
