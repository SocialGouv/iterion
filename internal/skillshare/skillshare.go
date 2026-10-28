// Package skillshare is the sharing primitive for skills a bundle does not own
// alone. A skill file lives with its bundle, so a skill several bundles need
// exists as several files; a comment pointing at the peers is a convention, not
// a mechanism, and the copies drift the first time somebody edits one.
//
// One declaration (families.go) names, per shared file name, the bundle whose
// copy is the SOURCE, the bundles whose copies are MIRRORS of it, and the
// bundles that carry a deliberate VARIANT with the reason it diverges. From
// that declaration:
//
//   - Sync writes every mirror from its source, so the source is the only copy
//     anyone edits — the generator step under `task gen`.
//   - Check reports every way the tree can disagree with the declaration: a
//     mirror that drifted, a variant that collapsed onto its source, a copy
//     whose bytes moved away from the pinned reading, a declared file that is
//     gone, and a file name shared across bundles that no family covers.
//   - Bless rewrites the pins. It is NOT part of `task gen`: a variant's own
//     content is pinned so that an edit to it is REVIEWED rather than absorbed,
//     the way the reading golden is re-pinned deliberately and never by an
//     everyday generator. Its diff is the review.
package skillshare

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// GoldenPath is where the pinned reading of every declared copy lives,
// relative to the repository root.
const GoldenPath = "bots/testdata/shared_skills.txt"

// bundlesDir holds one directory per bundle; a bundle's skills sit in
// skillsDir under it.
const (
	bundlesDir = "bots"
	skillsDir  = "skills"
)

// Kind is a copy's role in its family.
type Kind string

const (
	// SourceKind is the copy every mirror is written from.
	SourceKind Kind = "source"
	// MirrorKind is a copy the generator writes; it must equal its source.
	MirrorKind Kind = "mirror"
	// VariantKind is a copy that deliberately diverges, with a declared
	// reason; it must NOT equal its source.
	VariantKind Kind = "variant"
)

// Family is one shared skill file name and the bundles that carry it.
type Family struct {
	// Name is the skill's file name, shared by every member.
	Name string
	// Source is the bundle whose copy is the source of truth.
	Source string
	// Mirrors are the bundles whose copies Sync writes from Source.
	Mirrors []string
	// Variants are the bundles whose copies diverge on purpose.
	Variants []Variant
}

// Variant is a copy that diverges from its family's source, and the reason a
// reader needs in order to port a shared fix by hand instead of flattening the
// file onto the source.
type Variant struct {
	// Bundle carries the diverging copy.
	Bundle string
	// Reason says what this copy holds that the source must not.
	Reason string
}

// Members returns every bundle in the family, source first, then mirrors and
// variants in declaration order.
func (f Family) Members() []string {
	out := make([]string, 0, 1+len(f.Mirrors)+len(f.Variants))
	out = append(out, f.Source)
	out = append(out, f.Mirrors...)
	for _, v := range f.Variants {
		out = append(out, v.Bundle)
	}
	return out
}

// Path is the root-relative path of a bundle's copy of a family.
func (f Family) Path(bundle string) string {
	return bundlesDir + "/" + bundle + "/" + skillsDir + "/" + f.Name
}

// KindOf reports a bundle's declared role in the family. It is exported so a
// test asserting a particular family's shape reads the same predicate the gate
// does, instead of restating its verdict.
func (f Family) KindOf(bundle string) Kind {
	switch {
	case bundle == f.Source:
		return SourceKind
	case slices.Contains(f.Mirrors, bundle):
		return MirrorKind
	default:
		return VariantKind
	}
}

// Catalog is a declaration applied to a tree. Repo builds the one this
// repository ships; a test builds its own so the gate can be proved against a
// synthetic catalogue rather than against the tree it is guarding.
type Catalog struct {
	// Root is the repository root the declaration is read against.
	Root string
	// Families is the declaration.
	Families []Family
	// Golden is where the pins live, relative to Root. Empty means GoldenPath.
	Golden string
}

// Repo is the declaration this repository ships, applied to a tree.
func Repo(root string) Catalog {
	return Catalog{Root: root, Families: Families(), Golden: GoldenPath}
}

func (c Catalog) golden() string {
	if c.Golden == "" {
		return GoldenPath
	}
	return c.Golden
}

func (c Catalog) abs(rel string) string { return filepath.Join(c.Root, filepath.FromSlash(rel)) }

// Entry is one declared copy's reading.
type Entry struct {
	Kind   Kind
	Family string
	Bundle string
	Path   string
	Sum    string
}

// Pin is the entry's golden form: its kind and digest, space-separated.
func (e Entry) Pin() string { return string(e.Kind) + " " + e.Sum }

// Finding is one way the tree disagrees with the declaration.
type Finding struct {
	Family  string
	Path    string
	Problem string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s (%s)", f.Path, f.Problem, f.Family)
}

