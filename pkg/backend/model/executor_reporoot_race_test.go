package model

import (
	"sync"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The neighbor of the race #2897 closed: repoRoot carries the same
// pushed-field/live-reader shape as workDir — the engine pushes it at run
// start and buildTask reads it on every node — with no reader live per tool
// call today, so the run-start push never actually overlaps a read. The
// field is atomic all the same, so the class stays closed by construction:
// a caller that ever pushed mid-run hammers this exact pair instead of
// racing a plain string. The witness hammers RepoRoot reads while
// SetRepoRoot repoints the root, in both orders; against the pre-fix plain
// field the race detector reddens here, against the atomic field every read
// observes one of the pushed roots, never a torn or empty one.
//
// Mutation: read and write a plain string field in SetRepoRoot/RepoRoot —
// the witness fails on its own DATA RACE report.
func TestSetRepoRootConcurrentWithRepoRootReads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// seedPulses is the number of SetRepoRoot calls the writer makes
		// before the reader joins, so the pair is exercised in both
		// orders: writes-first and reads-first.
		seedPulses int
	}{
		{name: "writer moves first", seedPulses: 32},
		{name: "reader resolves first", seedPulses: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rootOne := t.TempDir()
			rootTwo := t.TempDir()

			e := &ClawExecutor{logger: iterlog.Nop()}
			e.SetRepoRoot(rootOne)

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
						e.SetRepoRoot(rootTwo)
					} else {
						e.SetRepoRoot(rootOne)
					}
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				// The buildTask reader's shape: one live read per node.
				for i := 0; i < rounds; i++ {
					observe(e.RepoRoot())
				}
			}()
			close(start)
			wg.Wait()

			mu.Lock()
			defer mu.Unlock()
			for i, v := range seen {
				if v != rootOne && v != rootTwo {
					t.Fatalf("read %d observed repo root %q; want %q or %q — a torn or empty read", i, v, rootOne, rootTwo)
				}
			}
			if len(seen) != rounds {
				t.Fatalf("observed %d reads; want %d", len(seen), rounds)
			}
		})
	}
}

// RepoRoot reports the last pushed value once the writes settle and a zero
// executor reads empty: the engine's run-start push replaces whatever a
// host option wrote, whichever wrote last wins.
func TestRepoRootLastWriteWins(t *testing.T) {
	t.Parallel()

	rootOne := t.TempDir()
	rootTwo := t.TempDir()

	e := &ClawExecutor{logger: iterlog.Nop()}
	if got := e.RepoRoot(); got != "" {
		t.Fatalf("zero executor reported %q; want empty", got)
	}
	e.SetRepoRoot(rootOne)
	if got := e.RepoRoot(); got != rootOne {
		t.Fatalf("SetRepoRoot push left %q; want %q", got, rootOne)
	}
	e.SetRepoRoot(rootTwo)
	if got := e.RepoRoot(); got != rootTwo {
		t.Fatalf("second SetRepoRoot push left %q; want %q", got, rootTwo)
	}
}
