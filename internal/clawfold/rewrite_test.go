package clawfold

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const rewriteSrc = `package demo

// Package demo talks about github.com/SocialGouv/claw-code-go in a comment.

import (
	"fmt"

	claw "github.com/SocialGouv/claw-code-go"
	"github.com/SocialGouv/claw-code-go/internal/api"
	. "github.com/SocialGouv/claw-code-go/pkg/dot"
	_ "github.com/SocialGouv/claw-code-go/pkg/blank"
	gopher "github.com/SocialGouv/claw-code-gopher/x"
)

import raw ` + "`github.com/SocialGouv/claw-code-go/pkg/raw`" + `

const modulePath = "github.com/SocialGouv/claw-code-go"

var _ = fmt.Sprint(claw.X, api.Y, Dot, gopher.Z, raw.W, modulePath)
`

func TestRewriteTouchesImportSpecsOnly(t *testing.T) {
	t.Parallel()
	out, lines, err := Fold.Rewrite("demo.go", []byte(rewriteSrc))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		`claw "github.com/SocialGouv/claw-code-go"`, `claw "github.com/SocialGouv/iterion/pkg/claw"`,
		`"github.com/SocialGouv/claw-code-go/internal/api"`, `"github.com/SocialGouv/iterion/pkg/claw/internal/api"`,
		`. "github.com/SocialGouv/claw-code-go/pkg/dot"`, `. "github.com/SocialGouv/iterion/pkg/claw/pkg/dot"`,
		`_ "github.com/SocialGouv/claw-code-go/pkg/blank"`, `_ "github.com/SocialGouv/iterion/pkg/claw/pkg/blank"`,
		"`github.com/SocialGouv/claw-code-go/pkg/raw`", "`github.com/SocialGouv/iterion/pkg/claw/pkg/raw`",
	).Replace(rewriteSrc)
	if string(out) != want {
		t.Fatalf("rewrite:\n%s\nwant:\n%s", out, want)
	}
	if !reflect.DeepEqual(lines, []int{8, 9, 10, 11, 15}) {
		t.Fatalf("rewritten lines = %v, want [8 9 10 11 15]", lines)
	}
	for _, kept := range []string{
		"// Package demo talks about github.com/SocialGouv/claw-code-go in a comment.",
		`const modulePath = "github.com/SocialGouv/claw-code-go"`,
		`gopher "github.com/SocialGouv/claw-code-gopher/x"`,
	} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("the rewrite moved %q, which is not an import of the module", kept)
		}
	}
}

func TestRewriteInverseIsExactAndBothAreIdempotent(t *testing.T) {
	t.Parallel()
	src := []byte(rewriteSrc)
	out, _, err := Fold.Rewrite("demo.go", src)
	if err != nil {
		t.Fatal(err)
	}
	back, lines, err := Fold.Inverse().Rewrite("demo.go", out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, src) {
		t.Fatalf("unrewrite(rewrite(src)) != src:\n%s", back)
	}
	if len(lines) != 5 {
		t.Fatalf("unrewrite touched lines %v, want the 5 rewritten ones", lines)
	}
	again, lines, err := Fold.Rewrite("demo.go", out)
	if err != nil || len(lines) != 0 || !bytes.Equal(again, out) {
		t.Fatalf("second rewrite: lines %v, err %v, changed %t", lines, err, !bytes.Equal(again, out))
	}
	againBack, lines, err := Fold.Inverse().Rewrite("demo.go", src)
	if err != nil || len(lines) != 0 || !bytes.Equal(againBack, src) {
		t.Fatalf("unrewrite of a never-rewritten file: lines %v, err %v", lines, err)
	}
	if changed, ok := ChangedLines(src, out); !ok || !reflect.DeepEqual(changed, []int{8, 9, 10, 11, 15}) {
		t.Fatalf("changed lines %v (same count %t)", changed, ok)
	}
}

