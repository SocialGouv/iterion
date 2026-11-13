package clawfold

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// GoTool runs the go command in a directory.
type GoTool interface {
	Run(dir string, args ...string) ([]byte, error)
}

type goBinary struct {
	path string
	env  []string // nil: the process environment
}

// NewGoTool resolves the go command through the trusted PATH.
func NewGoTool() (GoTool, error) {
	p, err := envtrust.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("locate go: %w", err)
	}
	return goBinary{path: p}, nil
}

func (g goBinary) Run(dir string, args ...string) ([]byte, error) {
	env := g.env
	if env == nil {
		env = os.Environ()
	}
	cmd := exec.Command(g.path, args...) // #nosec G204 -- argv built by this package
	cmd.Dir = dir
	// The fold edits the main module only; a go.work around it would make
	// `go mod vendor` refuse to run at all.
	cmd.Env = append(slices.Clone(env), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("go %s in %s: %w\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out, nil
}

// ErrDirtyModule is returned when go.mod, go.sum or vendor/ carry changes
// before apply runs: it rewrites those paths, and could not tell its own
// change from work in progress.
var ErrDirtyModule = errors.New("go.mod, go.sum or vendor/ carry uncommitted changes")

// ErrDirtyTree is returned when the switch finds uncommitted Go changes: it
// rewrites tracked Go files, and its diff must be its own.
var ErrDirtyTree = errors.New("the tree carries uncommitted Go changes")

// ErrNotAPrefix is returned when the tree does not hold an exact prefix of
// the plan: an earlier slice incomplete, a later one present while the asked
// one is not, or a file under the destination that no slice carries.
var ErrNotAPrefix = errors.New("the tree does not hold a prefix of this plan")

// ErrConflict is returned when a destination file exists with other bytes
// or another mode than the fold would write.
var ErrConflict = errors.New("a destination file holds other bytes than the fold")

// ApplyReport says what an apply did.
type ApplyReport struct {
	Slice    int
	Written  []string
	Skipped  int
	GoRan    [][]string
	Resorted []string // for the switch: consumers gofmt re-sorted after the rewrite
	ModDelta []string // for the switch: requirement changes in go.mod
	// Owes names what the landing must regenerate before it is committed:
	// committed indexes over the files the slice adds.
	Owes []string
	// NewExternal repeats the plan: external packages tidy+vendor added.
	NewExternal []string
}

func porcelain(root string, pathspecs ...string) (string, error) {
	g, err := newGit()
	if err != nil {
		return "", err
	}
	// --untracked-files=all overrides a status.showUntrackedFiles=no that
	// would hide exactly what these checks look for.
	out, err := g.output(root, nil, append([]string{"status", "--porcelain", "--untracked-files=all", "--"}, pathspecs...)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func moduleClean(root string) error {
	s, err := porcelain(root, "go.mod", "go.sum", "vendor/")
	if err != nil {
		return err
	}
	if s != "" {
		return fmt.Errorf("%w:\n%s", ErrDirtyModule, s)
	}
	return nil
}

// destPath resolves rel under root, refusing a symbolic link anywhere on
// the way: a link would carry the write — or the proof — outside the tree
// git records.
func destPath(root, rel string) (string, error) {
	p := root
	for _, seg := range strings.Split(rel, "/") {
		p = filepath.Join(p, seg)
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.Join(root, filepath.FromSlash(rel)), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symbolic link; the fold writes and proves regular files only", strings.TrimPrefix(filepath.ToSlash(p), filepath.ToSlash(root)+"/"))
		}
	}
	return p, nil
}

// executable is git's reading of a mode: the owner's execute bit.
func executable(mode fs.FileMode) bool { return mode.Perm()&0o100 != 0 }

// state is a destination file's relation to the plan.
type state int

const (
	absent state = iota
	matches
	differs
)

// fileState compares the destination of m with the plan: the bytes AND the
// mode, on disk and, once git tracks it, in the index — what a commit records.
func fileState(root string, m *Member, index map[string]indexEntry) (state, string, error) {
	if e, tracked := index[m.Dest]; tracked && (e.mode != m.Mode || e.blob != m.DestBlob) {
		return differs, fmt.Sprintf("%s: the index records %s %s, the fold writes %s %s", m.Dest, e.mode, e.blob, m.Mode, m.DestBlob), nil
	}
	dst, err := destPath(root, m.Dest)
	if err != nil {
		return differs, err.Error(), nil
	}
	info, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return absent, "", nil
	}
	if err != nil {
		return absent, "", err
	}
	if !info.Mode().IsRegular() {
		return differs, m.Dest + " is not a regular file", nil
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		return absent, "", err
	}
	if got := BlobID(data, len(m.DestBlob)); got != m.DestBlob {
		return differs, fmt.Sprintf("%s holds blob %s, the fold writes %s", m.Dest, got, m.DestBlob), nil
	}
	if executable(info.Mode()) != (m.Mode == "100755") {
		return differs, fmt.Sprintf("%s has mode %v, the fold writes %s", m.Dest, info.Mode().Perm(), m.Mode), nil
	}
	return matches, "", nil
}

func (p *Plan) members(slice Slice) []*Member {
	byPath := make(map[string]*Member, len(p.Members))
	for _, m := range p.Members {
		byPath[m.Path] = m
	}
	var out []*Member
	for _, u := range slice.Units {
		for _, f := range u.Files {
			out = append(out, byPath[f])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// strays lists what lies under the destination that no slice carries: on
// disk (links included) and in git's index.
func (p *Plan) strays(root string) ([]string, error) {
	planned := map[string]bool{}
	for _, m := range p.Members {
		planned[m.Dest] = true
	}
	found := map[string]bool{}
	base := filepath.Join(root, DestDir)
	err := filepath.WalkDir(base, func(q string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && q == base {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, q)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !planned[rel] || d.Type()&fs.ModeSymlink != 0 {
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				rel += " (a symbolic link)"
			case strings.HasPrefix(d.Name(), ".clawfold-"):
				rel += " (left by an interrupted apply: remove it, then re-run)"
			}
			found[rel] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	index, err := indexEntries(root)
	if err != nil {
		return nil, err
	}
	for rel := range index {
		if !planned[rel] {
			found[rel] = true
		}
	}
	return sortedKeys(found), nil
}

type indexEntry struct{ mode, blob string }

// indexEntries reads git's index under the destination: what a commit
// would record, which a working-tree reading alone does not prove (a
// `core.fileMode=false` checkout, a .gitattributes normalisation).
func indexEntries(root string) (map[string]indexEntry, error) {
	g, err := newGit()
	if err != nil {
		return nil, err
	}
	out, err := g.output(root, nil, "ls-files", "-s", "-z", "--", DestDir)
	if err != nil {
		return nil, err
	}
	entries := map[string]indexEntry{}
	for _, record := range bytes.Split(out, []byte{0}) {
		meta, f, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) < 2 {
			return nil, fmt.Errorf("ls-files record %q", meta)
		}
		entries[path.Clean(string(f))] = indexEntry{mode: fields[0], blob: fields[1]}
	}
	return entries, nil
}

// Apply writes slice k (1-based) into root, then runs `go mod tidy` and `go
// mod vendor` when the slice carries Go. A file already holding the fold's
// bytes and mode is skipped, so a second run writes nothing: apply is
// idempotent. Each file is written whole or not at all, so an interrupted
// run resumes.
func (p *Plan) Apply(root string, k int, tool GoTool) (*ApplyReport, error) {
	if k < 1 || k > len(p.Slices) {
		return nil, fmt.Errorf("slice %d does not exist: the plan has %d", k, len(p.Slices))
	}
	if err := moduleClean(root); err != nil {
		return nil, err
	}
	strays, err := p.strays(root)
	if err != nil {
		return nil, err
	}
	if len(strays) > 0 {
		return nil, fmt.Errorf("%w: %s holds %d file(s) no slice carries: %s", ErrNotAPrefix, DestDir, len(strays), strings.Join(firstN(strays, 10), ", "))
	}
	index, err := indexEntries(root)
	if err != nil {
		return nil, err
	}
	// A destination holding other bytes or another mode is a conflict in
	// whichever slice it sits, and is named as one before any reasoning
	// about which slices are complete.
	complete := make([]bool, len(p.Slices))
	present := make([]bool, len(p.Slices))
	var conflicts []string
	for i, s := range p.Slices {
		complete[i] = true
		for _, m := range p.members(s) {
			st, why, err := fileState(root, m, index)
			if err != nil {
				return nil, err
			}
			switch st {
			case matches:
				present[i] = true
			case differs:
				complete[i] = false
				conflicts = append(conflicts, fmt.Sprintf("%s (slice %d)", why, i+1))
			default:
				complete[i] = false
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("%w (nothing written): %s", ErrConflict, strings.Join(firstN(conflicts, 10), "; "))
	}
	for i := 0; i < k-1; i++ {
		if !complete[i] {
			return nil, fmt.Errorf("%w: slice %d is not fully landed (verify it), so slice %d would not build on it", ErrNotAPrefix, i+1, k)
		}
	}
	if !complete[k-1] {
		for i := k; i < len(p.Slices); i++ {
			if present[i] {
				return nil, fmt.Errorf("%w: slice %d is (partly) present while slice %d is not complete — was it applied with other caps than -cap %d -asset-cap %d?", ErrNotAPrefix, i+1, k, p.Cap, p.AssetCap)
			}
		}
	}

	slice := p.Slices[k-1]
	report := &ApplyReport{Slice: k}
	var toWrite []*Member
	for _, m := range p.members(slice) {
		st, _, err := fileState(root, m, index)
		if err != nil {
			return nil, err
		}
		if st == matches {
			report.Skipped++
		} else {
			toWrite = append(toWrite, m)
		}
	}
	for _, m := range toWrite {
		if err := writeMember(root, m); err != nil {
			return report, err
		}
		report.Written = append(report.Written, m.Dest)
	}
	report.Owes = owes(p.members(slice), slice.Kind)
	if slice.Kind == SliceGo {
		before, err := requirements(root, tool)
		if err != nil {
			return report, err
		}
		for _, args := range [][]string{{"mod", "tidy"}, {"mod", "vendor"}} {
			if _, err := tool.Run(root, args...); err != nil {
				return report, fmt.Errorf("%w\nslice %d's files are in place; once the cause is fixed, run `go mod tidy && go mod vendor` and commit — a re-run of apply then writes nothing", err, k)
			}
			report.GoRan = append(report.GoRan, args)
		}
		after, err := requirements(root, tool)
		if err != nil {
			return report, err
		}
		// A slice's tidy moves go.mod too: every requirement it changes is
		// named, a version that moved first among them.
		report.ModDelta = requirementDelta(before, after)
		report.NewExternal = slice.NewExternal
	}
	return report, nil
}

// owes names the committed indexes a slice's files change: the corpus
// reading pins every tracked .md/.yaml/.yml, the package map every package.
func owes(members []*Member, kind SliceKind) []string {
	var out []string
	for _, m := range members {
		switch path.Ext(m.Path) {
		case ".md", ".yaml", ".yml":
			out = append(out, "git add the slice, then `task corpus:reading -- --write` (it pins every tracked .md/.yaml/.yml)")
		}
		if len(out) > 0 {
			break
		}
	}
	if kind == SliceGo {
		out = append(out, "git add the slice, then `task map:gen` (the package map lists every package)")
	}
	return out
}

func perm(mode string) os.FileMode {
	if mode == "100755" {
		return 0o755
	}
	return 0o644
}

// writeMember writes m's destination through a temporary file renamed into
// place: a run cut short leaves the file absent or whole, never a prefix
// that a re-run would refuse as a conflict.
func writeMember(root string, m *Member) error {
	dst, err := destPath(root, m.Dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".clawfold-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(m.dest)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), perm(m.Mode))); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// Finding is one failed proof.
type Finding struct {
	Dest    string `json:"dest"`
	Problem string `json:"problem"`
}

// VerifyReport is the outcome of [Plan.Verify].
type VerifyReport struct {
	Checked  int       `json:"checked"`
	Absent   int       `json:"absent"`
	Findings []Finding `json:"findings,omitempty"`
}

// Verify replays the proofs on every destination file of the given slices
// (all slices when none is given) present in root:
//
//  1. the file's blob is the one the plan computed;
//  2. un-rewriting it gives back the pin's blob, byte for byte;
//  3. it differs from the pin's file on the rewritten import lines only;
//  4. its owner execute bit matches the pin's mode;
//  5. when git tracks it, the index records that same mode and blob.
//
// Whatever lies under the destination that no slice carries is a finding,
// as is a symbolic link on the way to a file. With explicit slices an
// absent file is a finding; over the whole plan it is only counted, since a
// tree mid-landing holds a prefix.
func (p *Plan) Verify(root string, slices ...int) (*VerifyReport, error) {
	strict := len(slices) > 0
	if !strict {
		for i := range p.Slices {
			slices = append(slices, i+1)
		}
	}
	index, err := indexEntries(root)
	if err != nil {
		return nil, err
	}
	r := &VerifyReport{}
	for _, k := range slices {
		if k < 1 || k > len(p.Slices) {
			return nil, fmt.Errorf("slice %d does not exist: the plan has %d", k, len(p.Slices))
		}
		for _, m := range p.members(p.Slices[k-1]) {
			problems, isAbsent, err := verifyMember(root, m, index)
			if err != nil {
				return nil, err
			}
			if isAbsent {
				r.Absent++
				if strict {
					r.Findings = append(r.Findings, Finding{Dest: m.Dest, Problem: fmt.Sprintf("absent (slice %d)", k)})
				}
				continue
			}
			r.Checked++
			for _, problem := range problems {
				r.Findings = append(r.Findings, Finding{Dest: m.Dest, Problem: problem})
			}
		}
	}
	strays, err := p.strays(root)
	if err != nil {
		return nil, err
	}
	for _, s := range strays {
		r.Findings = append(r.Findings, Finding{Dest: s, Problem: "not in the plan: no slice carries this file"})
	}
	return r, nil
}

func verifyMember(root string, m *Member, index map[string]indexEntry) (problems []string, isAbsent bool, err error) {
	if e, tracked := index[m.Dest]; tracked && (e.mode != m.Mode || e.blob != m.DestBlob) {
		problems = append(problems, fmt.Sprintf("the index records %s %s, the fold planned %s %s", e.mode, e.blob, m.Mode, m.DestBlob))
	}
	dst, err := destPath(root, m.Dest)
	if err != nil {
		return append(problems, err.Error()), false, nil
	}
	info, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		return problems, len(problems) == 0, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return append(problems, "not a regular file"), false, nil
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		return nil, false, err
	}
	if got := BlobID(data, len(m.DestBlob)); got != m.DestBlob {
		problems = append(problems, fmt.Sprintf("blob %s, the fold wrote %s", got, m.DestBlob))
	}
	if m.Kind != KindAsset {
		back, _, err := Fold.Inverse().Rewrite(m.Dest, data)
		if err != nil {
			problems = append(problems, "unrewrite: "+err.Error())
		} else if got := BlobID(back, len(m.SrcBlob)); got != m.SrcBlob {
			problems = append(problems, fmt.Sprintf("unrewrite gives blob %s, the pin holds %s", got, m.SrcBlob))
		}
		changed, sameCount := ChangedLines(m.src, data)
		switch {
		case !sameCount:
			problems = append(problems, "line count differs from the pin's file")
		case !equalInts(changed, m.RewrittenLines):
			problems = append(problems, fmt.Sprintf("differs from the pin's file on lines %v, the rewrite touches %v", changed, m.RewrittenLines))
		}
	} else if got := BlobID(data, len(m.SrcBlob)); got != m.SrcBlob {
		problems = append(problems, fmt.Sprintf("an asset differs from the pin's file: blob %s, the pin holds %s", got, m.SrcBlob))
	}
	if executable(info.Mode()) != (m.Mode == "100755") {
		problems = append(problems, fmt.Sprintf("mode %v, the pin's is %s", info.Mode().Perm(), m.Mode))
	}
	return problems, false, nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplySwitch is the last step: every slice must be landed and verified, and
// the tree must carry no uncommitted Go change; then iterion's own imports
// of the module move under the destination — gofmt re-sorting the files that
// were formatted — and `go mod tidy` + `go mod vendor` drop the module from
// go.mod and vendor/. It refuses to report success while the module is
// still required or its vendored copy still there. The report names every
// requirement whose version moved, not only those removed.
func (p *Plan) ApplySwitch(root string, tool GoTool) (*ApplyReport, *SwitchPlan, error) {
	if err := moduleClean(root); err != nil {
		return nil, nil, err
	}
	if dirty, err := p.uncommittedForSwitch(root); err != nil {
		return nil, nil, err
	} else if len(dirty) > 0 {
		return nil, nil, fmt.Errorf("%w — the switch rewrites committed Go files and proves a committed fold; commit or remove:\n%s", ErrDirtyTree, strings.Join(firstN(dirty, 20), "\n"))
	}
	v, err := p.Verify(root)
	if err != nil {
		return nil, nil, err
	}
	if v.Absent > 0 || len(v.Findings) > 0 {
		return nil, nil, fmt.Errorf("%w: the switch needs every slice landed and proven (%d absent, %d finding(s)); run verify", ErrNotAPrefix, v.Absent, len(v.Findings))
	}
	sp, err := PlanSwitch(root)
	if err != nil {
		return nil, nil, err
	}
	report := &ApplyReport{} // Slice 0: the switch, after every slice
	// Every consumer's rewrite is proven before the first one is written, so
	// a refusal leaves the tree as it was.
	writes, err := rewriteAll(root, sp.consumerFiles(), Fold)
	if err != nil {
		return report, sp, err
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(w.file)), w.out, w.perm); err != nil {
			return report, sp, err
		}
		report.Written = append(report.Written, w.file)
		if w.resorted {
			report.Resorted = append(report.Resorted, w.file)
		}
	}
	before, err := requirements(root, tool)
	if err != nil {
		return report, sp, err
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"mod", "vendor"}} {
		if _, err := tool.Run(root, args...); err != nil {
			return report, sp, err
		}
		report.GoRan = append(report.GoRan, args)
	}
	after, err := requirements(root, tool)
	if err != nil {
		return report, sp, err
	}
	report.ModDelta = requirementDelta(before, after)
	if r, ok := after[SourceModule]; ok {
		return report, sp, fmt.Errorf("go.mod still requires %s %s after tidy: a file the switch did not rewrite still imports it", SourceModule, r.Version)
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(VendorDir))); err == nil {
		return report, sp, fmt.Errorf("%s survived go mod vendor", VendorDir)
	}
	return report, sp, nil
}

// uncommittedForSwitch lists what would make the switch's diff not its own:
// a Go file the go tool compiles that git does not track (untracked or
// ignored — `go build` reads it all the same), a tracked file changed or
// deleted, and a fold member git does not hold.
func (p *Plan) uncommittedForSwitch(root string) ([]string, error) {
	g, err := newGit()
	if err != nil {
		return nil, err
	}
	out, err := g.output(root, nil, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			tracked[string(f)] = true
		}
	}
	var dirty []string
	visible, err := goVisibleFiles(root)
	if err != nil {
		return nil, err
	}
	for _, f := range visible {
		if !tracked[f] {
			dirty = append(dirty, f+" (a Go file the go tool compiles, not tracked)")
		}
	}
	changes, err := g.output(root, nil, "status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	for _, rec := range bytes.Split(changes, []byte{0}) {
		if len(rec) > 3 && (strings.HasSuffix(string(rec), ".go") || strings.HasPrefix(string(rec[3:]), DestDir+"/")) {
			dirty = append(dirty, string(rec))
		}
	}
	index, err := indexEntries(root)
	if err != nil {
		return nil, err
	}
	for _, m := range p.Members {
		if _, ok := index[m.Dest]; !ok {
			dirty = append(dirty, m.Dest+" (a fold member git does not hold)")
		}
	}
	sort.Strings(dirty)
	return dirty, nil
}

// goVisibleFiles lists the .go files of the main module the go tool would
// compile under `./...`: not under a directory named testdata, vendor, or
// starting with "." or "_", not in a nested module, not named _x.go/.x.go.
func goVisibleFiles(root string) ([]string, error) {
	// WalkDir does not descend a root that is itself a link.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(root, func(q string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if q == root {
				return nil
			}
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(q, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			rel, err := filepath.Rel(root, q)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

func (sp *SwitchPlan) consumerFiles() []string {
	files := make([]string, 0, len(sp.Consumers))
	for _, c := range sp.Consumers {
		files = append(files, c.File)
	}
	return files
}

type requirement struct {
	Version  string
	Indirect bool
}

func requirements(root string, tool GoTool) (map[string]requirement, error) {
	out, err := tool.Run(root, "mod", "edit", "-json")
	if err != nil {
		return nil, err
	}
	var mod struct {
		Require []struct {
			Path     string
			Version  string
			Indirect bool
		}
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, fmt.Errorf("read go mod edit -json: %w", err)
	}
	reqs := map[string]requirement{}
	for _, r := range mod.Require {
		reqs[r.Path] = requirement{Version: r.Version, Indirect: r.Indirect}
	}
	return reqs, nil
}

func requirementDelta(before, after map[string]requirement) []string {
	var delta []string
	for path, b := range before {
		a, ok := after[path]
		switch {
		case !ok:
			delta = append(delta, fmt.Sprintf("removed %s %s", path, b.Version))
		case a.Version != b.Version:
			delta = append(delta, fmt.Sprintf("VERSION MOVED %s %s -> %s", path, b.Version, a.Version))
		case a.Indirect != b.Indirect:
			delta = append(delta, fmt.Sprintf("indirect %t -> %t %s", b.Indirect, a.Indirect, path))
		}
	}
	for path, a := range after {
		if _, ok := before[path]; !ok {
			delta = append(delta, fmt.Sprintf("added %s %s", path, a.Version))
		}
	}
	sort.Strings(delta)
	return delta
}

// rewrite is one file's proven rewrite, not yet written.
type rewrite struct {
	file     string
	out      []byte
	perm     os.FileMode
	resorted bool
}

// rewriteChecked computes the rewrite of the Go file at p under m and proves
// it, writing nothing. A file gofmt leaves unchanged comes back formatted —
// a rewritten import can sort differently within its group — and its proof
// is then "gofmt of the inverse gives the file back"; any other file comes
// back unformatted, and its inverse must give it back byte for byte.
func rewriteChecked(p string, m Mapping) (*rewrite, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	out, lines, err := m.Rewrite(p, data)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	taken, err := m.ImportsUnderDestination(p, data)
	if err != nil {
		return nil, err
	}
	if len(taken) > 0 {
		return nil, fmt.Errorf("%s imports both under %s and under %s (line(s) %v): the rewrite would not have an exact inverse", p, m.From, m.To, taken)
	}
	clean, ferr := GofmtStable(data)
	formatted := ferr == nil && clean
	r := &rewrite{file: p, out: out, perm: info.Mode().Perm()}
	if formatted {
		sorted, err := format.Source(out)
		if err != nil {
			return nil, fmt.Errorf("gofmt rewritten %s: %w", p, err)
		}
		// Re-sorted means the order of a block's imports moved; a longer
		// path that only re-aligns trailing comments is not a re-sort. In a
		// re-sorted block, gofmt moves the specs and leaves a block-leading
		// comment where it was: it would end up above another import.
		before, err := importBlocks(p, out)
		if err != nil {
			return nil, err
		}
		after, err := importBlocks(p, sorted)
		if err != nil {
			return nil, err
		}
		for i := range before {
			if i < len(after) && slices.Equal(before[i].paths, after[i].paths) {
				continue
			}
			r.resorted = true
			if before[i].commented {
				return nil, fmt.Errorf("%s: gofmt re-sorts an import block after the rewrite and that block holds comments gofmt may leave behind; rewrite it by hand", p)
			}
		}
		r.out = sorted
	}
	back, _, err := m.Inverse().Rewrite(p, r.out)
	if err != nil {
		return nil, err
	}
	if formatted {
		if back, err = format.Source(back); err != nil {
			return nil, fmt.Errorf("gofmt the inverse of %s: %w", p, err)
		}
	}
	if !bytes.Equal(back, data) {
		return nil, fmt.Errorf("%s: the inverse of the rewrite does not give the file back", p)
	}
	return r, nil
}

// importBlock is one import declaration: its paths in order, and whether a
// comment sits inside its parentheses.
type importBlock struct {
	paths     []string
	commented bool
}

func importBlocks(p string, src []byte) ([]importBlock, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var blocks []importBlock
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		var b importBlock
		for _, spec := range gen.Specs {
			b.paths = append(b.paths, spec.(*ast.ImportSpec).Path.Value)
		}
		for _, group := range file.Comments {
			if gen.Lparen.IsValid() && group.Pos() > gen.Lparen && group.End() < gen.Rparen {
				b.commented = true
			}
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

// rewriteAll proves the rewrite of every file under root before any is
// written.
func rewriteAll(root string, files []string, m Mapping) ([]*rewrite, error) {
	var out []*rewrite
	for _, f := range files {
		r, err := rewriteChecked(filepath.Join(root, filepath.FromSlash(f)), m)
		if err != nil {
			return nil, err
		}
		if r != nil {
			r.file = f
			out = append(out, r)
		}
	}
	return out, nil
}

// RewriteFile applies m to the Go file at p in place, refusing — before
// writing anything — a rewrite it cannot prove (see rewriteChecked). It
// reports whether the file changed.
func RewriteFile(p string, m Mapping) (bool, error) {
	r, err := rewriteChecked(p, m)
	if err != nil || r == nil {
		return false, err
	}
	return true, os.WriteFile(p, r.out, r.perm)
}

// RewritePaths applies m to every Go file of a package at or below the
// given paths — skipping vendor/, hidden directories, and the files the go
// tool treats as data (testdata/, _x/, _x.go), which the fold carries
// verbatim — and returns the files it changed. Every rewrite is proven
// before the first is written; a second run changes nothing. A file named
// explicitly is rewritten whatever its name.
func RewritePaths(paths []string, m Mapping) ([]string, error) {
	var files []string
	for _, named := range paths {
		if named == "" {
			// EvalSymlinks would read "" as "." — an unset $DIR would rewrite
			// the working directory.
			return nil, errors.New(`an empty path names nothing; pass "." for the working directory`)
		}
		// A root given as a link is walked where it points; data is judged
		// on the path inside the repository, so a root that is itself under
		// testdata/ or _x/ stays data.
		root, err := filepath.EvalSymlinks(named)
		if err != nil {
			return nil, err
		}
		// Absolute and physical, as the toplevel it is compared with: a
		// relative root (`rewrite .`) is the documented use, and Abs would
		// prefix the logical $PWD of a cwd reached through a link.
		if !filepath.IsAbs(root) {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
				return nil, err
			}
			root = filepath.Join(cwd, root)
		}
		base := root
		if info, err := os.Stat(root); err == nil && !info.IsDir() {
			base = filepath.Dir(root)
		}
		if top, err := Toplevel(base); err == nil {
			base = top
		}
		err = filepath.WalkDir(root, func(q string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if q != root && (name == "vendor" || strings.HasPrefix(name, ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !strings.HasSuffix(q, ".go") {
				return nil
			}
			if q != root {
				rel, err := filepath.Rel(base, q)
				if err != nil {
					return err
				}
				if !isPackageFile(filepath.ToSlash(rel)) {
					return nil
				}
			}
			files = append(files, q)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	writes, err := rewriteAll("", files, m)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, w := range writes {
		if err := os.WriteFile(w.file, w.out, w.perm); err != nil {
			return changed, err
		}
		changed = append(changed, w.file)
	}
	return changed, nil
}
