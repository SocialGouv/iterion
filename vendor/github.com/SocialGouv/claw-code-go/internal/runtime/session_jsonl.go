package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session JSONL format constants.
const (
	SessionVersion   = 1
	RotateAfterBytes = 256 * 1024 // 256 KB
	MaxRotatedFiles  = 3
)

// metaRecord holds session metadata (first line of JSONL).
type metaRecord struct {
	Type          string       `json:"type"`
	Version       int          `json:"version"`
	SessionID     string       `json:"session_id"`
	CreatedAtMs   int64        `json:"created_at_ms"`
	UpdatedAtMs   int64        `json:"updated_at_ms"`
	WorkspaceRoot string       `json:"workspace_root,omitempty"`
	Fork          *SessionFork `json:"fork,omitempty"`
}

// messageRecord holds a single conversation message.
type messageRecord struct {
	Type    string      `json:"type"`
	Message api.Message `json:"message"`
}

// compactionRecord holds compaction state.
type compactionRecord struct {
	Type                string `json:"type"`
	Count               int    `json:"count"`
	RemovedMessageCount int    `json:"removed_message_count"`
	Summary             string `json:"summary"`
}

// promptHistoryRecord holds a user prompt entry.
type promptHistoryRecord struct {
	Type        string `json:"type"`
	TimestampMs int64  `json:"timestamp_ms"`
	Text        string `json:"text"`
}

// RenderJSONLSnapshot renders a full session as a JSONL snapshot string.
// Each line is a self-contained JSON record.
func RenderJSONLSnapshot(s *Session) (string, error) {
	var sb strings.Builder

	// Meta record (first line).
	meta := metaRecord{
		Type:        "session_meta",
		Version:     SessionVersion,
		SessionID:   s.ID,
		CreatedAtMs: s.CreatedAt.UnixMilli(),
		UpdatedAtMs: s.UpdatedAt.UnixMilli(),
		Fork:        s.Fork,
	}
	if err := writeJSONLine(&sb, meta); err != nil {
		return "", NewSessionJSONError("render_meta", err)
	}

	// Compaction record (if any).
	if s.CompactionSummary != "" {
		comp := compactionRecord{
			Type:                "compaction",
			Count:               s.CompactionCount,
			RemovedMessageCount: 0, // not tracked in current Session struct
			Summary:             s.CompactionSummary,
		}
		if err := writeJSONLine(&sb, comp); err != nil {
			return "", NewSessionJSONError("render_compaction", err)
		}
	}

	// Prompt history records.
	for _, ph := range s.PromptHistory {
		rec := promptHistoryRecord{
			Type:        "prompt_history",
			TimestampMs: ph.TimestampMs,
			Text:        ph.Text,
		}
		if err := writeJSONLine(&sb, rec); err != nil {
			return "", NewSessionJSONError("render_prompt_history", err)
		}
	}

	// Message records.
	for _, msg := range s.Messages {
		rec := messageRecord{
			Type:    "message",
			Message: msg,
		}
		if err := writeJSONLine(&sb, rec); err != nil {
			return "", NewSessionJSONError("render_message", err)
		}
	}

	return sb.String(), nil
}

// ParseJSONL parses a JSONL session file, reconstructing a Session.
// Uses first-byte detection: '{' on first line = check if legacy JSON, else JSONL lines.
// Malformed lines are skipped with best-effort recovery.
func ParseJSONL(data []byte) (*Session, error) {
	content := strings.TrimSpace(string(data))
	if content == "" {
		return NewSession(), nil
	}

	// First-byte detection for backward compatibility.
	// JSONL sessions always start with a session_meta record on the first line.
	// Legacy JSON files are a single JSON object with an "id" field at top level.
	// We check if the first line looks like a JSONL session_meta record.
	if content[0] == '{' {
		firstLine := strings.SplitN(content, "\n", 2)[0]
		// JSONL files start with {"type":"session_meta",...}
		// If the first line does NOT look like a session_meta record, try legacy JSON.
		if !strings.Contains(firstLine, `"type":"session_meta"`) && !strings.Contains(firstLine, `"type": "session_meta"`) {
			var s Session
			if err := json.Unmarshal(data, &s); err == nil && s.ID != "" {
				return &s, nil
			}
			// If legacy JSON parse fails, fall through to JSONL parser.
		}
	}

	s := NewSession()
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB max line

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Peek at type field.
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &peek); err != nil {
			// Skip malformed lines (corrupt line recovery).
			continue
		}

		switch peek.Type {
		case "session_meta":
			var rec metaRecord
			if json.Unmarshal([]byte(line), &rec) == nil {
				s.ID = rec.SessionID
				s.CreatedAt = time.UnixMilli(rec.CreatedAtMs)
				s.UpdatedAt = time.UnixMilli(rec.UpdatedAtMs)
				s.Fork = rec.Fork
			}

		case "message":
			var rec messageRecord
			if json.Unmarshal([]byte(line), &rec) == nil {
				s.Messages = append(s.Messages, rec.Message)
			}

		case "compaction":
			var rec compactionRecord
			if json.Unmarshal([]byte(line), &rec) == nil {
				s.CompactionSummary = rec.Summary
				s.CompactionCount = rec.Count
			}

		case "prompt_history":
			var rec promptHistoryRecord
			if json.Unmarshal([]byte(line), &rec) == nil {
				s.PromptHistory = append(s.PromptHistory, PromptHistoryEntry{
					TimestampMs: rec.TimestampMs,
					Text:        rec.Text,
				})
			}

		default:
			// Unknown record type — skip for forward compatibility.
		}
	}

	return s, scanner.Err()
}

