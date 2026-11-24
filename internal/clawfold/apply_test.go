package clawfold

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/testfs"
)

const consumerSrc = `package main

import "github.com/SocialGouv/claw-code-go/pkg/api"

func main() { println(api.Hello()) }
`

// fixtureSum is the h1: go.sum would hold for the fixture claw at HEAD as
// v0.0.0. (ModuleHash itself is pinned against an independent computation
// by TestModuleHashIsGoSumsH1.)
func fixtureSum(t *testing.T, src string) string {
	t.Helper()
	tree, err := ReadTree(src, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ModuleHash(tree, SourceModule, "v0.0.0")
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// newRoot builds an iterion-like checkout that vendors the fixture claw by
// hand (no go command), with one consumer, one path residual and one prose
// mention for the switch to find.
func newRoot(t *testing.T, src string) string {
	t.Helper()
	root := gittest.SourceRepo(t)
	files := map[string]string{
		"go.mod":               "module github.com/SocialGouv/iterion\n\ngo 1.26\n",
		"cmd/consumer/main.go": consumerSrc,
		"pkg/residual/r.go":    "package residual\n\nconst vendored = \"vendor/github.com/SocialGouv/claw-code-go/x\"\n",
		"docs/notes.md":        "claw-code-go folds into pkg/claw.\n",
		"vendor/modules.txt": "# github.com/SocialGouv/claw-code-go v0.0.0\n## explicit; go 1.26\n" +
			"github.com/SocialGouv/claw-code-go/internal/core\ngithub.com/SocialGouv/claw-code-go/pkg/api\n",
		"go.sum": SourceModule + " v0.0.0 " + fixtureSum(t, src) + "\n",
	}
	for _, p := range vendoredFixture {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		files[VendorDir+"/"+p] = string(data)
	}
	writeTree(t, root, files)
	commitAll(t, root, "root")
	return root
}

type fakeGo struct {
	calls    [][]string
	edits    [][]byte         // successive outputs of `go mod edit -json`
	onVendor func(dir string) // what `go mod vendor` does to the tree, if anything
}

func (f *fakeGo) Run(dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if slices.Equal(args, []string{"mod", "vendor"}) && f.onVendor != nil {
		f.onVendor(dir)
	}
	if slices.Equal(args, []string{"mod", "edit", "-json"}) {
		if len(f.edits) == 0 {
			return []byte(`{"Require":[]}`), nil
		}
		out := f.edits[0]
		f.edits = f.edits[1:]
		return out, nil
	}
	return nil, nil
}

// fixturePlan loads the fixture through the same entry point the command
// uses, with caps small enough to give several Go slices.
func fixturePlan(t *testing.T) (*Plan, string) {
	t.Helper()
	src, pin := newSource(t)
	root := newRoot(t, src)
	c, err := Load(Inputs{Root: root, Source: src, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	p, err := BuildPlan(c, goFloor(t, c), 40)
	if err != nil {
		t.Fatal(err)
	}
	return p, root
}

// goFloor returns the largest Go unit's size, the smallest cap a plan takes.
func goFloor(t *testing.T, c *Census) int {
	t.Helper()
	_, err := Partition(c, 1, 1<<30)
	var floor *FloorError
	if !errors.As(err, &floor) {
		t.Fatalf("a cap of 1 line was accepted: %v", err)
	}
	return floor.Lines
}

func landAll(t *testing.T, p *Plan, root string, tool GoTool) {
	t.Helper()
	for k := 1; k <= len(p.Slices); k++ {
		if _, err := p.Apply(root, k, tool); err != nil {
			t.Fatalf("apply slice %d: %v", k, err)
		}
		commitAll(t, root, "slice")
	}
}

func TestApplyLandsEachSliceThenWritesNothingTheSecondTime(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	if kinds := []SliceKind{p.Slices[0].Kind, p.Slices[len(p.Slices)-1].Kind}; kinds[0] != SliceAssets || kinds[1] != SliceGo || len(p.Slices) < 3 {
		t.Fatalf("fixture plan too small to prove the slicing: %+v", p.Slices)
	}
	tool := &fakeGo{}
	for k := 1; k <= len(p.Slices); k++ {
		before := len(tool.calls)
		r, err := p.Apply(root, k, tool)
		if err != nil {
			t.Fatalf("apply slice %d: %v", k, err)
		}
		if r.Skipped != 0 || len(r.Written) != p.Slices[k-1].Files {
			t.Fatalf("slice %d: wrote %d, skipped %d, want %d written", k, len(r.Written), r.Skipped, p.Slices[k-1].Files)
		}
		ran := fmt.Sprint(tool.calls[before:])
		want := "[]"
		if p.Slices[k-1].Kind == SliceGo {
			want = "[[mod edit -json] [mod tidy] [mod vendor] [mod edit -json]]"
		}
		if ran != want {
			t.Fatalf("slice %d (%s) ran %v, want %v", k, p.Slices[k-1].Kind, ran, want)
		}
		commitAll(t, root, "slice")
	}
	info, err := os.Stat(filepath.Join(root, "pkg/claw/internal/core/skills/run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("run.sh landed without its executable bit (%v, %v)", info, err)
	}
	for k := 1; k <= len(p.Slices); k++ {
		r, err := p.Apply(root, k, tool)
		if err != nil {
			t.Fatalf("second apply of slice %d: %v", k, err)
		}
		if len(r.Written) != 0 || r.Skipped != p.Slices[k-1].Files {
			t.Fatalf("second apply of slice %d wrote %v", k, r.Written)
		}
	}
	if status := gittest.Run(t, root, "status", "--porcelain"); status != "" {
		t.Fatalf("a second apply left the tree changed:\n%s", status)
	}
	v, err := p.Verify(root)
	if err != nil || v.Absent != 0 || len(v.Findings) != 0 || v.Checked != len(p.Members) {
		t.Fatalf("verify after the fold: %+v, %v", v, err)
	}
}

func TestApplyRefusesATreeThatIsNotAPrefixOfThePlan(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	if _, err := p.Apply(root, 2, &fakeGo{}); !errors.Is(err, ErrNotAPrefix) {
		t.Fatalf("slice 2 before slice 1: err = %v, want ErrNotAPrefix", err)
	}
	if _, err := p.Apply(root, 1, &fakeGo{}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "slice 1")
	later := p.members(p.Slices[2])[0]
	if err := writeMember(root, later); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "a file of slice 3")
	_, err := p.Apply(root, 2, &fakeGo{})
	if !errors.Is(err, ErrNotAPrefix) || !strings.Contains(err.Error(), "other caps") {
		t.Fatalf("slice 3 present, slice 2 absent: err = %v, want ErrNotAPrefix naming the caps", err)
	}
	if _, err := p.Apply(root, len(p.Slices)+1, &fakeGo{}); err == nil {
		t.Fatal("a slice past the plan was accepted")
	}
}

func TestApplyRefusesAConflictAndWritesNothing(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	first := p.members(p.Slices[0])
	taken := filepath.Join(root, filepath.FromSlash(first[0].Dest))
	if err := os.MkdirAll(filepath.Dir(taken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taken, []byte("somebody else's bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := p.Apply(root, 1, &fakeGo{})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), first[0].Dest) {
		t.Fatalf("err = %v, want ErrConflict naming %s", err, first[0].Dest)
	}
	for _, m := range first[1:] {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(m.Dest))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was written although the slice was refused", m.Dest)
		}
	}
}

func TestApplyRefusesADirtyModule(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/SocialGouv/iterion\n\ngo 1.26\n// wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Apply(root, 1, &fakeGo{}); !errors.Is(err, ErrDirtyModule) {
		t.Fatalf("err = %v, want ErrDirtyModule", err)
	}
}

// onePlan isolates one member in a plan of its own, so a scenario can
// tamper with the plan without touching the shared one.
func onePlan(m Member) *Plan {
	return &Plan{
		Census: &Census{Members: []*Member{&m}},
		Slices: []Slice{{Index: 1, Units: []*Unit{{Files: []string{m.Path}}}}},
	}
}

func findMember(t *testing.T, p *Plan, path string) *Member {
	t.Helper()
	for _, m := range p.Members {
		if m.Path == path {
			return m
		}
	}
	t.Fatalf("no member %s", path)
	return nil
}

func problems(t *testing.T, p *Plan, root string) string {
	t.Helper()
	v, err := p.Verify(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, f := range v.Findings {
		all = append(all, f.Problem)
	}
	return strings.Join(all, " | ")
}

// Each guard of verify gets a scenario only it can name: the assertion is on
// that guard's own wording.
func TestVerifyNamesEachBrokenProof(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	api := *findMember(t, p, "pkg/api/api.go")
	dst := filepath.Join(root, filepath.FromSlash(api.Dest))
	restore := func(m Member) {
		t.Helper()
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(m.Dest))); err != nil {
			t.Fatal(err)
		}
		if err := writeMember(root, &m); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("blob: the pin's bytes landed without the rewrite", func(t *testing.T) {
		if err := os.WriteFile(dst, api.src, 0o644); err != nil {
			t.Fatal(err)
		}
		defer restore(api)
		if got := problems(t, onePlan(api), root); !strings.Contains(got, "the fold wrote") {
			t.Fatalf("problems = %q, want the blob guard", got)
		}
	})

	t.Run("inverse: the plan's rewrite carried another edit", func(t *testing.T) {
		bad := api
		bad.dest = bytes.Replace(api.dest, []byte("return core.Name()"), []byte("return core.Name()+\"\""), 1)
		bad.DestBlob = BlobID(bad.dest, len(api.DestBlob))
		if err := os.WriteFile(dst, bad.dest, 0o644); err != nil {
			t.Fatal(err)
		}
		defer restore(api)
		if got := problems(t, onePlan(bad), root); !strings.Contains(got, "unrewrite gives blob") || strings.Contains(got, "the fold wrote") {
			t.Fatalf("problems = %q, want the inverse guard alone of the two", got)
		}
	})

	t.Run("lines: the plan misrecorded which lines the rewrite touched", func(t *testing.T) {
		bad := api
		bad.RewrittenLines = []int{1}
		if got := problems(t, onePlan(bad), root); !strings.Contains(got, "the rewrite touches [1]") {
			t.Fatalf("problems = %q, want the changed-lines guard", got)
		}
	})

	t.Run("mode: the executable bit is lost", func(t *testing.T) {
		run := *findMember(t, p, "internal/core/skills/run.sh")
		path := filepath.Join(root, filepath.FromSlash(run.Dest))
		if err := os.Chmod(path, 0o654); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chmod(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}()
		if got := problems(t, onePlan(run), root); !strings.Contains(got, "mode -rw-r-xr--, the pin's is 100755") {
			t.Fatalf("problems = %q, want the mode guard", got)
		}
	})

	t.Run("asset: a verbatim file was edited", func(t *testing.T) {
		md := *findMember(t, p, "internal/core/skills/a.md")
		path := filepath.Join(root, filepath.FromSlash(md.Dest))
		if err := os.WriteFile(path, []byte("# edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		defer restore(md)
		if got := problems(t, onePlan(md), root); !strings.Contains(got, "an asset differs") {
			t.Fatalf("problems = %q, want the asset guard", got)
		}
	})

	if v, err := p.Verify(root); err != nil || len(v.Findings) != 0 {
		t.Fatalf("the scenarios did not restore the fold: %+v, %v", v, err)
	}
}

func TestVerifyCountsAbsentFilesAndAnExplicitSliceNamesThem(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	v, err := p.Verify(root)
	if err != nil || v.Absent != len(p.Members) || len(v.Findings) != 0 {
		t.Fatalf("verify of an unfolded tree: %+v, %v — absence is a count over the whole plan", v, err)
	}
	v, err = p.Verify(root, 1)
	if err != nil || len(v.Findings) != p.Slices[0].Files || !strings.Contains(v.Findings[0].Problem, "absent") {
		t.Fatalf("verify -slice 1 of an unfolded tree: %+v, %v — absence is a finding for a named slice", v, err)
	}
}

const (
	modBefore = `{"Require":[{"Path":"github.com/SocialGouv/claw-code-go","Version":"v0.1.1-0.20261106142829-71976d7241ee"},{"Path":"example.com/moved","Version":"v1.0.0","Indirect":true},{"Path":"example.com/flag","Version":"v1.0.0","Indirect":true}]}`
	modAfter  = `{"Require":[{"Path":"example.com/moved","Version":"v1.1.0","Indirect":true},{"Path":"example.com/flag","Version":"v1.0.0"},{"Path":"example.com/new","Version":"v0.1.0"}]}`
)

func TestApplySwitchNeedsTheWholeFoldThenMovesTheConsumers(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	if _, _, err := p.ApplySwitch(root, &fakeGo{}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "a fold member git does not hold") {
		t.Fatalf("switch before the fold: err = %v, want ErrDirtyTree naming the members git does not hold", err)
	}
	landAll(t, p, root, &fakeGo{})
	dropVendor := func(dir string) { testfs.RemoveAll(t, dir, filepath.Join(dir, filepath.FromSlash(VendorDir))) }
	tool := &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modAfter)}, onVendor: dropVendor}
	r, sp, err := p.ApplySwitch(root, tool)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Written, []string{"cmd/consumer/main.go"}) || sp.ImportSites != 1 {
		t.Fatalf("switch wrote %v (%d sites), want the one consumer", r.Written, sp.ImportSites)
	}
	got, err := os.ReadFile(filepath.Join(root, "cmd/consumer/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(consumerSrc, "github.com/SocialGouv/claw-code-go/", "github.com/SocialGouv/iterion/pkg/claw/", 1); string(got) != want {
		t.Fatalf("consumer after the switch:\n%s", got)
	}
	wantDelta := []string{
		"VERSION MOVED example.com/moved v1.0.0 -> v1.1.0",
		"added example.com/new v0.1.0",
		"indirect true -> false example.com/flag",
		"removed github.com/SocialGouv/claw-code-go v0.1.1-0.20261106142829-71976d7241ee",
	}
	if !slices.Equal(r.ModDelta, wantDelta) {
		t.Fatalf("go.mod delta = %q, want %q", r.ModDelta, wantDelta)
	}
	if len(sp.PathResiduals) != 1 || sp.PathResiduals[0].File != "pkg/residual/r.go" || sp.ProseFiles != 1 {
		t.Fatalf("residuals = %+v, prose files %d", sp.PathResiduals, sp.ProseFiles)
	}
}

func TestApplySwitchRefusesBeforeWritingAnything(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	mixed := "package mixed\n\nimport (\n\t_ \"github.com/SocialGouv/claw-code-go/pkg/api\"\n\t_ \"github.com/SocialGouv/iterion/pkg/claw/internal/core\"\n)\n"
	writeTree(t, root, map[string]string{"zz/mixed/m.go": mixed})
	commitAll(t, root, "a consumer the inverse could not undo")
	if _, _, err := p.ApplySwitch(root, &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modAfter)}}); err == nil || !strings.Contains(err.Error(), "exact inverse") {
		t.Fatalf("err = %v, want the inverse refusal", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "cmd/consumer/main.go")); string(got) != consumerSrc {
		t.Fatal("a refused switch rewrote a consumer anyway")
	}
}

