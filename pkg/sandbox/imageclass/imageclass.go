// Package imageclass holds the operator-controlled vocabulary of stock
// sandbox image classes: the bare names a workflow's `sandbox: { image: … }`
// may declare instead of a full image reference (`image: rgaa`). Each class
// names one `sandbox/<class>/Dockerfile` of this tree, published as
// ghcr.io/socialgouv/iterion-sandbox-<class> (docs/sandbox.md); pkg/runtime
// resolves the name to that reference, pinned to the running binary.
//
// The vocabulary is a closed list on purpose (#2632): the image is the code
// a run's isolation boundary is built from, so the images a bot can name in
// one word are the variants iterion itself builds, publishes and measures
// (pkg/backend/backendcaps derives its image-presence cells from those very
// manifests). Everything else remains sayable as the full reference it is —
// `image: "registry/repo:tag"`, `build:` and `--sandbox-default-image` are
// the escape hatch, and a bare word that is no class is passed through
// verbatim (a locally built image), with the compiler's C325 saying so.
//
// A leaf: the DSL compiler (C325) and the runtime (resolution, the stock
// family the image admission judges) both read this list, and neither may
// import the other.
package imageclass

import "strings"

// classes is the vocabulary, in the order docs and diagnostics name it. A
// class joins the list when its `sandbox/<class>/Dockerfile` and its publish
// wiring (.github/workflows/sandbox-images.yml, _finalize.yml) land —
// TestTheStockSandboxFamilyIsTheOneTheTreeBuilds (pkg/runtime) holds the
// list to the tree in both directions.
var classes = []string{"slim", "full", "sec", "browser", "rgaa"}

// browserCarriers are the classes whose image ships Chromium (through the
// browser base): the DSL's Playwright-MCP gate (C060) reads this rather than
// re-deriving it from the name.
var browserCarriers = map[string]bool{"browser": true, "rgaa": true}

// Classes returns the vocabulary, a fresh copy on every call.
func Classes() []string { return append([]string(nil), classes...) }

// Known reports whether name is a declared class.
func Known(name string) bool {
	for _, c := range classes {
		if c == name {
			return true
		}
	}
	return false
}

// CarriesBrowser reports whether the class's image ships Chromium.
func CarriesBrowser(name string) bool { return browserCarriers[name] }

// LooksLikeClassName reports whether s is SHAPED like a class declaration:
// one bare word. A value carrying '/', ':', '@' or a reference form ('$',
// '{{') is an image reference or an indirection, never a class name — those
// pass through untouched by design.
func LooksLikeClassName(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, "/:@$ \t\n") && !strings.Contains(s, "{{")
}
