package catalogcorpus

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/testparallel"
)

// catalogcorpusParallelCap bounds how many of this package's tests run at
// once, at the width bots holds its own to: the parallel tests here spawn
// subprocesses (python harnesses, throwaway repositories) whose memory -race
// multiplies, and they keep that bound outside bots.
const catalogcorpusParallelCap = 8

// ITERION_BOTS_PARALLEL overrides the cap, the same knob that sets bots'
// width: these tests guard the bots catalogue from another package.
const catalogcorpusParallelEnv = "ITERION_BOTS_PARALLEL"

// TestMain runs every test of this package from <module root>/bots, the
// directory these guards were written in. Their paths stay spelled from
// there — the frozen keys of bots/testdata/untrusted_input_boundary_members.txt,
// stillDialled, routableNonWorkflows and every "../examples" root — so a key
// reads byte for byte as it did when the guards lived in bots.
//
// The directory is changed once, before any test runs: several tests call
// t.Parallel, which t.Chdir refuses.
//
// The operator's global and system git config must not reach the git calls
// these guards make (check-ignore, a throwaway repository), so both point at
// os.DevNull as they do in CI.
//
// -parallel is capped at catalogcorpusParallelCap, as bots caps its own
// (testparallel.Cap: an explicit -parallel still wins).
func TestMain(m *testing.M) {
	testparallel.Cap(catalogcorpusParallelEnv, catalogcorpusParallelCap)
	if err := chdirToBots(); err != nil {
		fmt.Fprintln(os.Stderr, "catalogcorpus:", err)
		os.Exit(2)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(pricingtest.Run(m.Run))
}

// chdirToBots walks up from the package directory to the module root (the
// first directory holding go.mod) and enters its bots/ directory.
func chdirToBots() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("no go.mod above the test's working directory")
		}
		dir = parent
	}
	bots := filepath.Join(dir, "bots")
	if st, err := os.Stat(bots); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory: %v", bots, err)
	}
	return os.Chdir(bots)
}
