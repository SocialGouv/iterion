package envtrust

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A planted PATH is the repository under review choosing which executable
// the launcher runs — the authority #1945 closed at the config, reopened one
// fork down wherever a bare command name is resolved against the live PATH
// (#2809). applyDotEnv fills only names that were UNSET, so a planted PATH
// means the operator's process had none: LookPath resolves nothing, loudly.
// Red when the Planted check goes: the probe binary then resolves under the
// planted PATH and the refusal case fails on its own assertion.
func TestLookPathResolvesNothingUnderAPlantedPATH(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	t.Setenv(EnvPlantedNames, "")

	dir := t.TempDir()
	bin := filepath.Join(dir, "envtrust-probe-bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did.
	if _, err := LookPath("envtrust-probe-bin"); err != nil {
		t.Fatalf("an unplanted PATH must resolve: %v", err)
	}

	// The repository's does not.
	MarkPlanted("PATH")
	if _, err := LookPath("envtrust-probe-bin"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("under a planted PATH LookPath = %v, want a loud ErrNotFound — the repository does not choose the executable", err)
	}

	// An explicit path consults no PATH and resolves either way.
	if got, err := LookPath(bin); err != nil || got != bin {
		t.Errorf("an explicit path must resolve under a planted PATH: got %q, %v", got, err)
	}
}

// The separator set is the platform's, as exec.LookPath uses it: on Windows
// '/' names a path too, so it must consult no PATH there either way.
func TestNamesAPathFollowsThePlatform(t *testing.T) {
	if namesAPath("bare-name") {
		t.Error("a bare name consults PATH")
	}
	if !namesAPath("dir" + string(os.PathSeparator) + "bin") {
		t.Error("the platform separator names a path")
	}
	defer func(prev string) { goos = prev }(goos)
	goos = "windows"
	if !namesAPath("dir/bin") {
		t.Error("on Windows '/' names a path — exec.LookPath honours it")
	}
	goos = "linux"
	if namesAPath("dir\\bin") {
		t.Error("on Linux a backslash is a name character, not a separator")
	}
}