func TestRewritePathsSkipsVendorAndHiddenTreesAndUndoesExactly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	files := map[string]string{
		"a/a.go":         consumerSrc,
		"vendor/v/v.go":  consumerSrc,
		".hidden/h/h.go": consumerSrc,
		"a/notes.txt":    "github.com/SocialGouv/claw-code-go/pkg/api\n",
	}
	writeTree(t, dir, files)
	changed, err := RewritePaths([]string{dir}, Fold)
	if err != nil || len(changed) != 1 || changed[0] != filepath.Join(dir, "a/a.go") {
		t.Fatalf("rewrite changed %v, %v; want a/a.go alone", changed, err)
	}
	if again, err := RewritePaths([]string{dir}, Fold); err != nil || len(again) != 0 {
		t.Fatalf("a second rewrite changed %v, %v", again, err)
	}
	if back, err := RewritePaths([]string{dir}, Fold.Inverse()); err != nil || len(back) != 1 {
		t.Fatalf("unrewrite changed %v, %v", back, err)
	}
	for rel, body := range files {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(got) != body {
			t.Fatalf("%s after rewrite+unrewrite differs (%v)", rel, err)
		}
	}
}

// goEnv is the environment the real go command runs under in a test:
// offline, its module cache a directory this test owns and may delete.
func goEnv(t *testing.T) []string {
	t.Helper()
	env := []string{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "GOFLAGS", "GOPROXY", "GOWORK", "GOTOOLCHAIN", "GOMODCACHE", "GOSUMDB", "GONOSUMDB", "GOPRIVATE":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GOFLAGS=-modcacherw",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOMODCACHE="+t.TempDir(),
	)
}

