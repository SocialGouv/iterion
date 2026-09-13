package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	defaultChatHistoryMessages = 8
	defaultChatHistoryTokens   = 12_000
)

type projectedChatMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
	Seq  int64  `json:"seq,omitempty"`
}

// assistantChatHostInputs rebuilds the bounded transcript requested by the
// paused chat node's manifest. events.jsonl remains the sole authority: the
// returned value is ephemeral and is never written back as a human answer.
func (s *Server) assistantChatHostInputs(ctx context.Context, run *store.Run) (map[string]any, error) {
	return s.assistantChatHostInputsWithService(ctx, s.runs, run)
}

func (s *Server) assistantChatHostInputsWithService(ctx context.Context, runs *runview.Service, run *store.Run) (map[string]any, error) {
	if run == nil || run.Checkpoint == nil {
		return nil, nil
	}
	chat, err := s.loadAssistantChatSurface(ctx, run)
	if err != nil || chat == nil {
		return nil, err
	}
	node, ok := chat.Nodes[run.Checkpoint.NodeID]
	if !ok || node.Kind != bundle.ChatNodeHuman || node.History == nil {
		return nil, nil
	}

	messages := make([]projectedChatMessage, 0, 16)
	if seed, ok := run.Inputs[chat.SeedVar].(string); ok && strings.TrimSpace(seed) != "" {
		messages = append(messages, projectedChatMessage{Role: "operator", Text: seed})
	}
	err = runs.ScanEventsCtx(ctx, run.ID, func(evt *store.Event) bool {
		if evt == nil {
			return true
		}
		chatNode, known := chat.Nodes[evt.NodeID]
		if !known || chatNode.Kind != bundle.ChatNodeHuman {
			return true
		}
		switch evt.Type {
		case store.EventHumanInputRequested:
			if store.IsAsyncHumanInput(*evt) {
				return true
			}
			if text, _ := evt.Data["instructions"].(string); strings.TrimSpace(text) != "" {
				messages = append(messages, projectedChatMessage{Role: "assistant", Text: text, Seq: evt.Seq})
			}
		case store.EventHumanAnswersRecorded:
			answers, _ := evt.Data["answers"].(map[string]any)
			if answers == nil {
				// Mongo/BSON and JSON round-trips can leave a map behind a raw
				// value. Normalise without teaching the projection store shapes.
				if raw, marshalErr := json.Marshal(evt.Data["answers"]); marshalErr == nil {
					_ = json.Unmarshal(raw, &answers)
				}
			}
			if text, _ := answers[chatNode.TextField].(string); strings.TrimSpace(text) != "" {
				messages = append(messages, projectedChatMessage{Role: "operator", Text: text, Seq: evt.Seq})
			}
			if chatNode.HostEventField != "" {
				if raw := answers[chatNode.HostEventField]; raw != nil {
					if encoded, marshalErr := json.Marshal(raw); marshalErr == nil && string(encoded) != "null" {
						messages = append(messages, projectedChatMessage{Role: "host_event", Text: string(encoded), Seq: evt.Seq})
					}
				}
			}
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("project assistant chat history: %w", err)
	}
	maxMessages := node.History.MaxMessages
	if maxMessages <= 0 {
		maxMessages = defaultChatHistoryMessages
	}
	maxTokens := node.History.MaxEstimatedTokens
	if maxTokens <= 0 {
		maxTokens = defaultChatHistoryTokens
	}
	messages = boundProjectedChatHistory(messages, maxMessages, maxTokens)
	return map[string]any{node.History.Field: messages}, nil
}

func boundProjectedChatHistory(messages []projectedChatMessage, maxMessages, maxTokens int) []projectedChatMessage {
	if len(messages) == 0 || maxMessages <= 0 || maxTokens <= 0 {
		return nil
	}
	start, tokens := len(messages), 0
	for start > 0 && len(messages)-start < maxMessages {
		next := estimatedChatTokens(messages[start-1].Text)
		if tokens+next > maxTokens && start < len(messages) {
			break
		}
		tokens += next
		start--
	}
	return append([]projectedChatMessage(nil), messages[start:]...)
}

func estimatedChatTokens(text string) int {
	// Deterministic, deliberately conservative approximation. Exact provider
	// tokenizers would make this generic host projection route-dependent.
	return (utf8.RuneCountInString(text) + 2) / 3
}

// loadAssistantChatSurface resolves a run's bundle manifest and returns its
// `chat:` surface, or (nil, nil) when the bot declares none. It is the single
// manifest read shared by the chat-history projection, the watch-delivery
// safety check, and the capability probe — three callers that were each
// re-implementing the resolve → LoadManifest → ManifestFileAlt dance.
func (s *Server) loadAssistantChatSurface(ctx context.Context, run *store.Run) (*bundle.ChatSurface, error) {
	if run == nil {
		return nil, nil
	}
	lb, err := s.resolveBotTiered(ctx, run.TenantID, run.BotID, run.FilePath)
	if err != nil || lb == nil {
		return nil, err
	}
	defer lb.Cleanup()
	if lb.Manifest != nil {
		return lb.Manifest.Chat, nil
	}
	dir := lb.BundleDir
	if dir == "" {
		dir = filepath.Dir(lb.Path)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFile))
	if err == nil && m == nil {
		m, err = bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFileAlt))
	}
	if err != nil {
		return nil, fmt.Errorf("assistant chat manifest: %w", err)
	}
	if m == nil {
		return nil, nil
	}
	return m.Chat, nil
}

// assistantResumePolicy is wired into runview so EVERY continuation surface
// sees the same manifest-declared state transition. In particular a watch
// host event can win the race to resume a chat boundary before the browser's
// next message; evaluating only in the HTTP handler would miss that turn.
func (s *Server) assistantResumePolicy(ctx context.Context, run *store.Run, spec *runview.ResumeSpec) error {
	if run == nil || spec == nil || run.Checkpoint == nil {
		return nil
	}
	if run.BudgetOverrides != nil && run.BudgetOverrides.UnlimitedWorkflow {
		return nil // runview replays the persisted activation automatically
	}
	chat, err := s.loadAssistantChatSurface(ctx, run)
	if err != nil || chat == nil {
		return err
	}
	if chat.UnlimitedWorkflowForOutputs(run.Checkpoint.Outputs) {
		spec.Budget = &ir.BudgetOverrides{UnlimitedWorkflow: true}
	}
	return nil
}
