package supervise

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// transcriptPollInterval is how often the tailer re-stats the transcript
// for new bytes. Claude Code flushes asynchronously, so observation is
// near-real-time but not instantaneous.
const transcriptPollInterval = 300 * time.Millisecond

// TranscriptObserver tails a raw Claude Code session transcript
// (~/.claude/projects/<key>/<sessionId>.jsonl) and normalises its
// records into store.Events the Coordinator already reasons over —
// tool_use → tool_called, a failed tool_result → tool_error, and an
// assistant message that ends with text (no pending tool_use) → a
// turn-boundary llm_step_finished. A raw session has no nodes, so the
// supervisor watching it must have an empty Watches set (always armed,
// session-scoped injection).
type TranscriptObserver struct {
	path string
}

// NewTranscriptObserver tails the given transcript file path.
func NewTranscriptObserver(path string) *TranscriptObserver {
	return &TranscriptObserver{path: path}
}

// ObserveRun implements the Observer seam. runID is ignored — the
// transcript path identifies the session.
func (o *TranscriptObserver) ObserveRun(ctx context.Context, _ string) (<-chan *store.Event, func(), error) {
	out := make(chan *store.Event, subscriberBufferSize)
	cctx, cancel := context.WithCancel(ctx)
	go o.tail(cctx, out)
	return out, cancel, nil
}

const subscriberBufferSize = 256

// tail polls the file for appended bytes, splits complete lines, and
// emits synthesized events. A remainder buffer holds a partial trailing
// line until its newline arrives.
func (o *TranscriptObserver) tail(ctx context.Context, out chan<- *store.Event) {
	defer close(out)
	var (
		offset    int64
		remainder []byte
		seq       int64
		seen      = map[string]string{} // uuid -> "1" (dedup set)
		toolNames = map[string]string{} // tool_use_id -> tool name
	)
	emit := func(typ store.EventType, ts time.Time, nodeData map[string]any) bool {
		seq++
		evt := &store.Event{Seq: seq, Type: typ, Timestamp: ts, Data: nodeData}
		select {
		case out <- evt:
			return true
		case <-ctx.Done():
			return false
		}
	}

	ticker := time.NewTicker(transcriptPollInterval)
	defer ticker.Stop()
	for {
		data, newOffset, err := readFrom(o.path, offset)
		if err == nil && len(data) > 0 {
			offset = newOffset
			buf := append(remainder, data...)
			remainder = nil
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					remainder = append(remainder[:0], buf...)
					break
				}
				line := buf[:idx]
				buf = buf[idx+1:]
				if cont := o.handleLine(line, seen, toolNames, emit); !cont {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// readFrom reads the file from offset to EOF, returning the new bytes and
// the new offset. A missing file (session not started yet) is not an
// error — returns empty.
func readFrom(path string, offset int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if info.Size() < offset {
		// File shrank/rotated (new session into same path) — restart.
		offset = 0
	}
	if info.Size() == offset {
		return nil, offset, nil
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset, err
	}
	data := make([]byte, info.Size()-offset)
	n, err := f.Read(data)
	return data[:n], offset + int64(n), err
}

// transcriptRecord is the subset of a transcript JSONL line we read.
type transcriptRecord struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	Timestamp        string          `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
}

// messageUsage is the per-message token accounting the claude_code
// transcript carries on every assistant message — the intra-node split
// the turn-boundary step events dropped (nil data), which a bench read
// needs to attribute spend INSIDE a node (#1481). Field names on the
// emitted event match the model hooks' step events.
type messageUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// decodeUsage reads the message object's usage, nil when the record
// carries none.
func decodeUsage(message json.RawMessage) *messageUsage {
	if len(message) == 0 {
		return nil
	}
	var m struct {
		Usage *messageUsage `json:"usage"`
	}
	if err := json.Unmarshal(message, &m); err != nil || m.Usage == nil {
		return nil
	}
	return m.Usage
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// handleLine parses one transcript line and emits the synthesized
// events. Returns false only when the output channel's context is done.
func (o *TranscriptObserver) handleLine(line []byte, seen, toolNames map[string]string, emit func(store.EventType, time.Time, map[string]any) bool) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return true
	}
	var rec transcriptRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return true // tolerate a torn/foreign line
	}
	if rec.IsMeta || rec.IsSidechain || rec.IsCompactSummary {
		return true
	}
	if rec.UUID != "" {
		if seen[rec.UUID] != "" {
			return true // dedup (resumed sessions replay history)
		}
		seen[rec.UUID] = "1"
	}
	ts := parseTS(rec.Timestamp)

	blocks := decodeContent(rec.Message)
	switch rec.Type {
	case "assistant":
		var hasToolUse, hasText bool
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				hasToolUse = true
				if b.ID != "" && b.Name != "" {
					toolNames[b.ID] = b.Name
				}
				if !emit(store.EventToolCalled, ts, map[string]any{"tool": b.Name, "tool_use_id": b.ID}) {
					return false
				}
			case "text":
				if b.Text != "" {
					hasText = true
				}
			}
		}
		// Turn boundary: the model produced final text and is yielding.
		if hasText && !hasToolUse {
			data := map[string]any{}
			if u := decodeUsage(rec.Message); u != nil {
				data["input_tokens"] = u.InputTokens
				data["output_tokens"] = u.OutputTokens
				if u.CacheReadInputTokens > 0 {
					data["cache_read_tokens"] = u.CacheReadInputTokens
				}
				if u.CacheCreationInputTokens > 0 {
					data["cache_write_tokens"] = u.CacheCreationInputTokens
				}
			}
			if !emit(store.EventLLMStepFinished, ts, data) {
				return false
			}
		}
	case "user":
		for _, b := range blocks {
			if b.Type != "tool_result" || !b.IsError {
				continue
			}
			data := map[string]any{"error": "tool_result reported an error"}
			if name := toolNames[b.ToolUseID]; name != "" {
				data["tool"] = name
			}
			if !emit(store.EventToolError, ts, data) {
				return false
			}
		}
	}
	return true
}

// decodeContent extracts content blocks from a record's message.content,
// tolerating both the array form (assistant / tool results) and a bare
// string (a plain user prompt → one text block).
func decodeContent(message json.RawMessage) []contentBlock {
	if len(message) == 0 {
		return nil
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &m); err != nil || len(m.Content) == 0 {
		return nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
		return []contentBlock{{Type: "text", Text: s}}
	}
	return nil
}

// parseTS parses an ISO-8601 transcript timestamp. On failure it returns
// the zero time, which the coordinator treats as pre-attach (catch-up)
// and does not act on.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
