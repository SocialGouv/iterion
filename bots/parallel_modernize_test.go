package bots

import (
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/testparallel"
)

// The t.Parallel campaign of the modernize family — what it freed, what holds
// the rest, and the rule that keeps it (#2564 P2b, ARB-43).
//
// This package is the `race` leg's wall: ~1556 s under -race on merge_group,
// and its heaviest tests are the bot-level ones that drive a python harness
// which itself runs git in a throwaway repository. Every one of them built its
// workspace with modernizeRepo, which installed the auto-maintenance refusal
// on the PROCESS with t.Setenv — and t.Setenv forbids t.Parallel outright (it
// panics the whole binary once the calling test's goroutine tree has gone
// parallel). So the family could not be made parallel by adding t.Parallel: the
// refusal had to stop being process state first. It now travels per command
// (modernizeChildEnv), which is also the only form that reaches the git a
// python GRANDchild runs.
//
// The rule below is per TEST, not per file: this family's files mix bot-level
// tests with pure readers, and the chokepoint is what decides, not the file
// name. It is directional on purpose — a new test that reaches modernizeRepo
// must answer "parallel" or earn a line in modernizeSerial — so it costs a
// neighbour nothing until they touch the family.
const modernizeChokepoint = "modernizeRepo"

// modernizeSerial names the tests of the family that must NOT go parallel,
// with the reason. One line per test, and the guard checks both directions: a
// line whose test is no longer in the family, or which no longer needs to be
// serial, is stale and reddens.
var modernizeSerial = map[string]string{
	"TestModernizeRepoPythonInheritsAutoMaintenanceOff": "drives the PROCESS environment with t.Setenv on purpose — it is the witness that the fixture's entries reach a python child's git and win over entries already there",
}

// childSeam is the environment chokepoint itself: the functions allowed to
// read the ambient environment and to build a subprocess for this family. It
// is also the escape hatch of the guard below — a helper that reads the
// environment for an unrelated reason belongs here WITH that reason, rather
// than making the rule vaguer for everybody.
var childSeam = map[string]string{
	"modernizeChild":    "builds the family's subprocess, carrier included",
	"modernizeChildEnv": "IS the carrier: the one place that reads the ambient environment",
}

// processState are the calls whose effect outlives the calling goroutine, so a
// test reaching one of them cannot be parallel. A new process-global setter
// used by this family belongs here the day it lands.
var processState = map[string]bool{
	"Setenv":   true,
	"Unsetenv": true,
	"Chdir":    true,
}

// spawnSites are the calls that start a child, and the one that reads the
// ambient environment to build its own. Both have to go through childSeam, or
// the child loses the auto-maintenance refusal silently — a loss no assertion
// of the test itself would notice, and one that has ejected pull requests from
// the merge queue (#821, #828).
//
// Known limit, stated rather than implied: these are CALL sites. A test that
// passed os.Environ as a function VALUE, or wrote its child's environment out
// by hand, would not be seen here — which is why the one override path of the
// family (modernizeLotVerifyRaw's env parameter) ADDS to the carrier instead
// of replacing it, so a hand-built environment cannot drop the refusal either.
var spawnSites = map[string]bool{
	"exec.Command":        true,
	"exec.CommandContext": true,
	"os.Environ":          true,
}

func readModernizeCampaign(t *testing.T) *testparallel.Campaign {
	t.Helper()
	c, err := testparallel.ReadCampaign(".", testparallel.LiveOnly)
	if err != nil {
		t.Fatalf("read this package's test files: %v", err)
	}
	// A walk that read nothing would make every verdict below vacuous.
	if got := len(c.Files()); got < 150 {
		t.Fatalf("the walk read %d test files of a package that has well over 150 — it is judging nothing, so no verdict below means anything", got)
	}
	return c
}

// modernizeFamily returns the top-level tests that reach the chokepoint, each
// with the file declaring it and how it gets there.
func modernizeFamily(t *testing.T, c *testparallel.Campaign) map[string]string {
	t.Helper()
	reach := c.Reach(map[string]bool{modernizeChokepoint: true})
	family := map[string]string{}
	for _, f := range c.Files() {
		for _, fn := range c.Tests(f) {
			if why := reach[fn.Name.Name]; why != "" {
				family[fn.Name.Name] = why
			}
		}
	}
	if len(family) == 0 {
		t.Fatalf("no test reaches %s() — the chokepoint this campaign is built on is gone, and these guards now certify nothing", modernizeChokepoint)
	}
	return family
}

