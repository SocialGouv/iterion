package store

import (
	"context"
	"testing"
)

func TestCreateQueuedRunPersistsQueuedDoc(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	run, err := s.CreateQueuedRun(ctx, "q1", "shorts_series", "/bots/main.bot", "main", map[string]any{"character": "boudicca"})
	if err != nil {
		t.Fatalf("CreateQueuedRun: %v", err)
	}
	if run.Status != RunStatusQueued {
		t.Errorf("status = %q, want queued", run.Status)
	}
	if run.QueuedAt == nil {
		t.Error("QueuedAt must be stamped on a queued run")
	}
	if run.FilePath != "/bots/main.bot" || run.BotID != "main" {
		t.Errorf("file/bot = %q/%q, want the launch identity persisted", run.FilePath, run.BotID)
	}

	loaded, err := s.LoadRun(ctx, "q1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if loaded.Status != RunStatusQueued || loaded.Inputs["character"] != "boudicca" {
		t.Errorf("loaded = %+v, want queued with inputs", loaded)
	}

	// No-clobber, like CreateRun.
	if _, err := s.CreateQueuedRun(ctx, "q1", "wf", "/x.bot", "", nil); err == nil {
		t.Error("CreateQueuedRun must reject a duplicate run id")
	}
}

func TestAsQueuedRunCreator(t *testing.T) {
	s := tmpStore(t)
	if AsQueuedRunCreator(s) == nil {
		t.Error("filesystem store must satisfy QueuedRunCreator")
	}
	if AsQueuedRunCreator(nil) != nil {
		t.Error("AsQueuedRunCreator(nil) must be nil")
	}
}
