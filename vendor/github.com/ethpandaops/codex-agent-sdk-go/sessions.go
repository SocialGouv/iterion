package codexsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	internalerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/session"
)

const sessionScannerBufferSize = 1 << 20 // 1 MB

var (
	// errSkipPersistedRolloutMessage marks persisted wrapper records that should
	// be ignored instead of falling back to the generic parser.
	errSkipPersistedRolloutMessage = errors.New("skip persisted rollout message")

	// errNotPersistedRolloutMessage marks records that are not persisted wrapper
	// messages and should be passed to the generic parser.
	errNotPersistedRolloutMessage = errors.New("not a persisted rollout message")
)

func sessionReadLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func joinTextContent(blocks []any, allowedTypes map[string]struct{}) []string {
	texts := make([]string, 0, len(blocks))

	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}

		blockType, _ := block["type"].(string)
		if _, ok := allowedTypes[blockType]; !ok {
			continue
		}

		text, _ := block["text"].(string)
		if text != "" {
			texts = append(texts, text)
		}
	}

	return texts
}

func parsePersistedResponseItem(payload map[string]any) Message {
	payloadType, _ := payload["type"].(string)
	if payloadType != "message" {
		return nil
	}

	role, _ := payload["role"].(string)
	content, _ := payload["content"].([]any)

	switch role {
	case "user":
		texts := joinTextContent(content, map[string]struct{}{
			"input_text": {},
		})
		if len(texts) == 0 {
			return nil
		}

		text := texts[0]
		for i := 1; i < len(texts); i++ {
			text += "\n" + texts[i]
		}

		return &UserMessage{
			Type:    "user",
			Content: NewUserMessageContent(text),
		}

	case "assistant":
		texts := joinTextContent(content, map[string]struct{}{
			"output_text": {},
		})
		if len(texts) == 0 {
			return nil
		}

		blocks := make([]ContentBlock, 0, len(texts))
		for _, text := range texts {
			blocks = append(blocks, &TextBlock{Type: "text", Text: text})
		}

		return &AssistantMessage{
			Type:    "assistant",
			Content: blocks,
		}
	}

	return nil
}

func parsePersistedEventMessage(
	log *slog.Logger,
	payload map[string]any,
) (Message, error) {
	payloadType, _ := payload["type"].(string)

	switch payloadType {
	case "task_started":
		return message.Parse(log, map[string]any{
			"type":    "system",
			"subtype": "task.started",
			"data":    payload,
		})
	case "task_complete":
		return message.Parse(log, map[string]any{
			"type":    "system",
			"subtype": "task.complete",
			"data":    payload,
		})
	case "thread_rolled_back":
		return message.Parse(log, map[string]any{
			"type":    "system",
			"subtype": "thread.rolled_back",
			"data":    payload,
		})
	case "token_count":
		return message.Parse(log, map[string]any{
			"type":    "system",
			"subtype": "token.count",
			"data":    payload,
		})
	case "agent_message", "user_message":
		return nil, errSkipPersistedRolloutMessage
	}

	return nil, errSkipPersistedRolloutMessage
}

func parsePersistedRolloutMessage(log *slog.Logger, raw map[string]any) (Message, error) {
	recordType, _ := raw["type"].(string)
	payload, _ := raw["payload"].(map[string]any)

	switch recordType {
	case "response_item":
		return parsePersistedResponseItem(payload), nil
	case "event_msg":
		return parsePersistedEventMessage(log, payload)
	default:
		return nil, errNotPersistedRolloutMessage
	}
}

// ListSessions returns local Codex sessions from the state database, newest first.
// Use WithCwd to filter by project directory and WithCodexHome to override the
// default ~/.codex location.
func ListSessions(ctx context.Context, opts ...Option) ([]SessionStat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	o := applyAgentOptions(opts)

	codexHome, err := resolveCodexHome(o)
	if err != nil {
		return nil, fmt.Errorf("resolving codex home: %w", err)
	}

	rows, err := session.ListThreads(ctx, session.DatabasePath(codexHome), o.Cwd)
	if err != nil {
		return nil, err
	}

	stats := make([]SessionStat, 0, len(rows))
	for i := range rows {
		stat := buildSessionStat(&rows[i])
		applyRolloutFileStat(stat)
		stats = append(stats, *stat)
	}

	return stats, nil
}

// GetSessionMessages reads and parses persisted messages from a local Codex
// session rollout JSONL file.
// Use WithCwd to filter by project directory and WithCodexHome to override the
// default ~/.codex location.
func GetSessionMessages(ctx context.Context, sessionID string, opts ...Option) ([]Message, error) {
	stat, err := StatSession(ctx, sessionID, opts...)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(stat.RolloutPath)
	if err != nil {
		return nil, fmt.Errorf("open rollout file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), sessionScannerBufferSize)

	log := sessionReadLogger()
	messages := make([]Message, 0, 64)
	lineNum := 0

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		lineNum++

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, fmt.Errorf("decode rollout line %d: %w", lineNum, err)
		}

		persisted, err := parsePersistedRolloutMessage(log, raw)
		if err != nil {
			if errors.Is(err, errSkipPersistedRolloutMessage) {
				continue
			}

			if !errors.Is(err, errNotPersistedRolloutMessage) {
				return nil, fmt.Errorf("parse persisted rollout line %d: %w", lineNum, err)
			}
		}

		if persisted != nil {
			messages = append(messages, persisted)

			continue
		}

		msg, err := message.Parse(log, raw)
		if err != nil {
			if errors.Is(err, internalerrors.ErrUnknownMessageType) {
				continue
			}

			return nil, fmt.Errorf("parse rollout line %d: %w", lineNum, err)
		}

		messages = append(messages, msg)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan rollout file: %w", err)
	}

	return messages, nil
}
