// Package floorsalign realigns the syntax-floor pins at the release cut.
//
// A syntax floor (bundle.SyntaxFloors — parser.ProfileSince, ImportSince,
// ContractSince, bundle.ToolAliasesSince) is pinned by the lot that ships
// the syntax to the next minor above the release main carries, and
// bundle.HoldSyntaxFloor holds that guess on every tree (the release test).
// The cut is the one place that knows the number actually released:
// release-it runs `cmd/release-floors --apply` from its
// before:git:beforeRelease hook — package.json bumped, CHANGELOG.md
// rendered, nothing staged yet — and every pin at the next minor above the
// previous release is rewritten to the version being cut, inside the
// `chore: release` commit. Without it a patch cut that ships the syntax
// leaves the pin one minor above the build that first reads it — a shape
// the release test's ahead arm accepts, and that reddens one release later,
// on the pinned release's silent notes.
//
// Every other shape refuses the release before anything is committed: a
// pin ahead of the cut that is not the next minor above the previous
// release; and, for every pin the cut leaves at or below itself —
// realigned or long released — the release test's own released arm, run
// on the notes just rendered: the pinned section must exist and carry the
// floor's word (bundle.ReleaseNotesMarker). That last refusal is #1154
// caught before the commit: a floor merged under a subject the changelog
// does not render leaves the notes silent, and the test would redden on
// main the moment the cut landed. The hatch is the test's own — an
// exemption in the marker table, with its reason.
package floorsalign

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// Pin is one floor the registry carries, resolved to the file declaring it.
type Pin struct {
	// Name is the registry key, "parser.ContractSince" or
	// "parser.ProfileSince[2]".
	Name string
	// Ident is the Go identifier the name points at.
	Ident string
	// Index is the map key of a map-literal pin, -1 on a scalar.
	Index int
	// Value is the pinned release, from the registry.
	Value string
	// File is the declaring file, relative to the repo root.
	File string
}

var pinNameRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)\.([A-Za-z][A-Za-z0-9]*)(?:\[(\d+)\])?$`)

// ParsePinName splits a registry key into its package prefix, identifier and
// optional map index.
func ParsePinName(name string) (prefix, ident string, index int, err error) {
	m := pinNameRe.FindStringSubmatch(name)
	if m == nil {
		return "", "", -1, fmt.Errorf("floor pin name %q is not <pkg>.<Ident> or <pkg>.<Ident>[N]", name)
	}
	index = -1
	if m[3] != "" {
		if index, err = strconv.Atoi(m[3]); err != nil {
			return "", "", -1, fmt.Errorf("floor pin name %q carries a non-numeric index: %v", name, err)
		}
	}
	return m[1], m[2], index, nil
}

// ResolvePins walks <root>/pkg for the non-test .go file declaring each
// pin's identifier. The registry (bundle.SyntaxFloors) is the one list of
// pins; the walk is what keeps a file list out of this package, so a floor
// that moves files is followed, and a registry entry whose identifier
// nowhere declares itself is an error, never a silent skip.
//
// A declaration is read from the file's syntax tree, never from its text: a
// comment quoting the pin, a string literal naming it, or a `const (…)`
// block all read correctly, where matching source bytes answers one of the
// three wrongly.
func ResolvePins(root string, floors map[string]string) ([]Pin, error) {
	names := make([]string, 0, len(floors))
	for name := range floors {
		names = append(names, name)
	}
	sort.Strings(names) // map order is not a fact: the report reads the same twice

	pins := make([]Pin, 0, len(names))
	byIdent := map[string]bool{}
	for _, name := range names {
		_, ident, index, err := ParsePinName(name)
		if err != nil {
			return nil, err
		}
		pins = append(pins, Pin{Name: name, Ident: ident, Index: index, Value: floors[name]})
		byIdent[ident] = true
	}

	found := map[string][]string{}
	walkErr := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		file := d.Name()
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A file that does not carry the identifier at all cannot declare
		// it: skip the parse. Every file that does is parsed, so the answer
		// comes from the syntax tree and not from this filter.
		carries := false
		for ident := range byIdent {
			if bytes.Contains(src, []byte(ident)) {
				carries = true
				break
			}
		}
		if !carries {
			return nil
		}
		names, err := declaredNames(path, src)
		if err != nil {
			return err
		}
		for ident := range byIdent {
			if !names[ident] {
				continue
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			found[ident] = append(found[ident], rel)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("resolving floor pins under %s: %w", root, walkErr)
	}
	for i := range pins {
		files := found[pins[i].Ident]
		switch len(files) {
		case 1:
			pins[i].File = files[0]
		case 0:
			return nil, fmt.Errorf("%s: no non-test .go file under pkg/ declares %s — the registry and the source disagree", pins[i].Name, pins[i].Ident)
		default:
			return nil, fmt.Errorf("%s: %s is declared in %d files (%s) — the registry cannot tell which one pins it", pins[i].Name, pins[i].Ident, len(files), strings.Join(files, ", "))
		}
	}
	return pins, nil
}

// declaredNames is the set of package-level const and var names a file
// declares, read from its syntax tree — so a `const (…)` block is seen, and
// a comment or a string literal carrying the same word is not.
func declaredNames(path string, src []byte) (map[string]bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	names := map[string]bool{}
	for _, d := range file.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				names[name.Name] = true
			}
		}
	}
	return names, nil
}

// Kind is what the cut does with one pin.
type Kind int

const (
	// Holds: the pin names a release at or below the version and the
	// release test's released arm holds it; the cut leaves it alone.
	Holds Kind = iota
	// Realign: the cut claims the pin — it is rewritten to the version
	// being cut.
	Realign
	// Pending: preview only — the pin is ahead at the next minor, the next
	// cut realigns it.
	Pending
	// Refuse: a shape the cut does not realign; the release aborts.
	Refuse
)

func (k Kind) String() string {
	switch k {
	case Holds:
		return "holds"
	case Realign:
		return "realign"
	case Pending:
		return "pending"
	case Refuse:
		return "refuse"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Action is the per-pin decision of one run.
type Action struct {
	Pin    Pin
	Kind   Kind
	To     string // the rewritten value, Realign only
	Detail string // the report line, or the refusal
}

// Plan decides, per pin, what the cut does with it. version is the
// checkout's package.json — in a cut, the version being released — and
// changelog its CHANGELOG.md, the cut's section already rendered.
//
// A cut claims a pin no release carries yet — strictly above the release
// that preceded the cut, and not already the cut's own number: realigned
// when it is exactly the next minor above that previous release, the pin
// its lot wrote, and refused otherwise. The claim is measured against the
// PREVIOUS release, not against the version being cut: a major or a
// multi-step bump leaves the lot's pin below the number being released
// (3.180.0 under a 4.0.0 cut), and measuring against the cut would walk
// past it and ship a floor naming a release nobody cut. Everything else,
// in a cut or a preview, is bundle.HoldSyntaxFloor verbatim: a preview
// holds what the release test holds and refuses what it refuses; a cut
// runs the released arm on the value it is about to write, so notes silent
// about a floor refuse the release instead of reddening main after it.
func Plan(pins []Pin, version, changelog string, cutting bool) []Action {
	if cutting {
		if _, ok := bundle.ChangelogSection(changelog, version); !ok {
			return []Action{{Kind: Refuse, Detail: fmt.Sprintf("CHANGELOG.md carries no section for %s, the version being cut: the aligner runs once the changelog is rendered and before git stages the release commit (release-it's before:git:beforeRelease hook)", version)}}
		}
	}
	prev := bundle.NewestChangelogReleaseBelow(changelog, version)
	actions := make([]Action, 0, len(pins))
	for _, p := range pins {
		actions = append(actions, plan(p, version, prev, changelog, cutting))
	}
	return actions
}

func plan(p Pin, version, prev, changelog string, cutting bool) Action {
	marker, known := bundle.ReleaseNotesMarker(p.Name)
	if !known {
		return Action{Pin: p, Kind: Refuse, Detail: fmt.Sprintf("%s has no entry in releaseNotesMarkers (pkg/bundle): name the word its release notes carry, or exempt it with its reason", p.Name)}
	}
	c, ok := bundle.CompareVersions(p.Value, version)
	if !ok {
		return Action{Pin: p, Kind: Refuse, Detail: fmt.Sprintf("%s = %q or package.json = %q is not an orderable version", p.Name, p.Value, version)}
	}
	if cutting && c != 0 && claimedByTheCut(p.Value, prev, c) {
		want, ok := "", false
		if prev != "" {
			want, ok = bundle.NextMinor(prev)
		}
		if !ok {
			return Action{Pin: p, Kind: Refuse, Detail: fmt.Sprintf("%s = %s is ahead of the release being cut (%s), and CHANGELOG.md carries no release below %s to tell the next minor from", p.Name, p.Value, version, version)}
		}
		if p.Value != want {
			return Action{Pin: p, Kind: Refuse, Detail: fmt.Sprintf("%s = %s names no release the cut (%s) or the changelog carries, and is not the next minor above the previous release (%s → %s): below that, a release overtook the pin and it names a build that does not read the syntax; above it, the pin refuses every build in between that does — re-pin by hand to the release that first reads the syntax", p.Name, p.Value, version, prev, want)}
		}
		if err := bundle.HoldSyntaxFloor(p.Name, version, version, changelog, marker); err != nil {
			return Action{Pin: p, Kind: Refuse, Detail: cutRefusal(p, version, marker, err)}
		}
		return Action{Pin: p, Kind: Realign, To: version, Detail: fmt.Sprintf("%s = %s → %s (realigned to the release being cut)", p.Name, p.Value, version)}
	}
	if err := bundle.HoldSyntaxFloor(p.Name, p.Value, version, changelog, marker); err != nil {
		if cutting && c == 0 {
			return Action{Pin: p, Kind: Refuse, Detail: cutRefusal(p, version, marker, err)}
		}
		return Action{Pin: p, Kind: Refuse, Detail: err.Error()}
	}
	switch {
	case c > 0:
		return Action{Pin: p, Kind: Pending, Detail: fmt.Sprintf("%s = %s is ahead at the next minor: the next cut realigns it to the version it releases", p.Name, p.Value)}
	case cutting && c == 0:
		return Action{Pin: p, Kind: Holds, Detail: fmt.Sprintf("%s = %s names the release being cut, and its notes carry %q", p.Name, p.Value, marker)}
	}
	return Action{Pin: p, Kind: Holds, Detail: fmt.Sprintf("%s = %s is released; the release test holds it", p.Name, p.Value)}
}

// claimedByTheCut reports whether the cut is the release that turns this
// pin into a number: strictly above the release that preceded the cut, so
// no release carries it yet. cmpVersion is the pin against the version
// being cut, already known orderable; the caller has excluded a pin that
// already names the cut. With no release below the cut — the first release
// a changelog carries — only a pin ahead of the cut qualifies, and
// anything under it gets the released arm's "was ever cut" verdict, which
// is the honest one.
func claimedByTheCut(pin, prev string, cmpVersion int) bool {
	if prev == "" {
		return cmpVersion > 0
	}
	c, ok := bundle.CompareVersions(pin, prev)
	return ok && c > 0
}

// cutRefusal is the verdict on a pin the cut would leave naming itself. Any
// arm but one is the release test's own error, verbatim: what refuses the
// cut is what would redden main. The exception is the notes the cut just
// rendered saying nothing about the floor — the shape #1154 left behind (a
// floor merged under a subject the changelog does not render) — which the
// release alone can still repair, so it gets the remedy instead of the
// verdict re-stated.
func cutRefusal(p Pin, version, marker string, err error) string {
	var silent *bundle.SilentNotesError
	if !errors.As(err, &silent) {
		return err.Error()
	}
	return fmt.Sprintf("%s would name %s, the release being cut, whose notes do not carry %q — the commit that ships the syntax was merged under a subject the changelog does not render (#1154), or under one without that word: in releaseNotesMarkers (pkg/bundle), name a word these notes do carry, or exempt the floor with its reason (parser.ProfileSince[2] is the precedent), then cut again", p.Name, version, marker)
}

// ApplyPin rewrites the pin's release literal in src to to. Only the
// literal's bytes move — the file keeps everything else byte for byte.
//
// The literal is located through the file's syntax tree, not by matching
// source text: a comment inside the map literal spelling an entry, a string
// carrying the constant's name, or a second map in the same file cannot be
// written into. Anything but a plain quoted release on the declaration the
// registry names is an error — guessing would write a pin into a
// declaration nobody checked.
func ApplyPin(src []byte, p Pin, to string) ([]byte, error) {
	fset := token.NewFileSet()
	name := p.File
	if name == "" {
		name = p.Ident + ".go"
	}
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: parsing %s: %w", p.Name, name, err)
	}
	var specs []*ast.ValueSpec
	for _, d := range file.Decls {
		gen, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, ident := range value.Names {
				if ident.Name == p.Ident {
					specs = append(specs, value)
				}
			}
		}
	}
	if len(specs) != 1 {
		return nil, fmt.Errorf("%s: %s is declared %d times in %s (want exactly once) — the file and the registry disagree", p.Name, p.Ident, len(specs), name)
	}
	spec := specs[0]
	index := 0
	for i, ident := range spec.Names {
		if ident.Name == p.Ident {
			index = i
		}
	}
	if index >= len(spec.Values) {
		return nil, fmt.Errorf("%s: %s in %s has no value to rewrite — a floor pin is a release literal", p.Name, p.Ident, name)
	}
	lit, err := releaseLiteral(spec.Values[index], p)
	if err != nil {
		return nil, err
	}
	start, end := fset.Position(lit.Pos()).Offset, fset.Position(lit.End()).Offset
	quoted := strconv.Quote(to)
	out := make([]byte, 0, len(src)-(end-start)+len(quoted))
	out = append(out, src[:start]...)
	out = append(out, quoted...)
	out = append(out, src[end:]...)
	return out, nil
}

var releaseLiteralRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// releaseLiteral is the quoted release the pin names inside a declaration's
// value: the value itself on a scalar pin, the entry under the pin's key on
// a map one. A key that appears twice, or a value that is not a plain
// quoted release, is an error rather than a guess.
func releaseLiteral(value ast.Expr, p Pin) (*ast.BasicLit, error) {
	if p.Index < 0 {
		return plainRelease(value, p)
	}
	composite, ok := value.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("%s: %s is not a map literal — an indexed pin names one entry of one", p.Name, p.Ident)
	}
	var found []ast.Expr
	for _, element := range composite.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := entry.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.INT {
			continue
		}
		n, err := strconv.Atoi(key.Value)
		if err != nil || n != p.Index {
			continue
		}
		found = append(found, entry.Value)
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("%s: the key %d appears %d times in %s's literal (want exactly once)", p.Name, p.Index, len(found), p.Ident)
	}
	return plainRelease(found[0], p)
}

func plainRelease(value ast.Expr, p Pin) (*ast.BasicLit, error) {
	lit, ok := value.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil, fmt.Errorf("%s: %s does not pin a quoted release — the cut rewrites a literal, never an expression", p.Name, p.Ident)
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return nil, fmt.Errorf("%s: %s = %s is not a readable string literal: %v", p.Name, p.Ident, lit.Value, err)
	}
	if !releaseLiteralRe.MatchString(unquoted) {
		return nil, fmt.Errorf("%s: %s = %q is not a release number — the cut refuses to rewrite it", p.Name, p.Ident, unquoted)
	}
	return lit, nil
}
