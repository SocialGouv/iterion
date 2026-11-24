package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent WriteTurn for the same node (distinct iterations) must not
// lose index entries: the per-node index.json read-modify-write is
// guarded by s.mu. Without the lock, the parallel-branch turn hooks race
// the RMW and clobber each other (last-writer-wins on the whole file),
// so the final index is missing iterations.
func TestWriteTurnConcurrentIndexNoLostUpdates(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const node = "agent"
	const N = 40

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := s.WriteTurn(ctx, &TurnCheckpoint{RunID: "run-x", NodeID: node, LoopIter: i, TurnIndex: 0}); err != nil {
				t.Errorf("WriteTurn iter %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(s.runDir("run-x"), "turns", node, "index.json"))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var idx turnNodeIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if len(idx.Iterations) != N {
		t.Fatalf("index has %d iterations; want %d (lost updates under concurrent RMW)", len(idx.Iterations), N)
	}
}
