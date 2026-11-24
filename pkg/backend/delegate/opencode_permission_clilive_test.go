//go:build clilive

// Scripted-model measurement of opencode 2.x's in-process permission policy,
// the successor mechanism of the OPENCODE_PERMISSION seam #1640 was scoped on
// (measured on 1.1.19/1.19.0, REMOVED from the 2.x line: zero occurrences of
// the variable in the 2.2.5 binary; the env-delivered config is
// OPENCODE_CONFIG_CONTENT, highest user precedence since upstream #11628).
//
// It drives the CLI DIRECTLY, not through iterion's Execute: what is measured
// here is the policy engine, which iterion does not wire, not iterion's argv
// (TestCLILiveOpenCodeSessionResume proves that argv against the same 2.x
// CLI).
//
// The model is a scripted OpenAI-compatible server on 127.0.0.1 whose one
// agent turn calls `bash` with `touch <sentinel>`: the authority is the
// sentinel on disk, never model prose. A scripted model costs nothing and
// exercises opencode's real policy engine — it is NOT the live denial gate
// membership asks for (a real model's real tool call), which stays unearned
// until the operator's doctrine call (fleet #54) and a funded live run.
//
// Every arm is a measured FACT of the build on PATH: an arm reddens the day a
// future opencode changes the fact, which is exactly when iterion must
// re-read this page before wiring the gate.
//
// Run with:
//
//	devbox run -- go test -tags clilive -run TestCLILiveOpenCodeConfigContent -v ./pkg/backend/delegate/
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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/git"
)

