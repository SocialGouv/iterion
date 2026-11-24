package testparallel_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// The guards of pkg/backend/model and of the bots modernize family both rest
// on this reader: a Reach that stops one hop short, or a Closure that returns
// its roots alone, would leave BOTH of them green while they certify nothing.
// So the reader is exercised here against a package whose answers are written
// out by hand, rather than only through the two campaigns it serves.
const (
	alphaSrc = `package p

func helper(t *testing.T) { t.Setenv("K", "V") }

func TestDirect(t *testing.T) {
	t.Helper()
	t.Parallel()
	exec.Command("sh", "-c", "true")
}

func TestBorrows(t *testing.T) { helper(t) }

func TestClean(t *testing.T) { t.Parallel() }
`
	betaSrc = `package p

func deep(t *testing.T) { helper(t) }

func TestTwoHops(t *testing.T) { deep(t) }

// TestBoth borrows from two tainted helpers: deep (two hops) and helper
// (direct). Which one the reader names must not depend on a map's order.
func TestBoth(t *testing.T) {
	deep(t)
	helper(t)
}

func TestSpawns(t *testing.T) {
	env := os.Environ()
	exec.Command("x").Env = env
}

func notATest(t *testing.T, extra int) {}

func TestMain(m *testing.M) {}
`
	liveSrc = `//go:build live

package p

func TestLive(t *testing.T) { t.Setenv("L", "1") }
`
)

func fixture(t *testing.T) *testparallel.Campaign {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"alpha_test.go": alphaSrc,
		"beta_test.go":  betaSrc,
		"live_test.go":  liveSrc,
		"notes.md":      "not Go, and not read",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := testparallel.ReadCampaign(dir, testparallel.LiveOnly)
	if err != nil {
		t.Fatalf("ReadCampaign: %v", err)
	}
	return c
}

func TestReadCampaignReadsTheDeterministicTestFilesAlone(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	want := []string{"alpha_test.go", "beta_test.go"}
	got := c.Files()
	if len(got) != len(want) {
		t.Fatalf("Files() = %v, want %v — a live-tagged file is not part of the deterministic layer, and a non-Go file is not a test file", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Files() = %v, want %v (sorted)", got, want)
		}
	}
	if f := c.FileOf("deep"); f != "beta_test.go" {
		t.Errorf("FileOf(deep) = %q, want beta_test.go", f)
	}
	if f := c.FileOf("TestLive"); f != "" {
		t.Errorf("FileOf(TestLive) = %q — the live layer was read after all", f)
	}
}

func TestReachNamesTheSeedThenEveryHopToIt(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	got := c.Reach(map[string]bool{"Setenv": true})
	for fn, want := range map[string]string{
		"helper":      "Setenv",
		"TestBorrows": "via helper() in alpha_test.go",
		"deep":        "via helper() in alpha_test.go",
		// Two hops: the reason names the function it borrowed FROM, not the
		// seed — a reader follows one link at a time.
		"TestTwoHops": "via deep() in beta_test.go",
	} {
		if got[fn] != want {
			t.Errorf("Reach(Setenv)[%s] = %q, want %q", fn, got[fn], want)
		}
	}
	for _, clean := range []string{"TestDirect", "TestClean", "TestSpawns"} {
		if why := got[clean]; why != "" {
			t.Errorf("Reach(Setenv)[%s] = %q, want no reason", clean, why)
		}
	}
}

func TestReachTakesASeedThatIsAFunctionOfThePackage(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	// The shape the bots campaign uses: the seed is a fixture helper, not a
	// library call.
	got := c.Reach(map[string]bool{"helper": true})
	if got["TestBorrows"] != "helper" {
		t.Errorf("Reach(helper)[TestBorrows] = %q, want %q", got["TestBorrows"], "helper")
	}
	if got["TestTwoHops"] != "via deep() in beta_test.go" {
		t.Errorf("Reach(helper)[TestTwoHops] = %q, want the hop through deep()", got["TestTwoHops"])
	}
	if got["helper"] != "" {
		t.Errorf("Reach(helper)[helper] = %q — a function is not its own reason", got["helper"])
	}
}

func TestReachMatchesAQualifiedSeedAndABareOne(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	qualified := c.Reach(map[string]bool{"exec.Command": true})
	for _, fn := range []string{"TestDirect", "TestSpawns"} {
		if qualified[fn] != "exec.Command" {
			t.Errorf("Reach(exec.Command)[%s] = %q, want exec.Command", fn, qualified[fn])
		}
	}
	// A bare seed covers every receiver, which is what "Setenv" does for
	// t.Setenv and os.Setenv alike.
	if bare := c.Reach(map[string]bool{"Command": true}); bare["TestDirect"] != "Command" {
		t.Errorf("Reach(Command)[TestDirect] = %q, want Command", bare["TestDirect"])
	}
	if !c.CallsDirectly("TestSpawns", "os.Environ") || !c.CallsDirectly("TestSpawns", "Environ") {
		t.Error("CallsDirectly(TestSpawns, os.Environ/Environ) = false, want true in both forms")
	}
	if c.CallsDirectly("TestDirect", "os.Environ") {
		t.Error("CallsDirectly(TestDirect, os.Environ) = true, want false")
	}
	if c.CallsDirectly("TestBorrows", "Setenv") {
		t.Error("CallsDirectly(TestBorrows, Setenv) = true — it borrows the call, it does not make it")
	}
}

