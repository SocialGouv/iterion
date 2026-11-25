package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestFailuresAreReadFromRecordedJobLogs(t *testing.T) {
	for _, tc := range []struct {
		log  string
		want []Failure
	}{
		// A race shard: a subtest failed, so did its parent; only the leaf
		// is named, under the package of the verdict line ~1000 lines on.
		{"138931970338", []Failure{{"github.com/SocialGouv/iterion/pkg/server", "TestAssistantWatchSweep_K3HandoverThroughRetryDeliveries/every_resume_unreachable"}}},
		{"139171191168", []Failure{{"github.com/SocialGouv/iterion/pkg/backend/model", "TestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts"}}},
		{"139171191636", []Failure{{"github.com/SocialGouv/iterion/e2e", "TestProdWatch_SentryAnOversizedPageFloodsNoChannel"}}},
		// A -timeout panic names the tests still running.
		{"139080857550", []Failure{{"github.com/SocialGouv/iterion/pkg/store/mongo", "TestRunMetadataPublicWireGuard/acquisition/malformed"}}},
		// A job that died with no FAIL line, and an aggregator: nothing.
		{"139080857708", nil},
		{"139092614667", nil},
	} {
		t.Run(tc.log, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/logs/" + tc.log + ".log")
			if err != nil {
				t.Fatal(err)
			}
			if got := parseFailures(string(raw)); !slices.Equal(got, tc.want) {
				t.Fatalf("parseFailures = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailuresOfOnePackageNeverLeakIntoTheNext(t *testing.T) {
	log := strings.Join([]string{
		"--- FAIL: TestA (0.01s)",
		"FAIL",
		"FAIL\texample.com/m/a\t0.1s",
		"ok  \texample.com/m/b\t0.2s",
		"FAIL\texample.com/m/c [build failed]",
		"--- FAIL: TestD (0.01s)",
		"    --- FAIL: TestD/x+y (0.00s)",
		"FAIL\texample.com/m/d\t0.3s",
	}, "\n")
	want := []Failure{
		{"example.com/m/a", "TestA"},
		{"example.com/m/c", ""},
		{"example.com/m/d", "TestD/x+y"},
	}
	if got := parseFailures(log); !slices.Equal(got, want) {
		t.Fatalf("parseFailures = %q, want %q", got, want)
	}
}

func TestAQuarantinePathStopsAtTheFirstLevelARowCannotSpell(t *testing.T) {
	for in, want := range map[string]string{
		"TestA":                          "TestA",
		"TestA/every_resume_unreachable": "TestA/every_resume_unreachable",
		"TestA/x+y/z":                    "TestA",
		"TestA/ok/a*b":                   "TestA/ok",
		"TestA/v1.2:x=y,z":               "TestA/v1.2:x=y,z",
		"TestA/#00":                      "TestA",
	} {
		if got := quarantinePath(in); got != want {
			t.Errorf("quarantinePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The jobs main-watch may quarantine for are read from the workflow itself:
// on the committed tests.yml they are exactly the gating legs that skip the
// list (internal/ciguard's TestEveryGatingGoTestSkipsTheQuarantine pins the
// skip in each of them), and test-native-exec alone runs off the scale set
// where the nightly witness runs.
func TestTheQuarantinedJobsAreTheGatingLegsThatSkipTheList(t *testing.T) {
	src, err := os.ReadFile("../../../" + watchedPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := quarantinedJobs(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []gatingJob{{Name: "race-shard"}, {Name: "test-e2e"}, {Name: "test-native-exec", OffScaleSet: true}, {Name: "test-unit"}}
	if !slices.Equal(got, want) {
		t.Fatalf("quarantinedJobs = %v, want %v", got, want)
	}
	for name, in := range map[string]bool{"race-shard (2)": true, "test-unit": true, "test-unit-extra": false, "mongo-conformance": false, "test": false} {
		if _, found := gatingJobOf(name, got); found != in {
			t.Errorf("gatingJobOf(%q) found = %v, want %v", name, found, in)
		}
	}
	if _, err := quarantinedJobs([]byte("jobs:\n  a:\n    steps:\n      - run: go test ./...\n")); err == nil {
		t.Error("a workflow where no job skips the list must be refused, not read as an empty set")
	}
}