func goRun(t *testing.T, tool GoTool, dir string, args ...string) {
	t.Helper()
	if out, err := tool.Run(dir, args...); err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// The whole fold of the fixture with the real go command: the vendored set
// is what `go mod vendor` keeps, every slice builds and tests on what landed
// before it, and the switch leaves a checkout that builds without the
// module.
func TestTheFoldBuildsAndTestsSliceBySliceWithTheRealGoCommand(t *testing.T) {
	t.Parallel()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is required: %v", err)
	}
	tool := goBinary{path: goPath, env: goEnv(t)}
	src, pin := newSource(t)
	root := gittest.SourceRepo(t)
	writeTree(t, root, map[string]string{
		"go.mod":               "module github.com/SocialGouv/iterion\n\ngo 1.26\n\nrequire github.com/SocialGouv/claw-code-go v0.0.0\n\nreplace github.com/SocialGouv/claw-code-go => " + src + "\n",
		"cmd/consumer/main.go": consumerSrc,
		// a workspace around the checkout: `go mod vendor` refuses to run
		// in one, so the tool must step out of it itself
		"go.work": "go 1.26\n\nuse .\n",
	})
	goRun(t, tool, root, "mod", "tidy")
	goRun(t, tool, root, "mod", "vendor")
	commitAll(t, root, "iterion vendoring the fixture claw")

	// A local replace: go.sum holds no h1: for it, so nothing published
	// anchors the tree — the one place a test says so.
	c, err := Load(Inputs{Root: root, Source: src, Pin: pin, Unsummed: true})
	if err != nil {
		t.Fatal(err)
	}
	var vendored []string
	for _, m := range c.Members {
		if m.Origin == OriginVendored {
			vendored = append(vendored, m.Path)
		}
	}
	if !slices.Equal(vendored, vendoredFixture) {
		t.Fatalf("go mod vendor kept %v, the fixture's model of it is %v", vendored, vendoredFixture)
	}
	p, err := BuildPlan(c, goFloor(t, c), 40)
	if err != nil {
		t.Fatal(err)
	}
	goSlices := 0
	for k := 1; k <= len(p.Slices); k++ {
		if _, err := p.Apply(root, k, tool); err != nil {
			t.Fatalf("apply slice %d: %v", k, err)
		}
		commitAll(t, root, "slice")
		if p.Slices[k-1].Kind != SliceGo {
			continue
		}
		goSlices++
		goRun(t, tool, root, "build", "./...")
		goRun(t, tool, root, "test", "-count=1", "./pkg/claw/...")
	}
	if goSlices < 2 {
		t.Fatalf("the fixture plan has %d Go slice(s); the test needs several to prove each lands alone", goSlices)
	}
	if v, err := p.Verify(root); err != nil || v.Absent != 0 || len(v.Findings) != 0 {
		t.Fatalf("verify: %+v, %v", v, err)
	}

	r, _, err := p.ApplySwitch(root, tool)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.ModDelta, "removed github.com/SocialGouv/claw-code-go v0.0.0") {
		t.Fatalf("go.mod delta = %q, want the module removed", r.ModDelta)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(VendorDir))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the vendored module survived the switch: %v", err)
	}
	goRun(t, tool, root, "build", "./...")
	goRun(t, tool, root, "test", "-count=1", "./pkg/claw/...")
}

