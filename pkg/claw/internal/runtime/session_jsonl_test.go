package runtime

import (
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderJSONLSnapshotRoundtrip(t *testing.T) {
	s := NewSession()
	s.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Hello"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "Hi there!"}}},
	}
	s.CompactionSummary = "previous context summary"
	s.CompactionCount = 2
	s.PromptHistory = []PromptHistoryEntry{
		{TimestampMs: 1000, Text: "first prompt"},
		{TimestampMs: 2000, Text: "second prompt"},
	}

	snapshot, err := RenderJSONLSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}

	// Each line should be valid JSON.
	lines := strings.Split(strings.TrimSpace(snapshot), "\n")
	if len(lines) < 6 { // meta + compaction + 2 prompt_history + 2 messages = 6 lines minimum
		t.Fatalf("expected at least 6 lines, got %d", len(lines))
	}
	for i, line := range lines {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}

	// Parse back.
	parsed, err := ParseJSONL([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}

	if parsed.ID != s.ID {
		t.Errorf("ID: got %q, want %q", parsed.ID, s.ID)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("Messages: got %d, want 2", len(parsed.Messages))
	}
	if parsed.Messages[0].Content[0].Text != "Hello" {
		t.Errorf("Message[0] text: got %q", parsed.Messages[0].Content[0].Text)
	}
	if parsed.CompactionSummary != "previous context summary" {
		t.Errorf("CompactionSummary: got %q", parsed.CompactionSummary)
	}
	if parsed.CompactionCount != 2 {
		t.Errorf("CompactionCount: got %d", parsed.CompactionCount)
	}
	if len(parsed.PromptHistory) != 2 {
		t.Fatalf("PromptHistory: got %d, want 2", len(parsed.PromptHistory))
	}
	if parsed.PromptHistory[0].Text != "first prompt" {
		t.Errorf("PromptHistory[0].Text: got %q", parsed.PromptHistory[0].Text)
	}
	if parsed.PromptHistory[1].TimestampMs != 2000 {
		t.Errorf("PromptHistory[1].TimestampMs: got %d", parsed.PromptHistory[1].TimestampMs)
	}
}

func TestParseJSONLLegacyJSON(t *testing.T) {
	// Legacy JSON format (single JSON object, not JSONL).
	s := &Session{
		ID:        "session-legacy",
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "old format"}}}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseJSONL(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "session-legacy" {
		t.Errorf("ID: got %q", parsed.ID)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("Messages: got %d", len(parsed.Messages))
	}
}

func TestParseJSONLCorruptLineRecovery(t *testing.T) {
	// JSONL with one corrupt line — should skip it gracefully.
	lines := []string{
		`{"type":"session_meta","version":1,"session_id":"s1","created_at_ms":1000,"updated_at_ms":2000}`,
		`this is not json`,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
	}
	data := []byte(strings.Join(lines, "\n"))

	parsed, err := ParseJSONL(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "s1" {
		t.Errorf("ID: got %q", parsed.ID)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("Messages: got %d, want 1 (corrupt line skipped)", len(parsed.Messages))
	}
}

func TestParseJSONLEmpty(t *testing.T) {
	parsed, err := ParseJSONL([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Messages) != 0 {
		t.Errorf("expected no messages for empty input")
	}
}

func TestAppendMessageRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	msg := api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "appended"}}}
	if err := AppendMessageRecord(path, msg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	// Append another.
	msg2 := api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "response"}}}
	if err := AppendMessageRecord(path, msg2); err != nil {
		t.Fatal(err)
	}

	data, _ = os.ReadFile(path)
	lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines after second append, got %d", len(lines))
	}
}

func TestRotateSessionFileIfNeeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Small file — no rotation.
	if err := os.WriteFile(path, make([]byte, 100*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateSessionFileIfNeeded(path)
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Error("100KB file should not trigger rotation")
	}

	// Large file — should rotate.
	if err := os.WriteFile(path, make([]byte, 300*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	rotated, err = RotateSessionFileIfNeeded(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated {
		t.Error("300KB file should trigger rotation")
	}

	// Original file should be gone.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original file should be removed after rotation")
	}
}

func TestRotateSessionFileNonexistent(t *testing.T) {
	rotated, err := RotateSessionFileIfNeeded("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if rotated {
		t.Error("nonexistent file should not trigger rotation")
	}
}

func TestCleanupRotatedLogs(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "session.jsonl")

	// Create 5 rotated files.
	for i := 0; i < 5; i++ {
		name := filepath.Join(dir, "session.rot-"+strings.Repeat("0", i+1)+".jsonl")
		if err := os.WriteFile(name, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := CleanupRotatedLogs(basePath, MaxRotatedFiles); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	var rotatedCount int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".rot-") {
			rotatedCount++
		}
	}
	if rotatedCount != MaxRotatedFiles {
		t.Errorf("expected %d rotated files, got %d", MaxRotatedFiles, rotatedCount)
	}
}

func TestSaveAndLoadSessionJSONL(t *testing.T) {
	dir := t.TempDir()
	s := NewSession()
	s.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "test"}}},
	}

	if err := SaveSessionJSONL(dir, s); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSessionAuto(dir, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Errorf("ID: got %q, want %q", loaded.ID, s.ID)
	}
	if len(loaded.Messages) != 1 {
		t.Fatalf("Messages: got %d, want 1", len(loaded.Messages))
	}
}

func TestLoadSessionAutoFallbackJSON(t *testing.T) {
	dir := t.TempDir()
	s := &Session{
		ID:        "session-old",
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "legacy"}}}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Save as legacy JSON.
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(filepath.Join(dir, "session-old.json"), data, 0o644)

	loaded, err := LoadSessionAuto(dir, "session-old")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "session-old" {
		t.Errorf("ID: got %q", loaded.ID)
	}
}

func TestRenderJSONLSnapshotWithFork(t *testing.T) {
	s := NewSession()
	s.Fork = &SessionFork{
		ParentSessionID: "session-parent-123",
		BranchName:      "feature-x",
	}
	s.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "forked"}}},
	}

	snapshot, err := RenderJSONLSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}

	// Verify fork appears in the meta line.
	lines := strings.Split(strings.TrimSpace(snapshot), "\n")
	if !strings.Contains(lines[0], `"parent_session_id":"session-parent-123"`) &&
		!strings.Contains(lines[0], `"parent_session_id": "session-parent-123"`) {
		t.Errorf("meta line should contain fork parent_session_id, got: %s", lines[0])
	}

	// Round-trip.
	parsed, err := ParseJSONL([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Fork == nil {
		t.Fatal("Fork should not be nil after round-trip")
	}
	if parsed.Fork.ParentSessionID != "session-parent-123" {
		t.Errorf("Fork.ParentSessionID: got %q", parsed.Fork.ParentSessionID)
	}
	if parsed.Fork.BranchName != "feature-x" {
		t.Errorf("Fork.BranchName: got %q", parsed.Fork.BranchName)
	}
}

func TestRenderJSONLSnapshotWithPromptHistory(t *testing.T) {
	s := NewSession()
	s.PromptHistory = []PromptHistoryEntry{
		{TimestampMs: 100, Text: "what is Go?"},
		{TimestampMs: 200, Text: "show me an example"},
		{TimestampMs: 300, Text: "thanks"},
	}

	snapshot, err := RenderJSONLSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(snapshot), "\n")
	// meta + 3 prompt_history = 4 lines (no compaction, no messages).
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}

	// Verify prompt_history type tags.
	for i := 1; i <= 3; i++ {
		if !strings.Contains(lines[i], `"type":"prompt_history"`) &&
			!strings.Contains(lines[i], `"type": "prompt_history"`) {
			t.Errorf("line %d should be prompt_history, got: %s", i, lines[i])
		}
	}

	// Round-trip.
	parsed, err := ParseJSONL([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.PromptHistory) != 3 {
		t.Fatalf("PromptHistory: got %d, want 3", len(parsed.PromptHistory))
	}
	if parsed.PromptHistory[2].Text != "thanks" {
		t.Errorf("PromptHistory[2].Text: got %q", parsed.PromptHistory[2].Text)
	}
	if parsed.PromptHistory[0].TimestampMs != 100 {
		t.Errorf("PromptHistory[0].TimestampMs: got %d", parsed.PromptHistory[0].TimestampMs)
	}
}

