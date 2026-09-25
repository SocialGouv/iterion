package delegate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Exercise the actual host and sandbox CommandBuilders on BOTH spawns. No
// model is invoked: the host binary records only these synthetic credential
// fields, while the sandbox double records ExecOpts.Env.
func TestClaudeTaskEnvPrecedenceAtSpawn(t *testing.T) {
	const base = "ANTHROPIC_BASE_URL"
	const api = "ANTHROPIC_API_KEY"
	const auth = "ANTHROPIC_AUTH_TOKEN"
	cases := []struct {
		name            string
		ambient         map[string]string
		extra           []string
		keys            map[secrets.Provider]string
		hint            string
		want            map[string]string
		refused         bool
		fingerprint     string
		hostFingerprint string
	}{
		{name: "clear ambient base", ambient: map[string]string{base: "https://gateway.example/api"}, extra: []string{base + "="}, want: map[string]string{base: ""}},
		{name: "override ambient base", ambient: map[string]string{base: "https://old.example/api"}, extra: []string{base + "=https://new.example/api?version=1"}, want: map[string]string{base: "https://new.example/api?version=1"}},
		{name: "last explicit value wins", ambient: map[string]string{base: "https://old.example/api"}, extra: []string{base + "=https://first.example", "invalid", "=invalid", base + "="}, want: map[string]string{base: ""}},
		{name: "clear ambient API key", ambient: map[string]string{api: "ambient-key"}, extra: []string{api + "="}, want: map[string]string{api: ""}},
		{name: "inherited endpoint with task API key", ambient: map[string]string{base: "https://gateway.example/api"}, extra: []string{api + "=extra-key"}, want: map[string]string{base: "https://gateway.example/api", api: "extra-key"}, fingerprint: "facade:https://gateway.example/api"},
		{name: "inherited endpoint with task bearer", ambient: map[string]string{base: "https://gateway.example/api"}, extra: []string{auth + "=extra-token"}, want: map[string]string{base: "https://gateway.example/api", auth: "extra-token"}, fingerprint: "facade:https://gateway.example/api"},
		{name: "inherited cloud mode keeps host classification", ambient: map[string]string{base: "https://inactive.example/api", api: "ambient-key", "CLAUDE_CODE_USE_BEDROCK": "1"}, want: map[string]string{base: "https://inactive.example/api", api: "ambient-key"}, hostFingerprint: "anthropic-env"},
		{name: "task clears cloud mode before endpoint classification", ambient: map[string]string{base: "https://gateway.example/api", "CLAUDE_CODE_USE_BEDROCK": "1"}, extra: []string{"CLAUDE_CODE_USE_BEDROCK="}, want: map[string]string{base: "https://gateway.example/api", "CLAUDE_CODE_USE_BEDROCK": ""}, fingerprint: "facade:https://gateway.example/api"},
		{name: "context key remains authoritative", ambient: map[string]string{api: "ambient-key"}, extra: []string{api + "="}, keys: map[secrets.Provider]string{secrets.ProviderAnthropic: "context-key"}, want: map[string]string{api: "context-key"}},
		{name: "context key cannot be redirected", extra: []string{api + "=", base + "=https://unintended.example", auth + "=other-token", "CLAUDE_CODE_OAUTH_TOKEN=other-oauth", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_USE_FOUNDRY=1"}, keys: map[secrets.Provider]string{secrets.ProviderAnthropic: "context-key"}, want: map[string]string{api: "context-key", base: "", auth: "", "CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "", "CLAUDE_CODE_USE_FOUNDRY": ""}, fingerprint: "anthropic-direct"},
		{name: "direct hint keeps cloud switches off", hint: "anthropic", extra: []string{"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_USE_FOUNDRY=1"}, want: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "", "CLAUDE_CODE_USE_VERTEX": "", "CLAUDE_CODE_USE_FOUNDRY": ""}},
		{name: "explicit direct hint clears stale base", ambient: map[string]string{base: "https://stale.example/api"}, extra: []string{base + "=https://other.example/api", auth + "=extra-auth"}, hint: "anthropic", want: map[string]string{base: "", auth: ""}},
		{name: "context facade keeps endpoint and bearer", extra: []string{base + "=", auth + "=extra-auth", "CLAUDE_CODE_OAUTH_TOKEN=extra-oauth"}, keys: map[secrets.Provider]string{secrets.ProviderMoonshot: "context-moonshot"}, hint: "moonshot", want: map[string]string{base: secrets.MoonshotDefaultBaseURL, auth: "context-moonshot", "CLAUDE_CODE_OAUTH_TOKEN": ""}},
		{name: "ambient facade keeps endpoint and bearer", ambient: map[string]string{"ZAI_API_KEY": "ambient-zai"}, extra: []string{base + "="}, want: map[string]string{base: secrets.ZAIDefaultBaseURL, auth: "ambient-zai"}},
		{name: "task cannot suppress a funded facade", extra: []string{ForfaitSuppressedEnvKey + "=1", FacadeSlotEnvKey + "=zai"}, keys: map[secrets.Provider]string{secrets.ProviderMoonshot: "context-moonshot"}, hint: "moonshot", want: map[string]string{base: secrets.MoonshotDefaultBaseURL, auth: "context-moonshot", ForfaitSuppressedEnvKey: "", FacadeSlotEnvKey: "moonshot"}},
		{name: "task cannot forge facade accounting", extra: []string{base + "=https://gateway.example", FacadeSlotEnvKey + "=moonshot", ForfaitSuppressedEnvKey + "=1"}, want: map[string]string{base: "https://gateway.example", FacadeSlotEnvKey: "", ForfaitSuppressedEnvKey: ""}, fingerprint: "facade:https://gateway.example"},
		{name: "unfunded facade is refused", hint: "moonshot", extra: []string{base + "=", api + "=extra-api", "CLAUDE_CODE_OAUTH_TOKEN=extra-oauth"}, refused: true},
	}
	for _, tc := range cases {
		for _, sandboxed := range []bool{false, true} {
			for _, formatting := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/sandbox=%t/format=%t", tc.name, sandboxed, formatting), func(t *testing.T) {
					resetClaudeCredEnv(t)
					for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", FacadeSlotEnvKey, ForfaitSuppressedEnvKey} {
						t.Setenv(key, "")
					}
					for key, value := range tc.ambient {
						t.Setenv(key, value)
					}
					dir := t.TempDir()
					capture := filepath.Join(dir, "env")
					command := filepath.Join(dir, "fake-claude")
					script := `#!/bin/sh
: > "$ITERION_TEST_CAPTURE"
for key in ANTHROPIC_BASE_URL ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY ITERION_FORFAIT_SUPPRESSED ITERION_FACADE_SLOT; do
  if value=$(printenv "$key"); then
    printf '%s=%s\n' "$key" "$value" >> "$ITERION_TEST_CAPTURE"
  fi
done
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"{}","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"env-test"}'
`
					if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					if tc.keys != nil {
						ctx = ctxWithCreds(t, tc.keys, nil)
					}
					ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					task := Task{NodeID: "env-test", Command: command, WorkDir: dir, ProviderHint: tc.hint,
						ExtraEnv: append(append([]string{}, tc.extra...), "ITERION_TEST_CAPTURE="+capture), OutputSchema: []byte(`{"type":"object"}`)}
					fake := &captureSandboxCmdRun{}
					if sandboxed {
						task.Sandbox = fake
					}
					b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
					var err error
					var fingerprint string
					if formatting {
						_, err = b.formatOutput(ctx, task, "env-test")
					} else {
						opts, _ := b.buildTransportOptions(task)
						opts, fingerprint, err = b.setupCredsAndSession(ctx, task, opts)
						if err == nil {
							_, err = claudesdk.Prompt(ctx, "record env", opts...)
						}
					}
					if tc.refused {
						var refusal *ErrNoFacadeCredential
						if !errors.As(err, &refusal) {
							t.Fatalf("want typed refusal before either spawn, got %v", err)
						}
						if len(fake.envs) != 0 {
							t.Fatal("refused facade reached sandbox spawn")
						}
						if _, statErr := os.Stat(capture); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("refused facade reached host spawn: %v", statErr)
						}
						return
					}
					got := map[string]string{}
					if sandboxed {
						if len(fake.envs) != 1 {
							t.Fatalf("want one sandbox spawn, got %d (%v)", len(fake.envs), err)
						}
						got = fake.envs[0]
					} else {
						if err != nil {
							t.Fatal(err)
						}
						raw, readErr := os.ReadFile(capture)
						if readErr != nil {
							t.Fatal(readErr)
						}
						for line := range strings.SplitSeq(string(raw), "\n") {
							if key, value, ok := strings.Cut(line, "="); ok {
								got[key] = value
							}
						}
					}
					for key, want := range tc.want {
						if got[key] != want {
							t.Errorf("spawn %s=%q, want %q", key, got[key], want)
						}
					}
					if !formatting && tc.fingerprint != "" && fingerprint != tc.fingerprint {
						t.Errorf("fingerprint=%q, want %q", fingerprint, tc.fingerprint)
					}
					if !formatting && !sandboxed && tc.hostFingerprint != "" && fingerprint != tc.hostFingerprint {
						t.Errorf("host fingerprint=%q, want %q", fingerprint, tc.hostFingerprint)
					}
				})
			}
		}
	}
}
