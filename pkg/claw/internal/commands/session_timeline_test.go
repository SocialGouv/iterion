package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSession(t *testing.T, dir, id string, s timelineSession) {
	t.Helper()
	if s.ID == "" {
		s.ID = id
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestTimeline_OrdersEventsChronologically(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	writeSession(t, dir, "abc", timelineSession{
		ID:        "abc",
		CreatedAt: start,
		UpdatedAt: start.Add(10 * time.Second),
		PromptHistory: []timelinePromptEntry{
			{TimestampMs: start.Add(1 * time.Second).UnixMilli(), Text: "first prompt"},
			{TimestampMs: start.Add(5 * time.Second).UnixMilli(), Text: "second prompt"},
		},
		Messages: []timelineMessage{
			{Role: "user", Content: []timelineContentBlock{{Type: "text", Text: "first prompt"}}},
			{Role: "assistant", Content: []timelineContentBlock{
				{Type: "text", Text: "thinking..."},
				{Type: "tool_use", Name: "read_file"},
			}},
			{Role: "user", Content: []timelineContentBlock{
				{Type: "tool_result", Text: "file contents"},
			}},
		},
	})

	s, err := loadTimelineSession(dir, "abc")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var buf bytes.Buffer
	renderTimeline(&buf, s)
	out := buf.String()

	// First the session-created marker, then the two prompts.
	createdIdx := strings.Index(out, "session created")
	firstIdx := strings.Index(out, "first prompt")
	secondIdx := strings.Index(out, "second prompt")
	if createdIdx == -1 || firstIdx == -1 || secondIdx == -1 {
		t.Fatalf("missing expected markers in output:\n%s", out)
	}
	if !(createdIdx < firstIdx && firstIdx < secondIdx) {
		t.Errorf("events out of order: created=%d first=%d second=%d\n%s", createdIdx, firstIdx, secondIdx, out)
	}
	if !strings.Contains(out, "🔧") {
		t.Errorf("expected tool emoji in output:\n%s", out)
	}
	if !strings.Contains(out, "✅") {
		t.Errorf("expected tool result emoji in output:\n%s", out)
	}
}

func TestTimeline_TruncatesLongMessages(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	long := strings.Repeat("x", 200)
	writeSession(t, dir, "abc", timelineSession{
		ID:        "abc",
		CreatedAt: start,
		UpdatedAt: start.Add(time.Second),
		PromptHistory: []timelinePromptEntry{
			{TimestampMs: start.UnixMilli(), Text: long},
		},
	})
	s, err := loadTimelineSession(dir, "abc")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var buf bytes.Buffer
	renderTimeline(&buf, s)
	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Errorf("expected ellipsis in truncated output:\n%s", out)
	}
	// 80-char body + ellipsis. The full 200x must not be present.
	if strings.Contains(out, long) {
		t.Errorf("full long string leaked into output:\n%s", out)
	}
}

func TestLineage_BuildsForkTree(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)

	writeSession(t, dir, "root", timelineSession{ID: "root", CreatedAt: now, UpdatedAt: now})
	writeSession(t, dir, "child-a", timelineSession{
		ID:        "child-a",
		CreatedAt: now,
		UpdatedAt: now,
		Fork:      &timelineFork{ParentSessionID: "root", BranchName: "feat/a"},
	})
	writeSession(t, dir, "child-b", timelineSession{
		ID:        "child-b",
		CreatedAt: now,
		UpdatedAt: now,
		Fork:      &timelineFork{ParentSessionID: "root"},
	})
	writeSession(t, dir, "grandchild", timelineSession{
		ID:        "grandchild",
		CreatedAt: now,
		UpdatedAt: now,
		Fork:      &timelineFork{ParentSessionID: "child-a", BranchName: "feat/a-deep"},
	})

	root, err := buildLineage(dir, "root")
	if err != nil {
		t.Fatalf("buildLineage: %v", err)
	}
	if root.ID != "root" {
		t.Errorf("expected root id, got %q", root.ID)
	}
	if len(root.Children) != 2 {
		t.Fatalf("expected 2 direct children of root, got %d", len(root.Children))
	}
	// Children must be sorted alphabetically by ID.
	if root.Children[0].ID != "child-a" || root.Children[1].ID != "child-b" {
		t.Errorf("children not sorted: %v %v", root.Children[0].ID, root.Children[1].ID)
	}
	if len(root.Children[0].Children) != 1 || root.Children[0].Children[0].ID != "grandchild" {
		t.Errorf("expected child-a → grandchild, got %+v", root.Children[0].Children)
	}
	if root.Children[0].Branch != "feat/a" {
		t.Errorf("expected branch label feat/a, got %q", root.Children[0].Branch)
	}

	var buf bytes.Buffer
	renderLineage(&buf, root)
	out := buf.String()
	if !strings.Contains(out, "child-a") || !strings.Contains(out, "grandchild") {
		t.Errorf("lineage render missing nodes:\n%s", out)
	}
	if !strings.Contains(out, "└─") || !strings.Contains(out, "├─") {
		t.Errorf("expected ASCII tree connectors:\n%s", out)
	}
}

func TestLineage_NoForksShowsSingleNode(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC)
	writeSession(t, dir, "alone", timelineSession{ID: "alone", CreatedAt: now, UpdatedAt: now})

	root, err := buildLineage(dir, "alone")
	if err != nil {
		t.Fatalf("buildLineage: %v", err)
	}
	if len(root.Children) != 0 {
		t.Errorf("expected no children, got %d", len(root.Children))
	}
	var buf bytes.Buffer
	renderLineage(&buf, root)
	if !strings.Contains(buf.String(), "no forks") {
		t.Errorf("expected 'no forks' message, got:\n%s", buf.String())
	}
}

func TestLineage_RejectsMissingSession(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildLineage(dir, "missing"); err == nil {
		t.Fatal("expected error for missing session")
	}
}

// Stub provider so we can exercise resolveSessionDir without touching runtime.
type fakeDirProvider struct{ dir string }

func (f fakeDirProvider) SessionDir() string { return f.dir }

func TestRegisterSessionTimelineCommands_Registers(t *testing.T) {
	r := NewRegistry()
	RegisterSessionTimelineCommands(r)

	dir := t.TempDir()
	writeSession(t, dir, "x", timelineSession{
		ID:        "x",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})
	provider := fakeDirProvider{dir: dir}
	if _, err := r.Execute("/timeline x", provider); err != nil {
		t.Errorf("/timeline failed: %v", err)
	}
	if _, err := r.Execute("/lineage x", provider); err != nil {
		t.Errorf("/lineage failed: %v", err)
	}
}
