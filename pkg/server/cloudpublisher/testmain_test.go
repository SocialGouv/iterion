package cloudpublisher

import (
	"os"
	"testing"
)

// TestMain isolates ITERION_HOME across the whole cloudpublisher test suite.
//
// Why. SubmitLaunch → resolveContributionsFor → plugin.Load(): every launch
// path exercised here transitively reads <ITERION_HOME>/plugins. Without an
// override, that resolves to the operator's REAL ~/.iterion/plugins, and a
// live pack there ships a real contribution into every assertion — a
// deploy-target.md on a developer machine has reddened
// TestSubmitLaunch_BrokenPluginSourceIsSkippedNotFatal, whose assertion is
// exactly "no contribution named deploy.md rides the message". Same class as
// the isolation traps docs/agents/testing.md names.
//
// Setting it once at TestMain is a chokepoint — a per-test t.Setenv would
// need to land on every SubmitLaunch/SubmitResume/resolveContributionsFor
// caller, and the two mirrors of the same guard would drift the first time
// somebody added a new test and forgot the one line, exactly the failure
// mode this package's tests already carried. A test that WANTS a populated
// iterion home overrides the tempdir with its own t.Setenv, which the
// contributions_dedup_test tests do to install a specific pack.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "iterion-cloudpublisher-home-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv("ITERION_HOME", dir)
	code := m.Run()
	os.Exit(code)
}