// TestEveryModernizeFamilyTestIsParallel is the campaign's ratchet: the width
// this family won is a rule, not a habit somebody keeps.
func TestEveryModernizeFamilyTestIsParallel(t *testing.T) {
	t.Parallel()
	c := readModernizeCampaign(t)
	family := modernizeFamily(t, c)

	var missing []string
	for _, f := range c.Files() {
		for _, fn := range c.Tests(f) {
			name := fn.Name.Name
			if family[name] == "" {
				continue
			}
			if _, serial := modernizeSerial[name]; serial {
				continue
			}
			if !testparallel.OpensWithParallel(fn) {
				missing = append(missing, f+":"+name+"  (reaches "+modernizeChokepoint+" "+family[name]+")")
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d test(s) of the modernize family do not open with t.Parallel() — this family is the -race wall of the package (#2564 P2b) and its fixture no longer holds process state, so the width is there to take; add t.Parallel() as the first statement, or name what the test shares in modernizeSerial:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestTheModernizeSerialTableIsLive keeps the escape hatch honest in both
// directions: an entry for a test that left the family, that no longer exists,
// or that went parallel anyway is a reason that holds and covers nothing.
func TestTheModernizeSerialTableIsLive(t *testing.T) {
	t.Parallel()
	c := readModernizeCampaign(t)
	family := modernizeFamily(t, c)

	decl := map[string]*testparallelTest{}
	for _, f := range c.Files() {
		for _, fn := range c.Tests(f) {
			decl[fn.Name.Name] = &testparallelTest{file: f, parallel: testparallel.OpensWithParallel(fn)}
		}
	}
	for name, why := range childSeam {
		switch {
		case c.FileOf(name) == "":
			t.Errorf("childSeam names %s, which this package no longer declares — drop the line, or the hatch exempts a function that does not exist", name)
		case strings.TrimSpace(why) == "":
			t.Errorf("childSeam[%s] carries no reason", name)
		}
	}
	for name, why := range modernizeSerial {
		switch got, ok := decl[name]; {
		case !ok:
			t.Errorf("modernizeSerial names %s, which this package no longer declares — drop the line", name)
		case strings.TrimSpace(why) == "":
			t.Errorf("modernizeSerial[%s] carries no reason", name)
		case family[name] == "":
			t.Errorf("modernizeSerial names %s, which no longer reaches %s() — it is outside the family this table partitions, so the line covers nothing", name, modernizeChokepoint)
		case got.parallel:
			t.Errorf("%s:%s is listed serial (%s) AND opens with t.Parallel() — the table contradicts the code, and the code is what the -race binary runs", got.file, name, why)
		}
	}
}

type testparallelTest struct {
	file     string
	parallel bool
}

// TestNoModernizeFamilyTestInstallsProcessState is the half that keeps a
// 25-minute -race run from dying on a panic nobody can attribute: t.Setenv
// anywhere in a parallel test's goroutine tree kills the whole binary, and the
// panic names the test only if the binary gets that far.
func TestNoModernizeFamilyTestInstallsProcessState(t *testing.T) {
	t.Parallel()
	c := readModernizeCampaign(t)
	family := modernizeFamily(t, c)
	reach := c.Reach(processState)

	var offenders []string
	for name := range family {
		if _, serial := modernizeSerial[name]; serial {
			continue
		}
		if why := reach[name]; why != "" {
			offenders = append(offenders, name+"  ("+why+")")
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d parallel test(s) of the modernize family reach process-global state — t.Setenv PANICS a test whose goroutine tree went parallel, so this is a red -race run that names no test. Pass the value to the child instead (modernizeChildEnv), or declare the test in modernizeSerial:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestEveryModernizeFamilySpawnCarriesTheFixtureEnvironment holds every
// function this family drives to the one seam that gives a child its
// environment. A spawn built with the ambient environment instead loses the
// auto-maintenance refusal, and nothing the test asserts would notice: the
// detached `git maintenance run --auto` writes under .git/objects after the
// command returned, racing t.TempDir()'s removal (#821, #828).
func TestEveryModernizeFamilySpawnCarriesTheFixtureEnvironment(t *testing.T) {
	t.Parallel()
	c := readModernizeCampaign(t)
	family := modernizeFamily(t, c)

	roots := map[string]bool{}
	for name := range family {
		roots[name] = true
	}
	drives := c.Closure(roots)
	seam := c.Reach(map[string]bool{"modernizeChildEnv": true})

	var offenders []string
	for fn := range drives {
		if childSeam[fn] != "" {
			continue
		}
		var built []string
		for site := range spawnSites {
			if c.CallsDirectly(fn, site) {
				built = append(built, site)
			}
		}
		if len(built) == 0 || seam[fn] != "" {
			continue
		}
		sort.Strings(built)
		offenders = append(offenders, c.FileOf(fn)+":"+fn+"()  calls "+strings.Join(built, " and ")+" without reaching modernizeChildEnv")
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d function(s) the modernize family drives build a child, or its environment, outside the seam:\n  %s\n\nBuild the command with modernizeChild(t, …), or base its environment on modernizeChildEnv(t) instead of os.Environ().",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
