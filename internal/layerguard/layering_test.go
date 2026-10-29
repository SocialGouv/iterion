package layerguard

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRoot climbs from the test's working directory to the go.mod — the
// guard must run `go list` from the module root, wherever the test runs from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test — run from inside the module")
		}
		dir = parent
	}
}

// modulePath is the import prefix every package of this module carries. It
// is trimmed at ONE place, parseImportPairs, so no rule ever compares a
// full import path to a bare family prefix.
const modulePath = "github.com/SocialGouv/iterion/"

// layerRule forbids a package family from importing another. Both sides are
// MODULE-RELATIVE path prefixes (pkg/dsl/ir, internal), the form
// parseImportPairs hands out.
type layerRule struct {
	name    string
	sources []string // package path prefixes the rule binds
	banned  []string // package path prefixes they must not import
}

// The layer map, as an executable guard. Each rule forbids a package family
// from importing another — the map codifies what the tree already does, so
// the guard bites on the FIRST new violation, not after a refactor.
//
// Verdict faced on 2026-10-29 at 69b46b587, over the 212 packages and 964
// module-internal import edges go list named there: 12 pairs, every one of
// them accepted into allowExceptions below with its reason. Those counts
// are that reading's provenance — the pinned expectedAllowCount is the
// number this file keeps true. The previous reading of this file ("verified
// 2026-10-23 over 903 internal import pairs: zero violations") was not a
// verdict: the guard compared `go list`'s full import path to these bare
// prefixes, so no rule ever bound a package and nothing was checked.
//
// The ratchet: a violation that review accepts goes into allowExceptions
// with its reason, and expectedAllowCount moves with it — both are edits a
// reviewer sees. The list can only shrink without a review.
var layers = []layerRule{
	{"pkg/dsl/ir is a leaf — no executor, server, dispatcher, CLI, backend, forge or runner imports", []string{"pkg/dsl/ir"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend", "pkg/forge", "pkg/runner"}},
	{"pkg/dsl compiles without an executor behind it", []string{"pkg/dsl"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend"}},
	{"no pkg package imports the composition root — the server is reached through cmd/ and the seams", []string{"pkg"}, []string{"pkg/server"}},
	{"pkg/runtime never imports the server, the dispatcher or the CLI", []string{"pkg/runtime"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the backends never import the server, the dispatcher or the CLI", []string{"pkg/backend"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the dispatcher never imports the server or the CLI", []string{"pkg/dispatcher"}, []string{"pkg/server", "pkg/cli"}},
	{"internal never imports the composition root", []string{"internal"}, []string{"pkg/server"}},
	{"the store stays a leaf — no executor, server, dispatcher, backend or CLI imports", []string{"pkg/store"}, []string{"pkg/server", "pkg/runtime", "pkg/dispatcher", "pkg/backend", "pkg/cli"}},
}

// allowExceptions maps "source -> banned" violations that review has
// accepted, each with the reason that keeps it honest. expectedAllowCount
// pins the count so an addition is an edit of this file, visible in review,
// never a silent rot; TestEveryAcceptedExceptionIsALiveViolation refuses an
// entry the tree no longer carries, so the list can only shrink.
var allowExceptions = map[string]string{
	// The IR validates a .bot against what the chosen backend can do, so it
	// reads the capability vocabulary where it is defined instead of keeping
	// a second copy of it (AGENTS.md: never a second list). Each package
	// below is vocabulary — constants, Parse, SupportsBackend — never an
	// executor, and the five leaves carry 1 to 3 module packages each.
	"pkg/dsl/ir -> pkg/backend/backendcaps":        "capability-aware validation: validate_provider/command/fallbacks/async_backend/sandbox_image_presence refuse a .bot asking a backend for what the registry says it cannot do (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/toolcatalog":        "validate_tools/fallbacks/permission_rules resolve tool names against THE catalogue, never a second list (leaf: 2 module packages)",
	"pkg/dsl/ir -> pkg/backend/permission":         "validate_permission_rules parses a rule with the permission grammar that enforces it (leaf: 3 module packages)",
	"pkg/dsl/ir -> pkg/backend/modelroute":         "validate_gateway/nodes decide gateway shape with modelroute.Parse, the one reader of a model reference (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/ambient":            "validate_nodes names the backends that honour an ambient-context policy from ambient.SupportOf, the measurement's source (leaf: 1 module package)",
	"pkg/dsl/ir -> pkg/backend/automemory":         "validate_nodes asks automemory.SupportsBackend which backends serve durable memory — vocabulary, but the package drags pkg/store and pkg/knowledge (10 module packages): filed as iterion#2857",
	"pkg/dsl/ir -> pkg/dispatcher/native/boardops": "validate_capabilities names the board capabilities from boardops.CapBoard* — seven constants that drag the native dispatcher, pkg/store, pkg/forge and pkg/mail (43 module packages): filed as iterion#2857",

	// pkg/cli composes and starts an in-process server (server.New in
	// studio.go and workspace_studio.go) and reads its response types; its
	// only importers are cmd/iterion and cmd/iterion-desktop, so the
	// composition stays in the command layer. Accepted, not corrected away:
	// a third entry here would mean the CLI is reaching deeper, which is
	// the signal this list exists to carry.
	"pkg/cli -> pkg/server":          "iterion studio/dispatch compose the in-process server (server.New, server.Config, BrowserGuard, ImportForgeIssues) and the CLI aliases its response types",
	"pkg/cli -> pkg/server/projects": "iterion studio --workspace loads the project registry that NewWorkspaceHost is built from",

	// The claw backend exposes the board as in-process tools; every
	// operation goes through boardops so validation and event emission
	// match the stdio and HTTP transports byte-for-byte.
	"pkg/backend/tool -> pkg/dispatcher/native/boardops":     "RegisterClawBoardTools runs each board operation through boardops.Call, THE board implementation the other two transports use",
	"pkg/backend/tool -> pkg/dispatcher/native":              "BoardConfig.Store is the dispatcher's own *native.Store handle, shared so a tool call is visible to the dispatcher immediately",
	"pkg/backend/delegate -> pkg/dispatcher/native/boardops": "the MCP board bridge serves the same operations through the same boardops entry as the claw tools",
}

const expectedAllowCount = 12

// underPrefix reports whether a module-relative package path IS prefix or
// lives below it. THE comparison of this guard: both arguments come from
// parseImportPairs or from a rule, never from `go list` raw.
func underPrefix(pkg, prefix string) bool {
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

// inFamily reports whether pkg belongs to any of the families.
func inFamily(pkg string, prefixes []string) bool {
	for _, p := range prefixes {
		if underPrefix(pkg, p) {
			return true
		}
	}
	return false
}

// parseImportPairs turns the output of
// `go list -f '{{.ImportPath}}|{{join .Imports " "}}'` into MODULE-RELATIVE
// pairs: the key and every value lose the module prefix here, once, and
// imports outside the module are dropped — they belong to no layer of this
// map.
//
// The trim is this seam's job, not each comparison's. A rule's sources and
// banned families are bare paths (pkg/dsl/ir); a full import path reaching
// underPrefix binds nothing and the map goes unenforced in silence, which
// is what iterion#2829 measured. No full path survives this function.
func parseImportPairs(out string) map[string][]string {
	pairs := map[string][]string{}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		pkg, imps, ok := strings.Cut(line, "|")
		if !ok || !strings.HasPrefix(pkg, modulePath) {
			continue
		}
		rel := []string{}
		for _, imp := range strings.Fields(imps) {
			if !strings.HasPrefix(imp, modulePath) {
				continue
			}
			rel = append(rel, strings.TrimPrefix(imp, modulePath))
		}
		pairs[strings.TrimPrefix(pkg, modulePath)] = rel
	}
	return pairs
}

func importsPairs(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \" \"}}", "./pkg/...", "./internal/...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	pairs := parseImportPairs(string(out))
	if len(pairs) == 0 {
		t.Fatalf("go list named no package of this module — the guard would certify an empty tree:\n%s", out)
	}
	return pairs
}

// eachViolation walks the rules over module-relative pairs — sorted, so
// every reader gets the same order — and hands each rule hit to visit as
// (rule name, "<source> -> <import>"). THE evaluation: violationsOf and
// violatingPairs are its two readers, so neither reconstructs a verdict by
// re-parsing the other's message.
func eachViolation(rules []layerRule, pairs map[string][]string, visit func(rule, key string)) {
	for _, l := range rules {
		for _, pkg := range slices.Sorted(maps.Keys(pairs)) {
			if !inFamily(pkg, l.sources) {
				continue
			}
			for _, imp := range pairs[pkg] {
				for _, b := range l.banned {
					if !underPrefix(imp, b) {
						continue
					}
					// A rule forbids CROSSING into a family; a package of
					// that family importing its own family is cohesion, not
					// a layer crossing (pkg/server reads pkg/server/projects).
					if underPrefix(pkg, b) {
						continue
					}
					visit(l.name, pkg+" -> "+imp)
				}
			}
		}
	}
}

// violationsOf returns one line per rule hit, "<rule>: <source> -> <import>",
// minus the pairs accepted holds. accepted may be nil — the raw verdict.
func violationsOf(rules []layerRule, pairs map[string][]string, accepted map[string]string) []string {
	violations := []string{}
	eachViolation(rules, pairs, func(rule, key string) {
		if _, ok := accepted[key]; ok {
			return
		}
		violations = append(violations, rule+": "+key)
	})
	return violations
}

// violatingPairs is the set of "source -> import" keys the rules name,
// exceptions ignored — what allowExceptions is allowed to hold.
func violatingPairs(rules []layerRule, pairs map[string][]string) map[string]bool {
	keys := map[string]bool{}
	eachViolation(rules, pairs, func(_, key string) { keys[key] = true })
	return keys
}

func TestThePackageLayersHold(t *testing.T) {
	violations := violationsOf(layers, importsPairs(t), allowExceptions)
	if len(allowExceptions) != expectedAllowCount {
		t.Errorf("allowExceptions holds %d entries, want the pinned %d — an exception leaves only by shrinking this list", len(allowExceptions), expectedAllowCount)
	}
	if len(violations) != 0 {
		t.Errorf("the layer map is violated %d time(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestTheGuardBindsAPackageByItsModuleRelativePath drives the real seam
// with the shape `go list` really emits — full import paths on BOTH sides —
// and demands the rule name the pair. Trim the module prefix on one side
// only, the way this file read before iterion#2829, and the guard binds
// nothing: it reports zero violations over a tree that holds one, and this
// test is the only thing that says so.
func TestTheGuardBindsAPackageByItsModuleRelativePath(t *testing.T) {
	raw := "github.com/SocialGouv/iterion/pkg/dsl/ir|fmt github.com/SocialGouv/iterion/pkg/server github.com/spf13/cobra\n" +
		"github.com/SocialGouv/iterion/pkg/log|fmt\n"
	pairs := parseImportPairs(raw)
	if got := pairs["pkg/dsl/ir"]; !slices.Equal(got, []string{"pkg/server"}) {
		t.Fatalf("parseImportPairs(pkg/dsl/ir) = %q, want the module-relative [pkg/server] with stdlib and third-party dropped", got)
	}
	got := violationsOf(layers, pairs, nil)
	want := []string{
		layers[0].name + ": pkg/dsl/ir -> pkg/server",
		layers[1].name + ": pkg/dsl/ir -> pkg/server",
		layers[2].name + ": pkg/dsl/ir -> pkg/server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the guard named %d violation(s) for pkg/dsl/ir -> pkg/server:\n%s\nwant the three rules that bind it:\n%s",
			len(got), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestARuleDoesNotForbidAFamilyFromImportingItself fixes the other half of
// the verdict: "no pkg package imports the composition root" is about
// crossing INTO pkg/server, so pkg/server reading pkg/server/projects is
// cohesion. Drop the intra-family skip and the composition root is reported
// as violating the rule that describes it.
func TestARuleDoesNotForbidAFamilyFromImportingItself(t *testing.T) {
	pairs := map[string][]string{
		"pkg/server":  {"pkg/server/projects", "pkg/server/cloudpublisher"},
		"pkg/runtime": {"pkg/server"},
	}
	got := violationsOf(layers, pairs, nil)
	want := []string{
		layers[2].name + ": pkg/runtime -> pkg/server",
		layers[3].name + ": pkg/runtime -> pkg/server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("verdict on the intra-family fixture:\n%s\nwant only the crossing from outside pkg/server:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestEveryLayerRuleBindsAPackageOfTheTree: a rule that binds no package
// certifies nothing. This is the property the asymmetric comparison broke
// for every rule at once, silently, and the one a future refactor can break
// for a single rule by renaming a package family out from under it.
func TestEveryLayerRuleBindsAPackageOfTheTree(t *testing.T) {
	pairs := importsPairs(t)
	for _, l := range layers {
		bound := 0
		for pkg := range pairs {
			if inFamily(pkg, l.sources) {
				bound++
			}
		}
		if bound == 0 {
			t.Errorf("no package of ./pkg/... or ./internal/... is bound by %q (sources %v) — the rule checks nothing", l.name, l.sources)
		}
	}
}

// TestEveryAcceptedExceptionIsALiveViolation keeps the ratchet from rotting
// the other way: an entry whose import is gone, or whose rule no longer
// names it, inflates expectedAllowCount and buys silence for a pair nobody
// reviewed.
func TestEveryAcceptedExceptionIsALiveViolation(t *testing.T) {
	live := violatingPairs(layers, importsPairs(t))
	for _, key := range slices.Sorted(maps.Keys(allowExceptions)) {
		if !live[key] {
			t.Errorf("allowExceptions accepts %q, which no rule names on today's tree — drop the entry and lower expectedAllowCount", key)
		}
	}
	if len(allowExceptions) != len(live) {
		t.Logf("%d accepted exception(s) for %d violating pair(s) — the difference is what TestThePackageLayersHold reports", len(allowExceptions), len(live))
	}
}
