package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The class #2844's second half is about: the environment variables that
// decide WHICH HOST a model call is dialled at, and the credentials
// presented there. A project `.env` fills every name the operator left unset,
// so naming the endpoint alone picks the host their key is spent on — and the
// loader refuses exactly the names internal/envtrust answers for
// (credentialsPresentedAt). This test is the completeness guard on that
// table: a host-deciding name that is in neither the table nor the
// exclusions below fails, so the next provider arrives decided or red.
//
// The set is DERIVED from three sources that define it, never from one
// reviewer's list:
//
//   - providerRoutingEnvVars, this package's own declaration of the names
//     that decide "WHICH infrastructure, deployment, project, region or API
//     contract a forwarded credential is spent on";
//   - the classRouting half of vendoredProviderEnvClass, which
//     TestEveryVendoredProviderEnvNameIsDecided derives from the vendored
//     claw provider packages the registry imports — the reads that are not
//     iterion's and that no guard of iterion's can reach;
//   - the host-shaped env names read on a model's path, walked below.
//
// What the walk cannot see, named rather than hidden: a name read through a
// CONSTANT rather than a literal. compatgw's pair is the one such case today
// and is added by hand from its own exported constant; a package that hides a
// new endpoint behind a constant hides it from here too, which is why the
// first two sources are declarations rather than walks.
var hostDecidingEnvDirs = []string{
	"pkg/backend/model",
	"pkg/backend/compatgw",
	"pkg/backend/detect",
	"pkg/backend/delegate",
	"pkg/backend/modelspecs",
}

// notACredentialHost carries, for every host-deciding name the sources yield
// that the loader does NOT refuse, why a project `.env` filling it moves no
// credential off the operator's infrastructure. An entry the sources no
// longer yield fails too: a reason nothing applies to is a reason nobody
// re-reads.
var notACredentialHost = map[string]string{
	"AZURE_OPENAI_DEPLOYMENT":       "names a deployment INSIDE the resource AZURE_OPENAI_ENDPOINT names, so the key stays on the operator's own Azure resource; it picks WHICH model answers there, which is a capability question and not a credential one",
	"AZURE_OPENAI_API_VERSION":      "the API contract spoken to that same resource; claw defaults it to a date, so the crossing's explicit empty is what keeps host and container on one contract",
	"GOOGLE_CLOUD_PROJECT":          "vertex authenticates with Google ADC, which no variable here carries — the SDK resolves it from GOOGLE_APPLICATION_CREDENTIALS, gcloud's own file or the instance metadata server. A planted project would bill a project the repository named with whatever ADC resolves, and that is NOT closed here: the loader can only refuse a name whose credential it can see",
	"GOOGLE_CLOUD_REGION":           "picks among Google's own regions for the same ADC identity, so the credential never leaves Google's infrastructure",
	"CLAW_OPENAI_REASONING_SUMMARY": "a kill switch on claw's own openai path, not a host: empty is claw's `auto` and no credential moves",
}

// TestEveryHostDecidingEnvNameIsDecidedForTheCredentialAtIt is the class
// guard: every name the three sources yield is either refused at the plant
// (internal/envtrust) or excluded with a reason.
func TestEveryHostDecidingEnvNameIsDecidedForTheCredentialAtIt(t *testing.T) {
	root := moduleRoot(t)
	var dirs []string
	for _, dir := range hostDecidingEnvDirs {
		dirs = append(dirs, filepath.Join(root, dir))
	}
	walked := hostShapedEnvNamesReadIn(t, dirs)
	if len(walked) == 0 {
		t.Fatalf("no host-shaped env name found under %v: the walk no longer matches the source, so a green verdict here proves nothing", hostDecidingEnvDirs)
	}
	// The floor the walk must keep seeing: these four are read by this
	// package's own factories, so a walk that stops finding them has stopped
	// looking rather than been emptied by a fix.
	for _, want := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "XAI_BASE_URL", "MOONSHOT_BASE_URL"} {
		if !slices.Contains(walked, want) {
			t.Fatalf("the walk no longer finds %s (found %v): either the factory stopped reading it, or the walk stopped matching", want, walked)
		}
	}

	class := map[string]bool{compatgw.BaseURLEnv: true}
	for _, name := range walked {
		class[name] = true
	}
	for _, name := range providerRoutingEnvVars {
		class[name] = true
	}
	for name, entry := range vendoredProviderEnvClass {
		if entry.class == classRouting {
			class[name] = true
		}
	}

	refused := map[string]bool{}
	for _, name := range envtrust.EndpointNames() {
		refused[name] = true
		if len(envtrust.CredentialsPresentedAt(name)) == 0 {
			t.Errorf("%s is refused at the plant for no credential: a refusal with nothing at stake costs a laptop its configuration and protects nothing", name)
		}
		if !class[name] {
			t.Errorf("%s is refused at the plant but none of the three sources yields it: either it decides no host any more — then drop the entry — or the sources stopped covering the package that reads it", name)
		}
	}
	for name := range class {
		if refused[name] {
			if why, excluded := notACredentialHost[name]; excluded {
				t.Errorf("%s is both refused at the plant and excluded (%s): decide it once", name, why)
			}
			continue
		}
		if why, excluded := notACredentialHost[name]; excluded {
			if why == "" {
				t.Errorf("%s is excluded with no reason: the next reader must be able to argue with it", name)
			}
			continue
		}
		t.Errorf("%s decides where a model call goes and nothing decides the credential at it: add it to internal/envtrust's credentialsPresentedAt with the keys presented there, or exclude it here with the reason a project `.env` filling it moves no credential — until then the repository under review picks the host the operator's key is spent on (#2844)", name)
	}
	for name := range notACredentialHost {
		if !class[name] {
			t.Errorf("%s is excluded here, and none of the three sources yields it any more: drop the entry with the name", name)
		}
	}
}

