package unit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The AST a caller hands to LoadDirWithMainAST is read, never written: the
// unit's Merged is a copy with the fragments' declarations appended and the
// keyed blocks rebuilt, so the caller's file is byte-for-byte what it was
// while the merge did happen.
func TestLoadDirWithMainASTLeavesTheCallersASTUntouched(t *testing.T) {
	nl := string(rune(10))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	frag := strings.Join([]string{"vars:", "  extra: string", "", "schema h:", "  ok: bool", ""}, nl)
	if err := os.WriteFile(filepath.Join(root, "lib", "helper.bot"), []byte(frag), 0o644); err != nil {
		t.Fatal(err)
	}
	src := strings.Join([]string{`import "lib/helper.bot"`, "", "vars:", "  own: string", "", "schema s:", "  ok: bool", ""}, nl)
	pr := parser.Parse("main.bot", src)
	if len(pr.Diagnostics) > 0 {
		t.Fatal(pr.Diagnostics)
	}
	before, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	u := LoadDirWithMainAST(filepath.Join(root, "main.bot"), "main.bot.yaml", pr.File, []byte(src))
	after, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the caller's AST was written by the loader:\nbefore: %s\nafter:  %s", before, after)
	}
	if u.Merged == nil || u.Merged.Vars == nil || len(u.Merged.Vars.Fields) != 2 || len(u.Merged.Schemas) != 2 {
		t.Fatalf("the merge did not happen: %+v", u.Merged)
	}
	if len(pr.File.Vars.Fields) != 1 || len(pr.File.Schemas) != 1 {
		t.Fatalf("the caller's file gained the fragment's declarations: %d vars, %d schemas", len(pr.File.Vars.Fields), len(pr.File.Schemas))
	}
}
