package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// imageEnvScreenSeam is the function every start of this package must take
// its prepared spec FROM, spelled once so the guard below and its failure
// messages cannot drift apart.
const imageEnvScreenSeam = "screenAndReportSandboxImageEnv"

// driverStartArity is the shape the guard keys on: a sandbox driver's
// Start takes a context, the prepared spec and the run info. Keying on the
// SHAPE rather than on the receiver's spelling is what makes this a class
// guard — the only other `.Start(` of this package is the network proxy's
// one-argument `prx.Start(bind)`, and a start site that renames its local
// stays in the set.
const driverStartArity = 3

// imageEnvScreenExemptStarts names every function of this package that
// hands a prepared spec to a sandbox driver WITHOUT screening what the
// image bakes into its own `ENV`, each with the reason it owes none.
//
// A start that is not listed here must start the spec the screen seam
// RETURNED. The reason this is a class guard and not a line review: the
// drivers emit `--env` for `spec.Env` alone, so an unscreened start is one
// planted `ENV ANTHROPIC_BASE_URL=` away from re-routing every prompt of
// the run (#2317), and the next start site added to this package would
// inherit the hole in silence. The witnesses in
// sandbox_image_env_event_test.go prove what the seam DOES,
// TestTheContainerStartsWithTheSpecTheScreenReturned proves the boot
// starts what it returned, and this one proves no OTHER start site drops
// out of that promise.
//
// A call this guard flags that is not a sandbox start at all belongs here
// too, with that as its reason: an over-approximating shape is the price
// of a set no renaming can shrink.
var imageEnvScreenExemptStarts = map[string]string{
	"startNoopSandbox": "the noop passthrough runs its commands on the host, " +
		"so Capabilities().SupportsImage is false and there is no image env to read; " +
		"the screen's own capability guard answers the same way for the next passthrough",
}

// TestEveryDriverStartScreensTheImagesEnv reads this package's own
// source and holds the wiring of #2317 at the class: every start of a
// sandbox driver is handed the identifier the screen seam's result was
// assigned to, in its own function — or declared above with the reason it
// needs none.
//
// Binding on the identifier, not merely on the presence of a call, is the
// point: a boot that CALLS the seam — so the event fires, with a perfectly
// shaped `neutralized: […]` — and then starts the PRE-screen spec is the
// one state this whole seam exists to make impossible, and it satisfies
// any guard that only asks whether the seam was called.
func TestEveryDriverStartScreensTheImagesEnv(t *testing.T) {
	starts, screens := readDriverStarts(t)

	// Anti-vacuity: an empty reading finds no unscreened start and this
	// guard would certify a package it never parsed. HEAD has two.
	if len(starts) < 2 {
		t.Fatalf("parsed only %d function(s) handing a prepared spec to a sandbox driver (%v): the source reader is broken, and an empty set would make this guard pass vacuously",
			len(starts), starts)
	}

	var unscreened []string
	for _, fn := range starts {
		if reason, exempt := imageEnvScreenExemptStarts[fn]; exempt {
			// Anti-vacuity on the OTHER side of the reader: the exempt
			// function screens nothing, by construction. Reading it as
			// screened means the reader says yes to a start it never
			// bound — and would say yes to the next unscreened one.
			if screens[fn] {
				t.Errorf("%s is exempt and reads as SCREENED: either the reader binds a start it should not (every verdict below is then worthless), or the function now screens and this exemption is stale", fn)
			}
			t.Logf("%s starts a container without the image env screen — %s", fn, reason)
			continue
		}
		if screens[fn] {
			continue
		}
		unscreened = append(unscreened, fn)
	}
	if len(unscreened) > 0 {
		sort.Strings(unscreened)
		t.Fatalf("the following function(s) start a sandbox driver with a prepared spec that is NOT the one %s returned — the image's own ENV reaches every process iterion runs in the container, and the deny class the devcontainer seam enforces never sees it (#2317):\n  - %s\n\nAssign the seam's result and start THAT identifier, or add the function to imageEnvScreenExemptStarts with the reason it owes no screen.",
			imageEnvScreenSeam, strings.Join(unscreened, "\n  - "))
	}

	// A stale exemption is as bad as a missing one: it reads as a
	// reviewed hole in a function that no longer exists, and the next
	// reader trusts it.
	found := map[string]bool{}
	for _, fn := range starts {
		found[fn] = true
	}
	for fn := range imageEnvScreenExemptStarts {
		if !found[fn] {
			t.Errorf("imageEnvScreenExemptStarts names %q, which hands nothing to a sandbox driver any more: drop the row rather than leave a reviewed hole pointing at nothing", fn)
		}
	}
}