func TestFilesReachingGivesTheWholeSetAndPrefersADirectReason(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	got := c.FilesReaching(map[string]bool{"Setenv": true})
	// alpha declares helper (direct) AND TestBorrows (borrowed): the direct
	// reason wins, so the line says what the file itself does.
	if got["alpha_test.go"] != "Setenv" {
		t.Errorf("FilesReaching[alpha_test.go] = %q, want %q", got["alpha_test.go"], "Setenv")
	}
	// beta touches nothing itself: both borrowed reasons, sorted, as a set.
	want := "via deep() in beta_test.go, via helper() in alpha_test.go"
	if got["beta_test.go"] != want {
		t.Errorf("FilesReaching[beta_test.go] = %q, want %q", got["beta_test.go"], want)
	}
}

func TestClosureWalksForwardFromItsRoots(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	got := c.Closure(map[string]bool{"TestTwoHops": true})
	for _, want := range []string{"TestTwoHops", "deep", "helper"} {
		if !got[want] {
			t.Errorf("Closure(TestTwoHops) misses %s — a rule about what runs under a test would not see it", want)
		}
	}
	for _, unwanted := range []string{"TestBorrows", "TestDirect", "TestSpawns"} {
		if got[unwanted] {
			t.Errorf("Closure(TestTwoHops) contains %s, which it does not call", unwanted)
		}
	}
}

func TestOpensWithParallelAndIsTopLevelTestReadTheSignatureAndTheFirstStatements(t *testing.T) {
	t.Parallel()
	c := fixture(t)
	parallel := map[string]bool{}
	for _, f := range c.Files() {
		for _, fn := range c.Tests(f) {
			parallel[fn.Name.Name] = testparallel.OpensWithParallel(fn)
			if fn.Name.Name == "TestMain" || fn.Name.Name == "notATest" {
				t.Errorf("Tests(%s) returned %s — it is not a top-level test", f, fn.Name.Name)
			}
		}
	}
	for name, want := range map[string]bool{
		"TestDirect":  true, // t.Helper() first is still opening with t.Parallel()
		"TestClean":   true,
		"TestBorrows": false,
		"TestSpawns":  false,
	} {
		if got, seen := parallel[name]; !seen || got != want {
			t.Errorf("OpensWithParallel(%s) = %v (seen=%v), want %v", name, got, seen, want)
		}
	}
	if len(parallel) != 6 {
		t.Errorf("the walk collected %d top-level tests, want 6 (TestMain and the two-parameter helper are not tests)", len(parallel))
	}
}

func TestLiveOnlyReadsTheBuildTagAndNothingBelowTheFirstCode(t *testing.T) {
	t.Parallel()
	for src, want := range map[string]bool{
		liveSrc:  true,
		alphaSrc: false,
		"// a comment\n//go:build live\n\npackage p\n": true,
		"package p\n\n//go:build live\n":               false,
	} {
		if got := testparallel.LiveOnly(src); got != want {
			t.Errorf("LiveOnly(%q…) = %v, want %v", src[:min(24, len(src))], got, want)
		}
	}
}

func TestReadCampaignSaysWhichDirectoryItCouldNotRead(t *testing.T) {
	t.Parallel()
	if _, err := testparallel.ReadCampaign(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("ReadCampaign on a directory that does not exist returned no error — a silent empty campaign makes every guard above vacuous")
	}
}

// TestReachNamesTheSameBorrowedHelperOnEveryRun is the determinism witness:
// the callers of this reader assert the reason strings against a committed
// table, so a function that borrows from two tainted helpers must always be
// told it borrows from the same one. Go randomises map iteration order, so the
// repetition is what makes the witness bite — a single pass would agree with a
// map-ordered fixed point most of the time.
func TestReachNamesTheSameBorrowedHelperOnEveryRun(t *testing.T) {
	t.Parallel()
	// Source order inside TestBoth puts deep() first, and the sorted fixed
	// point reaches TestBoth before deep has a reason of its own, so the
	// stable answer is the direct one: helper().
	const want = "via helper() in alpha_test.go"
	for i := 0; i < 30; i++ {
		if got := fixture(t).Reach(map[string]bool{"Setenv": true})["TestBoth"]; got != want {
			t.Fatalf("run %d: Reach(Setenv)[TestBoth] = %q, want %q on every run — the verdict is following a map's iteration order, and the tables that assert it would fail at random", i, got, want)
		}
	}
}
