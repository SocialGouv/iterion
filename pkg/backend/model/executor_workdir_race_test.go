package model

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// mustCanonicalRoot resolves root the way ReadScope.AddExtraRoot stores
// it (absolute, symlinks evaluated), so assertions meet the scope on its
// own side of a symlinked TMPDIR — /var→/private/var on macOS, a linked
// scratch dir in a sandbox — instead of red on the spelling.
func mustCanonicalRoot(t *testing.T, root string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize %q: %v", root, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatalf("abs %q: %v", root, err)
	}
	return abs
}

// The race class #2897 closes: SetWorkDir writes the field the workspace
// provider reads LIVE on every workspace tool call, so a run whose engine
// pushes a per-run worktree while a tool call resolves its workspace raced
// on a plain string field. The witness wires the scope the way runview
// does — NewReadScopeWithProvider(executor.WorkDir) — and hammers provider
// reads while SetWorkDir repoints the workspace, exactly the pair the
// detector reported (Read WorkDir ← ReadScope.workspaceLocked vs Write
// SetWorkDir). Against the pre-fix plain field the race detector reddens
// here; against the atomic field every read observes one of the pushed
// directories, never a torn or empty one.
//
// Mutation: read and write a plain string field in SetWorkDir/WorkDir —
// the witness fails on its own DATA RACE report.
func TestSetWorkDirConcurrentWithWorkspaceProviderReads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// seedPulses weights the write/read mix (the writer runs 32 more
		// writes than the reader runs reads), but close(start) releases
		// both goroutines together — the interleaving order is the
		// scheduler's, not the test's. The witness needs no order: the
		// verdict holds either way, every read observing one of the
		// pushed workspaces, the count staying exact, the extra root
		// attaching exactly once.
		seedPulses int
	}{
		{name: "writer moves first", seedPulses: 32},
		{name: "reader resolves first", seedPulses: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirOne := t.TempDir()
			dirTwo := t.TempDir()
			extra := t.TempDir()

			e := &ClawExecutor{logger: iterlog.Nop()}
			e.SetWorkDir(dirOne)
			scope := tool.NewReadScopeWithProvider(e.WorkDir)

			const rounds = 200
			// Observations are fed by a goroutine: guarded by a mutex.
			var mu sync.Mutex
			var seen []string
			observe := func(v string) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, v)
			}

			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < tc.seedPulses+rounds; i++ {
					if i%2 == 0 {
						e.SetWorkDir(dirTwo)
					} else {
						e.SetWorkDir(dirOne)
					}
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				// AddExtraRoot consults the provider on every call under
				// the scope's lock — the same provider invocation the
				// read tools make per resolve.
				for i := 0; i < rounds; i++ {
					observe(e.WorkDir())
					scope.AddExtraRoot(extra)
				}
			}()
			close(start)
			wg.Wait()

			mu.Lock()
			defer mu.Unlock()
			for i, v := range seen {
				if v != dirOne && v != dirTwo {
					t.Fatalf("read %d observed workspace %q; want %q or %q — a torn or empty read", i, v, dirOne, dirTwo)
				}
			}
			if len(seen) != rounds {
				t.Fatalf("observed %d reads; want %d", len(seen), rounds)
			}
			wantRoot := mustCanonicalRoot(t, extra)
			roots := scope.ExtraRoots()
			if len(roots) != 1 || roots[0] != wantRoot {
				t.Fatalf("scope attached %v; want exactly the canonical %q once (idempotent attachment)", roots, wantRoot)
			}
		})
	}
}

// WorkDir reports the last pushed value once the writes settle: the
// construction-time option and the run-start push write the same single
// field, whichever wrote last wins, and a zero executor reads empty.
func TestWorkDirLastWriteWins(t *testing.T) {
	t.Parallel()

	dirOption := t.TempDir()
	dirPushed := t.TempDir()

	e := &ClawExecutor{logger: iterlog.Nop()}
	if got := e.WorkDir(); got != "" {
		t.Fatalf("zero executor reported %q; want empty", got)
	}
	if opt := WithWorkDir(dirOption); opt != nil {
		opt(e)
	}
	if got := e.WorkDir(); got != dirOption {
		t.Fatalf("WithWorkDir option left %q; want %q", got, dirOption)
	}
	e.SetWorkDir(dirPushed)
	if got := e.WorkDir(); got != dirPushed {
		t.Fatalf("SetWorkDir push left %q; want %q", got, dirPushed)
	}
}
