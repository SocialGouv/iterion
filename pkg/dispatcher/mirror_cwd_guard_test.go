package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

// Under this package's tests, a skill mirror whose destination is the test
// process's cwd — the package directory — would write into the developer's
// checkout (pkg/runner/.claude, seen 2026-09-24): the engine under test has
// no explicit workDir. The guard fails that run loudly, naming the remedy
// (#1803).
func init() {
	runtime.SetMirrorCwdGuardForTests(func(workDir string) error {
		wd, err := filepath.Abs(workDir)
		if err != nil {
			return nil
		}
		cwd, err := os.Getwd()
		if err != nil {
			return nil
		}
		if filepath.Clean(wd) == filepath.Clean(cwd) {
			return fmt.Errorf("the skill mirror's destination is the TEST process cwd (%s): the engine under test has no explicit workDir — set runtime.WithWorkDir(t.TempDir()) so the mirror writes into the test's own directory (#1803)", wd)
		}
		return nil
	})
}
