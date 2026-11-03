package secrets

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// BaseURLNamesZAI reads the z.ai family's hosts the way every reader of the
// ANTHROPIC_BASE_URL/AUTH_TOKEN pair needs them read: any spelling of either
// host, whatever its case, and nothing else.
func TestBaseURLNamesZAI(t *testing.T) {
	for _, tc := range []struct {
		base string
		want bool
	}{
		{ZAIDefaultBaseURL, true},
		{ZAIChinaBaseURL, true},
		{"HTTPS://API.Z.AI/api/anthropic", true},
		{"https://open.BigModel.cn/api/anthropic", true},
		{"", false},
		{"https://api.anthropic.com", false},
		{"https://llm-gw.example.test/anthropic", false},
	} {
		if got := BaseURLNamesZAI(tc.base); got != tc.want {
			t.Errorf("BaseURLNamesZAI(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

// The z.ai hostname predicate has ONE body. Its readers — the claw registry's
// env walk, the executor funding probe, provider detection — once each kept a
// copy, and a copy is a spelling that drifts on its own: a host one reader
// takes for z.ai's and another for an operator gateway funds the same env pair
// two ways. The defect is a second body, which no unit test of the first can
// see, so the guard is a scan: no non-test Go file outside this package spells
// either host fragment as a string literal of its own.
func TestOnlyBaseURLNamesZAISpellsTheZAIHosts(t *testing.T) {
	root := moduleRoot(t)
	out, err := gittest.Try(root, "ls-files", "*.go")
	if err != nil {
		t.Fatalf("git ls-files did not answer, so the tree's Go files were not held to the one predicate: %v", err)
	}
	fset := token.NewFileSet()
	scanned, offenders := 0, []string{}
	for _, rel := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case rel == "", strings.HasPrefix(rel, "vendor/"),
			strings.HasSuffix(rel, "_test.go"),
			strings.Contains(rel, "/testdata/"),
			// The one body.
			rel == "pkg/secrets/byok.go":
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if strings.EqualFold(v, "z.ai") || strings.EqualFold(v, "bigmodel") {
				offenders = append(offenders, fset.Position(lit.Pos()).String())
			}
			return true
		})
	}
	if scanned < 100 {
		t.Fatalf("only %d non-test Go files were scanned — the guard is not reaching the tree", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("a z.ai host fragment is spelled outside BaseURLNamesZAI (%d site(s)) — call secrets.BaseURLNamesZAI instead of a copy:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// moduleRoot climbs from the test's working directory to the go.mod, so the
// scan reaches the whole module wherever it runs from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test — run from inside the module")
		}
		dir = parent
	}
}