func sum(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Read returns the reading of every declared copy, in golden order. A declared
// path that does not exist yields no entry — Check reports it as a missing
// member — and any other read error is returned, since a declaration naming an
// unreadable file is a defect either way.
func (c Catalog) Read() ([]Entry, error) {
	var out []Entry
	for _, f := range c.Families {
		for _, bundle := range f.Members() {
			rel := f.Path(bundle)
			body, err := os.ReadFile(c.abs(rel))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("reading declared copy %s: %w", rel, err)
			}
			out = append(out, Entry{Kind: f.KindOf(bundle), Family: f.Name, Bundle: bundle, Path: rel, Sum: sum(body)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Format renders entries as the golden: one "kind sha path" line per copy.
func Format(entries []Entry) []byte {
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s\n", e.Kind, e.Sum, e.Path)
	}
	return b.Bytes()
}

// Parse reads a golden into path → pin. A line is a kind, a 64-digit hex
// digest and a path; a path pinned twice is refused, since the second pin would
// silently win.
func Parse(data []byte) (map[string]string, error) {
	pins := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("pins line %d: want \"kind sha path\", got %q", i+1, line)
		}
		kind, digest, path := fields[0], fields[1], fields[2]
		switch Kind(kind) {
		case SourceKind, MirrorKind, VariantKind:
		default:
			return nil, fmt.Errorf("pins line %d: unknown kind %q", i+1, kind)
		}
		if len(digest) != 64 {
			return nil, fmt.Errorf("pins line %d: %q is not a sha256 digest", i+1, digest)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("pins line %d: %q is not hex: %w", i+1, digest, err)
		}
		if _, dup := pins[path]; dup {
			return nil, fmt.Errorf("pins line %d: %s is pinned twice", i+1, path)
		}
		pins[path] = kind + " " + digest
	}
	return pins, nil
}

// Sync writes every mirror from its family's source and returns the
// root-relative paths it rewrote, in golden order. A mirror already equal to
// its source is left alone, so the generator is idempotent and a clean tree
// stays clean.
func (c Catalog) Sync() ([]string, error) {
	var written []string
	for _, f := range c.Families {
		if len(f.Mirrors) == 0 {
			continue
		}
		srcRel := f.Path(f.Source)
		source, err := os.ReadFile(c.abs(srcRel))
		if err != nil {
			return nil, fmt.Errorf("family %s: reading its source %s: %w", f.Name, srcRel, err)
		}
		for _, bundle := range f.Mirrors {
			rel := f.Path(bundle)
			current, err := os.ReadFile(c.abs(rel))
			if err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("family %s: reading its mirror %s: %w", f.Name, rel, err)
			}
			if err == nil && bytes.Equal(current, source) {
				continue
			}
			if err := os.WriteFile(c.abs(rel), source, 0o644); err != nil {
				return nil, fmt.Errorf("family %s: writing its mirror %s from %s: %w", f.Name, rel, srcRel, err)
			}
			written = append(written, rel)
		}
	}
	sort.Strings(written)
	return written, nil
}

// Bless pins what the tree currently reads for every declared copy.
func (c Catalog) Bless() (int, error) {
	entries, err := c.Read()
	if err != nil {
		return 0, err
	}
	path := c.abs(c.golden())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("making room for the pins %s: %w", c.golden(), err)
	}
	if err := os.WriteFile(path, Format(entries), 0o644); err != nil {
		return 0, fmt.Errorf("writing the pins %s: %w", c.golden(), err)
	}
	return len(entries), nil
}

