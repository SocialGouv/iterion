package server

import (
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// This package runs mostly serially under -race: a top-level test that calls
// t.Parallel() waits until every serial test has returned, then runs beside
// the other parallel ones only. So a parallel test may share nothing mutable
// with another parallel test, and the guard below reads that rule off the
// sources instead of trusting whoever adds the next t.Parallel() (#3798).
//
// processWideCalls are the calls of these test files whose effect or reading
// spans the whole process: the environment and the working directory, the
// goroutine census a leak test takes (another test's workers would move it),
// and errtrack, which installs its client once per process. Setters of
// process-wide state in other packages (a default spec registry, a page cap,
// a catalog cache, the ITERION_HOME resolution) are listed here by their
// qualified name, as pkg/backend/model's globalMutators does: the reader stops
// at the package boundary, so a new one belongs here the day a test of this
// package calls it. Package-level variables a test swaps (`serverBuild = …`,
// an authoring hook) come from the reader itself, through GlobalWriters.
var processWideCalls = map[string]bool{
	"Setenv":                             true,
	"Unsetenv":                           true,
	"Chdir":                              true,
	"NumGoroutine":                       true,
	"goroutinetest.ByFrame":              true,
	"goroutinetest.Carrying":             true,
	"goroutinetest.Dump":                 true,
	"errtrack.Init":                      true,
	"modelspecs.SetDefault":              true,
	"model.SetCredentialsLookup":         true,
	"runview.SetMaxEventsPerPageForTest": true,
	"compatgw.ResetCatalogCaches":        true,
	"compatgw.ResetFeedTableCache":       true,
	"envtrust.ResetForTest":              true,
	"store.ResolveIterionHomeAsInProductionForTests": true,
}

// TestAParallelTestSharesNoProcessGlobal refuses a test that goes parallel —
// t.Parallel() at its top or in one of its subtests — while it, or a helper of
// these test files it calls, swaps a package variable or reaches one of
// processWideCalls. A swapped hook or variable is read by every parallel test
// that drives the same production path, so the swap leaks into a neighbour, or
// races its restore; t.Setenv would panic the binary at run time instead.
//
// The reading is static and by name (internal/testparallel): it does not see a
// production function that writes a global, a write through a method call, or
// a shared directory such as the process-wide test ITERION_HOME. Those stay the
// author's reading when a test gains t.Parallel().
func TestAParallelTestSharesNoProcessGlobal(t *testing.T) {
	t.Parallel()
	c, err := testparallel.ReadCampaign(".", testparallel.LiveOnly)
	if err != nil {
		t.Fatalf("read this package's test files: %v", err)
	}
	if got := len(c.Files()); got < 100 {
		t.Fatalf("the walk read %d test files — it is judging nothing, so no verdict below means anything", got)
	}
	seeds := c.GlobalWriters()
	for call := range processWideCalls {
		seeds[call] = true
	}
	reach := c.Reach(seeds)
	// Witnesses on real tests of this package, one per path a verdict takes:
	// a helper that swaps a package variable, a setter of another package's
	// process-wide state, and a test body that assigns a hook itself.
	for name, want := range map[string]string{
		"TestAuthoringCommitRefusesASyntaxFloorGap":      "pinServerBuild",
		"TestModelCapabilities_QualifiedSpec":            "modelspecs.SetDefault",
		"TestRunWSMatrixAlertWhileTheReplayIsInProgress": "runview.SetMaxEventsPerPageForTest",
		"TestRunsWS_ReplayPaginatesPastMaxEventsPerPage": "runview.SetMaxEventsPerPageForTest",
	} {
		if got := reach[name]; got != want {
			t.Fatalf("witness: reach[%s] = %q, want %q — the guard no longer follows that call to a process-wide swap", name, got, want)
		}
	}
	if got := strings.Join(c.GlobalWrites("TestTheNewBotScaffoldNeverReplacesTheMainASaveAsCreated"), ","); got != "authoringStepHook" {
		t.Fatalf("witness: GlobalWrites = %q, want authoringStepHook — the guard no longer sees a test that assigns a hook", got)
	}

	parallel := 0
	var shared []string
	for _, f := range c.Files() {
		for _, fn := range c.Tests(f) {
			name := fn.Name.Name
			if !c.CallsDirectly(name, "Parallel") {
				continue
			}
			parallel++
			why := reach[name]
			if writes := c.GlobalWrites(name); len(writes) > 0 {
				why = "assigns " + strings.Join(writes, ", ")
			}
			if why != "" {
				shared = append(shared, f+":"+name+" ("+why+")")
			}
		}
	}
	if parallel == 0 {
		t.Fatal("the walk found no parallel test — it is judging nothing, so its silence means nothing")
	}
	sort.Strings(shared)
	if len(shared) > 0 {
		t.Errorf("%d parallel test(s) share process-wide state with the tests running beside them — drop their t.Parallel(), or give them a fixture of their own:\n  %s",
			len(shared), strings.Join(shared, "\n  "))
	}
}
