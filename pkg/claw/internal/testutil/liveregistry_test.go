package testutil

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testfs"
)

const (
	clawModulePrefix = "github.com/SocialGouv/iterion/pkg/claw/"
	apikitPackage    = clawModulePrefix + "internal/apikit"
)

// Every package of pkg/claw whose test binary links the model registry runs
// off the live one: its TestMain calls IsolateLiveRegistry. The set is read
// from the go tool's own dependency graph, so a test that starts linking the
// registry arrives isolated or red here.
func TestEveryTestBinaryLinkingTheRegistryIsIsolated(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("go", "list", "-test", "-f", "{{.ImportPath}}|{{join .Deps \" \"}}", "./pkg/claw/...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -test ./pkg/claw/...: %v", err)
	}
	testFiles := packageTestFiles(t, root)
	var linking, missing []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		binary, deps, ok := strings.Cut(line, "|")
		if !ok || !strings.HasSuffix(binary, ".test") {
			continue
		}
		pkg := strings.TrimSuffix(binary, ".test")
		if !slices.Contains(strings.Fields(deps), apikitPackage) {
			continue
		}
		linking = append(linking, pkg)
		if !testMainIsolates(t, testFiles[pkg]) {
			missing = append(missing, pkg)
		}
	}
	// The registry's own package links itself: a census without it read
	// nothing.
	if !slices.Contains(linking, apikitPackage) {
		t.Fatalf("no test binary links %s (found %v): the census no longer reads the dependency graph", apikitPackage, linking)
	}
	if len(missing) > 0 {
		t.Errorf("test binaries that link %s without isolating the live registry — add\n\tfunc TestMain(m *testing.M) { os.Exit(testutil.IsolateLiveRegistry(m.Run)) }\nto:\n  %s",
			apikitPackage, strings.Join(missing, "\n  "))
	}
}

// testMainIsolates reports whether pkg's TestMain — in a test file the go
// tool compiles for this platform and these tags — is exactly
// os.Exit(testutil.IsolateLiveRegistry(m.Run)): one statement, read from
// the syntax tree, so a call in a dead branch, after os.Exit or in a closure
// never called does not count.
func testMainIsolates(t *testing.T, files []string) bool {
	t.Helper()
	for _, name := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		testutilName := importName(f, "github.com/SocialGouv/iterion/pkg/claw/internal/testutil")
		osName := importName(f, "os")
		if testutilName == "" || osName == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil ||
				len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 || len(fn.Body.List) != 1 {
				continue
			}
			var stmt strings.Builder
			if err := printer.Fprint(&stmt, fset, fn.Body.List[0]); err != nil {
				t.Fatalf("print %s: %v", name, err)
			}
			m := fn.Type.Params.List[0].Names[0].Name
			if stmt.String() == osName+".Exit("+testutilName+".IsolateLiveRegistry("+m+".Run))" {
				return true
			}
		}
	}
	return false
}

// packageTestFiles maps each package of pkg/claw to the paths of the test
// files the go tool compiles for it, internal and external.
func packageTestFiles(t *testing.T, root string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{.Dir}}|{{join .TestGoFiles \" \"}} {{join .XTestGoFiles \" \"}}", "./pkg/claw/...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./pkg/claw/...: %v", err)
	}
	files := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("go list line %q: want import path|dir|files", line)
		}
		for _, name := range strings.Fields(parts[2]) {
			files[parts[0]] = append(files[parts[0]], filepath.Join(parts[1], name))
		}
	}
	return files
}

// importName is the name f refers to path by, or "" when f does not import it.
func importName(f *ast.File, path string) string {
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return filepath.Base(path)
	}
	return ""
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatalf("no go.mod above %s", dir)
		}
	}
}

// removeOwnTempDir removes the directory IsolateLiveRegistry created, and
// nothing that took its place.
func TestRemoveOwnTempDirRemovesOnlyTheDirectoryItCreated(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	create := func() (string, *os.File) {
		t.Helper()
		dir, held, err := createOwnTempDir(liveRegistryCachePrefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = held.Close() })
		return dir, held
	}
	refused := func(what, dir string, held *os.File) {
		t.Helper()
		if err := removeOwnTempDir(dir, held, liveRegistryCachePrefix); err == nil {
			t.Fatalf("%s was accepted", what)
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Fatalf("%s is gone: %v", what, err)
		}
	}

	dir, held := create()
	if err := removeOwnTempDir(dir, held, liveRegistryCachePrefix); err != nil {
		t.Fatalf("the directory as created: %v, want it removed", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the directory as created is still there (%v)", err)
	}

	dir, held = create()
	sibling, _ := create()
	testfs.RemoveAll(t, tmp, dir)
	if err := os.Symlink(sibling, dir); err != nil {
		t.Fatal(err)
	}
	refused("a symbolic link put in the directory's place", dir, held)
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("the sibling the link points at is gone: %v", err)
	}

	dir, held = create()
	testfs.RemoveAll(t, tmp, dir)
	other, err := os.MkdirTemp("", liveRegistryCachePrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, dir); err != nil {
		t.Fatal(err)
	}
	refused("another directory moved into the directory's place", dir, held)

	dir, held = create()
	testfs.RemoveAll(t, tmp, dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	refused("a directory made again at the same path", dir, held)

	dir, held = create()
	t.Setenv("TMPDIR", t.TempDir())
	refused("the directory, once the temp directory changed", dir, held)
	t.Setenv("TMPDIR", tmp)

	dir, held = create()
	testfs.RemoveAll(t, tmp, dir)
	if err := removeOwnTempDir(dir, held, liveRegistryCachePrefix); err == nil {
		t.Fatal("a directory that is gone was reported removed")
	}
}