func TestApplySwitchRefusesToReportSuccessWhileTheModuleStays(t *testing.T) {
	t.Parallel()
	t.Run("vendored copy survives", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		_, _, err := p.ApplySwitch(root, &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modAfter)}})
		if err == nil || !strings.Contains(err.Error(), "survived go mod vendor") {
			t.Fatalf("err = %v, want the vendored copy named", err)
		}
	})
	t.Run("module still required", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		_, _, err := p.ApplySwitch(root, &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modBefore)}})
		if err == nil || !strings.Contains(err.Error(), "still requires") {
			t.Fatalf("err = %v, want the requirement named", err)
		}
	})
}

func TestApplySwitchRefusesUncommittedGoChanges(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	writeTree(t, root, map[string]string{"wip/w.go": consumerSrc})
	if _, _, err := p.ApplySwitch(root, &fakeGo{}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "wip/") {
		t.Fatalf("err = %v, want ErrDirtyTree naming the untracked consumer", err)
	}
}

// unsortedAfter imports the module before a path that the rewrite sorts it
// after: gofmt-clean before the rewrite, not after it unless re-sorted.
const unsortedAfter = `package resort

import (
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend"
)

var _, _ = api.Hello, backend.X
`

func TestTheSwitchLeavesConsumersGofmtClean(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	writeTree(t, root, map[string]string{"pkg/resort/r.go": unsortedAfter})
	commitAll(t, root, "a consumer gofmt re-sorts")
	landAll(t, p, root, &fakeGo{})
	dropVendor := func(dir string) { testfs.RemoveAll(t, dir, filepath.Join(dir, filepath.FromSlash(VendorDir))) }
	r, _, err := p.ApplySwitch(root, &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modAfter)}, onVendor: dropVendor})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg/resort/r.go"))
	if err != nil {
		t.Fatal(err)
	}
	if clean, err := GofmtStable(got); err != nil || !clean {
		t.Fatalf("the switch left pkg/resort/r.go gofmt-dirty (%v):\n%s", err, got)
	}
	if !slices.Equal(r.Resorted, []string{"pkg/resort/r.go"}) {
		t.Fatalf("resorted = %v, want the one re-sorted consumer", r.Resorted)
	}
	back, _, err := Fold.Inverse().Rewrite("r.go", got)
	if err != nil {
		t.Fatal(err)
	}
	if formatted, _ := format.Source(back); string(formatted) != unsortedAfter {
		t.Fatalf("gofmt of the inverse does not give the consumer back:\n%s", formatted)
	}
}

