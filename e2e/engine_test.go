package e2e

import (
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newEngine is how this suite builds an engine: on a git repository the TEST
// owns, never on the checkout the tests happen to run in.
//
// runtime.New with no WithWorkDir defaults to os.Getwd() at Run() time — the
// e2e package directory, inside the developer's clone — so `worktree: auto`
// (the IR default, which every fixture here inherits) makes `git worktree add`
// register the run's worktree in the REAL repository. The checkout itself
// lives under the store's t.TempDir() and is deleted when the test returns;
// the registration is not, and nothing ever comes back for it (#870: 1 773
// dead entries / 2.5 GB measured after two days of agent work, 27 more per
// `go test ./e2e/`).
//
// A caller that genuinely needs a particular workspace still passes
// runtime.WithWorkDir: options apply in order, so an explicit one wins over
// the scratch repository below.
func newEngine(t *testing.T, wf *ir.Workflow, s store.RunStore, exec runtime.NodeExecutor, opts ...runtime.EngineOption) *runtime.Engine {
	t.Helper()
	full := append([]runtime.EngineOption{runtime.WithWorkDir(scratchRepo(t))}, opts...)
	return runtime.New(wf, s, exec, full...)
}

// scratchRepos memoises one repository per test. A test that runs then
// RESUMES builds two engines over the same run, and a second repository would
// re-anchor it on a tree it never executed against.
var scratchRepos sync.Map // *testing.T -> string

func scratchRepo(t *testing.T) string {
	t.Helper()
	if dir, ok := scratchRepos.Load(t); ok {
		return dir.(string)
	}
	dir := gittest.SourceRepo(t)
	scratchRepos.Store(t, dir)
	t.Cleanup(func() { scratchRepos.Delete(t) })
	return dir
}
