package runview

import (
	"context"
	"slices"
	"testing"
)

// TestBuildRunnerCmd_RunCarriesTheBundleDir: the detached runner compiles
// what the pre-flight admitted. For a studio launch the file is the store's
// materialised copy of the editor buffer — a name no promotion recognises
// — so the bundle it was admitted against travels as `--bundle-dir`; a
// launch with no bundle emits no flag.
func TestBuildRunnerCmd_RunCarriesTheBundleDir(t *testing.T) {
	cmd, err := buildRunnerCmd(context.Background(), "iterion", detachedSpec{
		Command:   runnerCommandRun,
		RunID:     "run-detached-1",
		FilePath:  "/store/inline-sources/a1b2c3d4e5f6-main.bot",
		BundleDir: "/work/bots/mf",
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	i := slices.Index(cmd.Args, "--bundle-dir")
	if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != "/work/bots/mf" {
		t.Fatalf("argv %v: want --bundle-dir /work/bots/mf (the subprocess would compile the copy alone and die on C003)", cmd.Args)
	}
	bare, err := buildRunnerCmd(context.Background(), "iterion", detachedSpec{
		Command:  runnerCommandRun,
		RunID:    "run-detached-2",
		FilePath: "/work/loose.bot",
	})
	if err != nil {
		t.Fatalf("buildRunnerCmd: %v", err)
	}
	if slices.Contains(bare.Args, "--bundle-dir") {
		t.Fatalf("argv %v: a launch with no bundle emits a --bundle-dir", bare.Args)
	}
}
