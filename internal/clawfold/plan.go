package clawfold

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Inputs locates what a plan is computed from.
type Inputs struct {
	Root   string // the iterion checkout the fold lands in
	Source string // a clone of claw-code-go holding the pin
	Pin    string // a revision of Source
	// Unsummed accepts a source go.sum holds no h1: for — a module vendored
	// through a local `replace`. Nothing then anchors the tree read to a
	// published checksum; the command never sets it.
	Unsummed bool
}

// ErrNotRequired is returned when go.mod no longer requires the source
// module: after the switch there is no vendored set to compute a membership
// from, and nothing left to fold.
var ErrNotRequired = errors.New("go.mod does not require " + SourceModule)

// pseudoVersionSHA reads the commit out of every pseudo-version form
// (vX.0.0-ts-sha, vX.Y.Z-pre.0.ts-sha, vX.Y.Z-0.ts-sha), +incompatible or not.
var pseudoVersionSHA = regexp.MustCompile(`[.-]\d{14}-([0-9a-f]{12})(?:\+incompatible)?$`)

// PinFromVendor reads the revision of the source module that vendor/
// holds — the membership's own source, so the pin and the vendored set
// cannot disagree on which line of go.mod is meant: the commit a
// pseudo-version names, or the version tag otherwise.
func PinFromVendor(root string) (string, error) {
	version, replaced, err := vendoredVersion(root)
	if err != nil {
		return "", err
	}
	if replaced != "" {
		return "", fmt.Errorf("vendor/modules.txt replaces %s (%s): the vendored files are not a published version, and nothing anchors a fold of them", SourceModule, replaced)
	}
	if sha := pseudoVersionSHA.FindStringSubmatch(version); sha != nil {
		return sha[1], nil
	}
	return strings.TrimSuffix(version, "+incompatible"), nil
}

// vendoredVersion reads the version vendor/modules.txt holds for the source
// module, and its replacement when it has one.
func vendoredVersion(root string) (version, replaced string, err error) {
	manifest := filepath.Join(root, "vendor", "modules.txt")
	data, err := os.ReadFile(manifest)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("%w: %s is absent", ErrNotRequired, manifest)
	}
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "#" || fields[1] != SourceModule {
			continue
		}
		return fields[2], strings.Join(fields[3:], " "), nil
	}
	return "", "", fmt.Errorf("%w (%s)", ErrNotRequired, manifest)
}

// sumOf reads go.sum's h1: for module@version.
func sumOf(root, version string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == SourceModule && f[1] == version {
			return f[2], nil
		}
	}
	return "", nil
}

// vendoredPackages lists the packages vendor/modules.txt holds.
func vendoredPackages(root string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(root, "vendor", "modules.txt"))
	if err != nil {
		return nil, err
	}
	pkgs := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			pkgs[strings.TrimSpace(line)] = true
		}
	}
	return pkgs, nil
}

// DefaultSource is the clone the primary checkout keeps under .repos/,
// reached through the common git dir so a linked worktree finds it too.
func DefaultSource(root string) (string, error) {
	g, err := newGit()
	if err != nil {
		return "", err
	}
	out, err := g.output(root, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	primary := filepath.Dir(strings.TrimSpace(string(out)))
	source := filepath.Join(primary, ".repos", "claw-code-go")
	if _, err := os.Stat(filepath.Join(source, ".git")); err != nil {
		return "", fmt.Errorf("no claw-code-go clone at %s (create it: mkdir -p %s && printf '**\\n' > %s && git -C %s clone https://%s claw-code-go): %w",
			source, filepath.Dir(source), filepath.Join(filepath.Dir(source), ".gitignore"), filepath.Dir(source), SourceModule, err)
	}
	return source, nil
}

// ReadVendored returns the vendored module's files, by path relative to its
// root.
func ReadVendored(root string) (map[string][]byte, error) {
	base := filepath.Join(root, filepath.FromSlash(VendorDir))
	files := map[string][]byte{}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the vendored set: %w", err)
	}
	return files, nil
}