func TestStraysUnderTheDestinationAreRefusedAndReported(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	if _, err := p.Apply(root, 1, &fakeGo{}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "slice 1")
	writeTree(t, root, map[string]string{"pkg/claw/pkg/api/zz_extra.go": "package api\n\nfunc init() { panic(\"not from the pin\") }\n"})
	if _, err := p.Apply(root, 2, &fakeGo{}); !errors.Is(err, ErrNotAPrefix) || !strings.Contains(err.Error(), "zz_extra.go") {
		t.Fatalf("apply over a stray: err = %v, want ErrNotAPrefix naming it", err)
	}
	v, err := p.Verify(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range v.Findings {
		found = found || (f.Dest == "pkg/claw/pkg/api/zz_extra.go" && strings.Contains(f.Problem, "not in the plan"))
	}
	if !found {
		t.Fatalf("verify findings = %+v, want the stray named", v.Findings)
	}
}

func TestSymbolicLinksOnTheWayAreNeverFollowed(t *testing.T) {
	t.Parallel()
	t.Run("a member replaced by a link to the right bytes", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		lic := findMember(t, p, "LICENSE")
		dst := filepath.Join(root, filepath.FromSlash(lic.Dest))
		copyPath := filepath.Join(t.TempDir(), "LICENSE")
		if err := os.WriteFile(copyPath, lic.dest, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(dst); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copyPath, dst); err != nil {
			t.Fatal(err)
		}
		if got := problems(t, onePlan(*lic), root); !strings.Contains(got, "LICENSE is a symbolic link") {
			t.Fatalf("problems = %q, want the link refused", got)
		}
		if st, why, _ := fileState(root, lic, nil); st != differs || !strings.Contains(why, "symbolic link") {
			t.Fatalf("apply reads the link as %v (%s), want a conflict", st, why)
		}
	})
	t.Run("the destination directory is a link", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		outside := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, DestDir)); err != nil {
			t.Fatal(err)
		}
		_, err := p.Apply(root, 1, &fakeGo{})
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("err = %v, want the link refused", err)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatalf("apply wrote %d entries through the link", len(entries))
		}
	})
}

