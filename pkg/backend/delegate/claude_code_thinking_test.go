package delegate

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestHandleAssistantMessage_ThinkingFoldsInRunLog proves a streamed
// ThinkingBlock's content lands in run.log as a foldable 🧠 LogBlock
// (header with metrics + "│ "-indented body — the shape the studio's
// LogBlockRow collapses), and that the best-effort metrics still
// accumulate on sessionMeta.
func TestHandleAssistantMessage_ThinkingFoldsInRunLog(t *testing.T) {
	var logBuf bytes.Buffer
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelInfo, &logBuf)}

	const thinking = "Let me reason about this carefully."
	m := &claudesdk.AssistantMessage{
		Message: &claudesdk.APIMessage{
			Content: []claudesdk.ContentBlock{
				&claudesdk.ThinkingBlock{Type: "thinking", Thinking: thinking},
			},
		},
	}

	meta := sessionMeta{}
	var lastText string
	err := b.handleAssistantMessage(m, Task{NodeID: "n1"}, map[string]string{},
		&meta, &lastText, time.Now(), func() {})
	if err != nil {
		t.Fatalf("handleAssistantMessage: %v", err)
	}

	out := logBuf.String()
	if !strings.Contains(out, "🧠") || !strings.Contains(out, "thinking ~") {
		t.Errorf("run.log missing 🧠 thinking header:\n%s", out)
	}
	if !strings.Contains(out, "│ "+thinking) {
		t.Errorf("run.log missing folded thinking body (block-indent continuation):\n%s", out)
	}
	if meta.thinkingTokens <= 0 {
		t.Errorf("meta.thinkingTokens = %d, want > 0", meta.thinkingTokens)
	}
}