// readDriverStarts returns the names of the functions of this package's
// non-test sources that hand a prepared spec to a sandbox driver, and
// which of them start the spec the image env screen RETURNED.
//
// A start is read by the SHAPE of its call — `<ident>.Start(a, b, c)` —
// never by the spelling of the local the driver is bound to: a renamed
// receiver would otherwise drop out of the set silently, and the
// anti-vacuity floor only notices when the set empties.
//
// Screened means BOUND: every start of the function passes, as its
// prepared-spec argument, an identifier the seam's result was assigned to
// in that same function. A function with a start and no such assignment
// is not screened, which is what makes a discarded screen result visible
// here and not only in the boot witness.
func readDriverStarts(t *testing.T) ([]string, map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var starts []string
	screens := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			screened := identsAssignedFromTheScreen(fn.Body)
			startsHere, bound := readStartCalls(fn.Body, screened)
			if startsHere {
				starts = append(starts, fn.Name.Name)
				screens[fn.Name.Name] = bound
			}
		}
	}
	sort.Strings(starts)
	return starts, screens
}

// identsAssignedFromTheScreen collects the identifiers this body binds to
// the screen seam's result. The seam's FIRST result is the spec to start,
// so the name it lands in is the only one a start may legitimately pass.
func identsAssignedFromTheScreen(body *ast.BlockStmt) map[string]bool {
	screened := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) == 0 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if fun, ok := call.Fun.(*ast.Ident); !ok || fun.Name != imageEnvScreenSeam {
			return true
		}
		if lhs, ok := assign.Lhs[0].(*ast.Ident); ok {
			screened[lhs.Name] = true
		}
		return true
	})
	return screened
}

// readStartCalls reports whether this body starts a sandbox driver, and
// whether EVERY such start is handed one of the screened identifiers. One
// unbound start is enough to answer no: the guard's subject is the spec a
// container is created with, and a function with two starts owes the
// screen at both.
func readStartCalls(body *ast.BlockStmt, screened map[string]bool) (startsHere, bound bool) {
	bound = true
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		target, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || target.Sel.Name != "Start" || len(call.Args) != driverStartArity {
			return true
		}
		// A selector on anything but a local — `otel.Tracer(x).Start(…)`
		// — is not a driver bound to a variable.
		if _, ok := target.X.(*ast.Ident); !ok {
			return true
		}
		startsHere = true
		spec, ok := call.Args[1].(*ast.Ident)
		if !ok || !screened[spec.Name] {
			bound = false
		}
		return true
	})
	return startsHere, bound
}

// TestTheWiringGuardReadsAStartWhateverItsReceiverIsCalled is the guard's
// own witness, and the reason F2 of the round-2 review is closed: the set
// of start sites must not depend on the spelling of a local. It feeds the
// two readers the shapes that matter, parsed from source the way they
// parse this package.
func TestTheWiringGuardReadsAStartWhateverItsReceiverIsCalled(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		wantStarts []string
		wantBound  map[string]bool
	}{{
		name: "a renamed receiver is still a start, and an unassigned screen is not bound",
		src: `package p
func boot(ctx C, d D, prepared P, info I) {
	screenAndReportSandboxImageEnv(ctx, d, prepared, nil, nil, "")
	_, _ = d.Start(ctx, prepared, info)
}`,
		wantStarts: []string{"boot"},
		wantBound:  map[string]bool{"boot": false},
	}, {
		name: "the seam's result assigned and started is bound",
		src: `package p
func boot(ctx C, driver D, prepared P, info I) {
	prepared, err = screenAndReportSandboxImageEnv(ctx, driver, prepared, nil, nil, "")
	_ = err
	_, _ = driver.Start(ctx, prepared, info)
}`,
		wantStarts: []string{"boot"},
		wantBound:  map[string]bool{"boot": true},
	}, {
		name: "the seam's result assigned and the ORIGINAL started is not bound",
		src: `package p
func boot(ctx C, driver D, prepared P, info I) {
	screened, err = screenAndReportSandboxImageEnv(ctx, driver, prepared, nil, nil, "")
	_, _ = screened, err
	_, _ = driver.Start(ctx, prepared, info)
}`,
		wantStarts: []string{"boot"},
		wantBound:  map[string]bool{"boot": false},
	}, {
		name: "a one-argument Start is not a sandbox start",
		src: `package p
func listen(prx P, bind string) {
	_ = prx.Start(bind)
}`,
		wantStarts: nil,
		wantBound:  map[string]bool{},
	}, {
		name: "a start on a method chain is not a driver bound to a local",
		src: `package p
func trace(ctx C, name string, opt O) {
	_, _ = otel.Tracer(name).Start(ctx, name, opt)
}`,
		wantStarts: nil,
		wantBound:  map[string]bool{},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "probe.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse the probe: %v", err)
			}
			var starts []string
			bound := map[string]bool{}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				startsHere, isBound := readStartCalls(fn.Body, identsAssignedFromTheScreen(fn.Body))
				if startsHere {
					starts = append(starts, fn.Name.Name)
					bound[fn.Name.Name] = isBound
				}
			}
			if !slices.Equal(starts, tc.wantStarts) {
				t.Fatalf("starts = %v, want %v", starts, tc.wantStarts)
			}
			for fn, want := range tc.wantBound {
				if bound[fn] != want {
					t.Errorf("%s bound = %v, want %v", fn, bound[fn], want)
				}
			}
		})
	}
}
