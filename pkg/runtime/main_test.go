package runtime

import (
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// Tool nodes and the fake shared sandboxes here start real shells, and a
// shell's own children (a `sleep` in a polling loop) only die because #955
// cancels the whole process group. The guard is the postcondition for that:
// it fails the package when a helper outlived the test that started it,
// instead of letting it run on into the next one. The engines resolve their
// plugins and scratch from the iterion home, which hometest keeps off the
// operator's; and a throw-away workdir that outlived the suite — never
// released, unremovable, or re-created after its release, wherever it lies —
// fails the package too, reclaimed by its recorded path.
func TestMain(m *testing.M) {
	os.Exit(hometest.Isolate(func() int {
		return proctest.NoProcessLeaks(func() int {
			return noThrowAwayWorkDirLeaks(&liveThrowAwayWorkDirs, &everThrowAwayWorkDirs, m.Run(), os.Stderr)
		})
	}))
}

// noThrowAwayWorkDirLeaks turns a clean run into a failure when a throw-away
// workdir outlived the suite, and names the cause.
func noThrowAwayWorkDirLeaks(live, ever *sync.Map, code int, w io.Writer) int {
	leaks, err := reclaimThrowAwayWorkDirs(live, ever)
	if len(leaks.NeverReleased) > 0 {
		fmt.Fprintf(w, "runtime: %d throw-away workdir(s) never released — an engine test drove a helper without Run or ResumeWithHostInputs; give it WithWorkDir(t.TempDir()) or t.Cleanup(e.releaseTempWorkDir): %v\n", len(leaks.NeverReleased), leaks.NeverReleased)
	}
	if len(leaks.Unremovable) > 0 {
		fmt.Fprintf(w, "runtime: %d throw-away workdir(s) could not be removed on release: %v\n", len(leaks.Unremovable), leaks.Unremovable)
	}
	if len(leaks.Recreated) > 0 {
		fmt.Fprintf(w, "runtime: %d throw-away workdir(s) re-created after their release — a writer outlived the call that owned it: %v\n", len(leaks.Recreated), leaks.Recreated)
	}
	if err != nil {
		fmt.Fprintf(w, "runtime: reclaim the throw-away workdirs: %v\n", err)
	}
	if (!leaks.empty() || err != nil) && code == 0 {
		return 1
	}
	return code
}
