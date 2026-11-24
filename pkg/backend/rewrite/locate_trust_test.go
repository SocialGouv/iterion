package rewrite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// A rewriter is a binary the LAUNCHER execs — host-side, on a sandboxed
// node's every shell command, with the launcher's own environment. Which
// binary that is, is therefore an authority question.
//
// iterion fills unset variables from the nearest `.env` walking up from the
// working directory, and that file sits in the repository under review. Read
// live, the locate variable let one line in that repository choose what the
// launcher runs. The trust root already recorded the plant; nothing asked it.
func TestTheLocateVariableIsReadFromTheOperatorsEnvironmentOnly(t *testing.T) {
	newBinary := func(t *testing.T, dir, name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("a value a project .env planted is not used", func(t *testing.T) {
		planted := newBinary(t, t.TempDir(), "tools/rtk")
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_TEST_REWRITER_BIN", planted)
		envtrust.MarkPlanted("ITERION_TEST_REWRITER_BIN")

		if got := locate(plugin.LocateSpec{Env: "ITERION_TEST_REWRITER_BIN"}); got == planted {
			t.Errorf("locate chose %q from a variable a repository planted — the launcher would exec it, "+
				"host-side, on a sandboxed node's every Bash call", got)
		}
	})

	t.Run("the operator's own value still is", func(t *testing.T) {
		theirs := newBinary(t, t.TempDir(), "bin/rtk")
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("ITERION_TEST_REWRITER_BIN", theirs)
		// Nothing marked planted: the operator exported it themselves.

		if got := locate(plugin.LocateSpec{Env: "ITERION_TEST_REWRITER_BIN"}); got != theirs {
			t.Errorf("locate = %q, want the operator's own %q — this variable is a supported knob",
				got, theirs)
		}
	})

	// The PATH fallback answers the same authority question: a project .env
	// planting PATH (applyDotEnv fills only names that were UNSET) would
	// choose the binary locate.bin resolves — #2809. Under a planted PATH
	// the fallback finds nothing; the operator's own PATH still resolves.
	t.Run("a planted PATH chooses no binary", func(t *testing.T) {
		dir := t.TempDir()
		newBinary(t, dir, "rtk")
		envtrust.ResetForTest()
		t.Cleanup(envtrust.ResetForTest)
		t.Setenv(envtrust.EnvPlantedNames, "")
		t.Setenv("PATH", dir)

		if got := locate(plugin.LocateSpec{Bin: "rtk"}); got == "" {
			t.Fatal("the operator's own PATH must resolve locate.bin")
		}

		envtrust.MarkPlanted("PATH")
		if got := locate(plugin.LocateSpec{Bin: "rtk"}); got != "" {
			t.Errorf("locate chose %q under a PATH a repository planted — the launcher would exec it, "+
				"host-side, on a sandboxed node's every Bash call", got)
		}
	})
}
