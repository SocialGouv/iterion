package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// InheritedIterionDataDir answers one question — which iterion home did the
// OPERATOR choose — and its empty answer means "they said nothing", which
// every caller reads as the closed one. So the last tier matters as much as
// the first: a fallback that returns a path when the environment named none
// turns a fail-closed guard into a fail-open one, and nothing else in the
// tree would notice.
func TestTheInheritedHomeFailsClosedWhenNothingOperatorsNamesIt(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	// Every tier planted by a project `.env`, nothing inherited.
	planted := t.TempDir()
	t.Setenv("ITERION_HOME", filepath.Join(planted, "home"))
	t.Setenv("HOME", filepath.Join(planted, "user"))
	t.Setenv("TMPDIR", filepath.Join(planted, "tmp"))
	envtrust.MarkPlanted("ITERION_HOME", "HOME", "TMPDIR", "USERPROFILE")

	if got := InheritedIterionDataDir(); got != "" {
		t.Errorf("with every tier planted the operator named no home; got %q, which some guard will now trust", got)
	}
	// The live resolution is unaffected — only the authority question is.
	if got := GlobalIterionDataDir(); got != filepath.Join(planted, "home") {
		t.Errorf("the data dir still follows the live value: %q", got)
	}
}

// And the mirror: the tiers must AGREE for an operator who planted nothing,
// or their own installed plugins lose their authority for no reason. A tier
// present in one function and missing from the other is how that happens.
func TestTheTwoHomeResolutionsAgreeWhenNothingWasPlanted(t *testing.T) {
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")

	base := t.TempDir()
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"ITERION_HOME set", map[string]string{"ITERION_HOME": filepath.Join(base, "explicit")}},
		{"only HOME set", map[string]string{"ITERION_HOME": "", "HOME": filepath.Join(base, "user")}},
		{"trailing separator", map[string]string{"ITERION_HOME": filepath.Join(base, "slash") + string(filepath.Separator)}},
		{"only TMPDIR set", map[string]string{"ITERION_HOME": "", "HOME": "", "TMPDIR": filepath.Join(base, "tmp")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				if v == "" {
					if err := os.Unsetenv(k); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Unsetenv(k) })
					continue
				}
				t.Setenv(k, v)
			}
			global, inherited := GlobalIterionDataDir(), InheritedIterionDataDir()
			if global != inherited {
				t.Errorf("the two resolutions disagree: global=%q inherited=%q — the operator's own "+
					"installed plugins would lose their authority here", global, inherited)
			}
		})
	}
}
