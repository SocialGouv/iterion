package clilocate

import (
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The binaries clilocate resolves are the agent CLIs the backends spawn
// with the operator's credentials — the most privileged authority question
// the launcher asks. A PATH a project .env planted must not answer it:
// under one, the PATH arm refuses (envtrust.LookPath) and only the
// well-known-absolute Fallbacks may still answer — #2809 closed the seam,
// #2876 this site. Red when Locate returns to exec.LookPath: the planted
// binary then wins over the fallback and the no-fallback miss resolves.
func TestLocateResolvesNothingFromAPlantedPATH(t *testing.T) {
	pathDir := t.TempDir()
	planted := makeExecutable(t, pathDir, "acli")
	fallback := makeExecutable(t, t.TempDir(), "acli")
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", pathDir)

	spec := Spec{Name: "acli", Fallbacks: []string{fallback}}

	// The operator's own PATH resolves first, as exec.LookPath always did —
	// the non-planted behaviour is strictly unchanged.
	if got, ok := Locate("", spec); !ok || got != planted {
		t.Fatalf("an unplanted PATH must resolve first: Locate = %q, %v, want %q, true", got, ok, planted)
	}

	envtrust.MarkPlanted("PATH")

	// Under the planted PATH the PATH arm refuses BEFORE the fallbacks are
	// consulted — and the fallback arm itself is unchanged: its absolute
	// well-known location still answers.
	if got, ok := Locate("", spec); !ok || got != fallback {
		t.Errorf("under a planted PATH Locate = %q, %v, want the fallback %q — the repository does not choose the agent CLI", got, ok, fallback)
	}

	// And with no fallback to answer, the refusal is the whole answer.
	if got, ok := Locate("", Spec{Name: "acli"}); ok {
		t.Errorf("under a planted PATH Locate without fallbacks = %q, true, want a miss", got)
	}
}
