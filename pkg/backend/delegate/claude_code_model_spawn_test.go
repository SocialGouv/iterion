package delegate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

var spawnModelKeys = []string{"ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_FABLE_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"}

type modelDefaultsRun struct {
	sandbox.Run
	env    map[string]string
	script string
}

func (r *modelDefaultsRun) Driver() string { return "kubernetes" }
func (r *modelDefaultsRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	r.env = map[string]string{}
	for k, v := range opts.Env {
		r.env[k] = v
	}
	cmd := exec.CommandContext(ctx, r.script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

// The fake CLI captures its real child environment on all four spawn paths.
// It performs no provider call. These assertions fail if the helper is unwired.
func TestClaudeModelDefaultsReachEverySpawn(t *testing.T) {
	type scenario struct {
		name     string
		env      map[string]string
		extra    []string
		hint     string
		creds    map[secrets.Provider]string
		oauth    bool
		defaults bool
		override map[string]string
		refusal  bool
		model    string
	}
	cases := []scenario{
		{name: "direct-unconfigured", defaults: true},
		{name: "context-zai-empty-hint", creds: map[secrets.Provider]string{secrets.ProviderZAI: "dummy-zai"}},
		{name: "custom-base", env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example/api"}},
		{name: "ambient-all-overrides", defaults: true, env: map[string]string{"ANTHROPIC_DEFAULT_OPUS_MODEL": "operator-opus", "ANTHROPIC_DEFAULT_SONNET_MODEL": "operator-sonnet", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "operator-haiku", "ANTHROPIC_DEFAULT_FABLE_MODEL": "operator-fable", "CLAUDE_CODE_SUBAGENT_MODEL": "operator-subagent"}, override: map[string]string{"ANTHROPIC_DEFAULT_OPUS_MODEL": "operator-opus", "ANTHROPIC_DEFAULT_SONNET_MODEL": "operator-sonnet", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "operator-haiku", "ANTHROPIC_DEFAULT_FABLE_MODEL": "operator-fable", "CLAUDE_CODE_SUBAGENT_MODEL": "operator-subagent"}},
		{name: "explicit-empties", defaults: true, env: map[string]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL": "", "CLAUDE_CODE_SUBAGENT_MODEL": ""}, extra: []string{"ANTHROPIC_DEFAULT_OPUS_MODEL=", "ANTHROPIC_DEFAULT_SONNET_MODEL="}, override: map[string]string{"ANTHROPIC_DEFAULT_OPUS_MODEL": "", "ANTHROPIC_DEFAULT_SONNET_MODEL": "", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "", "CLAUDE_CODE_SUBAGENT_MODEL": ""}},
		{name: "task-beats-ambient-and-duplicate", defaults: true, env: map[string]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL": "ambient-haiku"}, extra: []string{"ANTHROPIC_DEFAULT_HAIKU_MODEL=first", "ANTHROPIC_DEFAULT_HAIKU_MODEL=task-haiku"}, override: map[string]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL": "task-haiku"}},
	}
	for _, tc := range cases {
		for _, sand := range []bool{false, true} {
			for _, format := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/sandbox=%t/format=%t", tc.name, sand, format), func(t *testing.T) {
					resetClaudeCredEnv(t)
					for _, key := range append(append([]string{}, spawnModelKeys...), FacadeSlotEnvKey, ForfaitSuppressedEnvKey, "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY") {
						t.Setenv(key, "")
						os.Unsetenv(key)
					}
					for k, v := range tc.env {
						t.Setenv(k, v)
					}
					dir := t.TempDir()
					capture := filepath.Join(dir, "capture")
					script := filepath.Join(dir, "fake-claude")
					body := "#!/bin/sh\n: > \"$MODEL_DEFAULTS_CAPTURE\"\nfor key in " + strings.Join(spawnModelKeys, " ") + "; do\n if value=$(printenv \"$key\"); then printf '%s=%s\\n' \"$key\" \"$value\" >> \"$MODEL_DEFAULTS_CAPTURE\"; fi\ndone\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"{}\",\"num_turns\":1,\"duration_ms\":1,\"duration_api_ms\":1,\"session_id\":\"review\"}'\n"
					if err := os.WriteFile(script, []byte(body), 0755); err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					if tc.creds != nil {
						ctx = ctxWithCreds(t, tc.creds, nil)
					}
					if tc.oauth {
						ctx = ctxWithCreds(t, nil, map[string]string{string(secrets.OAuthKindClaudeCode): dir})
					}
					ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					task := Task{ProviderHint: tc.hint, Model: tc.model, Command: script, WorkDir: dir, OutputSchema: []byte(`{"type":"object"}`), ExtraEnv: append(append([]string{}, tc.extra...), "MODEL_DEFAULTS_CAPTURE="+capture)}
					fake := &modelDefaultsRun{script: script}
					if sand {
						task.Sandbox = fake
					}
					b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
					if format {
						_, _ = b.formatOutput(ctx, task, "review")
					} else {
						opts, _ := b.buildTransportOptions(task)
						opts, _, err := b.setupCredsAndSession(ctx, task, opts)
						if tc.refusal {
							if err == nil {
								t.Fatal("facade was not refused")
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						if _, err := claudesdk.Prompt(ctx, "review", opts...); err != nil {
							t.Fatal(err)
						}
					}
					raw, err := os.ReadFile(capture)
					if err != nil {
						t.Fatal(err)
					}
					got := map[string]string{}
					for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
						if k, v, ok := strings.Cut(line, "="); ok {
							got[k] = v
						}
					}
					wantDefaults := tc.defaults
					for _, key := range spawnModelKeys {
						value, present := got[key]
						if !wantDefaults {
							if present {
								t.Errorf("non-direct route injected %s=%q", key, value)
							}
							continue
						}
						want := "claude-opus-5-5"
						if v, ok := tc.override[key]; ok {
							want = v
						}
						if !present || value != want {
							t.Errorf("%s present=%t got=%q want=%q", key, present, value, want)
						}
						if sand {
							if v, ok := fake.env[key]; !ok || v != want {
								t.Errorf("sandbox map %s present=%t got=%q want=%q", key, ok, v, want)
							}
						}
					}
				})
			}
		}
	}
}
