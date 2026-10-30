package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The completeness guard on the belt. Every raw os.Getenv/os.LookupEnv read
// with a literal argument in this package must name a variable that is NOT
// operator-grade: a grade name — the origins, the CSRF and header guards,
// the WS origin, the webhook forge list, the project-trust switches, the
// server's secrets — is read through envOperator, which answers a planted
// value with nothing (the H-10 review measured `ITERION_REQUIRE_ORIGIN=0`
// planted by a project `.env` and honoured by the raw read). A grade name
// that shows up as a raw read fails here, the day it is written, instead of
// answering a repository under review.
//
// The walk cannot see through a composed constant read ONLY through
// os.Getenv of a variable (the constant itself is not a read): the
// constants' names are literals at their own readers, which the walk does
// see. What it cannot see at all — a read built by reflection or fmt — it
// cannot vouch for, like every walk in this tree.
func TestNoOperatorGradeNameIsReadAroundTheBelt(t *testing.T) {
	rawNames := map[string]string{}  // name -> file of the raw os.Getenv read
	beltNames := map[string]string{} // name -> file of the envOperator read
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "static" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(raw), "Getenv(") && !strings.Contains(string(raw), "envOperator(") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, raw, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		note := func(call *ast.CallExpr) {
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return
			}
			name := strings.Trim(lit.Value, `"`)
			if name == "" {
				return
			}
			if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == "envOperator" {
				beltNames[name] = path
			} else if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
				if pkg, pkgOK := sel.X.(*ast.Ident); pkgOK && pkg.Name == "os" &&
					(sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
					rawNames[name] = path
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 0 {
				note(call)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(rawNames) == 0 || len(beltNames) == 0 {
		t.Fatal("the walk found no reads at all: it has stopped matching the source, so a green verdict proves nothing")
	}

	// The floor: names each side of the belt must keep being seen at — a
	// walk that stops finding them has stopped looking, not stopped needing.
	for _, want := range []string{
		"ITERION_REQUIRE_ORIGIN", "ITERION_SECURITY_HEADERS",
		"ITERION_ALLOWED_ORIGINS", "ITERION_WEBHOOK_FORGE_HOSTS",
		"ITERION_ASSISTANT_PYTHON",
	} {
		if _, ok := beltNames[want]; !ok {
			t.Fatalf("the walk no longer sees %s through envOperator: the belt read is gone, or the walk stopped matching", want)
		}
	}
	for _, want := range []string{"ITERION_COMPRESS", "ITERION_WEBHOOK_MAX_ATTEMPTS"} {
		if _, ok := rawNames[want]; !ok {
			t.Fatalf("the walk no longer sees %s as a raw read: a non-grade knob vanished, or the walk stopped matching", want)
		}
	}

	for name, path := range rawNames {
		if envtrust.OperatorGradeRefused(name) {
			t.Errorf("%s is operator-grade and read RAW at %s: route it through envOperator, so a project `.env`'s plant answers nothing", name, path)
		}
	}
	for name := range beltNames {
		if !envtrust.OperatorGradeRefused(name) {
			t.Errorf("%s is not operator-grade and is read through envOperator at its site: drop it back to the plain read or widen the grade knowingly", name)
		}
	}
}

// The belt's effect, through the REAL resolvers where the server drives
// them: the security-headers handler runs for real (a planted
// ITERION_SECURITY_HEADERS=0 must not strip the headers), and the origins
// read is the exact expression the guard's loader evaluates. The two
// require-origin booleans are Server methods deep in the request path;
// their SITE is held by the completeness walk above, and their VALUE is
// the same envOperator read this test drives.
func TestPlantedGuardPosturesAnswerNothing(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "ITERION_REQUIRE_ORIGIN,ITERION_SECURITY_HEADERS,ITERION_ALLOWED_ORIGINS,ITERION_REQUIRE_WS_ORIGIN")
	envtrust.ResetForTest()

	t.Setenv("ITERION_REQUIRE_ORIGIN", "0")
	t.Setenv("ITERION_SECURITY_HEADERS", "0")
	t.Setenv("ITERION_ALLOWED_ORIGINS", "https://attacker.example")
	t.Setenv("ITERION_REQUIRE_WS_ORIGIN", "0")

	// A real response through the real handler: every header the middleware
	// sets must still be there.
	rec := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	for _, header := range []string{"X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options"} {
		if rec.Header().Get(header) == "" {
			t.Errorf("a planted ITERION_SECURITY_HEADERS=0 stripped %s", header)
		}
	}

	valid, _ := splitAllowedOrigins(envOperator("ITERION_ALLOWED_ORIGINS"))
	if slices.Contains(valid, "https://attacker.example") {
		t.Error("a planted ITERION_ALLOWED_ORIGINS admitted the attacker origin")
	}

	if got := envOperator("ITERION_REQUIRE_ORIGIN"); got != "" {
		t.Errorf("ITERION_REQUIRE_ORIGIN reads %q through the belt: a planted kill switch reached the guard", got)
	}
	if got := envOperator("ITERION_REQUIRE_WS_ORIGIN"); got != "" {
		t.Errorf("ITERION_REQUIRE_WS_ORIGIN reads %q through the belt: a planted WS switch reached the hub", got)
	}

	// The control: the operator's own export reaches the same reads.
	t.Setenv(envtrust.EnvPlantedNames, "")
	envtrust.ResetForTest()

	rec = httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if rec.Header().Get("X-Content-Type-Options") != "" {
		t.Error("the operator's own ITERION_SECURITY_HEADERS=0 stopped answering: the belt ate the operator's channel")
	}
	valid, _ = splitAllowedOrigins(envOperator("ITERION_ALLOWED_ORIGINS"))
	if !slices.Contains(valid, "https://attacker.example") {
		t.Error("the operator's own ITERION_ALLOWED_ORIGINS stopped answering")
	}
	if got := envOperator("ITERION_REQUIRE_ORIGIN"); got != "0" {
		t.Errorf("the operator's own ITERION_REQUIRE_ORIGIN=0 reads %q: the belt must keep their channel", got)
	}
	if got := envOperator("ITERION_REQUIRE_WS_ORIGIN"); got != "0" {
		t.Errorf("the operator's own ITERION_REQUIRE_WS_ORIGIN=0 reads %q: the belt must keep their channel", got)
	}
}
