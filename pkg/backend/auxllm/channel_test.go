package auxllm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// What the sweep cannot see: a second exported constructor added INSIDE
// pkg/backend/model would be a second channel this walk never judges. Its
// reason belongs in that package's docs, and if it serves auxiliary
// surfaces it belongs HERE instead.
func TestEveryAuxiliarySurfaceResolvesThroughTheChannel(t *testing.T) {
	// An exemption names a file AND how many model.NewRegistry calls its
	// reason covers — a file is not a licence, and a second call cannot
	// hide behind the first one's reason.
	exempt := map[string]int{
		// The run-plane executor: it builds the registry that resolves the
		// RUN'S OWN LLM nodes, funded through the run's credential ctx
		// (ResolveWithContext), not through the auxiliary channel.
		"pkg/runview/executor.go": 1,
		// The in-container runner: the run's own execution plane, inside the
		// sandbox the launcher assembled — same class as the executor above.
		"cmd/iterion/claw_runner.go": 1,
	}

	repoRoot := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			// Same skip list as runview's ExecutorSpec sweep and the MCP
			// start-policy sweep: a .go file under testdata is never
			// compiled, so judging it would accuse source that cannot run.
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A nested checkout (a git worktree, a sibling clone) holds
			// another tree's files: its older copies would report as
			// offenders of a rule they predate.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		// Test files build registries as fixtures — providers exercised
		// against planted env, run planes driven through their public API.
		// The contract judges PRODUCTION construction: any auxiliary
		// surface exists in a production file, and that file is judged.
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		// This package IS the channel: Registry is the one sanctioned
		// direct construction outside the run planes.
		if strings.HasPrefix(rel, "pkg/backend/auxllm/") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		calls := registryConstructions(file, modelImportName(file))
		if exempt[rel] > 0 {
			if calls != exempt[rel] {
				t.Errorf("%s is exempt for %d model.NewRegistry call(s) (the run's own execution plane) and now "+
					"has %d — the exemption covers that count, not the file; move any new construction behind "+
					"auxllm.Registry, or raise the count with the reason it deserves",
					rel, exempt[rel], calls)
			}
			return nil
		}
		if calls > 0 {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these build a model.Registry directly, outside the auxiliary credential channel — run-derived "+
			"content they send would be funded by whatever the process env carries, judged by nobody: %v\n"+
			"Call auxllm.Registry() instead (the one credential decision a server-side auxiliary LLM surface "+
			"gets, #2645); only the run's own execution planes may construct a registry of their own, and those "+
			"live in this test's exempt map with their reason.", offenders)
	}
}

// registryConstructions counts `<modelPkg>.NewRegistry(…)` calls in the file,
// where modelPkg is the name THIS file imports pkg/backend/model under.
// It reads the AST rather than scanning text: a `NewRegistry(` inside a
// comment or a string satisfies a grep, and a call split across lines
// defeats a line-oriented one.
func registryConstructions(file *ast.File, modelPkg string) (calls int) {
	if modelPkg == "" {
		return 0 // the file does not import the model package at all
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewRegistry" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == modelPkg {
			calls++
		}
		return true
	})
	return calls
}

// modelImportName returns the local name under which the file imports
// pkg/backend/model, or "" when it does not.
func modelImportName(file *ast.File) string {
	const path = `"github.com/SocialGouv/iterion/pkg/backend/model"`
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "model"
	}
	return ""
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
			if got := registryConstructions(file, modelImportName(file)); got != tc.calls {
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
