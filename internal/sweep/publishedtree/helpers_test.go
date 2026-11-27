package publishedtree

import (
	"os"
	"os/exec"
	"testing"
)

// requireAssessmentTools skips the test when a tool it shells out to is
// missing — except under CI, where a skip would turn the guard into a green
// no-op, so a missing tool is fatal there.
func requireAssessmentTools(t *testing.T, tools ...string) {
	t.Helper()
	if len(tools) == 0 {
		tools = []string{"python3", "git"}
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not on PATH under CI — the guard would skip into a green no-op; install it in the workflow", tool)
			}
			t.Skipf("%s not on PATH", tool)
		}
	}
}
