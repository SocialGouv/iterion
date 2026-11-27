package sweepguard

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// processWideCalls are the calls whose effect or reading spans the whole test
// process: the union of what the source packages' guards refuse
// (bots/parallel_isolation_test.go, pkg/server/parallel_guard_test.go,
// pkg/backend/model/parallel_campaign_test.go), because their tests moved here
// and keep calling the same helpers. The environment and the working
// directory; the goroutine census a leak test takes, which another test's
// workers move; errtrack, which installs its client once per process; and the
// setters of another package's process-wide state, by bare name where the
// source guard matched bare and qualified where it matched qualified. Package
// variables a sweep test swaps come from the reader itself, through
// GlobalWriters.
var processWideCalls = map[string]bool{
	"Setenv":                             true,
	"Unsetenv":                           true,
	"Chdir":                              true,
	"NumGoroutine":                       true,
	"SetDefault":                         true,
	"SetCredentialsLookup":               true,
	"SetOAuthDirLookup":                  true,
	"SetGatewayPricer":                   true,
	"goroutinetest.ByFrame":              true,
	"goroutinetest.Carrying":             true,
	"goroutinetest.Dump":                 true,
	"errtrack.Init":                      true,
	"runview.SetMaxEventsPerPageForTest": true,
	"compatgw.ResetCatalogCaches":        true,
	"compatgw.ResetFeedTableCache":       true,
	"envtrust.ResetForTest":              true,
	"store.ResolveIterionHomeAsInProductionForTests": true,
}

// minSweepDirs is the number of internal/sweep directories with test files
// when this guard landed, itself included. The walk discovers them; the floor
// only says a walk that reads fewer judged less than the tree holds.
const minSweepDirs = 15

// sweepDirs lists every directory beside this one under internal/sweep, this
// one included: the walk is discovered, so a sweep package added tomorrow is
// judged without anyone listing it here.
func sweepDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("read internal/sweep: %v", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// TestNoParallelSweepTestSharesProcessState refuses a test of any
// internal/sweep package that goes parallel — t.Parallel() in its body, in a
// subtest, or in a helper it calls — while it, or a helper of its package's
// test files, swaps a package variable or reaches one of processWideCalls. A
// parallel test runs beside every other parallel test of its binary, so an
// os.Setenv there rewrites its neighbours' environment without a word, and a
// swapped hook leaks into whoever drives the same path (t.Setenv panics
// instead).
//
// The reading is static and by name (internal/testparallel), per package: it
// does not see a production function that writes a global, a write through a
// method call, or a function value passed in. Those stay the author's reading
// when a sweep test gains t.Parallel().
func TestNoParallelSweepTestSharesProcessState(t *testing.T) {
	read, parallel := 0, 0
	parallelIn := map[string]int{}
	var shared []string
	for _, dir := range sweepDirs(t) {
		c, err := testparallel.ReadCampaign(filepath.Join("..", dir), testparallel.LiveOnly)
		if err != nil {
			t.Fatalf("read internal/sweep/%s: %v", dir, err)
		}
		if len(c.Files()) == 0 {
			continue
		}
		read++
		seeds := c.GlobalWriters()
		for call := range processWideCalls {
			seeds[call] = true
		}
		reach := c.Reach(seeds)
		goesParallel := c.Reach(map[string]bool{"Parallel": true})
		for _, f := range c.Files() {
			for _, fn := range c.Tests(f) {
				name := fn.Name.Name
				if goesParallel[name] == "" {
					continue
				}
				parallel++
				parallelIn[dir]++
				why := reach[name]
				if writes := c.GlobalWrites(name); len(writes) > 0 {
					why = "assigns " + strings.Join(writes, ", ")
				}
				if why != "" {
					shared = append(shared, dir+"/"+f+":"+name+" ("+why+")")
				}
			}
		}
	}
	t.Logf("read %d internal/sweep directories, %d parallel tests: %v", read, parallel, parallelIn)
	if read < minSweepDirs {
		t.Fatalf("the walk read %d internal/sweep directories with test files, want at least %d — it is judging less than the tree holds, so no verdict below means anything", read, minSweepDirs)
	}
	if parallel == 0 {
		t.Fatal("the walk found no parallel test under internal/sweep — it is judging nothing, so its silence means nothing")
	}
	// Witness: the package whose parallel tests the per-package guards lost
	// first (#3758 R1) is still read and still seen parallel.
	if parallelIn["catalogcorpus"] == 0 {
		t.Fatalf("witness: no parallel test seen in internal/sweep/catalogcorpus (per dir: %v) — the walk no longer reads it, or no longer sees t.Parallel()", parallelIn)
	}
	sort.Strings(shared)
	if len(shared) > 0 {
		t.Errorf("%d parallel sweep test(s) share process-wide state with the tests running beside them — drop their t.Parallel(), or give them a fixture of their own (t.TempDir, cmd.Env on the child):\n  %s",
			len(shared), strings.Join(shared, "\n  "))
	}
}
