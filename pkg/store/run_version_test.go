package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

func TestSaveRunLegacyVersion(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "legacy", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.runJSONPath("legacy"), []byte(`{"id":"legacy","workflow_name":"wf","status":"running"}`), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := s.LoadRun(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	stale := *run
	run.Name = "legacy renamed"
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if run.CASVersion != stale.CASVersion+1 {
		t.Fatalf("version=%d, previous=%d", run.CASVersion, stale.CASVersion)
	}
	if err := s.SaveRun(ctx, &stale); !errors.Is(err, ErrRunConflict) {
		t.Fatalf("stale legacy save=%v", err)
	}
}

func TestSaveRunCASAcrossStoreInstances(t *testing.T) {
	ctx := context.Background()
	a := tmpStore(t)
	b, err := New(a.Root())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateRun(ctx, "concurrent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	first, err := a.LoadRun(ctx, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.LoadRun(ctx, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	first.Name, second.Name = "first", "second"
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	var wg sync.WaitGroup
	for i, st := range []*FilesystemRunStore{a, b} {
		run := []*Run{first, second}[i]
		wg.Add(1)
		go func() { defer wg.Done(); <-start; outcomes <- st.SaveRun(ctx, run) }()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	successes, conflicts := 0, 0
	for err := range outcomes {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrRunConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}