// Load computes the census from the inputs. The tree read from the source
// must hash to the h1: go.sum holds for the vendored version — the checksum
// the Go checksum database attests — so a rewritten object anywhere in the
// clone (a tree as much as a blob) refuses the plan instead of folding
// something else under the pin's name.
func Load(in Inputs) (*Census, error) {
	vendored, err := ReadVendored(in.Root)
	if err != nil {
		return nil, err
	}
	srcFormat, err := ObjectFormat(in.Source)
	if err != nil {
		return nil, err
	}
	rootFormat, err := ObjectFormat(in.Root)
	if err != nil {
		return nil, err
	}
	if srcFormat != rootFormat {
		return nil, fmt.Errorf("the source names its objects with %s and %s with %s: the planned blobs could never match what git records here", srcFormat, in.Root, rootFormat)
	}
	tree, err := ReadTree(in.Source, in.Pin)
	if err != nil {
		return nil, err
	}
	version, replaced, err := vendoredVersion(in.Root)
	if err != nil {
		return nil, err
	}
	want, err := sumOf(in.Root, version)
	if err != nil {
		return nil, err
	}
	switch {
	case want == "" || replaced != "":
		if !in.Unsummed {
			return nil, fmt.Errorf("go.sum holds no h1: for %s %s (replaced: %q): nothing anchors the tree read to a published checksum", SourceModule, version, replaced)
		}
	default:
		got, err := ModuleHash(tree, SourceModule, version)
		if err != nil {
			return nil, err
		}
		if got != want {
			why := "the clone holds another version, a rewritten object (git fsck --full), or a module-zip rule this tool does not reproduce"
			if rules := archiveRules(tree); len(rules) > 0 {
				why += "; git archive, which builds the zip, may apply " + strings.Join(rules, "; ") + " — this tool reads the blobs as they are"
			}
			return nil, fmt.Errorf("the tree read at %s hashes to %s; go.sum holds %s for %s %s: %s", in.Pin, got, want, SourceModule, version, why)
		}
	}
	view := moduleView(tree)
	c, err := Compute(view, vendored)
	if err != nil {
		return nil, err
	}
	// What the module zip leaves out is neither anchored by go.sum nor
	// carried. A test's fixture among it would fail once landed, so every
	// such file is named, with the reason — none at the current pin.
	dropped := droppedFiles(tree)
	for _, p := range sortedKeys(dropped) {
		c.Warnings = append(c.Warnings, p+": the module zip leaves it out ("+dropped[p]+"): not anchored by go.sum, not carried — a member test that reads it fails once landed")
	}
	if c.vendorPkgs, err = vendoredPackages(in.Root); err != nil {
		return nil, fmt.Errorf("read vendor/modules.txt: %w", err)
	}
	return c, nil
}

// Plan is the fold, computed and not yet applied.
type Plan struct {
	*Census
	// ID names what this plan lands: every slice's files and their blobs.
	// `apply -slice` takes it, so the slice that lands is the slice printed.
	ID       string  `json:"id"`
	Cap      int     `json:"cap"`
	AssetCap int     `json:"asset_cap"`
	Slices   []Slice `json:"slices"`
	// Blocks are the components that hold more than one package: an order
	// constraint the tests impose (the source graph alone is acyclic).
	Blocks [][]string  `json:"blocks,omitempty"`
	Switch *SwitchPlan `json:"switch,omitempty"`
}

// BuildPlan partitions the census under the caps.
func BuildPlan(c *Census, cap, assetCap int) (*Plan, error) {
	slices, err := Partition(c, cap, assetCap)
	if err != nil {
		return nil, err
	}
	p := &Plan{Census: c, Cap: cap, AssetCap: assetCap, Slices: slices}
	for _, comp := range Components(c) {
		if len(comp) > 1 {
			p.Blocks = append(p.Blocks, comp)
		}
	}
	p.annotateExternal()
	p.ID = p.fingerprint()
	return p, nil
}

// annotateExternal names, per Go slice, the external packages its files
// import that vendor/ does not hold yet: `go mod tidy` + `go mod vendor`
// add them to the same landing, outside the cap the slice was cut under.
func (p *Plan) annotateExternal() {
	byPath := map[string]*Member{}
	for _, m := range p.Members {
		byPath[m.Path] = m
	}
	seen := map[string]bool{}
	for k := range p.vendorPkgs {
		seen[k] = true
	}
	for i := range p.Slices {
		s := &p.Slices[i]
		for _, u := range s.Units {
			for _, f := range u.Files {
				for _, imp := range byPath[f].extImports {
					if !seen[imp] {
						seen[imp] = true
						s.NewExternal = append(s.NewExternal, imp)
					}
				}
			}
		}
		sort.Strings(s.NewExternal)
	}
}

