package runview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestPreEngineRefusalViewsPreservePendingState(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	r := &store.Run{ID: "pending", Status: store.RunStatusQueued, Error: "repair MCP", PreEngineRefusal: &store.PreEngineRefusal{Phase: "mcp", Code: store.FailureExecutionFailed, Message: "repair MCP", QueuedAt: &at}}
	summary := summarizeRun(r, false)
	header := headerFromRun(r)
	for _, view := range []any{summary, header} {
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"pre_engine_refusal":{"phase":"mcp"`) || !strings.Contains(string(raw), `"status":"queued"`) {
			t.Fatalf("pending refusal lost: %s", raw)
		}
	}
	summary.PreEngineRefusal.Phase = "changed"
	*summary.PreEngineRefusal.QueuedAt = at.Add(time.Hour)
	if header.PreEngineRefusal.Phase != "mcp" || r.PreEngineRefusal.Phase != "mcp" || !header.PreEngineRefusal.QueuedAt.Equal(r.PreEngineRefusal.QueuedAt.UTC()) {
		t.Fatal("summary aliases stored refusal or header")
	}
	header.PreEngineRefusal.Message = "changed"
	if r.PreEngineRefusal.Message != "repair MCP" {
		t.Fatal("header aliases stored refusal")
	}
	r.PreEngineRefusal = nil
	if summarizeRun(r, false).PreEngineRefusal != nil || headerFromRun(r).PreEngineRefusal != nil {
		t.Fatal("cleared refusal retained by view")
	}
}

func TestPreEngineSnapshotIsolation(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	doc := &store.Run{ID: "run", Status: store.RunStatusQueued, PreEngineRefusal: &store.PreEngineRefusal{Phase: "mcp", Message: "repair MCP", QueuedAt: &at}}
	b := NewSnapshotBuilder(doc)
	first := b.Snapshot()
	second := b.Snapshot()
	first.Run.PreEngineRefusal.Message = "poisoned observer"
	*first.Run.PreEngineRefusal.QueuedAt = at.Add(time.Hour)
	third := b.Snapshot()
	if second.Run.PreEngineRefusal.Message != "repair MCP" || third.Run.PreEngineRefusal.Message != "repair MCP" || !third.Run.PreEngineRefusal.QueuedAt.Equal(at) {
		t.Fatalf("SNAPSHOT_ALIAS: second=%+v third=%+v stored=%+v", second.Run.PreEngineRefusal, third.Run.PreEngineRefusal, doc.PreEngineRefusal)
	}
	doc.PreEngineRefusal = nil
	b.SetRun(doc)
	if b.Snapshot().Run.PreEngineRefusal != nil {
		t.Fatal("clear lost")
	}
}