func TestImportsUnderDestinationGuardsTheInverse(t *testing.T) {
	t.Parallel()
	mixed := []byte("package x\n\nimport (\n\t\"github.com/SocialGouv/claw-code-go/a\"\n\t\"github.com/SocialGouv/iterion/pkg/claw/b\"\n)\n")
	lines, err := Fold.ImportsUnderDestination("x.go", mixed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lines, []int{5}) {
		t.Fatalf("lines under the destination = %v, want [5]", lines)
	}
	out, _, err := Fold.Rewrite("x.go", mixed)
	if err != nil {
		t.Fatal(err)
	}
	back, _, _ := Fold.Inverse().Rewrite("x.go", out)
	if bytes.Equal(back, mixed) {
		t.Fatal("a file already importing under the destination round-trips: the precondition would guard nothing")
	}
}

func TestRewriteRefusesAnEscapedImportLiteral(t *testing.T) {
	t.Parallel()
	src := []byte("package x\n\nimport \"github.com/SocialGouv/claw-code-go/\\x61pi\"\n")
	if _, _, err := Fold.Rewrite("x.go", src); err == nil || !strings.Contains(err.Error(), "escape") {
		t.Fatalf("err = %v, want a refusal naming the escape", err)
	}
}

func TestRewriteRefusesAnUnparsableFile(t *testing.T) {
	t.Parallel()
	if _, _, err := Fold.Rewrite("x.go", []byte("package x\nimport (\n")); err == nil {
		t.Fatal("an unparsable file was rewritten")
	}
}

func TestGofmtVerdictRefusesARewriteThatMovesTheImportOrder(t *testing.T) {
	t.Parallel()
	src := []byte("package x\n\nimport (\n\t\"github.com/SocialGouv/claw-code-go/a\"\n\t\"github.com/SocialGouv/foo\"\n)\n")
	dest, _, err := Fold.Rewrite("x.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	err = gofmtVerdict("x.go", src, dest, &warnings)
	if err == nil || !strings.Contains(err.Error(), "import order moved") {
		t.Fatalf("err = %v, want the order refusal", err)
	}

	unformatted := []byte("package x\n\nimport \"github.com/SocialGouv/claw-code-go/a\"\nvar  y = 1\n")
	dest, _, err = Fold.Rewrite("y.go", unformatted)
	if err != nil {
		t.Fatal(err)
	}
	warnings = nil
	if err := gofmtVerdict("y.go", unformatted, dest, &warnings); err != nil {
		t.Fatalf("a source the pin ships unformatted is the source's state, not a refusal: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not gofmt-clean") {
		t.Fatalf("warnings = %v, want one naming the unformatted source", warnings)
	}
}

func TestMapPathStopsAtASegmentBoundary(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"github.com/SocialGouv/claw-code-go":             "github.com/SocialGouv/iterion/pkg/claw",
		"github.com/SocialGouv/claw-code-go/x/y":         "github.com/SocialGouv/iterion/pkg/claw/x/y",
		"github.com/SocialGouv/claw-code-gopher":         "",
		"github.com/SocialGouv/claw-code-go-fork/x":      "",
		"example.com/github.com/SocialGouv/claw-code-go": "",
	} {
		got, under := Fold.mapPath(path)
		if under != (want != "") || got != want {
			t.Errorf("mapPath(%q) = %q, %t; want %q", path, got, under, want)
		}
	}
}

func TestBlobIDIsGitsObjectID(t *testing.T) {
	t.Parallel()
	// `printf 'hello\n' | git hash-object --stdin` and its SHA-256 twin
	// (`git hash-object --object-format=sha256`).
	if got := BlobID([]byte("hello\n"), 40); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("sha1 blob id = %s", got)
	}
	if got := BlobID([]byte("hello\n"), 64); got != "2cf8d83d9ee29543b34a87727421fdecb7e3f3a183d337639025de576db9ebb4" {
		t.Errorf("sha256 blob id = %s", got)
	}
}

func TestRewriteRecordsPhysicalLinesUnderALineDirective(t *testing.T) {
	t.Parallel()
	src := []byte("package x\n\n//line generated.go:100\nimport \"github.com/SocialGouv/claw-code-go/a\"\n")
	_, lines, err := Fold.Rewrite("x.go", src)
	if err != nil || !reflect.DeepEqual(lines, []int{4}) {
		t.Fatalf("lines = %v, %v; want [4], the physical line", lines, err)
	}
}
