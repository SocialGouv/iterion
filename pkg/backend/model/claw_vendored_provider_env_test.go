package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The in-container runner rebuilds its registry from the env that crossed,
// and that registry builds claw's own provider packages: whatever THEY read
// from the environment is read inside the container, under names no provider
// table carries. TestEveryEnvReadIsClassifiedForTheSandbox walks the reads of
// THIS package and says so itself — "a variable another package reads under a
// name it spells itself [is] that package's to forward (#1804)". This file is
// that forwarding, and its guard: the names are DERIVED from the vendored
// claw the registry imports, never hand-listed, so a provider claw gains
// arrives classified or red.
//
// The hole it closes: a sandboxed foundry node ran against claw's default
// API version while the launcher's AZURE_OPENAI_API_VERSION stayed on the
// launcher — the host and its sandboxed twin on two API contracts, with
// nothing said. Vertex was worse: GOOGLE_CLOUD_PROJECT unset in the
// container is refused by name, so the node simply did not run.
//
// The derivation fails LOUDLY when a package disappears: vendor/ is being
// pruned (ADR-142), and a pruned provider would otherwise shrink this list
// in silence — the shape that makes a guard look green over nothing.
//
// What it does not see, named rather than hidden: a name claw builds at run
// time instead of spelling it (os.Environ, a name from a constant of its
// own), and a name read OUTSIDE the provider packages the registry imports —
// a shared claw package on the same path, or claw's tool packages, whose
// twelve names are #2802's subject (the `web_search` five of them are
// decided below, because this ticket decided them).

// The classes a vendored provider's env name can be in.
const (
	classCredential = "credential" // the key the model call authenticates with
	classRouting    = "routing"    // WHICH deployment, project, region or API contract the key is spent on
	classLauncher   = "launcher"   // read by code that runs on the launcher, never in the container
)

// vendoredProviderEnvClass carries, for every env name claw's provider
// packages read, its class and why. A name the walk finds that is not here
// fails the test; an entry the walk no longer finds fails too.
var vendoredProviderEnvClass = map[string]struct {
	class string
	why   string
}{
	"AZURE_OPENAI_API_KEY":  {classCredential, "foundry authenticates with it when the run resolved no key of its own"},
	"AZURE_OPENAI_ENDPOINT": {classRouting, "the Azure resource foundry dials; refused by name when unset"},
	"AZURE_OPENAI_DEPLOYMENT": {classRouting,
		"the deployment a model id that names none falls back to: the launcher's choice of WHICH model answers"},
	"AZURE_OPENAI_API_VERSION": {classRouting,
		"the API contract foundry speaks; claw defaults it to a date, so an absent name is a silent second contract"},
	"GOOGLE_CLOUD_PROJECT": {classRouting,
		"the project vertex bills and dials; refused by name when unset, so a sandboxed vertex node did not run at all"},
	"GOOGLE_CLOUD_REGION": {classRouting,
		"the region vertex dials; claw defaults it, so an absent name is a silent second region"},
	"CLAW_OPENAI_REASONING_SUMMARY": {classRouting,
		"the reasoning-summary kill switch on claw's own openai path: empty is claw's `auto`, so the explicit empty says the launcher sets no override"},
}

// webSearchLauncherEnv are the names claw's web_search tool reads. They are
// the negative half of this ticket, decided EXPLICITLY rather than by
// omission (Jo, 2026-09-24): web_search is served on the launcher, with the
// launcher's credentials, so they stay home — and the test asserts that
// placement rather than quoting it, so moving the tool reopens the question.
var webSearchLauncherEnv = []string{"BRAVE_API_KEY", "SEARXNG_URL", "SEARXNG_ENDPOINT"}

// clawWebSearchSource is the vendored file those names are read in: reading
// them from it keeps the list honest against a rename, and against a prune.
const clawWebSearchSource = "vendor/github.com/SocialGouv/claw-code-go/internal/tools/web_search.go"