// TestCLILiveOpenCodeConfigContentDeny re-measures on the 2.x line what
// docs/backends.md's fail-open table measured on 1.19.0, with the policy
// delivered through OPENCODE_CONFIG_CONTENT (the 2.x env mechanism) and the
// operator's half living in the scrubbed global config file.
func TestCLILiveOpenCodeConfigContentDeny(t *testing.T) {
	cli, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode CLI not on PATH")
	}

	// Control: with no policy anywhere the scripted bash call EXECUTES, so
	// none of the arms below is vacuous.
	t.Run("control executes the call", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{})
		res.log(t)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("control: the scripted bash call did not execute (sentinel absent: %v)", err)
		}
	})

	// The env-delivered pattern deny holds AT THE CALL on 2.2.5: the tool is
	// offered, the evaluation blocks it ("Permission denied: shell" — the 2.x
	// internal name for bash), the denial is fed back to the model and the
	// turn finishes with exit 0.
	t.Run("pattern deny via OPENCODE_CONFIG_CONTENT blocks the call", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			configContent: `{"permission":{"bash":{"*":"allow","touch *":"deny"}}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("deny: the sentinel exists (stat err %v) — the policy did not hold", err)
		}
		if res.exitErr != nil {
			t.Fatalf("deny: the run failed instead of finishing the turn denied: %v", res.exitErr)
		}
		if !res.sawDenialEvidence {
			t.Fatal("deny: no denial evidence reached the model or the event stream")
		}
	})

	// Vocabulary of the 2.x line: the internal tool is `shell`; the `bash`
	// key above held through a compatibility mapping. The native 2.x key must
	// hold too, or a translation table written for 2.x would target a name
	// the policy engine ignores.
	t.Run("the 2.x-native shell key also holds", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			configContent: `{"permission":{"shell":{"*":"allow","touch *":"deny"}}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("shell key: the sentinel exists (stat err %v) — the 2.x key name did not hold", err)
		}
		if !res.sawDenialEvidence {
			t.Fatal("shell key: no denial evidence")
		}
	})

	// A whole-tool deny on 1.19 WITHHELD the tool from the model; measure the
	// 2.x behaviour — the scripted model calls bash regardless, so what the
	// CLI does with a call to a withheld-or-denied tool is the fact.
	t.Run("whole-tool deny on 2.x", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			configContent: `{"permission":{"bash":"deny"}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("whole-tool deny: the sentinel exists (stat err %v)", err)
		}
		if res.exitErr != nil {
			t.Fatalf("whole-tool deny: the run failed: %v", res.exitErr)
		}
	})

	// The 1.19 silent fail-open row, re-measured: an invalid policy string is
	// dropped WITHOUT a word — the call runs and the exit code is 0. A
	// marshalled-by-iterion policy can never be invalid, but a 2.x that one
	// day fails LOUD here reddens this arm, and that is the day the fact
	// changes.
	t.Run("invalid OPENCODE_CONFIG_CONTENT still fails OPEN and silent on 2.x", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			configContent: `{"permission":{"bash":"deny",}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("invalid policy string: sentinel absent (%v) — 2.x no longer fails open here; re-read this bench", err)
		}
		if res.exitErr != nil {
			t.Fatalf("invalid policy string: the run failed loudly (%v) — 2.x no longer fails silently here; re-read this bench", res.exitErr)
		}
	})

	// The v1 key-position fail-open, re-measured with env delivery: the
	// operator's global `permission` carries bash=allow placed AFTER *, the
	// env content carries the deny. Upstream #11628 gives the env content the
	// highest user precedence, so the deny must hold.
	t.Run("env content beats the operator's key-positioned allow", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			operatorPermission: `, "permission":{"*":"ask","bash":{"*":"allow","touch *":"allow"}}`,
			configContent:      `{"permission":{"bash":{"*":"allow","touch *":"deny"}}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("key-position: the sentinel exists (stat err %v) — the operator's allow beat the env deny", err)
		}
		if !res.sawDenialEvidence {
			t.Fatal("key-position: no denial evidence")
		}
	})

	// The v1 agent-level fail-open, re-measured: the operator's global config
	// grants the DEFAULT agent (build) bash=allow; the env content denies.
	// HARD arm, symmetric with the invalid-string one: the sentinel MUST
	// exist while the hole is open, and the run MUST exit 0 (the hole is
	// silent) — the day opencode closes it, this arm reddens.
	t.Run("an agent-level allow still beats the env deny on 2.x", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			operatorPermission: `, "agent":{"build":{"permission":{"bash":"allow"}}}`,
			configContent:      `{"permission":{"bash":{"*":"allow","touch *":"deny"}}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("agent-level: sentinel absent (%v) — opencode closed the agent-level fail-open; re-read this bench and the flip's owed list", err)
		}
		if res.exitErr != nil {
			t.Fatalf("agent-level: the run failed loudly (%v) — the fail-open is no longer silent; re-read this bench", res.exitErr)
		}
	})

	// The v1 default_agent fail-open, re-measured: the operator selects a
	// custom agent whose permission allows bash. HARD arm, same contract as
	// the agent-level one.
	t.Run("default_agent allow vs the env deny on 2.x", func(t *testing.T) {
		sentinel, res := runOpenCodePermissionProbe(t, cli, openCodeProbe{
			operatorPermission: `, "default_agent":"wide", "agent":{"wide":{"permission":{"bash":"allow"}}}`,
			configContent:      `{"permission":{"bash":{"*":"allow","touch *":"deny"}}}`,
		})
		res.log(t)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("default_agent: sentinel absent (%v) — opencode closed the default_agent fail-open; re-read this bench and the flip's owed list", err)
		}
		if res.exitErr != nil {
			t.Fatalf("default_agent: the run failed loudly (%v) — the fail-open is no longer silent; re-read this bench", res.exitErr)
		}
	})
}

// openCodeProbe parametrizes one probe run: the env-delivered policy (the
// would-be iterion half) and the operator's global config additions (the
// hostile half), beside the scripted-provider block every run needs.
type openCodeProbe struct {
	configContent      string // OPENCODE_CONFIG_CONTENT, "" = unset
	operatorPermission string // extra keys spliced into the global opencode.json
}

type openCodeProbeResult struct {
	stdout, stderr    string
	exitErr           error
	sawDenialEvidence bool
}

func (r openCodeProbeResult) log(t *testing.T) {
	t.Helper()
	t.Logf("exit: %v\nstdout:\n%s\nstderr:\n%s", r.exitErr, r.stdout, r.stderr)
}

