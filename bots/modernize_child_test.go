package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// modernizeChildEnv is the environment every subprocess of the modernize
// family runs with: the one the test inherited, plus the GIT_CONFIG_* entries
// that refuse git's auto-maintenance in whatever repository the child touches.
//
// Why the entries travel WITH the command instead of being installed on the
// process by t.Setenv: t.Setenv and t.Parallel exclude each other — t.Setenv
// panics once the calling test's goroutine tree has gone parallel — and this
// family is the subprocess-heaviest of the package, so it is where the `race`
// leg's wall is paid (#2564 P2b). An environment carried per command is also
// the only form that reaches a GRANDchild: these tests drive a python harness
// that runs git itself, and a `-c` flag on the test's own command says nothing
// to that git.
//
// What the entries are for is not a preference: since git 2.48 every writing
// command ends by detaching `git maintenance run --auto`, a child that calls
// setsid() and closes its descriptors — so os/exec returns while it still
// holds .git/objects/maintenance.lock and writes under .git/objects, racing
// t.TempDir()'s removal. That has ejected pull requests from the merge queue
// (#821, #828).
//
// os/exec keeps the LAST value of a duplicated key, so appending does exactly
// what t.Setenv did: a GIT_CONFIG_COUNT already in the environment is raised
// rather than shadowed, and modernizeAutoMaintenanceEnv reads that count from
// the very environment the child is about to receive, so the slots it appends
// land past the caller's.
//
// The redirection variables are dropped (gitlib.SanitizeEnv): an inherited
// GIT_DIR beats the `git -C <workspace>` the harnesses run, so a contributor
// with one exported would have the child answer about another repository. The
// identity and locale pins of gittest.Env() are deliberately NOT here:
// modernizeRepo writes a repository-local identity, and LC_ALL=C would change
// how a python child decodes its own subprocess output — one test of this
// family reads output that is not UTF-8 on purpose.
func modernizeChildEnv(t *testing.T) []string {
	t.Helper()
	pairs, err := modernizeAutoMaintenanceEnv()
	if err != nil {
		// An ambient GIT_CONFIG_COUNT that is not a number is not this
		// fixture's to repair: git itself then refuses EVERY GIT_CONFIG_*
		// entry ("bogus count in GIT_CONFIG_COUNT"), so there is no slot to
		// append to and no refusal that could travel. The child receives the
		// environment as it stands and the code under test refuses on its own
		// terms — which is what one test of this package installs on purpose
		// (TestAssessmentExtractorsRefuseAMalformedGitConfigCount, through a
		// helper the modernize family also drives). A t.Fatal here would have
		// the fixture pre-empt the very verdict that test asserts; the reason
		// is logged rather than swallowed.
		t.Logf("the child runs with the ambient environment unchanged: %v", err)
		return gitlib.SanitizeEnv(os.Environ())
	}
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	// A map ranges in random order; a child's environment is a fixture.
	sort.Strings(keys)
	env := gitlib.SanitizeEnv(os.Environ())
	for _, key := range keys {
		env = append(env, key+"="+pairs[key])
	}
	return env
}

// modernizeChild builds a subprocess for the modernize family with that
// environment already on it. It is the one place the family starts a child, so
// a new call site cannot forget the refusal:
// TestEveryModernizeFamilySpawnCarriesTheFixtureEnvironment holds every
// function the family drives to this helper.
func modernizeChild(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...) // #nosec G204 -- test-only helper, its args come from the test
	cmd.Env = modernizeChildEnv(t)
	return cmd
}

// TestModernizeChildCarriesTheRefusalAndYieldsToACallersOverride reads the
// carrier from the child's side, through the real os/exec merge rather than
// through a belief about it: the auto-maintenance entries arrive in the two
// slots past whatever the ambient environment already used, and an entry the
// caller appends afterwards wins — which is what lets the one override path of
// this family (modernizeLotVerifyRaw's env parameter) add to the carrier
// instead of replacing it.
//
// The slots are COMPUTED here, never hardcoded: a sandbox or a CI runner may
// export GIT_CONFIG_COUNT of its own (this repository's agent sandbox exports
// two safe.directory entries), and a fixture that assumed slot 0 would pass on
// one host and fail on the next.
func TestModernizeChildCarriesTheRefusalAndYieldsToACallersOverride(t *testing.T) {
	t.Parallel()
	requireModernizeTools(t)
	base := 0
	if raw, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok && raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("the ambient GIT_CONFIG_COUNT=%q is not a number — this test cannot say which slots the carrier should use", raw)
		}
		base = n
	}
	const read = "import json,os,sys\nsys.stdout.write(json.dumps(dict(os.environ)))"
	cmd := modernizeChild(t, "python3", "-c", read)
	// exec.Command resolved python3 from the current PATH already, so an
	// appended entry cannot break the spawn — it only changes what the child
	// reads. Overriding the carrier's OWN key is the interesting half.
	maintenanceValue := "GIT_CONFIG_VALUE_" + strconv.Itoa(base)
	cmd.Env = append(cmd.Env, "ITERION_OVERRIDDEN=by the caller", maintenanceValue+"=true")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("read the child's environment: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("child output is not JSON: %v (out %q)", err, out)
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT":                         strconv.Itoa(base + 2),
		"GIT_CONFIG_KEY_" + strconv.Itoa(base):     "maintenance.auto",
		"GIT_CONFIG_KEY_" + strconv.Itoa(base+1):   "gc.auto",
		"GIT_CONFIG_VALUE_" + strconv.Itoa(base+1): "0",
	} {
		if got[key] != want {
			t.Errorf("the child reads %s=%q, want %q — the carrier must reach the child, in the slots past the ambient %d", key, got[key], want, base)
		}
	}
	for key, want := range map[string]string{
		"ITERION_OVERRIDDEN": "by the caller",
		maintenanceValue:     "true",
	} {
		if got[key] != want {
			t.Errorf("the child reads %s=%q, want %q — an entry the caller appends must win, including over one the carrier set (os/exec keeps the last value of a duplicated key)", key, got[key], want)
		}
	}
}

// TestModernizeChildEnvLeavesAnUnusableAmbientCountAlone pins the branch
// above: with a GIT_CONFIG_COUNT git would reject, the carrier adds nothing and
// changes nothing, so the refusal the code under test owes is still its own to
// produce. The claim is compared against the ambient environment rather than
// against a list written here — a sandbox or a runner may export GIT_CONFIG_*
// entries of its own, and a fixture that assumed otherwise would pass on one
// host and fail on the next. Serial by necessity: it drives process state.
func TestModernizeChildEnvLeavesAnUnusableAmbientCountAlone(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "one")
	t.Setenv("GIT_CONFIG_KEY_0", "iterion.canary")
	want := gitlib.SanitizeEnv(os.Environ())
	got := modernizeChildEnv(t)
	sort.Strings(want)
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	if !slices.Equal(sorted, want) {
		added, missing := difference(sorted, want), difference(want, sorted)
		t.Errorf("the carrier changed the environment of a child whose ambient GIT_CONFIG_COUNT git will reject\n  added:   %q\n  missing: %q\nappending a slot to an unusable count is a fixture deciding what the code under test should have refused", added, missing)
	}
}

// difference returns the entries of a that b does not carry.
func difference(a, b []string) []string {
	var out []string
	for _, kv := range a {
		if !slices.Contains(b, kv) {
			out = append(out, kv)
		}
	}
	return out
}
