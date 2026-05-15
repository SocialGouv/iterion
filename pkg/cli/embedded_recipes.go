package cli

import (
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/examples"
)

// ResolveRecipePath returns a real on-disk path for `path`, transparently
// falling back to the recipes shipped embedded in the binary when the
// requested path does not exist on disk. This makes commands like
//
//	iterion run skill/minimal_linear.iter
//	iterion run bots/vibe_review_alternating.bot
//
// work from any working directory — the user does not have to
// explicitly point at `<repo>/examples/...`. Bundle directories
// (`examples/<name>/bot.iter`) and `.botz` archives are NOT in the
// embed glob; resolve them by explicit path or by `iterion run
// <name>.botz` when packed adjacent to the source.
//
// Resolution order:
//  1. If the path exists on disk, return it as-is.
//  2. Otherwise, look up `path` in the embedded recipe FS. If found,
//     materialise the content into a stable per-user cache directory
//     and return that path.
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
	data, ok := examples.Get(path)
	if !ok {
		return path
	}
	cacheDir, err := embeddedRecipeCacheDir()
	if err != nil {
		return path
	}
	dst := filepath.Join(cacheDir, path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return path
	}
	if existing, err := os.ReadFile(dst); err == nil && len(existing) == len(data) {
		return dst
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return path
	}
	return dst
}

// embeddedRecipeCacheDir picks a stable per-user directory to hold
// materialised embedded recipes. Prefers the OS-defined user cache
// dir so repeated runs hit the same path (good for idempotency and
// for the engine's resume / worktree assumptions); falls back to
// the temp dir when no cache dir is available.
func embeddedRecipeCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "iterion", "embedded-recipes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
