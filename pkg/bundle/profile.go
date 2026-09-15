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
	var declaredBy, unread, importedBy, contractedBy []string
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
			if f.AST != nil && len(f.AST.Contracts) > 0 {
				contractedBy = append(contractedBy, f.Name)
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
	contractedBy = slices.Compact(slices.Sorted(slices.Values(contractedBy)))
	return SyntaxRequirements{Profile: profile, DeclaredBy: declaredBy, ImportedBy: importedBy, ContractedBy: contractedBy, Unread: unread}
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
	// ContractedBy names the files that declare a `contract`: a bot with a
	// public contract needs the release that reads one
	// (parser.ContractSince), whatever its profile.
	ContractedBy []string
	Unread       []string
}

// UsesImport reports whether any source of the bundle imports.
func (r SyntaxRequirements) UsesImport() bool { return len(r.ImportedBy) > 0 }

// UsesContract reports whether any source of the bundle declares a contract.
func (r SyntaxRequirements) UsesContract() bool { return len(r.ContractedBy) > 0 }

// Asks reports whether the sources use anything a floor is asked for — the
// one predicate the push admission, `validate` and the scaffold read, so a
// syntax added to RequiredRelease is asked for everywhere at once.
func (r SyntaxRequirements) Asks() bool {
	_, reason := RequiredRelease(r)
	return reason != ""
}

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
	if r.UsesContract() {
		parts = append(parts, fmt.Sprintf("`contract` (%s)", strings.Join(r.ContractedBy, ", ")))
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

// syntaxFloor is one syntax a floor is asked for: the pins it contributes
// to the registry (by the name of the parser constant), and what it needs
// of a set of sources. The table below is the ONE list — RequiredRelease
// reads it, SyntaxFloors projects it — so a syntax added to it is asked
// for by `validate`'s C252, the push admission, the scaffold and the
// release test at once, and one added anywhere else is a test failure
// (TestEveryFloorTheTableAsksForIsPinned).
type syntaxFloor struct {
	pins map[string]string
	need func(SyntaxRequirements) (release, reason string, asks bool)
}

var syntaxFloors = []syntaxFloor{
	{
		pins: profilePins(),
		need: func(req SyntaxRequirements) (string, string, bool) {
			if req.Profile < 2 {
				return "", "", false
			}
			return parser.ProfileSince[req.Profile], fmt.Sprintf("dsl profile %d", req.Profile), true
		},
	},
	{
		pins: map[string]string{"parser.ImportSince": parser.ImportSince},
		need: func(req SyntaxRequirements) (string, string, bool) {
			return parser.ImportSince, "import", req.UsesImport()
		},
	},
	{
		pins: map[string]string{"parser.ContractSince": parser.ContractSince},
		need: func(req SyntaxRequirements) (string, string, bool) {
			return parser.ContractSince, "contract", req.UsesContract()
		},
	},
}

func profilePins() map[string]string {
	pins := map[string]string{}
	for profile, since := range parser.ProfileSince {
		pins[fmt.Sprintf("parser.ProfileSince[%d]", profile)] = since
	}
	return pins
}

// SyntaxFloors is the registry of the releases a syntax first ships in, by
// the name of the parser constant that pins each — the projection of the
// one table the floor predicate reads, so the release test and a reference
// walk exactly what asks for a floor.
func SyntaxFloors() map[string]string {
	out := map[string]string{}
	for _, f := range syntaxFloors {
		for name, release := range f.pins {
			out[name] = release
		}
	}
	return out
}

// RequiredRelease is the release a set of sources needs — the HIGHEST among
// what they use, read from the one table of syntaxes that ask for a floor —
// with the reason, or "" and "" when they use nothing a floor is asked for.
// The one arithmetic behind `validate`'s C252, the push admission, the
// scaffold's manifest and `dsl migrate`'s floor.
func RequiredRelease(req SyntaxRequirements) (release, reason string) {
	for _, f := range syntaxFloors {
		rel, why, asks := f.need(req)
		if !asks {
			continue
		}
		if reason == "" || laterRelease(rel, release) {
			release, reason = rel, why
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
