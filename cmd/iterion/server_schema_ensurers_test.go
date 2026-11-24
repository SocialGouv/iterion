package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Item 13 of #2401: the server never called usagecap.EnsureSchema — only the
// runner did. A deployment whose runner had never booted served the cap
// surface on a collection with no indexes, and nothing said so: EnsureSchema
// is a boot-time side effect, so its absence shows up as latency, never as an
// error.
//
// The class is "a package grows a schema and one process forgets it", and the
// shape that closes it is this gate: every package-level EnsureSchema(ctx, db)
// of the tree is named in the server's ensurer list, or declared below with
// the reason it belongs to another process.

// runnerOnlySchemas name the package-level ensurers the SERVER deliberately
// does not run, with the reason. Empty is the expected state: a schema the
// server does not ensure needs a reason, not a habit.
var runnerOnlySchemas = map[string]string{}

var ensureSchemaFunc = regexp.MustCompile(`(?m)^func EnsureSchema\(ctx context\.Context, db \*mongo\.Database\) error`)

// packagesWithSchemaEnsurer walks pkg/ and returns the import paths' last
// segment — the identifier the server's list calls the function through — for
// every package declaring a package-level EnsureSchema.
func packagesWithSchemaEnsurer(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..", "pkg")
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if ensureSchemaFunc.Match(raw) {
			out[filepath.Base(filepath.Dir(path))] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) < 8 {
		t.Fatalf("found %d package-level EnsureSchema function(s) under %s — the scan went blind and this gate would pass on an empty set", len(out), root)
	}
	return out
}

func TestEverySchemaEnsurerIsRunByTheServer(t *testing.T) {
	raw, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	wiring := string(raw)
	pkgs := packagesWithSchemaEnsurer(t)
	names := make([]string, 0, len(pkgs))
	for pkg := range pkgs {
		names = append(names, pkg)
	}
	sort.Strings(names)
	for _, pkg := range names {
		if strings.Contains(wiring, pkg+".EnsureSchema(") {
			continue
		}
		if reason, ok := runnerOnlySchemas[pkg]; ok {
			t.Logf("%s.EnsureSchema is not run by the server: %s", pkg, reason)
			continue
		}
		t.Errorf("%s declares a package-level EnsureSchema (%s) and the server's ensurer list never calls it — the collection's indexes exist only where another process happened to boot; add it to the list, or declare the reason in runnerOnlySchemas", pkg, pkgs[pkg])
	}
}
