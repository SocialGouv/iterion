package store

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// homeResolutionModes runs a check twice: as a test process resolves the
// iterion home — its own home standing in for the operator's — and as a
// production binary does, which is where the operator's tiers live.
var homeResolutionModes = []struct {
	name    string
	resolve func(testing.TB)
}{
	{"in a test process", func(testing.TB) {}},
	{"as in production", ResolveIterionHomeAsInProductionForTests},
}

// InheritedIterionDataDir answers one question — which iterion home did the
// OPERATOR choose — and its empty answer means "they said nothing", which
// every caller reads as the closed one. So the last tier matters as much as
// the first: a fallback that returns a path when the environment named none
// turns a fail-closed guard into a fail-open one, and nothing else in the
// tree would notice.
func TestTheInheritedHomeFailsClosedWhenNothingOperatorsNamesIt(t *testing.T) {
	for _, mode := range homeResolutionModes {
		t.Run(mode.name, func(t *testing.T) {
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")

			// Every tier planted by a project `.env`, nothing inherited.
			planted := t.TempDir()
			t.Setenv("ITERION_HOME", filepath.Join(planted, "home"))
			t.Setenv("HOME", filepath.Join(planted, "user"))
			t.Setenv("TMPDIR", filepath.Join(planted, "tmp"))
			envtrust.MarkPlanted("ITERION_HOME", "HOME", "TMPDIR", "USERPROFILE")
			mode.resolve(t)

			if got := InheritedIterionDataDir(); got != "" {
				t.Errorf("with every tier planted the operator named no home; got %q, which some guard will now trust", got)
			}
			// The live resolution is unaffected — only the authority question is.
			if got := GlobalIterionDataDir(); got != filepath.Join(planted, "home") {
				t.Errorf("the data dir still follows the live value: %q", got)
			}
		})
	}
}

// Without a home dir the writers fall back to <tmp>/iterion-data — a location
// any local user can create before the operator does — so the operator named
// no home there: the authority question answers none, whether TMPDIR was
// inherited or not, and a plugin installed in that fallback carries no
// authority.
func TestTheSharedFallbackIsNoHomeTheOperatorChose(t *testing.T) {
	base := t.TempDir()
	for _, mode := range homeResolutionModes {
		for _, tc := range []struct {
			name string
			tmp  string
		}{
			{"TMPDIR inherited", filepath.Join(base, "tmp")},
			{"no TMPDIR", ""},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				envtrust.ResetForTest()
				t.Cleanup(envtrust.ResetForTest)
				t.Setenv(envtrust.EnvPlantedNames, "")
				t.Setenv("ITERION_HOME", "")
				t.Setenv(homeEnvName(), "")
				t.Setenv("TMPDIR", tc.tmp)
				mode.resolve(t)

				if got := InheritedIterionDataDir(); got != "" {
					t.Errorf("with no home dir the operator chose no home; got %q, which the plugin registry would trust", got)
				}
			})
		}
	}
}

// And the mirror: the tiers must AGREE for an operator who planted nothing,
// or their own installed plugins lose their authority for no reason. A tier
// present in one function and missing from the other is how that happens —
// and so is a value one of them makes absolute and the other keeps relative.
func TestTheTwoHomeResolutionsAgreeWhenNothingWasPlanted(t *testing.T) {
	base := t.TempDir()
	user := filepath.Join(base, "user")
	for _, mode := range homeResolutionModes {
		for _, tc := range []struct {
			name string
			// An empty value leaves the variable empty, which every tier
			// reads as unset.
			env map[string]string
			// operatorsOwn makes ITERION_HOME the value the test process
			// inherited, which names the operator's real data.
			operatorsOwn bool
		}{
			{name: "ITERION_HOME set", env: map[string]string{"ITERION_HOME": filepath.Join(base, "explicit"), "HOME": user}},
			{name: "only HOME set", env: map[string]string{"ITERION_HOME": "", "HOME": user}},
			{name: "trailing separator", env: map[string]string{"ITERION_HOME": filepath.Join(base, "slash") + string(filepath.Separator), "HOME": user}},
			{name: "relative ITERION_HOME", env: map[string]string{"ITERION_HOME": "rel-home", "HOME": user}},
			{name: "relative HOME", env: map[string]string{"ITERION_HOME": "", "HOME": "rel-user"}},
			{name: "the operator's inherited ITERION_HOME", env: map[string]string{"ITERION_HOME": "/operator/real/iterion-home", "HOME": user}, operatorsOwn: true},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				envtrust.ResetForTest()
				t.Cleanup(envtrust.ResetForTest)
				t.Setenv(envtrust.EnvPlantedNames, "")
				t.Chdir(base)
				for k, v := range tc.env {
					t.Setenv(k, v)
				}
				if tc.operatorsOwn {
					prev := processIterionHome
					t.Cleanup(func() { processIterionHome = prev })
					processIterionHome = tc.env["ITERION_HOME"]
				}
				mode.resolve(t)

				global, inherited := GlobalIterionDataDir(), InheritedIterionDataDir()
				if global != inherited {
					t.Errorf("the two resolutions disagree: global=%q inherited=%q — the operator's own "+
						"installed plugins would lose their authority here", global, inherited)
				}
			})
		}
	}
}

// A TMPDIR a project `.env` planted leaves the operator's HOME the tier that
// decides, and the two resolutions agree on it.
func TestTheTwoHomeResolutionsAgreeWhenOnlyTMPDIRWasPlanted(t *testing.T) {
	for _, mode := range homeResolutionModes {
		t.Run(mode.name, func(t *testing.T) {
			envtrust.ResetForTest()
			t.Cleanup(envtrust.ResetForTest)
			t.Setenv(envtrust.EnvPlantedNames, "")

			base := t.TempDir()
			t.Setenv("ITERION_HOME", "")
			t.Setenv("HOME", filepath.Join(base, "user"))
			t.Setenv("TMPDIR", filepath.Join(base, "planted-tmp"))
			envtrust.MarkPlanted("TMPDIR")
			mode.resolve(t)

			if global, inherited := GlobalIterionDataDir(), InheritedIterionDataDir(); global != inherited {
				t.Errorf("the two resolutions disagree: global=%q inherited=%q — the operator's own "+
					"installed plugins would lose their authority here", global, inherited)
			}
		})
	}
}
