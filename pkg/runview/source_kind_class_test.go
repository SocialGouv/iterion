package runview

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// storeRunSourcePath and studioSourceMetaPath are the two halves of the
// class contract this file enforces: the engine's vocabulary of typed run
// provenance, and the studio table that gives each word a rendered label.
const (
	storeRunSourcePath   = "../store/run.go"
	studioSourceMetaPath = "../../studio/src/components/Runs/runSourceMeta.ts"
)

// studioOrderRe captures the body of SOURCE_KIND_ORDER — the studio's
// ordered list of source kinds, which its own vitest pins against the keys
// of SOURCE_META (so a kind present here HAS a label, an icon and a
// description).
var studioOrderRe = regexp.MustCompile(`(?s)SOURCE_KIND_ORDER[^=]*=\s*\[(.*?)\]`)

// TestEveryTypedRunSourceKindIsRenderable is the CLASS witness behind
// "a new source kind is honoured at every site that selects on one"
// (#1501/ARB-41): the engine's RunSourceKind* vocabulary is read from
// pkg/store/run.go itself, so a fifth kind cannot be added without this
// test seeing it, and each word is then required to
//
//  1. survive deriveSourceKind as the run list's own classifier — the
//     string the wire carries in source_kind;
//  2. appear in the studio's SOURCE_KIND_ORDER, whose vitest pins it
//     against SOURCE_META. A kind the studio does not know is normalised
//     to "manual", i.e. the run renders as a human launch.
//
// The comment on TestDeriveSourceKind claimed exactly this contract and
// nothing enforced it: "webhook" was in the studio table only because a
// structural fallback (the owner prefix) happened to emit it.
//
// Mutation that reddens it: drop `"webhook",` from SOURCE_KIND_ORDER in
// studio/src/components/Runs/runSourceMeta.ts — red on the studio-label
// assertion, naming the kind. Dropping the typed branch of
// deriveSourceKind (`case r.Source != nil && r.Source.Kind != ""`) reddens
// the classifier assertion for every kind but the webhook one, which the
// owner-prefix fallback would still answer — which is why the row below
// carries no OwnerID.
func TestEveryTypedRunSourceKindIsRenderable(t *testing.T) {
	kinds := parseRunSourceKinds(t)
	if len(kinds) < 4 {
		t.Fatalf("read %d RunSourceKind* constants from %s; want at least the four known kinds — the parse, not the vocabulary, is what regressed", len(kinds), storeRunSourcePath)
	}
	studio := parseStudioSourceKinds(t)
	for name, value := range kinds {
		t.Run(name, func(t *testing.T) {
			// No OwnerID, no ForkedFrom, no ParentRunID: the TYPED field is
			// the only thing that can answer here.
			r := store.Run{Source: &store.RunSource{Kind: value}}
			if got := deriveSourceKind(&r); got != value {
				t.Errorf("deriveSourceKind for %s = %q, want %q — the run list cannot classify a kind the launcher stamps", name, got, value)
			}
			if _, ok := studio[value]; !ok {
				t.Errorf("%s = %q is absent from SOURCE_KIND_ORDER in %s — the studio normalises an unknown kind to \"manual\", so a run of this provenance renders as a human launch", name, value, studioSourceMetaPath)
			}
		})
	}
}

// parseRunSourceKinds reads the RunSourceKind* constants from the store
// package's own source. Parsed rather than listed here on purpose: a table
// of kinds copied into this file would be the very drift the test exists to
// catch.
func parseRunSourceKinds(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, storeRunSourcePath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", storeRunSourcePath, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			name := vs.Names[0].Name
			if !strings.HasPrefix(name, "RunSourceKind") {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("%s is not a string literal constant — this test can no longer read the vocabulary", name)
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", name, err)
			}
			out[name] = value
		}
	}
	return out
}

// parseStudioSourceKinds reads SOURCE_KIND_ORDER out of the studio module.
// Reading the source rather than a build artefact keeps the check honest
// when the studio is not built (the Go gate never builds it).
func parseStudioSourceKinds(t *testing.T) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile(studioSourceMetaPath)
	if err != nil {
		t.Fatalf("read %s: %v", studioSourceMetaPath, err)
	}
	m := studioOrderRe.FindSubmatch(raw)
	if m == nil {
		t.Fatalf("SOURCE_KIND_ORDER not found in %s — the studio half of the contract moved; point this test at its new home rather than deleting it", studioSourceMetaPath)
	}
	out := map[string]struct{}{}
	for _, entry := range strings.Split(string(m[1]), ",") {
		entry = strings.Trim(strings.TrimSpace(entry), `"'`)
		if entry != "" {
			out[entry] = struct{}{}
		}
	}
	if len(out) == 0 {
		t.Fatalf("SOURCE_KIND_ORDER parsed empty from %s", studioSourceMetaPath)
	}
	return out
}