// fingerprint hashes what lands: every slice's files with the mode and the
// blob each writes. Two caps that cut the same slices name the same plan; two pins
// that differ by a single byte do not.
func (p *Plan) fingerprint() string {
	blob, mode := map[string]string{}, map[string]string{}
	for _, m := range p.Members {
		blob[m.Path], mode[m.Path] = m.DestBlob, m.Mode
	}
	h := sha256.New()
	for _, s := range p.Slices {
		fmt.Fprintf(h, "%d %s\n", s.Index, s.Kind)
		for _, u := range s.Units {
			for _, f := range u.Files {
				fmt.Fprintf(h, "%s %s %s %s\n", f, path.Join(DestDir, f), mode[f], blob[f])
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// SwitchPlan is the last step: iterion's own imports of the module move to
// the folded packages, and the module leaves go.mod.
type SwitchPlan struct {
	Consumers   []Consumer `json:"consumers"`
	ImportSites int        `json:"import_sites"`
	// PathResiduals are Go lines outside the fold that spell the module or
	// its vendored path as a path, not an import: they keep pointing at a
	// tree the switch removes.
	PathResiduals []Residual `json:"path_residuals,omitempty"`
	// ProseFiles counts the tracked files that mention the module's name
	// outside the imports the switch rewrites (comments, docs, bots) — a
	// consumer among them; they need no change to build.
	ProseFiles int `json:"prose_files"`
}

// Consumer is one iterion file the switch rewrites.
type Consumer struct {
	File  string `json:"file"`
	Sites int    `json:"sites"`
}

var pathForm = regexp.MustCompile(`SocialGouv/claw-code-go|"claw-code-go"`)

// PlanSwitch reads the tracked files of root outside vendor/ and the fold's
// destination.
func PlanSwitch(root string) (*SwitchPlan, error) {
	files, err := trackedFiles(root)
	if err != nil {
		return nil, err
	}
	sp := &SwitchPlan{}
	nested := nestedModules(files)
	for _, f := range files {
		if outsideSwitch(f, nested) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) {
			if isPackageFile(f) {
				// A sparse checkout or a deletion hides it: the switch cannot
				// read whether it imports the module, nor rewrite it.
				return nil, fmt.Errorf("%w: %s is tracked and not on disk (sparse checkout, or deleted)", ErrDirtyTree, f)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data, []byte("claw-code-go")) {
			continue
		}
		if !isPackageFile(f) {
			sp.ProseFiles++
			continue
		}
		_, lines, sites, err := Fold.rewrite(f, data)
		if err != nil {
			return nil, err
		}
		if sites > 0 {
			sp.Consumers = append(sp.Consumers, Consumer{File: f, Sites: sites})
			sp.ImportSites += sites
		}
		rewritten := map[int]bool{}
		for _, l := range lines {
			rewritten[l] = true
		}
		prose := false
		for i, line := range bytes.Split(data, []byte("\n")) {
			if rewritten[i+1] || !bytes.Contains(line, []byte("claw-code-go")) {
				continue
			}
			if pathForm.Match(line) {
				sp.PathResiduals = append(sp.PathResiduals, Residual{File: f, Line: i + 1, Text: strings.TrimSpace(string(line))})
			} else {
				prose = true
			}
		}
		if prose {
			sp.ProseFiles++
		}
	}
	return sp, nil
}

// toolDirs hold this tool, which names the source module on purpose — to
// read it, and to rewrite other branches after the switch.
var toolDirs = []string{"internal/clawfold/", "scripts/clawfold/"}

// outsideSwitch reports the tracked files the switch never reads: the
// vendored tree it removes, the fold itself, the module files `go mod`
// owns, this tool, and every nested module — its imports resolve through
// its own go.mod, which the switch does not touch.
func outsideSwitch(f string, nested []string) bool {
	if strings.HasPrefix(f, "vendor/") || strings.HasPrefix(f, DestDir+"/") || f == "go.mod" || f == "go.sum" {
		return true
	}
	for _, dir := range append(nested, toolDirs...) {
		if strings.HasPrefix(f, dir) {
			return true
		}
	}
	return false
}

// nestedModules returns the directories (with a trailing slash) holding a
// go.mod of their own.
func nestedModules(files []string) []string {
	var dirs []string
	for _, f := range files {
		if path.Base(f) == "go.mod" && f != "go.mod" {
			dirs = append(dirs, path.Dir(f)+"/")
		}
	}
	return dirs
}

func trackedFiles(root string) ([]string, error) {
	g, err := newGit()
	if err != nil {
		return nil, err
	}
	out, err := g.output(root, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, record := range bytes.Split(out, []byte{0}) {
		meta, f, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			continue
		}
		// Regular files only: a symbolic link or a submodule has no source
		// of its own to rewrite.
		if mode := string(bytes.Fields(meta)[0]); mode != "100644" && mode != "100755" {
			continue
		}
		files = append(files, path.Clean(string(f)))
	}
	return files, nil
}

// Toplevel returns the root of the git work tree holding dir.
func Toplevel(dir string) (string, error) {
	g, err := newGit()
	if err != nil {
		return "", err
	}
	out, err := g.output(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