func requireSessionError(t *testing.T, err error, wantKind SessionErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var sessErr *SessionError
	if !errors.As(err, &sessErr) {
		t.Fatalf("expected *SessionError, got %T: %v", err, err)
	}
	if sessErr.Kind != wantKind {
		t.Errorf("Kind = %v, want %v", sessErr.Kind, wantKind)
	}
}

func TestSessionJSONLReturnsStructuredErrors(t *testing.T) {
	t.Run("AppendMessageRecord on open failure", func(t *testing.T) {
		err := AppendMessageRecord("/nonexistent/dir/session.jsonl", api.Message{
			Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}},
		})
		requireSessionError(t, err, SessionErrIO)
	})

	t.Run("LoadSessionAuto on missing file", func(t *testing.T) {
		_, err := LoadSessionAuto(t.TempDir(), "nonexistent-session")
		requireSessionError(t, err, SessionErrIO)
	})

	t.Run("LoadSessionAuto on invalid JSON", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "bad-session.json"), []byte("{not json"), 0o644)
		_, err := LoadSessionAuto(dir, "bad-session")
		requireSessionError(t, err, SessionErrJSON)
	})

	t.Run("SaveSessionJSONL on bad dir", func(t *testing.T) {
		err := SaveSessionJSONL("/dev/null/impossible", NewSession())
		requireSessionError(t, err, SessionErrIO)
	})
}

func TestSaveSessionJSONLRotation(t *testing.T) {
	dir := t.TempDir()
	s := NewSession()
	s.Messages = []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
	}

	// Create an oversized existing file to trigger rotation.
	path := filepath.Join(dir, s.ID+".jsonl")
	if err := os.WriteFile(path, make([]byte, 300*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// SaveSessionJSONL should rotate the old file and write a new one.
	if err := SaveSessionJSONL(dir, s); err != nil {
		t.Fatal(err)
	}

	// The new file should exist.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("new session file should exist: %v", err)
	}

	// There should be exactly one rotated file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var rotatedCount int
	for _, e := range entries {
		if strings.Contains(e.Name(), ".rot-") {
			rotatedCount++
		}
	}
	if rotatedCount != 1 {
		t.Errorf("expected 1 rotated file, got %d", rotatedCount)
	}

	// Verify the new file is a valid JSONL session.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ParseJSONL(data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Errorf("loaded ID: got %q, want %q", loaded.ID, s.ID)
	}
}

func TestSessionJSONLGoldenFixture(t *testing.T) {
	goldenPath := "../../testdata/golden/session_jsonl_snapshot.jsonl"
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file: %v", err)
	}

	// Parse the golden fixture
	parsed, err := ParseJSONL(golden)
	if err != nil {
		t.Fatalf("parse golden fixture: %v", err)
	}

	// Verify expected structure
	if parsed.ID != "test-session" {
		t.Errorf("session ID: got %q, want %q", parsed.ID, "test-session")
	}
	if parsed.CompactionSummary != "test summary" {
		t.Errorf("compaction summary: got %q, want %q", parsed.CompactionSummary, "test summary")
	}
	if parsed.CompactionCount != 1 {
		t.Errorf("compaction count: got %d, want 1", parsed.CompactionCount)
	}
	if len(parsed.PromptHistory) != 1 {
		t.Fatalf("prompt history: got %d entries, want 1", len(parsed.PromptHistory))
	}
	if parsed.PromptHistory[0].Text != "what is the weather" {
		t.Errorf("prompt history text: got %q", parsed.PromptHistory[0].Text)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("messages: got %d, want 2", len(parsed.Messages))
	}
	if parsed.Messages[0].Content[0].Text != "Hello" {
		t.Errorf("message[0] text: got %q", parsed.Messages[0].Content[0].Text)
	}
	if parsed.Messages[1].Content[0].Text != "Hi there!" {
		t.Errorf("message[1] text: got %q", parsed.Messages[1].Content[0].Text)
	}
}
