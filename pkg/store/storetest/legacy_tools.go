package storetest

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func LegacyTool(t *testing.T, variable string) string {
	t.Helper()
	path := os.Getenv(variable)
	if path == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatalf("required old executable test needs %s; see scripts/build-port-legacy-fixture.sh", variable)
		}
		t.Skip(variable + " not set")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func RunLegacyTool(t *testing.T, tool, workspace string, expectSuccess bool, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Dir = workspace
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ITERION_HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "NO_COLOR=1"}
	out, err := cmd.CombinedOutput()
	if (err == nil) != expectSuccess {
		t.Fatalf("old executable %v: %v\n%s", args, err, out)
	}
	if ctx.Err() != nil {
		t.Fatalf("old executable timed out: %v", args)
	}
	return string(out)
}
