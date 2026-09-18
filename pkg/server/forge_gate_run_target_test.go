package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

const (
	gateTestBase  = "https://iterion.cloud"
	gateTestRunID = "01a09ec6-b5b3-7660-8ef1-295031b7260d"
	gateOtherRun  = "01a09eb5-b298-7543-b45a-9de9e37044a7"
)

func statusAt(target string) forge.CommitStatus {
	return forge.CommitStatus{State: forge.CommitStatePending, Context: "revi/review", TargetURL: target}
}

// The whole reason gateRunTarget is a pair. A pull request opened before the
// studio moved carries a status whose target URL has no /studio in it, and it
// is read by a build that only ever writes the new shape. Getting "not mine"
// there is not cosmetic: the reconciler would leave a dead run's own claim
// unrepaired, and the pull request waits on a pending check nothing resolves.
func TestGateRunTargetRecognisesBothSpellingsOfItsOwnRun(t *testing.T) {
	target := gateRunTargetFor(gateTestBase, gateTestRunID)

	current := gateTestBase + "/studio/runs/" + gateTestRunID
	legacy := gateTestBase + "/runs/" + gateTestRunID

	if target.url != current {
		t.Fatalf("written target = %q, want %q", target.url, current)
	}
	if target.legacy != legacy {
		t.Fatalf("legacy spelling = %q, want %q", target.legacy, legacy)
	}
	if !target.speaksFor(statusAt(current)) {
		t.Errorf("a status this build wrote (%s) is not recognised as its own", current)
	}
	if !target.speaksFor(statusAt(legacy)) {
		t.Errorf("a status written before the /studio move (%s) is not recognised — every pull request already in flight would be mis-attributed", legacy)
	}
	// Forges round-trip the URL through their own storage; case and stray
	// whitespace were already tolerated before the move and still must be.
	if !target.speaksFor(statusAt("  " + strings.ToUpper(legacy) + "  ")) {
		t.Errorf("a legacy target with different case/whitespace is not recognised")
	}
}

// Mutating the witness towards the FORBIDDEN alternative, not towards empty:
// an empty target only proves the comparison looks at something. Another run's
// URL is the case that decides whether a live review gets painted over.
func TestGateRunTargetRefusesAnotherRun(t *testing.T) {
	target := gateRunTargetFor(gateTestBase, gateTestRunID)

	for _, other := range []string{
		gateTestBase + "/studio/runs/" + gateOtherRun,
		gateTestBase + "/runs/" + gateOtherRun,
		"https://someone-else.example/studio/runs/" + gateTestRunID,
		// A prefix of the run id must not pass for the run id.
		gateTestBase + "/studio/runs/" + gateTestRunID[:8],
	} {
		if target.speaksFor(statusAt(other)) {
			t.Errorf("a status at %q was claimed by run %s", other, gateTestRunID)
		}
	}
}

func TestGateRunTargetIsUnknowableWithoutAPublicURL(t *testing.T) {
	target := gateRunTargetFor("", gateTestRunID)
	if !target.unset() {
		t.Fatalf("with no PublicURL the run cannot be named, got url=%q", target.url)
	}
	// Both directions of the ambiguity: an unnameable run owns nothing, and a
	// status with no target belongs to nobody.
	if target.speaksFor(statusAt("https://iterion.cloud/studio/runs/" + gateTestRunID)) {
		t.Error("an unnameable run claimed a status")
	}
	named := gateRunTargetFor(gateTestBase, gateTestRunID)
	if named.speaksFor(statusAt("")) {
		t.Error("a status with no target URL was claimed")
	}
}

// What the gate WRITES must be one shape. gateRunURL feeds both the commit
// status and the escalation body published on the pull request; if they
// disagree an operator reads two different addresses for one run.
func TestGateWritesOnlyTheCurrentSpelling(t *testing.T) {
	want := gateTestBase + "/studio/runs/" + gateTestRunID
	if got := gateRunURL(gateTestBase, gateTestRunID); got != want {
		t.Errorf("gateRunURL = %q, want %q", got, want)
	}
	if got := gateRunRef(gateTestBase, gateTestRunID); got != want {
		t.Errorf("gateRunRef (escalation body) = %q, want %q", got, want)
	}
	if got := gateRunURL("", gateTestRunID); got != "" {
		t.Errorf("gateRunURL with no base = %q, want empty", got)
	}
}
