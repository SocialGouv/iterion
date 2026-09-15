package bundle

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"os"
	"path"
	"path/filepath"
	"slices"
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
func MaxSyntaxRequirements(files map[string]string) SyntaxRequirements {
	return walkSyntax(func(rel string) (string, sourceState) {
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
func MaxSyntaxRequirementsDir(dir string) SyntaxRequirements {
	root := filepath.Clean(dir)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	collection := filepath.Dir(root)
	return walkSyntax(func(rel string) (string, sourceState) {
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

func walkSyntax(read func(rel string) (string, sourceState)) SyntaxRequirements {
	profile := 0
	var declaredBy, unread, importedBy []string
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
		// The workflow's unit: the file and the fragments its imports reach,
		// each read where the workflow is; a fragment that resolves outside
		// is unread, like a child that does, and a child a fragment
		// declares is followed like the main's own.
		base := filepath.ToSlash(filepath.Dir(rel))
		join := func(frag string) string {
			if base == "." {
				return frag
			}
			return path.Join(base, frag)
		}
		main := path.Base(rel)
		u := unit.Load(func(frag string) ([]byte, error) {
			if frag == main {
				return []byte(src), nil
			}
			body, state := read(join(frag))
			switch state {
			case sourceOutside:
				unread = append(unread, join(frag))
				return nil, unit.ErrOutside
			case sourceMissing:
				return nil, os.ErrNotExist
			}
			return []byte(body), nil
		}, main, join)
		for _, f := range u.Files {
			if f.AST != nil && len(f.AST.Imports) > 0 {
				importedBy = append(importedBy, f.Name)
			}
			p := f.Profile
			switch {
			case p > profile:
				profile = p
				declaredBy = nil
				if p > 1 {
					declaredBy = []string{f.Name}
				}
			case p == profile && p > 1:
				declaredBy = append(declaredBy, f.Name)
			}
		}
		if u.Merged == nil {
			return
		}
		for _, sb := range u.Merged.Subbots {
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
	declaredBy = slices.Compact(slices.Sorted(slices.Values(declaredBy)))
	unread = slices.Compact(slices.Sorted(slices.Values(unread)))
	importedBy = slices.Compact(slices.Sorted(slices.Values(importedBy)))
	return SyntaxRequirements{Profile: profile, DeclaredBy: declaredBy, ImportedBy: importedBy, Unread: unread}
}

// MaxSyntaxProfile is MaxSyntaxRequirements projected on the profile.
func MaxSyntaxProfile(files map[string]string) (profile int, declaredBy, unread []string) {
	r := MaxSyntaxRequirements(files)
	return r.Profile, r.DeclaredBy, r.Unread
}

// MaxSyntaxProfileDir is MaxSyntaxRequirementsDir projected on the profile.
func MaxSyntaxProfileDir(dir string) (profile int, declaredBy, unread []string) {
	r := MaxSyntaxRequirementsDir(dir)
	return r.Profile, r.DeclaredBy, r.Unread
}

// SyntaxRequirements is what a bundle's executable sources ask of the
// engine that reads them: the highest `dsl: N` profile they declare and
// whether any of them imports — each with the files that do — plus the
// children the walk could not read.
type SyntaxRequirements struct {
	Profile    int
	DeclaredBy []string
	// ImportedBy names the files that carry `import` lines: a bot in
	// several files needs the release that reads them (parser.ImportSince),
	// whatever its profile.
	ImportedBy []string
	Unread     []string
}

// UsesImport reports whether any source of the bundle imports.
func (r SyntaxRequirements) UsesImport() bool { return len(r.ImportedBy) > 0 }

// Describe names what the sources use, for a diagnostic: "dsl profile 2
// (main.bot)", "`import` (main.bot)", or both.
func (r SyntaxRequirements) Describe() string {
	var parts []string
	if r.Profile >= 2 {
		parts = append(parts, fmt.Sprintf("dsl profile %d (%s)", r.Profile, strings.Join(r.DeclaredBy, ", ")))
	}
	if r.UsesImport() {
		parts = append(parts, fmt.Sprintf("`import` (%s)", strings.Join(r.ImportedBy, ", ")))
	}
	return strings.Join(parts, " and ")
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
	// Reason names what asks for the floor: "dsl profile 2", "import".
	Reason string
	// OK: nothing asks for a floor; otherwise a declared floor that is
	// ordered at or above Need (or any declared floor, when Need is unknown).
	OK bool
}

// CheckSyntaxFloor holds a manifest's engine floor against what its sources
// use — a syntax profile above 1, `import` — one predicate for the author's
// `validate` (C252) and the deployment's push admission (409), so the two
// never read a floor two different ways. A floor's PRESENCE is not enough:
// `>= 0.0.1` on such a bundle admits every runner that cannot read it, which
// is exactly what the floor exists to refuse.
func CheckSyntaxFloor(m *Manifest, req SyntaxRequirements) ProfileFloor {
	pf := ProfileFloor{Profile: req.Profile}
	pf.Need, pf.Reason = RequiredRelease(req)
	if pf.Reason == "" {
		pf.OK = true
		return pf
	}
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
	needParts, ok := numericVersionParts(pf.Need)
	if !ok {
		pf.OK = true // no release on record: a declared floor is what can be asked
		return pf
	}
	pf.OK = compareVersionParts(c.Min, needParts) >= 0
	return pf
}

// RequiredRelease is the release a set of sources needs — the HIGHEST among
// what they use: the profile's (parser.ProfileSince) and `import`'s
// (parser.ImportSince) — with the reason, or "" and "" when they use
// nothing a floor is asked for. The one arithmetic behind `validate`'s
// C252, the push admission and the scaffold's manifest.
func RequiredRelease(req SyntaxRequirements) (release, reason string) {
	type need struct{ release, reason string }
	var needs []need
	if req.Profile >= 2 {
		needs = append(needs, need{parser.ProfileSince[req.Profile], fmt.Sprintf("dsl profile %d", req.Profile)})
	}
	if req.UsesImport() {
		needs = append(needs, need{parser.ImportSince, "import"})
	}
	for _, n := range needs {
		if reason == "" || laterRelease(n.release, release) {
			release, reason = n.release, n.reason
		}
	}
	return release, reason
}

// laterRelease reports whether a orders after b; a release not on record
// (unorderable) is earlier than any that is.
func laterRelease(a, b string) bool {
	pa, okA := numericVersionParts(a)
	pb, okB := numericVersionParts(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	return compareVersionParts(pa, pb) > 0
}

// CheckProfileFloor is CheckSyntaxFloor for a profile alone.
func CheckProfileFloor(m *Manifest, profile int) ProfileFloor {
	return CheckSyntaxFloor(m, SyntaxRequirements{Profile: profile})
}
