//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// A runner whose launcher ends the node's session draws none of its own: what
// its shell leaves runs on until the launcher's sweep, which stops it and
// reports it where the launcher records the node's events. A runner told
// nothing — by a launcher that predates the field — ends a session of its own.
func TestClawRunner_LeavesTheSessionToALauncherThatEndsIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		launcherEnds bool
		wantRunning  bool
	}{
		{name: "the launcher ends it", launcherEnds: true, wantRunning: true},
		{name: "a launcher that predates it", launcherEnds: false, wantRunning: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pids := filepath.Join(t.TempDir(), "left.pids")
			t.Cleanup(func() {
				raw, _ := os.ReadFile(pids)
				for _, f := range strings.Fields(string(raw)) {
					if cmdline, err := os.ReadFile("/proc/" + f + "/cmdline"); err == nil && string(cmdline) == "sleep\x00300\x00" {
						pid, _ := strconv.Atoi(f)
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			args, _ := json.Marshal(map[string]any{"command": fmt.Sprintf("nohup sleep 300 >/dev/null 2>&1 & echo $! >> %s", pids)})
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
						"role": "assistant",
						"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
							"function": map[string]any{"name": "bash", "arguments": string(args)}}},
					}}}})
					_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(srv.Close)
			t.Setenv("OPENAI_API_KEY", "sk-test")
			t.Setenv("OPENAI_BASE_URL", srv.URL)
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

			_, res := driveRunnerToResult(t, delegate.IOTask{
				NodeID: "n", Model: "openai/gpt-test", UserPrompt: "work", HasTools: true, ToolMaxSteps: 5,
				ToolDefs: []delegate.IOToolDef{{Name: "bash", Description: "shell",
					InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}},
				LauncherEndsNodeSession: tc.launcherEnds,
			})
			if res.Error != "" {
				t.Fatalf("the runner failed: %s", res.Error)
			}
			raw, _ := os.ReadFile(pids)
			left := strings.Fields(string(raw))
			if len(left) != 1 {
				t.Fatalf("scenario broken: the shell recorded %q", left)
			}
			if running := clawRunnerProcessRuns(left[0]); running != tc.wantRunning {
				t.Errorf("the process the node's shell left runs after the runner: %v, want %v", running, tc.wantRunning)
			}
		})
	}
}

func clawRunnerProcessRuns(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}