// TestEveryVendoredProviderEnvNameIsDecided derives the env names claw's
// provider packages read and requires each to be forwarded with its class, or
// declared launcher-side with a reason.
func TestEveryVendoredProviderEnvNameIsDecided(t *testing.T) {
	root := moduleRootForVendorWalk(t)
	dirs, providers := vendoredProviderDirs(t, root)
	// The providers the registry builds today. A rename or a prune that
	// hides one shrinks the derivation, so the floor is pinned: this is the
	// dep-slim hazard the ticket names.
	for _, want := range []string{"anthropic", "bedrock", "foundry", "moonshot", "openai", "vertex"} {
		if !slices.Contains(providers, want) {
			t.Fatalf("the registry no longer imports claw's %s provider (found %v): either the provider left claw — then its names leave this classification with it, deliberately — or the derivation stopped seeing registry.go's imports, and every name below is unguarded", want, providers)
		}
	}
	names := envNamesReadIn(t, dirs)
	if len(names) == 0 {
		t.Fatalf("no env read found in claw's provider packages (%v): the walk no longer matches the vendored source, so a green verdict here proves nothing", dirs)
	}

	forwarded := map[string]bool{}
	for _, name := range providerCredentialEnvVars {
		forwarded[name] = true
	}
	for _, name := range names {
		got, ok := vendoredProviderEnvClass[name]
		if !ok {
			t.Errorf("claw's provider packages read %s inside the container, and nothing decides it: forward it with its class (providerCredentialEnvVars, plus providerRoutingEnvVars when it decides WHERE the credential is spent), or declare it launcher-side with the reason — until then a sandboxed node runs against another endpoint, deployment or contract than the one the launcher is configured for (#1804)", name)
			continue
		}
		switch got.class {
		case classCredential, classRouting:
			if !forwarded[name] {
				t.Errorf("%s is classified %s (%s) but is not on providerCredentialEnvVars: the launcher holds it and the in-container runner never sees it", name, got.class, got.why)
			}
			if got.class == classRouting && !slices.Contains(providerRoutingEnvVars, name) {
				t.Errorf("%s decides WHERE a forwarded credential is spent but is not in providerRoutingEnvVars: a name the host leaves unset then crosses ABSENT, and the workspace .env of the repository under review fills it", name)
			}
		case classLauncher:
			if forwarded[name] {
				t.Errorf("%s is declared launcher-side (%s) but crosses anyway: decide it once", name, got.why)
			}
		default:
			t.Errorf("%s has no class (%q): say credential, routing or launcher", name, got.class)
		}
		if strings.TrimSpace(got.why) == "" {
			t.Errorf("%s is classified %s with no reason: the next reader must be able to argue with it", name, got.class)
		}
	}
	for name := range vendoredProviderEnvClass {
		if !slices.Contains(names, name) {
			t.Errorf("the classification carries %s, which claw's provider packages no longer read: drop the entry, and drop the forwarding with it if nothing else needs the name", name)
		}
	}
}

// TestClawsWebSearchCredentialsStayOnTheLauncher is the negative half: the
// names claw's web_search reads must NOT cross, and the reason is read from
// the placement table rather than quoted — a web_search served in the
// container would make this a hole, and the test says so in that voice.
func TestClawsWebSearchCredentialsStayOnTheLauncher(t *testing.T) {
	root := moduleRootForVendorWalk(t)
	source := filepath.Join(root, clawWebSearchSource)
	names := envNamesReadIn(t, []string{source})
	for _, want := range webSearchLauncherEnv {
		if !slices.Contains(names, want) {
			t.Errorf("%s names %v and not %s: the list this ticket decided no longer matches claw's tool, so re-read it against %s", clawWebSearchSource, names, want, clawWebSearchSource)
		}
	}
	placement, why := tool.SandboxPlacementOf("web_search")
	if placement != tool.PlacementLauncher {
		t.Fatalf("web_search is placed %s (%s), not on the launcher: its credentials now have to reach the container, so this ticket's negative half must be decided again, not left to omission (#1804)", placement, why)
	}
	for _, name := range webSearchLauncherEnv {
		if slices.Contains(providerCredentialEnvVars, name) {
			t.Errorf("%s crosses into the container, but web_search runs on the launcher (%s): a search credential in a container a fork's agent runs in buys nothing and leaks a key", name, why)
		}
	}
}

