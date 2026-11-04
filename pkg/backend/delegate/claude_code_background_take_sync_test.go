//go:build !windows

package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inputBeforeTakeTimer arms the real timer at the production-selected duration,
// then waits for the scenario's precondition: the fake CLI actually read and
// logged the wrap-up. The elapsed timer stays elapsed; no timeout is extended.
// Only this read-but-not-replayed scenario opts in. The existing context's
// safety net still fails a fixture whose subprocess never receives the message.
type inputBeforeTakeTimer struct {
	backgroundSessionTimer
	t          *testing.T
	ctx        context.Context
	stdin      string
	finalizing func() bool
}

func wrapUpInputBeforeTakeTimer(t *testing.T) func(context.Context, string, func() bool) backgroundSessionTimer {
	return func(ctx context.Context, stdin string, finalizing func() bool) backgroundSessionTimer {
		return &inputBeforeTakeTimer{backgroundSessionTimer: newBackgroundSessionTimer(), t: t, ctx: ctx, stdin: stdin, finalizing: finalizing}
	}
}
func (timer *inputBeforeTakeTimer) Reset(d time.Duration) bool {
	armed := timer.backgroundSessionTimer.Reset(d)
	if !timer.finalizing() {
		return armed
	}
	timer.t.Helper()
	if err := waitForLoggedWrapUp(timer.ctx, timer.stdin); err != nil {
		timer.t.Fatalf("fake CLI never logged a complete wrap-up before its session safety net: %v", err)
	}
	return armed
}

// A phase event is emitted before SendTagged, and a substring could occur in a
// partially written log line. Require the complete tagged user-message frame,
// including the final instruction, from the subprocess's stdin transcript.
func loggedWrapUp(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		var msg struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg.Type == "user" && msg.UUID != "" && msg.Message.Role == "user" &&
			strings.HasPrefix(msg.Message.Content, "[iterion] the background wait budget (") &&
			strings.Contains(msg.Message.Content, "Stop waiting and report now with what you have;") &&
			strings.HasSuffix(msg.Message.Content, "state explicitly which parts those tasks were to cover and that they were not verified.") {
			return true
		}
	}
	return false
}
func waitForLoggedWrapUp(ctx context.Context, path string) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read fake CLI stdin log: %w", err)
		}
		if loggedWrapUp(raw) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
func TestBackgroundWrapUpReadSignalRequiresCompleteTaggedFrame(t *testing.T) {
	content := backgroundWrapUpMessage(nil, "the background wait budget (1s) is spent")
	full := jsonLine(map[string]any{"type": "user", "uuid": "tag", "message": map[string]any{"role": "user", "content": content}})
	for _, raw := range []string{`{"phase":"finalizing"}`, full[:len(full)-1], strings.Replace(full, `"uuid":"tag"`, `"uuid":""`, 1), jsonLine(map[string]any{"type": "user", "uuid": "tag", "message": map[string]any{"role": "user", "content": "[iterion] the background wait budget (1s) is spent"}})} {
		if loggedWrapUp([]byte(raw)) {
			t.Fatalf("incomplete signal accepted: %s", raw)
		}
	}
	if !loggedWrapUp([]byte(full + "\n")) {
		t.Fatal("complete tagged wrap-up refused")
	}
}
func TestBackgroundWrapUpReadSignalHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdin.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForLoggedWrapUp(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("missing wrap-up escaped context: %v", err)
	}
}
