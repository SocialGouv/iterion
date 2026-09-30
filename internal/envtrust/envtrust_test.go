package envtrust

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestAPlantedVariableIsNotInherited(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")
	t.Setenv("ITERION_TEST_TRUST", "operator-value")

	if got := Inherited("ITERION_TEST_TRUST"); got != "operator-value" {
		t.Fatalf("an untouched variable is inherited as it stands, got %q", got)
	}

	MarkPlanted("ITERION_TEST_TRUST")

	if Inherited("ITERION_TEST_TRUST") != "" {
		t.Error("a variable a project .env planted must not answer for the operator")
	}
	if os.Getenv("ITERION_TEST_TRUST") != "operator-value" {
		t.Error("the value itself must keep working — only its authority is in question")
	}
}

// The record has to travel to child processes: applyDotEnv plants with
// os.Setenv, so `iterion run --background` and the operator MCP server inherit
// the value. A child re-deriving the answer from its own environment would
// find the planted value indistinguishable from an operator's — the
// repository's answer laundered through one fork.
func TestThePlantedRecordReachesAChildProcess(t *testing.T) {
	if os.Getenv("ENVTRUST_CHILD") == "1" {
		// In the child: the marker arrived through the environment alone.
		ResetForTest()
		if Inherited("ITERION_TEST_TRUST") != "" {
			os.Exit(childLaundered)
		}
		if !Planted("ITERION_TEST_TRUST") {
			os.Exit(childMarkerLost)
		}
		// A POSITIVE receipt, not just exit 0: a test binary whose
		// -test.run matches nothing also exits 0, so "the child did not
		// fail" cannot distinguish "the child agreed" from "the child never
		// ran a single assertion".
		fmt.Println(childReceipt)
		os.Exit(0)
	}

	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")
	t.Setenv("ITERION_TEST_TRUST", "planted-by-a-repository")
	MarkPlanted("ITERION_TEST_TRUST")

	cmd := exec.Command(os.Args[0], "-test.run=TestThePlantedRecordReachesAChildProcess")
	// Deliberately NOT setting cmd.Env — exactly how iterion spawns its own
	// children (pkg/runview/detached.go, pkg/operatormcp/spawn.go).
	cmd.Env = append(os.Environ(), "ENVTRUST_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		if !strings.Contains(string(out), childReceipt) {
			t.Fatalf("the child exited 0 without reaching its assertions — an unmatched -test.run does that "+
				"too, so this test would pass having proved nothing\n%s", out)
		}
		return
	}
	// Each exit code says which of the two failures happened, and anything
	// else is neither: a test binary exits 1 on a failing test, so reporting
	// every non-zero code as "trusted a planted value" names a defect that
	// may not be the one that occurred.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case childLaundered:
			t.Fatalf("the child re-derived the answer from its inherited environment and trusted a planted "+
				"value — the repository's answer laundered through one fork\n%s", out)
		case childMarkerLost:
			t.Fatalf("the child did not inherit the planted record at all: the marker is not re-exported, so "+
				"every child starts trusting again\n%s", out)
		}
	}
	t.Fatalf("the child failed for neither reason this test checks (%v) — read its output\n%s", err, out)
}

// Exit codes the child answers with, distinct from a test binary's own 1.
const (
	childLaundered  = 3
	childMarkerLost = 4
)

// childReceipt is what the child prints once it has actually checked both
// facts. Its absence is a failure even on exit 0.
const childReceipt = "envtrust-child-checked-both"

// A forged marker can only ever REMOVE trust, so it needs no protection.
func TestAMarkerFromTheEnvironmentIsHonoured(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "A, B ,,C")
	t.Setenv("A", "x")

	for _, name := range []string{"A", "B", "C"} {
		if !Planted(name) {
			t.Errorf("%s should be marked planted", name)
		}
	}
	if Planted("D") {
		t.Error("a name the marker does not list is not planted")
	}
	if Inherited("A") != "" {
		t.Error("a marked name answers empty")
	}
}

// Marking is cumulative and the exported marker stays stable, so a child sees
// every name its ancestors planted rather than only the last one.
func TestMarkingIsCumulativeAndStable(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")

	MarkPlanted("ZEBRA")
	MarkPlanted("ALPHA")
	MarkPlanted("ZEBRA")

	if got := os.Getenv(EnvPlantedNames); got != "ALPHA,ZEBRA" {
		t.Errorf("marker = %q, want a sorted, de-duplicated list", got)
	}
	for _, name := range strings.Split("ALPHA,ZEBRA", ",") {
		if !Planted(name) {
			t.Errorf("%s lost from the set", name)
		}
	}
}

// The marker is a comma-joined list with no escaping, and a `.env` key may
// carry a comma. `ITERION_HOME,HARMLESS=1` therefore reached a child as TWO
// planted names, the second of which denies the OPERATOR's own home its
// authority in every descendant — no escalation, but an injectable denial of
// the operator's own capability, with no diagnostic anywhere.
func TestAMarkedNameCarryingTheSeparatorNeverReachesAChild(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")

	MarkPlanted("ITERION_HOME,HARMLESS", "LEGITIMATE")

	marker := os.Getenv(EnvPlantedNames)
	if strings.Contains(marker, "ITERION_HOME") {
		t.Errorf("the marker exported %q: a child splitting on \",\" reads ITERION_HOME as planted and the "+
			"operator's own home loses its authority there", marker)
	}
	if !strings.Contains(marker, "LEGITIMATE") {
		t.Errorf("a well-formed name must still travel: %q", marker)
	}
	// In THIS process the odd name keeps its (harmless) record — it is the
	// crossing that cannot represent it.
	if !Planted("ITERION_HOME,HARMLESS") {
		t.Error("the local record is unchanged; only the marker cannot carry it")
	}
}

// The comma was the first character found to cross badly, not the class: the
// reader TrimSpaces each name while the writer does not, so " ITERION_HOME"
// leaves as one name and arrives as another — the same injectable denial of
// the operator's own authority, through a different character.
func TestOnlyNamesAShellCouldExportCrossToAChild(t *testing.T) {
	for _, odd := range []string{"ITERION_HOME ", " ITERION_HOME", "ITERION_HOME\tX", "ITERION,HOME", "9LEADING"} {
		t.Run(odd, func(t *testing.T) {
			ResetForTest()
			t.Cleanup(ResetForTest)
			t.Setenv(EnvPlantedNames, "")

			MarkPlanted(odd, "LEGITIMATE")
			marker := os.Getenv(EnvPlantedNames)

			// The child seeds its set from the marker alone.
			ResetForTest()
			t.Setenv(EnvPlantedNames, marker)
			if Planted("ITERION_HOME") {
				t.Errorf("marker %q makes a child read ITERION_HOME as planted, so the operator's own home "+
					"loses its authority there", marker)
			}
			if !Planted("LEGITIMATE") {
				t.Errorf("a well-formed name must still travel: %q", marker)
			}
		})
	}
}
