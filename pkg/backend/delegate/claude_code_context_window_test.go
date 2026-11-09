package delegate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The review-pool incident this pins: a CLI harness that never learns the
// catalog window compacts too late and dies one token over the provider
// ceiling (deepseek 256k, 253953 loaded + 8192 output requested). The
// stamp goes into the spawn env at every boundary, and the guards keep an
// operator's explicit choice authoritative.
func TestClaudeContextWindowEnv(t *testing.T) {
	const key = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	unsetAmbient(t, key)
	strptr := func(s string) *string { return &s }
	cases := []struct {
		name string
		task Task
		env  map[string]string
		want *string
	}{
		{name: "stamps the catalog window", task: Task{Model: "deepseek-v4.1-flash", ContextWindow: 262144}, want: strptr("262144")},
		{name: "stamps a gateway-prefixed non-claude model", task: Task{Model: "zai/glm-5.3", ContextWindow: 262144}, want: strptr("262144")},
		{name: "zero window stamps nothing", task: Task{Model: "deepseek-v4.1-flash"}, want: nil},
		{name: "negative window stamps nothing", task: Task{Model: "deepseek-v4.1-flash", ContextWindow: -1}, want: nil},
		{name: "empty model stamps nothing", task: Task{ContextWindow: 262144}, want: nil},
		{name: "native claude id is skipped", task: Task{Model: "claude-opus-5-5", ContextWindow: 200000}, want: nil},
		{name: "anthropic-prefixed native id is skipped", task: Task{Model: "anthropic/claude-sonnet-4-5", ContextWindow: 200000}, want: nil},
		{name: "explicit spawn env wins", task: Task{Model: "deepseek-v4.1-flash", ContextWindow: 262144}, env: map[string]string{key: "131072"}, want: strptr("131072")},
		{name: "explicit empty suppresses the stamp", task: Task{Model: "deepseek-v4.1-flash", ContextWindow: 262144}, env: map[string]string{key: ""}, want: strptr("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeContextWindowEnv(tc.env, tc.task)
			if tc.want == nil {
				if value, ok := got[key]; ok {
					t.Fatalf("%s=%q stamped, want nothing (window=%d model=%q)", key, value, tc.task.ContextWindow, tc.task.Model)
				}
				return
			}
			if got[key] != *tc.want {
				t.Fatalf("%s=%q, want %q", key, got[key], *tc.want)
			}
		})
	}

	t.Run("ambient explicit setting wins", func(t *testing.T) {
		t.Setenv(key, "999999")
		env := map[string]string{}
		got := claudeContextWindowEnv(env, Task{Model: "deepseek-v4.1-flash", ContextWindow: 262144})
		if _, ok := got[key]; ok {
			t.Fatalf("ambient %s was overridden by the stamp; the explicit setting must win", key)
		}
	})
}