// TestASandboxedFoundryNodeCarriesTheLaunchersAzureRouting asserts the
// envelope the in-container runner RECEIVES, not the host's own environment:
// the routing names the launcher holds are in it, and the launcher-side
// search credentials are not.
func TestASandboxedFoundryNodeCarriesTheLaunchersAzureRouting(t *testing.T) {
	t.Setenv("AZURE_OPENAI_ENDPOINT", "https://launcher-resource.openai.azure.com")
	t.Setenv("AZURE_OPENAI_API_KEY", "launcher-key")
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "launcher-deployment")
	t.Setenv("AZURE_OPENAI_API_VERSION", "2025-04-01-preview")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "launcher-project")
	t.Setenv("GOOGLE_CLOUD_REGION", "europe-west4")
	for _, name := range webSearchLauncherEnv {
		t.Setenv(name, "launcher-search-secret")
	}

	env, err := forwardableProviderEnv(context.Background(), "foundry/gpt-4o-mini")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	for name, want := range map[string]string{
		"AZURE_OPENAI_ENDPOINT":    "https://launcher-resource.openai.azure.com",
		"AZURE_OPENAI_DEPLOYMENT":  "launcher-deployment",
		"AZURE_OPENAI_API_VERSION": "2025-04-01-preview",
		"GOOGLE_CLOUD_PROJECT":     "launcher-project",
		"GOOGLE_CLOUD_REGION":      "europe-west4",
	} {
		got, ok := env[name]
		if !ok {
			t.Errorf("%s is set on the launcher and absent from the env the runner receives: claw reads it inside the container, so the sandboxed node runs against another %s than the host's", name, name)
			continue
		}
		if got != want {
			t.Errorf("%s crossed as %q, want the launcher's %q", name, got, want)
		}
	}
	for _, name := range webSearchLauncherEnv {
		if got, ok := env[name]; ok {
			t.Errorf("%s crossed as %q: web_search is served on the launcher, so the container has no use for the key and every reason not to hold it", name, got)
		}
	}
	// A routing name the launcher leaves unset crosses as an explicit
	// empty, never absent: the runner auto-loads the workspace .env, which
	// fills only what is missing.
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "")
	env, err = forwardableProviderEnv(context.Background(), "foundry/gpt-4o-mini")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if got, ok := env["AZURE_OPENAI_DEPLOYMENT"]; !ok || got != "" {
		t.Errorf("AZURE_OPENAI_DEPLOYMENT crossed as (%q, present=%v) with the launcher's unset, want the explicit empty: absent lets the repository under review name the deployment its own .env picks", got, ok)
	}
}

// vendoredProviderDirs derives, from the imports of registry.go, the vendored
// claw provider directories whose env reads run inside the container: the
// re-exporting package the registry imports, and the internal package it
// forwards to.
func vendoredProviderDirs(t *testing.T, root string) (dirs []string, providers []string) {
	t.Helper()
	const providerPrefix = "github.com/SocialGouv/claw-code-go/"
	registry := filepath.Join(root, "pkg", "backend", "model", "registry.go")
	seen := map[string]bool{}
	var walkImports func(path string)
	walkImports = func(importPath string) {
		if seen[importPath] {
			return
		}
		seen[importPath] = true
		dir := filepath.Join(root, "vendor", filepath.FromSlash(importPath))
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("claw's %s is imported but not vendored (%v): a prune took the package, and with it every env name it reads — decide the names it used to read before pruning it", importPath, err)
		}
		dirs = append(dirs, dir)
		for _, next := range importsOfDir(t, dir) {
			if strings.HasPrefix(next, providerPrefix+"internal/api/providers/") {
				walkImports(next)
			}
		}
	}
	for _, importPath := range importsOfFile(t, registry) {
		name, ok := strings.CutPrefix(importPath, providerPrefix+"pkg/api/providers/")
		if !ok || strings.Contains(name, "/") {
			continue
		}
		providers = append(providers, name)
		walkImports(importPath)
	}
	sort.Strings(providers)
	sort.Strings(dirs)
	return dirs, providers
}

func importsOfFile(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func importsOfDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, path := range nonTestGoFilesIn(t, dir) {
		out = append(out, importsOfFile(t, path)...)
	}
	return out
}

// envNamesReadIn returns, sorted and deduplicated, every env name the given
// files and directories read as a literal.
func envNamesReadIn(t *testing.T, paths []string) []string {
	t.Helper()
	found := map[string]bool{}
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.IsDir() {
			files = append(files, nonTestGoFilesIn(t, path)...)
			continue
		}
		files = append(files, path)
	}
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "os" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if name, err := strconv.Unquote(lit.Value); err == nil && name != "" {
				found[name] = true
			}
			return true
		})
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func nonTestGoFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// moduleRootForVendorWalk walks up from the test's working directory to the
// module root, so the walk reads the vendored tree it is built against.
func moduleRootForVendorWalk(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory: cannot locate the module root")
		}
		dir = parent
	}
}
