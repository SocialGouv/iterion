package auxllm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// The channel's whole value is being the ONLY door: an auxiliary surface
// that builds its own model.Registry resolves run-derived content from
// whatever the env of the process happens to carry, judged by nobody — the
// exact class #2645 names. This sweep reads every Go file in the repository
// and holds the contract: outside pkg/backend/model (whose own factories
// are the env's readers), a Registry is built in exactly three places —
// the run's own execution planes (whose credential story is the run's ctx,
// not the aux channel's), and this package. Anything else is an auxiliary
// surface reaching around the door.
//
// What the sweep cannot judge through call sites: a second exported
// constructor defined INSIDE pkg/backend/model would be a second channel —
// an auxiliary surface would call it as model.NewSomethingRegistry, and the
// predicate below reads NewRegistry calls only. modelRegistryConstructors
// pins that package's constructor set, and the package doc of
// pkg/backend/model carries the doctrine the pin enforces.
func TestEveryAuxiliarySurfaceResolvesThroughTheChannel(t *testing.T) {
	// An exemption names a file AND how many model.NewRegistry calls its
	// reason covers — a file is not a licence, and a second call cannot
	// hide behind the first one's reason.
	exempt := map[string]int{
		// The run-plane executor: it builds the registry that resolves the
		// RUN'S OWN LLM nodes, funded through the run's credential ctx
		// (ResolveWithContext), not through the auxiliary channel.
		"pkg/runview/executor.go": 1,
		// cmd/iterion/claw_runner.go held an exemption for one NewRegistry
		// call (the run's own execution plane, same class as the executor
		// above) until #2244 moved its construction to the runner's own
		// constructor — NewSandboxRunnerRegistry, pinned below. It has zero
		// NewRegistry calls now; the file needs no licence.
	}

	// modelRegistryConstructors pins the exported constructors pkg/backend/
	// model defines that return *Registry, on the record like every ledger
	// in these sweeps. An unpinned addition reddens until it carries its
	// reason — and if it serves auxiliary surfaces it belongs behind
	// auxllm.Registry instead. A pinned name that vanished reddens until
	// the entry is dropped.
	modelRegistryConstructors := map[string]string{
		// The one constructor today: the env-funded registry the channel
		// wraps.
		"NewRegistry": "the env-funded registry; auxllm.Registry is the auxiliary channel wrapping it",
		// The sandbox runner's own registry (iterion __claw-runner): the
		// run's execution plane inside the crossing-assembled sandbox, whose
		// env is forwarded by the crossing itself — not an auxiliary surface.
		"NewSandboxRunnerRegistry": "the runner plane's registry; the crossing forwards its env deliberately (#2244)",
	}
	constructors := map[string]string{}

	var offenders []string
	sweepwalk.GoFiles(t, filepath.Join("..", "..", ".."), func(rel, path string) {
		// Test files build registries as fixtures — providers exercised
		// against planted env, run planes driven through their public API.
		// The contract judges PRODUCTION construction: any auxiliary
		// surface exists in a production file, and that file is judged.
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		// This package IS the channel: Registry is the one sanctioned
		// direct construction outside the run planes.
		if strings.HasPrefix(rel, "pkg/backend/auxllm/") {
			return
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return
		}
		if strings.HasPrefix(rel, "pkg/backend/model/") {
			for _, name := range registryConstructors(file) {
				constructors[name] = rel
			}
		}
		calls := registryConstructions(file, modelImportsOf(file))
		if exempt[rel] > 0 {
			if calls != exempt[rel] {
				t.Errorf("%s is exempt for %d model.NewRegistry call(s) (the run's own execution plane) and now "+
					"has %d — the exemption covers that count, not the file; move any new construction behind "+
					"auxllm.Registry, or raise the count with the reason it deserves",
					rel, exempt[rel], calls)
			}
			return
		}
		if calls > 0 {
			offenders = append(offenders, rel)
		}
	})

	// Both directions of the constructor pin, each in one stable order:
	// every constructor model defines is pinned, and every pinned one is
	// still defined.
	for _, name := range slices.Sorted(maps.Keys(constructors)) {
		if _, pinned := modelRegistryConstructors[name]; !pinned {
			t.Errorf("pkg/backend/model now defines %s (%s), a second exported constructor returning "+
				"*Registry — a funding decision no call site of this sweep judges, so an auxiliary surface "+
				"could resolve through it unseen. Add it to modelRegistryConstructors with the reason it "+
				"deserves; if it serves auxiliary surfaces it belongs behind auxllm.Registry instead.",
				name, constructors[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(modelRegistryConstructors)) {
		if _, found := constructors[name]; !found {
			t.Errorf("modelRegistryConstructors pins %s, and pkg/backend/model no longer defines it — "+
				"drop the entry", name)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("these build a model.Registry directly, outside the auxiliary credential channel — run-derived "+
			"content they send would be funded by whatever the process env carries, judged by nobody: %v\n"+
			"Call auxllm.Registry() instead (the one credential decision a server-side auxiliary LLM surface "+
			"gets, #2645); only the run's own execution planes may construct a registry of their own, and those "+
			"live in this test's exempt map with their reason.", offenders)
	}
}

// registryConstructions counts the NewRegistry calls the file reaches the
// model package's constructor through: `<name>.NewRegistry(…)` for any name
// the file binds the package to, and — under a dot import — the bare
// `NewRegistry(…)`, which is that same constructor with no selector left to
// name it by. It reads the AST rather than scanning text: a `NewRegistry(`
// inside a comment or a string satisfies a grep, and a call split across
// lines defeats a line-oriented one.
func registryConstructions(file *ast.File, mi modelImports) (calls int) {
	if !mi.dotBound && len(mi.names) == 0 {
		return 0 // the file does not import the model package at all
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// A dot import declares the package's exported names bare, so its
		// constructor reads as a plain ident call. Without a dot import a
		// bare NewRegistry cannot be the model package's: the file reaches
		// the package only through the names it binds.
		if id, ok := call.Fun.(*ast.Ident); ok {
			if mi.dotBound && id.Name == "NewRegistry" {
				calls++
			}
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRegistry" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && mi.names[pkg.Name] {
			calls++
		}
		return true
	})
	return calls
}

// modelImports records every binding a file holds on the model package: the
// named ones (default or aliased) in names, and dotBound for the dot import,
// which binds no name at all — its exported names read bare.
type modelImports struct {
	names    map[string]bool
	dotBound bool
}

// modelImportsOf returns every binding the file holds on the model package,
// or the zero modelImports when it holds none. One name is one shape, not
// the answer: Go accepts importing the same package twice under distinct
// names, and a dot import declares its names with no selector — a gate
// reading one name reads one shape, and the sweep judges every shape the
// file can reach the package through.
func modelImportsOf(file *ast.File) modelImports {
	const path = `"github.com/SocialGouv/iterion/pkg/backend/model"`
	var mi modelImports
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		name := "model"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "." {
			mi.dotBound = true
			continue
		}
		if mi.names == nil {
			mi.names = map[string]bool{}
		}
		mi.names[name] = true
	}
	return mi
}

// registryConstructors returns the exported, receiver-less functions of the
// file whose sole result is *Registry — the constructors of the model
// package's own registry type. It reads declarations, so a constructor is
// counted however its body builds the registry.
func registryConstructors(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil || !fn.Name.IsExported() {
			continue
		}
		results := fn.Type.Results
		if results == nil || len(results.List) != 1 {
			continue
		}
		star, ok := results.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); ok && id.Name == "Registry" {
			names = append(names, fn.Name.Name)
		}
	}
	return names
}