// TestClaudeContextWindowStampedAtEverySpawn exercises the real host and
// sandbox CommandBuilders on BOTH passes (work + format), like the env
// precedence test: the host fake-claude records the stamped value, the
// sandbox double records ExecOpts.Env. A helper-only test would not catch
// a spawn boundary that forgets to apply the stamp.
func TestClaudeContextWindowStampedAtEverySpawn(t *testing.T) {
	const key = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	unsetAmbient(t, key)
	strptr := func(s string) *string { return &s }
	cases := []struct {
		name        string
		model       string
		window      int
		extra       []string
		wantHost    *string
		wantSandbox *string
	}{
		{name: "catalog window stamped", model: "deepseek-v4.1-flash", window: 262144, wantHost: strptr("262144"), wantSandbox: strptr("262144")},
		{name: "prefixed non-claude model stamped", model: "zai/glm-5.3", window: 262144, wantHost: strptr("262144"), wantSandbox: strptr("262144")},
		{name: "zero window stamps nothing", model: "deepseek-v4.1-flash", wantHost: nil, wantSandbox: nil},
		{name: "native claude id skipped", model: "claude-opus-5-5", window: 200000, wantHost: nil, wantSandbox: nil},
		{
			// The ExtraEnv entry reaches BOTH spawns' env maps (the SDK applies
			// per-task entries before either builder runs), so the explicit
			// value wins over the catalog stamp on the sandbox too.
			name: "explicit extra env wins at every spawn", model: "deepseek-v4.1-flash", window: 262144,
			extra: []string{key + "=131072"}, wantHost: strptr("131072"), wantSandbox: strptr("131072"),
		},
	}
	for _, tc := range cases {
		for _, sandboxed := range []bool{false, true} {
			for _, formatting := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/sandbox=%t/format=%t", tc.name, sandboxed, formatting), func(t *testing.T) {
					resetClaudeCredEnv(t)
					dir := t.TempDir()
					capture := filepath.Join(dir, "env")
					command := filepath.Join(dir, "fake-claude")
					script := `#!/bin/sh
: > "$ITERION_TEST_CAPTURE"
if value=$(printenv CLAUDE_CODE_MAX_CONTEXT_TOKENS); then
  printf 'CLAUDE_CODE_MAX_CONTEXT_TOKENS=%s\n' "$value" >> "$ITERION_TEST_CAPTURE"
fi
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{}","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"ctxwin-test"}'
`
					if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					task := Task{NodeID: "ctxwin-test", Command: command, WorkDir: dir, Model: tc.model, ContextWindow: tc.window,
						ExtraEnv: append(append([]string{}, tc.extra...), "ITERION_TEST_CAPTURE="+capture), OutputSchema: []byte(`{"type":"object"}`)}
					fake := &captureSandboxCmdRun{}
					if sandboxed {
						task.Sandbox = fake
					}
					b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
					// The sandbox double runs `true`: no result message is its
					// designed outcome — the capture has happened by then. Only
					// the host fake-claude returns a result, so only host runs
					// may fail here.
					spawnErr := func() error {
						if formatting {
							_, _, _, err := b.formatOutput(ctx, task, "ctxwin-test")
							return err
						}
						opts, cleanup := b.buildTransportOptions(task)
						defer cleanup()
						opts, _, _, err := b.setupCredsAndSession(ctx, task, opts)
						if err != nil {
							return err
						}
						_, err = claudesdk.Prompt(ctx, "record env", opts...)
						return err
					}()
					if spawnErr != nil && !sandboxed {
						t.Fatal(spawnErr)
					}
					want := tc.wantHost
					if sandboxed {
						want = tc.wantSandbox
					}
					got := map[string]string{}
					if sandboxed {
						_, _, envs := fake.spawns()
						if len(envs) != 1 {
							t.Fatalf("want one sandbox spawn, got %d", len(envs))
						}
						got = envs[0]
					} else {
						raw, err := os.ReadFile(capture)
						if err != nil {
							t.Fatal(err)
						}
						for line := range strings.SplitSeq(string(raw), "\n") {
							if k, v, ok := strings.Cut(line, "="); ok {
								got[k] = v
							}
						}
					}
					if want == nil {
						if value, ok := got[key]; ok {
							t.Fatalf("%s=%q at spawn, want nothing (window=%d model=%q)", key, value, tc.window, tc.model)
						}
						return
					}
					if got[key] != *want {
						t.Fatalf("spawn %s=%q, want %q", key, got[key], *want)
					}
				})
			}
		}
	}
}

// unsetAmbient removes an env key for the test's duration so the stamp
// guards see a clean slate regardless of the machine running the suite.
func unsetAmbient(t *testing.T, key string) {
	t.Helper()
	if value, ok := os.LookupEnv(key); ok {
		os.Unsetenv(key)
		t.Cleanup(func() { os.Setenv(key, value) })
	} else {
		t.Cleanup(func() { os.Unsetenv(key) })
	}
}