func TestTheModeIsPartOfTheStateApplyCompares(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	run := findMember(t, p, "internal/core/skills/run.sh")
	// Group may execute, the owner may not: git records 100644, which an
	// "any execute bit" reading would miss.
	if err := os.Chmod(filepath.Join(root, filepath.FromSlash(run.Dest)), 0o654); err != nil {
		t.Fatal(err)
	}
	k := 0
	for i, s := range p.Slices {
		for _, u := range s.Units {
			if slices.Contains(u.Files, run.Path) {
				k = i + 1
			}
		}
	}
	if _, err := p.Apply(root, k, &fakeGo{}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the fold writes 100755") {
		t.Fatalf("err = %v, want the lost exec bit named as a conflict", err)
	}
}

func TestVerifyReadsWhatTheIndexRecords(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	run := findMember(t, p, "internal/core/skills/run.sh")
	gittest.Run(t, root, "update-index", "--chmod=-x", run.Dest)
	if got := problems(t, onePlan(*run), root); !strings.Contains(got, "the index records 100644") {
		t.Fatalf("problems = %q, want the index's mode named", got)
	}
}

func TestRewritePathsProvesEveryFileBeforeWritingAndSkipsData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := "package z\n\nimport (\n\t_ \"github.com/SocialGouv/claw-code-go/a\"\n\t_ \"github.com/SocialGouv/iterion/pkg/claw/b\"\n)\n"
	writeTree(t, dir, map[string]string{
		"a/a.go":             consumerSrc,
		"z/z.go":             bad,
		"a/testdata/data.go": consumerSrc,
		"a/_tmpl.go":         consumerSrc,
	})
	if _, err := RewritePaths([]string{dir}, Fold); err == nil || !strings.Contains(err.Error(), "exact inverse") {
		t.Fatalf("err = %v, want the refusal of z/z.go", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a/a.go")); string(got) != consumerSrc {
		t.Fatal("a/a.go was rewritten although another file of the run was refused")
	}
	if err := os.Remove(filepath.Join(dir, "z/z.go")); err != nil {
		t.Fatal(err)
	}
	changed, err := RewritePaths([]string{dir}, Fold)
	if err != nil || len(changed) != 1 || changed[0] != filepath.Join(dir, "a/a.go") {
		t.Fatalf("changed %v, %v: want a/a.go alone, the data files left verbatim", changed, err)
	}
}

