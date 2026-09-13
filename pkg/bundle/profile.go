package bundle

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// sourceState is what a reader could do with a subbot child's source.
type sourceState int

const (
	sourceRead    sourceState = iota // read: its profile counts
	sourceMissing                    // inside the bundle and not there: the compiler's to report
	sourceOutside                    // beyond what this reader may read: reported as unread
)

// MaxSyntaxProfile is the highest `dsl: N` profile declared by a bundle's
// executable sources: its main.bot and every child a `subbot source:`
// reaches from it, transitively. files maps a bundle-relative path to its
// content, as a bot source carries it. declaredBy names the files that
// declare the profile returned. unread names the children whose source lies
// beyond the files — a sibling bundle (`../other/main.bot`, a child shape
// the runner resolves within the bundle's collection), an absolute path —
// so a profile of 1 never passes for "checked" when part of the executable
// sources was not read; the caller reports them (C253). A child that is
// simply missing is the compiler's to report.
//
// Why the children count: a cloud runner receives the main workflow as an
// AST, but re-parses a subbot child as TEXT with its own binary, so a child
// written in a newer profile than the runner reads fails at its first parse
// — which is what a declared `requires.iterion` floor exists to refuse at
// admission instead.
func MaxSyntaxProfile(files map[string]string) (profile int, declaredBy, unread []string) {
	return maxSyntaxProfile(func(rel string) (string, sourceState) {
		if escapesBundle(rel) {
			return "", sourceOutside
		}
		src, ok := files[rel]
		if !ok {
			return "", sourceMissing
		}
		return src, sourceRead
	})
}

// MaxSyntaxProfileDir is MaxSyntaxProfile over a bundle directory on disk.
// It reads within the bundle's COLLECTION — the directory that holds the
// bundle — because a sibling bundle is a child shape the runner resolves
// there; a source that resolves beyond the collection, through `..` or a
// symlink, is not read and is reported as unread.
func MaxSyntaxProfileDir(dir string) (profile int, declaredBy, unread []string) {
	root := filepath.Clean(dir)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	collection := filepath.Dir(root)
	return maxSyntaxProfile(func(rel string) (string, sourceState) {
		if strings.HasPrefix(rel, "../../") || rel == "../.." {
			return "", sourceOutside // two levels up leaves the collection by construction
		}
		real, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if escapesBundle(rel) {
				return "", sourceOutside // a sibling that is not there: beyond the bundle either way
			}
			return "", sourceMissing
		}
		if !within(real, collection) {
			return "", sourceOutside
		}
		src, err := os.ReadFile(real)
		if err != nil {
			return "", sourceMissing
		}
		return string(src), sourceRead
	})
}

// escapesBundle reports a cleaned slash path that leaves the bundle root.
func escapesBundle(rel string) bool {
	return filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../")
}

// within reports whether path lies under root (both resolved).
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func maxSyntaxProfile(read func(rel string) (string, sourceState)) (int, []string, []string) {
	profile := 0
	var declaredBy, unread []string
	visited := map[string]bool{}
	var visit func(rel string)
	visit = func(rel string) {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if visited[rel] || rel == "." {
			return
		}
		visited[rel] = true
		src, state := read(rel)
		switch state {
		case sourceOutside:
			unread = append(unread, rel)
			return
		case sourceMissing:
			return
		}
		pr := parser.Parse(rel, src)
		p := pr.File.EffectiveProfile()
		switch {
		case p > profile:
			profile = p
			declaredBy = nil
			if p > 1 {
				declaredBy = []string{rel}
			}
		case p == profile && p > 1:
			declaredBy = append(declaredBy, rel)
		}
		base := filepath.ToSlash(filepath.Dir(rel))
		for _, sb := range pr.File.Subbots {
			if sb.Source == "" {
				continue
			}
			child := sb.Source
			if !filepath.IsAbs(child) && base != "." {
				child = filepath.ToSlash(filepath.Join(base, child))
			}
			visit(child)
		}
	}
	visit(MainBotFile)
	sort.Strings(declaredBy)
	sort.Strings(unread)
	return profile, declaredBy, unread
}

// ProfileFloor is what a manifest's `requires.iterion` says about the
// syntax profile a bundle's sources are written in.
type ProfileFloor struct {
	// Profile is the sources' highest profile (MaxSyntaxProfile).
	Profile int
	// Need is the release that reads it (parser.ProfileSince), "" when the
	// profile has none on record.
	Need string
	// Declared is the manifest's requires.iterion, trimmed; "" when absent.
	Declared string
	// OK: below profile 2 always; otherwise a declared floor that is ordered
	// at or above Need (or any declared floor, when Need is unknown).
	OK bool
}

// CheckProfileFloor holds a manifest's engine floor against the profile its
// sources are written in — one predicate for the author's `validate` (C252)
// and the deployment's push admission (409), so the two never read a floor
// two different ways. A floor's PRESENCE is not enough: `>= 0.0.1` on a
// profile-2 bundle admits every runner that cannot read it, which is
// exactly what the floor exists to refuse.
func CheckProfileFloor(m *Manifest, profile int) ProfileFloor {
	pf := ProfileFloor{Profile: profile}
	if profile < 2 {
		pf.OK = true
		return pf
	}
	pf.Need = parser.ProfileSince[profile]
	if m != nil && m.Requires != nil {
		pf.Declared = strings.TrimSpace(m.Requires.Iterion)
	}
	if pf.Declared == "" {
		return pf
	}
	c, err := ParseEngineConstraint(pf.Declared)
	if err != nil {
		return pf
	}
	need, ok := numericVersionParts(pf.Need)
	if !ok {
		pf.OK = true // no release on record: a declared floor is what can be asked
		return pf
	}
	pf.OK = compareVersionParts(c.Min, need) >= 0
	return pf
}