// runOpenCodePermissionProbe runs `opencode run` against the scripted model
// and returns the sentinel path plus what the run produced. Every opencode
// root lives under the test's own directories; the operator's real settings
// never reach the child.
func runOpenCodePermissionProbe(t *testing.T, cli string, probe openCodeProbe) (string, openCodeProbeResult) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(tmp, "permission-sentinel")
	model := newScriptedToolCallModel(t, sentinel)
	repo := filepath.Join(tmp, "repo")
	for _, root := range []string{"home", "config", "data", "cache", "state", "repo"} {
		if err := os.MkdirAll(filepath.Join(tmp, root), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", git.NoAutoMaintenance(args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustWriteFile(t, filepath.Join(tmp, "config", "opencode", "opencode.json"), `{
  "provider": {
    "scripted": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Scripted",
      "options": {"baseURL": "`+model.URL+`", "apiKey": "scripted"},
      "models": {"m1": {"name": "m1", "tool_call": true}}
    }
  },
  "share": "disabled",
  "autoupdate": false`+probe.operatorPermission+`
}
`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 2.x accepts --format and -m only AFTER the run subcommand; --standalone
	// keeps the run off the host's managed background service (a private
	// server, so the probe never touches the operator's long-lived one).
	cmd := exec.CommandContext(ctx, cli, "run", "--standalone", "--format", "json", "-m", "scripted/m1", "run the touch command now") // #nosec G204 — test fixture driving the probed CLI
	cmd.Dir = repo
	env := []string{
		"HOME=" + filepath.Join(tmp, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(tmp, "config"),
		"XDG_DATA_HOME=" + filepath.Join(tmp, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(tmp, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(tmp, "state"),
		"PATH=" + os.Getenv("PATH"),
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	}
	if probe.configContent != "" {
		env = append(env, "OPENCODE_CONFIG_CONTENT="+probe.configContent)
	}
	cmd.Env = env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	exitErr := cmd.Run()

	res := openCodeProbeResult{stdout: outBuf.String(), stderr: errBuf.String(), exitErr: exitErr}
	lower := strings.ToLower(res.stdout + "\n" + res.stderr + "\n" + model.toolResults())
	for _, needle := range []string{"denied", "deny", "permission"} {
		if strings.Contains(lower, needle) {
			res.sawDenialEvidence = true
			break
		}
	}
	return sentinel, res
}

// scriptedToolCallModel answers the FIRST agent turn with a `bash` tool call
// (`touch <sentinel>`) and every later one with a short text, recording the
// tool results the CLI sends back — the denial's evidence.
type scriptedToolCallModel struct {
	URL      string
	sentinel string

	mu        sync.Mutex
	toolFeeds []string
}

func newScriptedToolCallModel(t *testing.T, sentinel string) *scriptedToolCallModel {
	t.Helper()
	m := &scriptedToolCallModel{sentinel: sentinel}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		called := false
		for _, raw := range req.Messages {
			var msg struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &msg); err == nil && msg.Role == "tool" {
				m.mu.Lock()
				m.toolFeeds = append(m.toolFeeds, msg.Content)
				m.mu.Unlock()
				called = true
			}
		}

		var body strings.Builder
		switch {
		case len(req.Tools) == 0:
			writeScriptedChunk(&body, map[string]any{"role": "assistant", "content": "scripted title"}, nil)
			writeScriptedChunk(&body, map[string]any{}, "stop")
		case called:
			// The follow-up turn after the tool result: end the run.
			writeScriptedChunk(&body, map[string]any{"role": "assistant", "content": "done"}, nil)
			writeScriptedChunk(&body, map[string]any{}, "stop")
		default:
			args := fmt.Sprintf(`{"command":"touch %s","description":"probe"}`, m.sentinel)
			writeScriptedChunk(&body, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"index": 0, "id": "call_probe", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": ""},
			}}}, nil)
			writeScriptedChunk(&body, map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": args},
			}}}, nil)
			writeScriptedChunk(&body, map[string]any{}, "tool_calls")
		}
		body.WriteString("data: [DONE]\n\n")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		_, _ = io.WriteString(w, body.String())
	}))
	t.Cleanup(srv.Close)
	m.URL = srv.URL + "/v1"
	return m
}

func (m *scriptedToolCallModel) toolResults() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.toolFeeds, "\n")
}
