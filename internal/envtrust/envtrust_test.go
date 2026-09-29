package envtrust

import (
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
			os.Exit(3) // laundered
		}
		if !Planted("ITERION_TEST_TRUST") {
			os.Exit(4)
		}
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
	if err != nil {
		t.Fatalf("the child re-derived the answer from its inherited environment and trusted a planted value: %v\n%s",
			err, out)
	}
}

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
