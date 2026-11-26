package modelseams

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/backend/backendcaps"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestTheShippedCellsAreExactlyTheLoopsThatInstallTheTool closes the
// direction every other guard misses, and it is the DANGEROUS one.
//
// A registry cell carrying a wrong THIRD verdict (`partial`, `inert`, …)
// splits the compiler's predicate from the engine's and dies loud at
// dispatch. A cell carrying a wrong `shipped` says nothing at all: the
// compiler accepts, the executor grants, the task carries `Codemode: true`
// to a loop that never reads it, and the node runs without the capability its
// author declared — the silent degradation codemodePolicy exists to prevent.
// A closing adversarial round flipped claude_code's cell to `shipped`,
// regenerated the golden as any registry edit does, and watched
// pkg/dsl/ir, pkg/backend/backendcaps and pkg/backend/model all stay green.
//
// So the claim is pinned at BOTH ends: the set of cells that say shipped, and
// the set of generation loops that actually call installCodemode — the
// second read from the source with an AST walk rather than from a list, so a
// second loop cannot appear without this test seeing it.
func TestTheShippedCellsAreExactlyTheLoopsThatInstallTheTool(t *testing.T) {
	t.Parallel()

	root := sweepwalk.ModuleRoot(t)
	fset := token.NewFileSet()
	var callers []string
	for _, top := range []string{"pkg", "cmd", "internal"} {
		walkErr := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == ".works" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte("installCodemode(")) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "installCodemode" {
					callers = append(callers, filepath.ToSlash(rel))
					// And WHAT it passes, not only who calls it. The
					// bounds argument must be the TASK's, read from the
					// field: `installCodemode(&opts, nil, …)` compiles
					// and leaves every program on DefaultLimits while the
					// author declared otherwise, and it was measured to
					// survive the whole of ./pkg/backend/model/.
					//
					// This is a SOURCE guard, not a behavioural one, and
					// it is here because the behavioural reach is the
					// generation loop's opts — which no seam exposes
					// without a live model. The sandbox crossing has the
					// behavioural witness for its own direction
					// (TestCodemodeChangesNothingOnTheWireToASandboxRunner),
					// and production is the NON-sandboxed path, so this
					// one call is what carries it there.
					if len(call.Args) < 3 {
						t.Errorf("%s: installCodemode called with %d argument(s)", rel, len(call.Args))
					} else {
						// The defer state rides the same rule: the TASK's
						// resolved field, never a literal that would defer
						// (or not) every node whatever its author wrote.
						if sel, ok := call.Args[1].(*ast.SelectorExpr); !ok || sel.Sel == nil || sel.Sel.Name != "CodemodeDefer" {
							t.Errorf("%s: installCodemode's defer argument is %T, want the task's .CodemodeDefer", rel, call.Args[1])
						}
						sel, ok := call.Args[2].(*ast.SelectorExpr)
						if !ok || sel.Sel == nil || sel.Sel.Name != "CodemodeBounds" {
							t.Errorf("%s: installCodemode's bounds argument is %T, want the task's .CodemodeBounds — a literal there runs every program on the defaults", rel, call.Args[2])
						}
					}
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", top, walkErr)
		}
	}
	slices.Sort(callers)
	callers = slices.Compact(callers)
	if len(callers) == 0 {
		t.Fatal("the sweep found no installCodemode call at all — the predicate stopped matching, and this test is blind")
	}
	if want := []string{"pkg/backend/model/claw_backend.go"}; !slices.Equal(callers, want) {
		t.Errorf("installCodemode is called from %v, want %v — a new install site must change a registry cell in the same commit", callers, want)
	}

	// The registry's side of the same fact. The two sets are named
	// separately and compared, rather than one being derived from the other:
	// a derivation would make this test agree with itself.
	if got, want := ir.CodemodeServingBackends(), []string{backendcaps.Claw}; !slices.Equal(got, want) {
		t.Errorf("the registry says %v serve codemode, but only the claw generation loop installs the tool: a `shipped` cell on %v hands those nodes a Codemode no loop reads", got, got)
	}
}
