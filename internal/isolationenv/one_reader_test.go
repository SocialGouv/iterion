package isolationenv

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// repoRoot climbs from the test's working directory to the go.mod, so the
// guard scans the whole module wherever it runs from.
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

// envReadCall matches a read of the process environment by any of the shapes
// the tree uses, with the variable named as a STRING LITERAL.
var envReadCall = regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv|Getenv|LookupEnv|envtrust\.Inherited|envtrust\.Planted|lookup|lookupString|lookupInt|lookupBool|lookupDuration|lookupUint64)\(\s*"([A-Z0-9_]+)"`)

// envReadByIdentifier matches the same read with the name passed as a
// CONSTANT. The aliases below are the identifiers the tree declares equal to a
// member of the inventory; a read through one of them is as much a second
// reader as a read through the literal.
var envReadByIdentifier = regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv|envtrust\.Inherited|envtrust\.Planted)\(\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\)`)

var inventoryAliases = map[string]string{
	"SandboxFloorEnvVar":               SandboxFloor,
	"HostStateGuardEnvVar":             HostStateGuard,
	"EnvSandboxDefaultImage":           SandboxDefaultImage,
	"isolationenv.SandboxDefault":      SandboxDefault,
	"isolationenv.SandboxOverride":     SandboxOverride,
	"isolationenv.SandboxDefaultImage": SandboxDefaultImage,
	"isolationenv.SandboxFloor":        SandboxFloor,
	"isolationenv.HostStateGuard":      HostStateGuard,
	"isolationenv.ModelHosts":          ModelHosts,
}

// The ONE reader, as an executable guard: a name that can lower a run's
// isolation is read through [Value] and nowhere else.
//
// Why a source scan rather than a unit test: the defect this closes was not a
// wrong answer, it was a SECOND reader. pkg/runtime/sandbox_floor.go claimed to
// be "the ONE reader" of the isolation hatches while
// ResolveGlobalSandboxDefault read ITERION_SANDBOX_DEFAULT with os.Getenv — the
// tier the floor falls through to — so a one-line `.env` in the repository
// under review undid the floor the sentence promised. No unit test of either
// function can see that; only the question "who else reads this name" can, and
// that question is a grep over what the program really does.
//
// Test files are out of scope on purpose: a test SETS these variables
// (t.Setenv) and may read them back to assert its own fixture.
func TestOnlyIsolationenvReadsTheIsolationLoweringVariables(t *testing.T) {
	root := repoRoot(t)
	out, err := gittest.Try(root, "ls-files", "*.go")
	if err != nil {
		t.Fatalf("git ls-files did not answer, so the tree's Go files were not held to the one reader: %v", err)
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(files) < 100 {
		t.Fatalf("git ls-files returned %d Go files — the scan would prove nothing", len(files))
	}
	scanned, offenders := 0, []string{}
	for _, rel := range files {
		switch {
		case rel == "", strings.HasPrefix(rel, "vendor/"),
			strings.HasSuffix(rel, "_test.go"),
			// The one reader itself, and its testdata.
			strings.HasPrefix(rel, "internal/isolationenv/"),
			strings.Contains(rel, "/testdata/"):
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		for _, line := range strings.Split(string(body), "\n") {
			code := line
			if i := strings.Index(strings.TrimSpace(code), "//"); i == 0 {
				continue // a comment may name the variable freely
			}
			for _, m := range envReadCall.FindAllStringSubmatch(code, -1) {
				if Covers(m[1]) {
					offenders = append(offenders, rel+": "+strings.TrimSpace(code))
				}
			}
			for _, m := range envReadByIdentifier.FindAllStringSubmatch(code, -1) {
				if name, ok := inventoryAliases[m[1]]; ok && Covers(name) {
					offenders = append(offenders, rel+": "+strings.TrimSpace(code))
				}
			}
		}
	}
	if scanned < 100 {
		t.Fatalf("only %d non-test Go files were scanned — the guard is not reaching the tree", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("a name that can LOWER a run's isolation is read outside internal/isolationenv (%d site(s)) — route it through isolationenv.Value, which reads the INHERITED environment, so a project `.env` cannot answer for the operator:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
	t.Logf("%d non-test Go files scanned for the %d inventoried names", scanned, len(Names))
}

// The inventory is not a wish list: every name in it must be WIRED to the
// production tree through this package's const. A member nothing references is
// a name that was renamed or deleted, and a guard that still watches it proves
// nothing while reading as though it did.
func TestEveryInventoriedNameIsWiredToTheTree(t *testing.T) {
	root := repoRoot(t)
	out, err := gittest.Try(root, "ls-files", "*.go")
	if err != nil {
		t.Fatalf("git ls-files did not answer: %v", err)
	}
	seen := map[string]string{}
	for _, rel := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rel == "" || strings.HasPrefix(rel, "vendor/") || strings.HasSuffix(rel, "_test.go") ||
			strings.HasPrefix(rel, "internal/isolationenv/") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := string(body)
		for _, name := range Names {
			konst := constNameOf(name)
			// Word-bounded: "isolationenv.SandboxDefault" is a PREFIX of
			// "isolationenv.SandboxDefaultImage", and a substring match let
			// the image's wiring vouch for the mode tier's.
			if regexp.MustCompile(`isolationenv\.` + konst + `\b`).MatchString(text) {
				seen[name] = rel
				continue
			}
			// A package may re-export the const under its own name (the
			// runtime's SandboxFloorEnvVar / HostStateGuardEnvVar /
			// EnvSandboxDefaultImage); the alias table is what makes that
			// re-export countable rather than a hole.
			for alias, target := range inventoryAliases {
				if target == name && !strings.Contains(alias, ".") && regexp.MustCompile(`=\s*isolationenv\.`+konst+`\b`).MatchString(text) {
					seen[name] = rel
					_ = alias
				}
			}
		}
	}
	for _, name := range Names {
		if seen[name] == "" {
			t.Errorf("%s is in the inventory and no production file references isolationenv.%s — the name was renamed or dropped, and the guard is watching a ghost", name, constNameOf(name))
			continue
		}
		t.Logf("%-38s wired in %s", name, seen[name])
	}
}

// constNameOf maps a variable name back to the exported const that spells it,
// so the guards above can recognise the const form of a reference.
func constNameOf(name string) string {
	switch name {
	case SandboxDefault:
		return "SandboxDefault"
	case SandboxOverride:
		return "SandboxOverride"
	case SandboxDefaultImage:
		return "SandboxDefaultImage"
	case SandboxFloor:
		return "SandboxFloor"
	case HostStateGuard:
		return "HostStateGuard"
	case ModelHosts:
		return "ModelHosts"
	}
	return name
}