func TestTheSwitchProvesACommittedFold(t *testing.T) {
	t.Parallel()
	dropVendor := func(t *testing.T) func(string) {
		return func(dir string) { testfs.RemoveAll(t, dir, filepath.Join(dir, filepath.FromSlash(VendorDir))) }
	}
	t.Run("a member git does not hold", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		md := findMember(t, p, "internal/core/skills/a.md")
		gittest.Run(t, root, "rm", "-q", "--cached", md.Dest)
		gittest.Run(t, root, "commit", "-q", "-m", "drop a member from git, keep it on disk")
		if _, _, err := p.ApplySwitch(root, &fakeGo{onVendor: dropVendor(t)}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), md.Dest) {
			t.Fatalf("err = %v, want ErrDirtyTree naming %s", err, md.Dest)
		}
	})
	t.Run("an ignored consumer the go tool compiles", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		writeTree(t, root, map[string]string{".gitignore": "wip/\n"})
		commitAll(t, root, "ignore wip/")
		writeTree(t, root, map[string]string{"wip/w.go": consumerSrc})
		if _, _, err := p.ApplySwitch(root, &fakeGo{onVendor: dropVendor(t)}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "wip/w.go") {
			t.Fatalf("err = %v, want ErrDirtyTree naming the ignored consumer", err)
		}
	})
	t.Run("a tracked consumer missing on disk", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		if err := os.Remove(filepath.Join(root, "cmd/consumer/main.go")); err != nil {
			t.Fatal(err)
		}
		if _, err := PlanSwitch(root); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "not on disk") {
			t.Fatalf("PlanSwitch: err = %v, want the missing consumer refused", err)
		}
	})
	t.Run("a tracked Go file changed and not committed", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		writeTree(t, root, map[string]string{"cmd/consumer/main.go": consumerSrc + "\n// work in progress\n"})
		if _, _, err := p.ApplySwitch(root, &fakeGo{onVendor: dropVendor(t)}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "cmd/consumer/main.go") {
			t.Fatalf("err = %v, want ErrDirtyTree naming the changed consumer", err)
		}
	})
	t.Run("a committed member with other bytes", func(t *testing.T) {
		t.Parallel()
		p, root := fixturePlan(t)
		landAll(t, p, root, &fakeGo{})
		md := findMember(t, p, "internal/core/skills/a.md")
		writeTree(t, root, map[string]string{md.Dest: "# committed, not the pin\n"})
		commitAll(t, root, "a member edited and committed")
		if _, _, err := p.ApplySwitch(root, &fakeGo{onVendor: dropVendor(t)}); !errors.Is(err, ErrNotAPrefix) {
			t.Fatalf("err = %v, want ErrNotAPrefix from verify", err)
		}
	})
}

func TestApplyJudgesWhatTheIndexRecords(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	run := findMember(t, p, "internal/core/skills/run.sh")
	gittest.Run(t, root, "update-index", "--chmod=-x", run.Dest)
	if _, err := p.Apply(root, 1, &fakeGo{}); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the index records 100644") {
		t.Fatalf("err = %v, want the index's mode named as a conflict", err)
	}
}

func TestAnInterruptedApplysLeftoverIsNamed(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	writeTree(t, root, map[string]string{"pkg/claw/internal/core/.clawfold-123": "half a file"})
	if _, err := p.Apply(root, 1, &fakeGo{}); !errors.Is(err, ErrNotAPrefix) || !strings.Contains(err.Error(), "left by an interrupted apply") {
		t.Fatalf("err = %v, want the leftover named as one", err)
	}
}

func TestApplyNamesWhatASlicesTidyMovesInGoMod(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	for k := 1; k <= len(p.Slices); k++ {
		tool := &fakeGo{}
		if p.Slices[k-1].Kind == SliceGo {
			tool.edits = [][]byte{[]byte(modBefore), []byte(modAfter)}
		}
		r, err := p.Apply(root, k, tool)
		if err != nil {
			t.Fatal(err)
		}
		commitAll(t, root, "slice")
		if p.Slices[k-1].Kind == SliceGo {
			if !slices.Contains(r.ModDelta, "VERSION MOVED example.com/moved v1.0.0 -> v1.1.0") {
				t.Fatalf("slice %d: go.mod delta = %q, want the moved version named", k, r.ModDelta)
			}
			return
		}
	}
	t.Fatal("no Go slice in the fixture plan")
}

func TestARewriteGofmtWouldDetachCommentsFromIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	commented := `package c

import (
	// the claw client
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend"
)

var _, _ = api.Hello, backend.X
`
	writeTree(t, dir, map[string]string{"c/c.go": commented})
	if _, err := RewritePaths([]string{dir}, Fold); err == nil || !strings.Contains(err.Error(), "holds comments") {
		t.Fatalf("err = %v, want the commented re-sort refused", err)
	}
}

func TestRewritePathsJudgesDataFromTheRepositoryNotTheRoot(t *testing.T) {
	t.Parallel()
	repo := gittest.SourceRepo(t)
	writeTree(t, repo, map[string]string{
		"mod/a/testdata/data.go": consumerSrc,
		"mod/b/b.go":             consumerSrc,
	})
	changed, err := RewritePaths([]string{filepath.Join(repo, "mod/a/testdata")}, Fold)
	if err != nil || len(changed) != 0 {
		t.Fatalf("a root under testdata/ rewrote %v (%v): its files are data", changed, err)
	}
	link := filepath.Join(t.TempDir(), "b-link")
	if err := os.Symlink(filepath.Join(repo, "mod/b"), link); err != nil {
		t.Fatal(err)
	}
	changed, err = RewritePaths([]string{link}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("a root given as a link rewrote %v (%v), want the file it leads to", changed, err)
	}
}

