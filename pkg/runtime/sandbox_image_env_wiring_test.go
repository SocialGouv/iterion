package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// imageEnvScreenExemptStarts names every function of this package that
// hands a prepared spec to a sandbox driver WITHOUT screening what the
// image bakes into its own `ENV`, each with the reason it owes none.
//
// A start that is not listed here must call the screen seam. The reason
// this is a class guard and not a line review: the drivers emit `--env`
// for `spec.Env` alone, so an unscreened start is one planted
// `ENV ANTHROPIC_BASE_URL=` away from re-routing every prompt of the run
// (#2317), and the next start site added to this package would inherit
// the hole in silence. The witnesses in
// sandbox_image_env_event_test.go prove what the seam DOES; this one
// proves the boot still goes through it.
var imageEnvScreenExemptStarts = map[string]string{
	"startNoopSandbox": "the noop passthrough runs its commands on the host, " +
		"so Capabilities().SupportsImage is false and there is no image env to read; " +
		"the screen's own capability guard answers the same way for the next passthrough",
}

// TestEveryDriverStartScreensTheImagesEnv reads this package's own
// source and holds the wiring of #2317 at the class: every
// `driver.Start` is preceded, in its own function, by the screen seam —
// or declared above with the reason it needs none.
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
		if screens[fn] {
			continue
		}
		if reason, exempt := imageEnvScreenExemptStarts[fn]; exempt {
			t.Logf("%s starts a container without the image env screen — %s", fn, reason)
			continue
		}
		unscreened = append(unscreened, fn)
	}
	if len(unscreened) > 0 {
		sort.Strings(unscreened)
		t.Fatalf("the following function(s) hand a prepared spec to a sandbox driver without calling screenAndReportSandboxImageEnv — the image's own ENV reaches every process iterion runs in the container, and the deny class the devcontainer seam enforces never sees it (#2317):\n  - %s\n\nCall the seam before Start, or add the function to imageEnvScreenExemptStarts with the reason it owes no screen.",
			strings.Join(unscreened, "\n  - "))
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

// readDriverStarts returns the names of the functions calling
// `driver.Start(…)` in this package's non-test sources, and which of them
// also call the image env screen seam. The receiver is read by name:
// both start sites bind the driver to `driver`, and a renamed receiver
// drops out of the set, which the anti-vacuity floor above catches.
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
			var startsHere, screensHere bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch target := call.Fun.(type) {
				case *ast.SelectorExpr:
					recv, ok := target.X.(*ast.Ident)
					if ok && recv.Name == "driver" && target.Sel.Name == "Start" {
						startsHere = true
					}
				case *ast.Ident:
					if target.Name == "screenAndReportSandboxImageEnv" {
						screensHere = true
					}
				}
				return true
			})
			if startsHere {
				starts = append(starts, fn.Name.Name)
				screens[fn.Name.Name] = screensHere
			}
		}
	}
	sort.Strings(starts)
	return starts, screens
}