// The constructor pin is a blocking gate too, so its predicate owes the same
// proof as the call-site one: a change to the shape it reads reddens HERE
// instead of silently pinning nothing.
func TestTheConstructorPredicateReadsTheShapesItClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		names []string
	}{
		{
			name: "an exported constructor returning *Registry",
			src: `package p
type Registry struct{}
func NewRegistry() *Registry { return &Registry{} }`,
			names: []string{"NewRegistry"},
		},
		{
			name: "a method returning the type is not a constructor",
			src: `package p
type Registry struct{}
func (r *Registry) Clone() *Registry { return r }`,
		},
		{
			name: "an unexported function is not a constructor",
			src: `package p
type Registry struct{}
func newRegistry() *Registry { return &Registry{} }`,
		},
		{
			name: "a second result disqualifies the function",
			src: `package p
type Registry struct{}
func Pair() (*Registry, error) { return &Registry{}, nil }`,
		},
		{
			name: "another type's pointer is not the registry",
			src: `package p
type Cache struct{}
func NewCache() *Cache { return &Cache{} }`,
		},
		{
			name: "a value result is not the pinned pointer shape",
			src: `package p
type Registry struct{}
func Value() Registry { return Registry{} }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := registryConstructors(file)
			if len(got) != len(tc.names) {
				t.Fatalf("registryConstructors = %v, want %v", got, tc.names)
			}
			for i := range got {
				if got[i] != tc.names[i] {
					t.Errorf("registryConstructors = %v, want %v", got, tc.names)
				}
			}
		})
	}
}

// The sweep is a blocking gate, so its predicate owes proof that it reads
// the shapes it claims: a change to the import matching reddens HERE
// instead of silently judging nothing.
func TestTheSweepPredicateReadsTheShapesItClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		src   string
		calls int
	}{
		{
			name: "a direct construction",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/model"
func f() { r := model.NewRegistry(); _ = r }`,
			calls: 1,
		},
		{
			name: "an aliased import is still ours",
			src: `package p
import m "github.com/SocialGouv/iterion/pkg/backend/model"
func f() { r := m.NewRegistry(); _ = r }`,
			calls: 1,
		},
		{
			name: "another package's NewRegistry is not ours",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/delegate"
func f() { r := delegate.NewRegistry(); _ = r }`,
			calls: 0,
		},
		{
			// A file that imports model beside a foreign package must count
			// OUR call and refuse the foreign one in the same walk — without
			// this case the import-based early return is the only path that
			// ever refuses a foreign NewRegistry.
			name: "a foreign NewRegistry beside a model import is not ours",
			src: `package p
import (
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)
func f() { d := delegate.NewRegistry(); r := model.NewRegistry(); _, _ = d, r }`,
			calls: 1,
		},
		{
			// A dot import binds no name: the constructor reads as a bare
			// NewRegistry() call, and the gate that refuses the selector
			// form must refuse this one too.
			name: "a dot-imported construction is still ours",
			src: `package p
import . "github.com/SocialGouv/iterion/pkg/backend/model"
func f() { r := NewRegistry(); _ = r }`,
			calls: 1,
		},
		{
			// The same package may be imported twice under distinct names,
			// and the second binding is as much ours as the first — a
			// gate matching one name judges one binding of many.
			name: "a construction through a second binding of the same package is still ours",
			src: `package p
import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	m2 "github.com/SocialGouv/iterion/pkg/backend/model"
)
func f() { r := m2.NewRegistry(); _ = r }`,
			calls: 1,
		},
		{
			// Without a dot import a bare NewRegistry is not the model
			// package's: the file reaches the package only through the
			// names it binds, and a file's own NewRegistry shadows nothing
			// of ours.
			name: "a bare NewRegistry without a dot import is not ours",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/model"
func NewRegistry() *struct{} { return nil }
func f() { r := NewRegistry(); _ = r }`,
			calls: 0,
		},
		{
			// The gate judges construction, not import presence: a dot
			// import that builds no registry is no offence.
			name: "a dot import without a construction is not a call",
			src: `package p
import . "github.com/SocialGouv/iterion/pkg/backend/model"
func f() { s := ParseModelSpec("openai/x"); _ = s }`,
			calls: 0,
		},
		{
			name: "no import of the model package at all",
			src: `package p
import "strings"
func f() string { return strings.ToUpper("x") }`,
			calls: 0,
		},
		{
			name: "a mention in a comment or string is not a call",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/model"
// swap model.NewRegistry() for auxllm.Registry() someday
const hint = "call model.NewRegistry()"
func f() { _ = model.ParseModelSpec("openai/x"); _ = hint }`,
			calls: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := registryConstructions(file, modelImportsOf(file)); got != tc.calls {
				t.Errorf("registryConstructions = %d, want %d", got, tc.calls)
			}
		})
	}
}

// The channel's decision today is the process env, so the registry it hands
// out must be funded exactly by that env: a credential present in the env
// resolves, the same spec without it refuses by name. The day the decision
// changes (#2005 PR-C' moves the funding to the run's gateway credential),
// this test is the tripwire that names the change — rewrite it with the new
// decision, never silently.
func TestRegistryServesTheProcessEnvCredential(t *testing.T) {
	const spec = "moonshot/kimi-k2"

	t.Setenv("MOONSHOT_API_KEY", "sk-witness-channel-env")
	funded := Registry()
	client, err := funded.Resolve(spec)
	if err != nil {
		t.Fatalf("resolve %q with the env credential planted: %v", spec, err)
	}
	if client == nil {
		t.Fatalf("resolve %q returned a nil client with no error", spec)
	}

	// A fresh registry: Resolve caches per spec, and the unfunded arm must
	// judge the env, not the first arm's cache entry.
	t.Setenv("MOONSHOT_API_KEY", "")
	unfunded := Registry()
	_, err = unfunded.Resolve(spec)
	if err == nil {
		t.Fatalf("resolve %q succeeded with no credential in the env — the channel handed out a client it has no funding for", spec)
	}
	if !strings.Contains(err.Error(), "MOONSHOT_API_KEY") {
		t.Fatalf("the unfunded refusal must name the variable that funds the spec, got: %v", err)
	}
}