// AppendMessageRecord appends a single message record to a JSONL file.
// Creates the file if it doesn't exist.
func AppendMessageRecord(path string, msg api.Message) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return NewSessionIOError("open", path, err)
	}
	defer f.Close()

	rec := messageRecord{
		Type:    "message",
		Message: msg,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return NewSessionJSONError("marshal_message", err)
	}
	if _, err := fmt.Fprintf(f, "%s\n", data); err != nil {
		return NewSessionIOError("append", path, err)
	}
	return nil
}

// RotateSessionFileIfNeeded rotates the session file if it exceeds RotateAfterBytes.
// The rotated file is renamed to path.rot-{timestamp}.jsonl.
// Returns true if rotation occurred.
func RotateSessionFileIfNeeded(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, NewSessionIOError("stat", path, err)
	}

	if info.Size() < int64(RotateAfterBytes) {
		return false, nil
	}

	// Rotate: rename current file.
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	rotatedPath := fmt.Sprintf("%s.rot-%d%s", base, time.Now().UnixMilli(), ext)

	if err := os.Rename(path, rotatedPath); err != nil {
		return false, NewSessionIOError("rotate", path, err)
	}

	return true, nil
}

// CleanupRotatedLogs removes old rotated session files, keeping at most maxKeep.
// Files are sorted by name (which includes timestamp) and oldest are removed first.
func CleanupRotatedLogs(path string, maxKeep int) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + ".rot-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return NewSessionIOError("list_rotated", dir, err)
	}

	var rotated []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), prefix) {
			rotated = append(rotated, filepath.Join(dir, e.Name()))
		}
	}

	if len(rotated) <= maxKeep {
		return nil
	}

	sort.Strings(rotated)
	// Remove oldest (keep last maxKeep).
	toRemove := rotated[:len(rotated)-maxKeep]
	for _, p := range toRemove {
		if err := os.Remove(p); err != nil {
			return NewSessionIOError("cleanup_rotated", p, err)
		}
	}

	return nil
}

// SaveSessionJSONL persists a session to disk in JSONL format using atomic write.
func SaveSessionJSONL(dir string, s *Session) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return NewSessionIOError("create_dir", dir, err)
	}

	s.UpdatedAt = time.Now()

	snapshot, err := RenderJSONLSnapshot(s)
	if err != nil {
		return err // already a SessionError from RenderJSONLSnapshot
	}

	path := filepath.Join(dir, s.ID+".jsonl")

	// Rotate if current file exceeds threshold.
	if _, err := RotateSessionFileIfNeeded(path); err != nil {
		return err // already a SessionError from RotateSessionFileIfNeeded
	}

	// Atomic write: write to temp file, then rename.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(snapshot), 0o644); err != nil {
		return NewSessionIOError("write", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // cleanup on failure
		return NewSessionIOError("rename", path, err)
	}

	// Cleanup old rotated files.
	if err := CleanupRotatedLogs(path, MaxRotatedFiles); err != nil {
		return err // already a SessionError from CleanupRotatedLogs
	}

	return nil
}

// LoadSessionAuto loads a session from disk, trying JSONL first, then legacy JSON.
func LoadSessionAuto(dir, id string) (*Session, error) {
	// Try JSONL first.
	jsonlPath := filepath.Join(dir, id+".jsonl")
	if data, err := os.ReadFile(jsonlPath); err == nil {
		return ParseJSONL(data)
	}

	// Fall back to legacy JSON.
	jsonPath := filepath.Join(dir, id+".json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, NewSessionIOError("read", jsonPath, err)
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, NewSessionJSONError("unmarshal", err)
	}

	return &s, nil
}

func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}
