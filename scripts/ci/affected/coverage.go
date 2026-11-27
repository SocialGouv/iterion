package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// undeclared returns, for each scanned read the declarations do not cover,
// a line naming the package, the site and what is missing.
//
//   - Every package with a read outside its directory needs a `packages:`
//     entry, unless each of its reads falls under a global trigger.
//   - A file read must match one of the entry's globs (or a global one).
//   - A directory read must cover every tracked file under it that the
//     package does not own.
//   - A glob read must cover every tracked file the pattern matches, and
//     every one under a matching directory, that the package does not own.
//   - A root read (a root finder, or a climb to the root) needs the entry
//     alone: what a root-anchored test reads is the entry's word.
func undeclared(in *inputs, tracked []string, ev []evidence) []string {
	isGlobal := func(p string) bool {
		for _, r := range in.Global {
			if matchGlob(r.Pattern, p) {
				return true
			}
		}
		return false
	}
	var out []string
	for _, e := range ev {
		decl, ok := in.Packages[e.Pkg]
		covered := func(p string) bool {
			return isGlobal(p) || (ok && firstMatch(decl.Reads, p) != "")
		}
		switch {
		case e.Glob:
			var missing []string
			for _, t := range tracked {
				reads := matchGlob(e.Path, t) || (e.Dir && matchGlob(e.Path+"/**", t))
				if !reads {
					continue
				}
				if within(t, e.Pkg) || covered(t) {
					continue
				}
				missing = append(missing, t)
			}
			if len(missing) > 0 {
				out = append(out, fmt.Sprintf("%s: %s: %d tracked file(s) matching %s match no declared glob (first: %s)", e.Pkg, e, len(missing), e.Path, missing[0]))
			}
		case e.Kind == "root" || e.Path == ".":
			if !ok {
				out = append(out, fmt.Sprintf("%s: %s: reaches the repository root and has no `packages:` entry", e.Pkg, e))
			}
		case !e.Dir:
			// An untracked path never appears in a diff: nothing to select on.
			if !isTracked(tracked, e.Path) {
				continue
			}
			if !covered(e.Path) {
				out = append(out, fmt.Sprintf("%s: %s: no declared glob matches %s", e.Pkg, e, e.Path))
			}
		default:
			var missing []string
			for _, t := range filesUnder(tracked, e.Path) {
				if within(t, e.Pkg) || covered(t) {
					continue
				}
				missing = append(missing, t)
			}
			if len(missing) > 0 {
				out = append(out, fmt.Sprintf("%s: %s: %d tracked file(s) under %s/ match no declared glob (first: %s)", e.Pkg, e, len(missing), e.Path, missing[0]))
			}
		}
	}
	return out
}

func isTracked(tracked []string, p string) bool {
	i := sort.SearchStrings(tracked, p)
	return i < len(tracked) && tracked[i] == p
}

// filesUnder returns the tracked files at or below dir (sorted input).
func filesUnder(tracked []string, dir string) []string {
	prefix := dir + "/"
	i := sort.SearchStrings(tracked, prefix)
	var out []string
	for ; i < len(tracked) && strings.HasPrefix(tracked[i], prefix); i++ {
		out = append(out, tracked[i])
	}
	return out
}

// trackedFiles lists the tracked files of the repository at root, sorted.
func trackedFiles(ctx context.Context, root string) ([]string, error) {
	out, err := gitOut(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	sort.Strings(files)
	return files, nil
}