// Every key a provider authenticates with is presented at SOME host, so the
// table has to know about it: byokEnvVar is this package's own declaration
// of that set, and a key missing from the table is a key the loader would
// let a repository redirect.
func TestEveryProviderKeyIsPresentedAtAHostTheTableKnows(t *testing.T) {
	presented := map[string]bool{}
	for _, endpoint := range envtrust.EndpointNames() {
		for _, cred := range envtrust.CredentialsPresentedAt(endpoint) {
			presented[cred] = true
		}
	}
	if len(presented) == 0 {
		t.Fatal("the table presents no credential at all: the rest of this case proves nothing")
	}
	for provider, name := range byokEnvVar {
		if !presented[name] {
			t.Errorf("%s authenticates provider %q and sits at no host the table knows: a project `.env` can name the endpoint it is presented to", name, provider)
		}
	}
}

// hostShapedEnvNamesReadIn walks non-test Go files for env reads with a
// literal name — os.Getenv / os.LookupEnv, and envtrust's own Inherited /
// Planted, since a name already read through the provenance seam is still
// part of the class — and keeps the ones whose last underscore-separated
// segment names a host: a *_BASE_URL, an *_ENDPOINT, a *_HOST.
func hostShapedEnvNamesReadIn(t *testing.T, paths []string) []string {
	t.Helper()
	found := map[string]bool{}
	var files []string
	for _, path := range paths {
		files = append(files, nonTestGoFilesIn(t, path)...)
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
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv"):
			case pkg.Name == "envtrust" && (sel.Sel.Name == "Inherited" || sel.Sel.Name == "Planted"):
			default:
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil || !hostShapedName(name) {
				return true
			}
			found[name] = true
			return true
		})
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// hostShapedName reports whether a variable's name says it carries a host:
// the shape, not a list, so the next provider's endpoint is covered the day
// it is read and not the day someone remembers this rule.
func hostShapedName(name string) bool {
	switch {
	case len(name) > len("_URL") && name[len(name)-len("_URL"):] == "_URL":
		return true
	case len(name) > len("_ENDPOINT") && name[len(name)-len("_ENDPOINT"):] == "_ENDPOINT":
		return true
	case len(name) > len("_HOST") && name[len(name)-len("_HOST"):] == "_HOST":
		return true
	}
	return false
}

// The container half of the same rule. The crossing hands the in-container
// runner the RUN's own credentials — a tenant's BYOK key, a lent
// subscription — so a routing name this host's project `.env` planted would
// send that credential to a host the repository under review chose, one hop
// past the loader. It crosses as the explicit empty every registry reads as
// "use the published default".
func TestAPlantedRoutingNameCrossesAsAnEmptyIntoTheSandbox(t *testing.T) {
	const operatorEndpoint = "https://gw.operator.example"
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("OPENAI_BASE_URL", operatorEndpoint)

	// Control: the operator's own, which must keep crossing — host and
	// container on one endpoint is what the forwarding exists for.
	env, _, err := forwardableProviderEnv(context.Background(), "openai/gpt-4o-mini")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if got := env["OPENAI_BASE_URL"]; got != operatorEndpoint {
		t.Fatalf("the operator's endpoint crossed as %q, want %q: a sandboxed node runs against another host than its launcher", got, operatorEndpoint)
	}

	// The same bytes, planted by this host's project `.env`.
	envtrust.MarkPlanted("OPENAI_BASE_URL")
	env, _, err = forwardableProviderEnv(context.Background(), "openai/gpt-4o-mini")
	if err != nil {
		t.Fatalf("forwardableProviderEnv with a planted endpoint: %v", err)
	}
	got, crossed := env["OPENAI_BASE_URL"]
	if !crossed {
		t.Error("the name did not cross at all: absent is the one state the workspace .env of the repository under review gets to fill")
	}
	if got != "" {
		t.Errorf("the planted endpoint crossed as %q: the run's own credential is spent at a host the repository chose", got)
	}
}

// The slice of the class that decides when a node compacts: a repository
// under review would otherwise choose the window its reviewer's context is
// summarised at, and how much of the recent turn survives it. Bounded —
// no credential, no price — and closed in the same words as the catalog's
// own knobs.
func TestAProjectDotenvDoesNotChooseWhenANodeCompacts(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv(EnvCompactThresholdRatio, "0.05")
	t.Setenv(EnvCompactPreserveRecent, "1")

	// Control: the operator's own deployment-wide default answers.
	ratio, preserve := resolveCompaction(nil, nil)
	if ratio != 0.05 || preserve != 1 {
		t.Fatalf("an exported pair resolved %v/%d: the deployment lost its compaction defaults", ratio, preserve)
	}

	envtrust.MarkPlanted(EnvCompactThresholdRatio, EnvCompactPreserveRecent)
	ratio, preserve = resolveCompaction(nil, nil)
	if ratio != 0 || preserve != 0 {
		t.Errorf("a project `.env` chose the compaction window: %v/%d, want the built-in fall-through (0/0)", ratio, preserve)
	}

	// A workflow still outranks the env layer, planted or not: the cascade
	// is node → workflow → env → built-in, and this narrowing touches one
	// rung of it.
	ratio, preserve = resolveCompaction(nil, &ir.Compaction{Threshold: 0.8, PreserveRecent: 4})
	if ratio != 0.8 || preserve != 4 {
		t.Errorf("the workflow's own compaction resolved %v/%d, want 0.8/4", ratio, preserve)
	}
}
