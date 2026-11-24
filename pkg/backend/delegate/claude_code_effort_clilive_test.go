//go:build clilive

// Real-CLI proof of the claude CLI's effort sending — gated by the `clilive`
// build tag because it needs the `claude` CLI on PATH (or ~/.local/bin). It
// costs nothing: the CLI talks to a local recording endpoint with a dummy
// key, in an isolated HOME. Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILiveClaudeCodeEffort -v ./pkg/backend/delegate/
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const cliliveAnthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// cliliveEffortSent runs the real CLI once for model and answers the
// output_config.effort of its requests for that model ("" when none carried
// one), with the extra args and the project settings given.
func cliliveEffortSent(t *testing.T, cli, model, envLevel string, args []string, projectSettings string) string {
	t.Helper()
	var mu sync.Mutex
	var efforts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" && body["model"] == model {
			effort := ""
			if oc, ok := body["output_config"].(map[string]any); ok {
				effort, _ = oc["effort"].(string)
			}
			mu.Lock()
			efforts = append(efforts, effort)
			mu.Unlock()
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, cliliveAnthropicSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, model)
	}))
	defer srv.Close()
	home, work := t.TempDir(), t.TempDir()
	if projectSettings != "" {
		mustWriteFile(t, filepath.Join(work, ".claude", "settings.json"), projectSettings)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, append([]string{"-p", "say ok", "--model", model, "--output-format", "json"}, args...)...)
	cmd.Dir = work
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + t.TempDir(), "ANTHROPIC_BASE_URL=" + srv.URL,
		"ANTHROPIC_API_KEY=sk-ant-api03-dummy", claudeEffortLevelEnv + "=" + envLevel, "DISABLE_TELEMETRY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "NO_PROXY=127.0.0.1,localhost"}
	out, err := cmd.CombinedOutput()
	mu.Lock()
	defer mu.Unlock()
	if len(efforts) == 0 {
		t.Fatalf("the CLI sent no request for %s (err %v): %s", model, err, out)
	}
	for _, e := range efforts[1:] {
		if e != efforts[0] {
			t.Fatalf("the CLI's requests for %s carried different efforts: %v", model, efforts)
		}
	}
	return efforts[0]
}

func cliliveClaude(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".local", "bin", "claude")
	if _, err := os.Stat(p); err != nil {
		t.Skip("claude CLI not on PATH")
	}
	return p
}

// TestCLILiveClaudeCodeEffort re-measures, on the real CLI, the two facts the
// backend's effort report stands on: the level the CLI sends per model for a
// level handed (claudeCLIEffortMeasurements, the rule claudeCodeWireEffort
// applies), and the pin's reach — the --settings object the backend builds
// carries the node's level over a project settings `env` naming another.
func TestCLILiveClaudeCodeEffort(t *testing.T) {
	cli := cliliveClaude(t)
	for _, m := range claudeCLIEffortMeasurements {
		t.Run(m.model+"/"+m.level, func(t *testing.T) {
			sent := cliliveEffortSent(t, cli, m.model, m.level, nil, "")
			if sent != m.sent || sent != claudeCodeWireEffort(m.model, m.level) {
				t.Errorf("the CLI sent %q; the table says %q; the backend reports %q", sent, m.sent, claudeCodeWireEffort(m.model, m.level))
			}
		})
	}
	t.Run("the pinned settings env over a project settings env", func(t *testing.T) {
		_, settings := claudeSpawnPins(Task{ReasoningEffort: "high"})
		project := `{"env":{"` + claudeEffortLevelEnv + `":"low"}}`
		without := cliliveEffortSent(t, cli, "claude-opus-5-5", "low", []string{"--setting-sources", "project"}, project)
		with := cliliveEffortSent(t, cli, "claude-opus-5-5", "low", []string{"--setting-sources", "project", "--settings", string(settings)}, project)
		if without != "low" || with != "high" {
			t.Errorf("without the pin the CLI sent %q (want low, the premise), with it %q (want high)", without, with)
		}
	})
}