// Check reports every way the tree disagrees with the declaration. The
// findings are ordered by path so a run's output is stable.
func (c Catalog) Check() ([]Finding, error) {
	var findings []Finding
	add := func(f Finding) { findings = append(findings, f) }

	for _, f := range c.Families {
		srcRel := f.Path(f.Source)
		source, srcErr := os.ReadFile(c.abs(srcRel))
		if srcErr != nil {
			add(Finding{f.Name, srcRel, fmt.Sprintf("the family's declared source is unreadable: %v", srcErr)})
			continue
		}
		for _, bundle := range f.Mirrors {
			rel := f.Path(bundle)
			body, err := os.ReadFile(c.abs(rel))
			if err != nil {
				add(Finding{f.Name, rel, fmt.Sprintf("is declared a mirror of %s but unreadable: %v", srcRel, err)})
				continue
			}
			if !bytes.Equal(body, source) {
				add(Finding{f.Name, rel, fmt.Sprintf("has drifted from its source %s — run `task skills:sync`, or declare it a variant with the reason it diverges", srcRel)})
			}
		}
		for _, v := range f.Variants {
			rel := f.Path(v.Bundle)
			body, err := os.ReadFile(c.abs(rel))
			if err != nil {
				add(Finding{f.Name, rel, fmt.Sprintf("is declared a variant (%s) but unreadable: %v", v.Reason, err)})
				continue
			}
			if bytes.Equal(body, source) {
				add(Finding{f.Name, rel, fmt.Sprintf("is declared a variant (%s) but is byte-identical to its source %s — declare it a mirror instead", v.Reason, srcRel)})
			}
		}
	}

	// The pins make an edit to a declared copy — a variant above all, which no
	// generator rewrites — visible as a reviewed line rather than a silent
	// divergence.
	entries, err := c.Read()
	if err != nil {
		return nil, err
	}
	pinned, err := os.ReadFile(c.abs(c.golden()))
	if err != nil {
		return nil, fmt.Errorf("reading the pins %s: %w", c.golden(), err)
	}
	pins, err := Parse(pinned)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.golden(), err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Path] = true
		want, ok := pins[e.Path]
		if !ok {
			add(Finding{e.Family, e.Path, "is declared but carries no pin — run `task skills:bless`"})
			continue
		}
		if want != e.Pin() {
			add(Finding{e.Family, e.Path, fmt.Sprintf("reads %q where its pin says %q — if the edit is wanted, run `task skills:bless` so it is reviewed", e.Pin(), want)})
		}
	}
	for path := range pins {
		if !seen[path] {
			add(Finding{filepath.Base(path), path, "is pinned but no longer exists under a declared family — run `task skills:bless`"})
		}
	}

	// Completeness: a file name several bundles carry and the declaration does
	// not cover is a family with no mechanism at all — the defect this package
	// exists to close, so a new one must not land unnoticed.
	shared, err := c.scanShared()
	if err != nil {
		return nil, err
	}
	declared := map[string]map[string]bool{}
	for _, f := range c.Families {
		declared[f.Name] = map[string]bool{}
		for _, b := range f.Members() {
			declared[f.Name][b] = true
		}
	}
	for name, bundles := range shared {
		members, ok := declared[name]
		if !ok {
			add(Finding{name, bundlesDir + "/*/" + skillsDir + "/" + name, fmt.Sprintf("is carried by %d bundles (%s) and no family declares it — add it to internal/skillshare/families.go", len(bundles), strings.Join(bundles, ", "))})
			continue
		}
		for _, b := range bundles {
			if !members[b] {
				add(Finding{name, c.familyByName(name).Path(b), "carries a declared shared skill without being a member of its family — declare it a mirror or a variant"})
			}
		}
	}
	for name, members := range declared {
		have := map[string]bool{}
		for _, b := range shared[name] {
			have[b] = true
		}
		for b := range members {
			if !have[b] {
				add(Finding{name, c.familyByName(name).Path(b), "is declared a member of its family but the tree does not carry that file"})
			}
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Problem < findings[j].Problem
	})
	return findings, nil
}

// scanShared returns every skill file name carried by more than one bundle,
// with the bundles that carry it, sorted. Only files the index carries count: a
// local draft under a bundle's skills/ is not part of the committed catalogue,
// and a plain filesystem walk would report whatever a build or a draft left
// behind.
//
// The scan reads bots/<bundle>/skills/<name>.md and nothing deeper, because
// that is the shape the runtime mirrors into <workspace>/.claude/skills. A
// skill nested one directory further would be invisible here AND unreachable
// at run time, so the limit is the contract rather than a gap in the scan.
func (c Catalog) scanShared() (map[string][]string, error) {
	tracked, err := trackedUnder(c.Root, bundlesDir)
	if err != nil {
		return nil, err
	}
	byName := map[string][]string{}
	for _, rel := range tracked {
		parts := strings.Split(rel, "/")
		if len(parts) != 4 || parts[0] != bundlesDir || parts[2] != skillsDir || !strings.HasSuffix(parts[3], ".md") {
			continue
		}
		byName[parts[3]] = append(byName[parts[3]], parts[1])
	}
	out := map[string][]string{}
	for name, bundles := range byName {
		if len(bundles) < 2 {
			continue
		}
		sort.Strings(bundles)
		out[name] = bundles
	}
	return out, nil
}

func (c Catalog) familyByName(name string) Family {
	for _, f := range c.Families {
		if f.Name == name {
			return f
		}
	}
	return Family{Name: name}
}

// trackedUnder returns the root-relative slash paths the index carries under a
// subdirectory.
func trackedUnder(root, dir string) ([]string, error) {
	args := []string{"-C", root, "ls-files", "-z", "--", dir}
	cmd := exec.Command("git", gitlib.NoRunHooks(args...)...)
	cmd.Env = gitlib.SanitizeEnv(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-files %s in %s: %v: %s", dir, root, err, strings.TrimSpace(stderr.String()))
	}
	var out []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// Options configures Run. Without Sync or Bless the run only checks.
type Options struct {
	Root   string
	Sync   bool
	Bless  bool
	Stdout io.Writer
}

// Run is the command behind `task skills:sync`, `task skills:bless` and
// `task skills:check`. A check over a tree that disagrees with the declaration
// returns an error naming every copy.
func Run(opts Options) error {
	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}
	cat := Repo(opts.Root)
	if opts.Sync || opts.Bless {
		written, err := cat.Sync()
		if err != nil {
			return err
		}
		if len(written) == 0 {
			fmt.Fprintln(out, "every mirror already reads its source.")
		}
		for _, rel := range written {
			fmt.Fprintf(out, "wrote %s from its source\n", rel)
		}
	}
	if opts.Bless {
		n, err := cat.Bless()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "pinned %d declared copies in %s — its diff is the review\n", n, cat.golden())
		return nil
	}
	findings, err := cat.Check()
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d shared skills disagree with internal/skillshare/families.go:", len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "\n  %s", f)
	}
	return fmt.Errorf("%s", b.String())
}
