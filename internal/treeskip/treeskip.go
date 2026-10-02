// Package treeskip names the trees this repository's generated artifacts
// never describe: vendored or installed third-party code, sibling
// worktrees, the engine's own run scratch, build output, and the
// documentation site's own machinery.
//
// One set, read by every generator. The names are a property of the
// REPOSITORY, not of one artifact, and a repository-wide fact held in two
// places is a repository-wide fact with two answers: the map and the graph
// carried byte-identical copies until one of them gained `.vitepress`, and
// nothing said so. A tree added here is skipped by every artifact at once.
//
// `testdata` is here for the same reason the Go toolchain ignores it: it is
// where a parser project keeps DELIBERATELY broken fixtures. A `.go` file
// that does not parse is a hard error in both generators, so without this
// entry a fixture nobody intended to compile turns the repository's required
// `test` check red.
package treeskip

import (
	"sort"
	"strings"
)

// dirs is the set itself. Unexported: a caller that could range over a
// mutable map could also write to it, and the whole point is one answer.
var dirs = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true, ".works": true,
	".repos": true, ".iterion": true, ".devbox": true, "graphify-out": true,
	".claude": true, ".task": true, "dist": true, ".pnpm-store": true,
	".vitepress": true, "testdata": true,
}

// Dir reports whether a directory of this name names a skipped tree. It is
// the question a walk asks about the directory it is standing in, and the
// answer is `filepath.SkipDir`.
func Dir(name string) bool { return dirs[name] }

// Path reports whether a slash-separated repository-relative path lies
// inside a skipped tree.
//
// A walk asks Dir about the directory it stands in and prunes. A path has no
// walk behind it — the one site that reached the filesystem by `os.Stat`
// instead of by a walk minted nodes for files the fingerprint never hashes,
// so deleting one left the cache serving a graph of a tree that no longer
// existed.
func Path(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if dirs[seg] {
			return true
		}
	}
	return false
}

// Names returns the skipped directory names, sorted. It exists so a guard
// can enumerate the set instead of restating it — a second literal list is
// the defect this package was extracted to remove.
func Names() []string {
	out := make([]string, 0, len(dirs))
	for name := range dirs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