func TestTheSwitchIgnoresWhatTheGoToolDoesNotCompile(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	writeTree(t, root, map[string]string{"tools/go.mod": "module example.com/tools\n\ngo 1.26\n"})
	commitAll(t, root, "a nested module")
	landAll(t, p, root, &fakeGo{})
	writeTree(t, root, map[string]string{
		"tools/scratch.go":           consumerSrc,
		"cmd/consumer/testdata/x.go": consumerSrc,
		"cmd/consumer/_draft.go":     consumerSrc,
	})
	dropVendor := func(dir string) { testfs.RemoveAll(t, dir, filepath.Join(dir, filepath.FromSlash(VendorDir))) }
	if _, _, err := p.ApplySwitch(root, &fakeGo{edits: [][]byte{[]byte(modBefore), []byte(modAfter)}, onVendor: dropVendor}); err != nil {
		t.Fatalf("untracked files the go tool never compiles refused the switch: %v", err)
	}
}

func TestTheSwitchSeesTheTreeThroughALinkedRoot(t *testing.T) {
	t.Parallel()
	p, root := fixturePlan(t)
	landAll(t, p, root, &fakeGo{})
	writeTree(t, root, map[string]string{"cmd/wip/main.go": consumerSrc})
	link := filepath.Join(t.TempDir(), "iterion")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ApplySwitch(link, &fakeGo{}); !errors.Is(err, ErrDirtyTree) || !strings.Contains(err.Error(), "cmd/wip/main.go") {
		t.Fatalf("err = %v, want the untracked consumer named through the link", err)
	}
}

func TestARealignmentIsNotAResort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	aligned := `package c

import (
	"fmt"                                       // printing
	"github.com/SocialGouv/claw-code-go/pkg/api" // the claw API
)

var _, _ = fmt.Sprint, api.Hello
`
	clean, err := format.Source([]byte(aligned))
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"c/c.go": string(clean)})
	changed, err := RewritePaths([]string{dir}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, %v: comments that only move column are not left behind", changed, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "c/c.go"))
	if clean, err := GofmtStable(got); err != nil || !clean {
		t.Fatalf("the rewrite is not gofmt-clean:\n%s", got)
	}
}

// Not parallel: it changes the working directory, as `clawfold rewrite .`
// run from the repository does.
func TestRewritePathsTakesRelativeRoots(t *testing.T) {
	repo := gittest.SourceRepo(t)
	writeTree(t, repo, map[string]string{"mod/b/b.go": consumerSrc, "mod/c/c.go": consumerSrc})
	t.Chdir(repo)
	changed, err := RewritePaths([]string{"mod/b"}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("rewrite mod/b: %v, %v", changed, err)
	}
	changed, err = RewritePaths([]string{"."}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("rewrite .: %v, %v — want mod/c alone, mod/b already done", changed, err)
	}
}

func TestACommentInAnotherBlockDoesNotRefuseTheReSort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	twoBlocks := `package c

import (
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend"
)

import (
	// formatting
	"fmt"
)

var _, _, _ = api.Hello, backend.X, fmt.Sprint
`
	writeTree(t, dir, map[string]string{"c/c.go": twoBlocks})
	changed, err := RewritePaths([]string{dir}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, %v: the comment sits in a block gofmt does not re-sort", changed, err)
	}
}

// Not parallel: it changes the working directory to one reached through a
// link, whose logical $PWD is not the physical path git answers with.
func TestRewritePathsResolvesACwdReachedThroughALink(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "real", "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, repo)
	writeTree(t, base, map[string]string{
		"real/repo/cmd/a.go": consumerSrc,
		"real/sibling/x.go":  consumerSrc,
		"sibling/x.go":       consumerSrc, // what a logical "../sibling" would name
	})
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	changed, err := RewritePaths([]string{"."}, Fold)
	if err != nil || len(changed) != 1 {
		t.Fatalf("rewrite . from a linked cwd: %v, %v — want cmd/a.go", changed, err)
	}
	if _, err := RewritePaths([]string{"../sibling"}, Fold); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(base, "sibling/x.go")); string(got) != consumerSrc {
		t.Fatal("../sibling was read logically: the file beside the link was rewritten")
	}
	if got, _ := os.ReadFile(filepath.Join(base, "real/sibling/x.go")); string(got) == consumerSrc {
		t.Fatal("../sibling, physically real/sibling, was not rewritten")
	}
}

func TestRewritePathsRefusesAnEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := RewritePaths([]string{""}, Fold); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("err = %v, want an empty path refused", err)
	}
}
