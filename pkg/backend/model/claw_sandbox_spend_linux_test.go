//go:build linux

package model

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A sandboxed claw call's credential keys are the runner's (ADR-161 decision
// 8: written by the process that built the call): the launcher hands the
// runner the crossing's record of the env it composed — here the run's own
// Anthropic key, its slot and fingerprint named under ANTHROPIC_API_KEY — and
// relays the Spends the runner named — a step's (a subagent's, on another
// provider), the result's — as they came, stamping none of its own. A
// stand-in runner records the task it is handed and answers with Spends that
// are not the crossing's, so only a verbatim relay passes them through.
// Mutants — the task envelope without the record: the runner could name
// nothing; the dispatch stamping its own resolution of the crossing on the
// result, or on the steps it relays: they carry the crossing's Spend, not
// the runner's.
func TestASandboxedClawResultCarriesTheRunnersOwnSpend(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	bin := t.TempDir()
	runner := `#!/bin/sh
[ "$1" = __claw-runner ] || exit 2
while read -r line; do
	case "$line" in *'"type":"task"'*) printf '%s\n' "$line" > "$(dirname "$0")/task.json"; break ;; esac
done
printf '%s\n' '{"type":"event","data":{"type":"llm_step_finished","payload":{"step":1,"input_tokens":3,"output_tokens":1,"iteration":0,"provider":"openai","credential":{"channel":"pod env"}}}}'
printf '%s\n' '{"type":"result","data":{"output":{"text":"ok"},"backend_name":"claw","credential":{"channel":"pod env","fingerprint":"fp-runner-named"}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "iterion"), []byte(runner), 0o755); err != nil { // #nosec G306 — the stand-in must be executable
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	})
	host := &lockedEmitter{}
	b := NewClawBackend(NewRegistry(), NewStoreEventHooks(context.Background(), host, "run-sandboxed", iterlog.Nop(), nil, nil), RetryPolicy{MaxAttempts: 1})
	res, err := b.Execute(ctx, delegate.Task{
		NodeID: "agent", Model: "anthropic/claude-sonnet-4", UserPrompt: "work",
		WorkDir: t.TempDir(), Sandbox: &clawLocalRun{path: bin},
	})
	if err != nil {
		t.Fatalf("scenario broken: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bin, "task.json"))
	if err != nil {
		t.Fatalf("inert bench: the stand-in recorded no task: %v", err)
	}
	var env struct {
		Data delegate.IOTask `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode the task envelope: %v", err)
	}
	crossing := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: string(secrets.ProviderAnthropic), Fingerprint: "fp-run"}
	if got := env.Data.CredentialFunds["ANTHROPIC_API_KEY"]; got != crossing {
		t.Errorf("the task hands the runner %+v for ANTHROPIC_API_KEY, want the crossing's record %+v — the runner could not name its calls", got, crossing)
	}

	runnerNamed := credroute.Spend{Channel: credroute.ChannelPodEnv, Fingerprint: "fp-runner-named"}
	if res.Credential != runnerNamed {
		t.Errorf("the sandboxed result carries %+v, want the runner's own %+v relayed verbatim — the launcher stamps nothing", res.Credential, runnerNamed)
	}
	steps := 0
	for _, e := range host.snapshot() {
		if e.Type != store.EventLLMStepFinished {
			continue
		}
		steps++
		if got, ok := credroute.SpendFromEvent(e.Data); !ok || got.Channel != credroute.ChannelPodEnv || got.Slot != "" {
			t.Errorf("the relayed openai step carries %+v (present %v), want the runner's own pod-env Spend — not the node's crossing %+v", got, ok, crossing)
		}
	}
	if steps != 1 {
		t.Fatalf("inert bench: %d relayed step events, want the stand-in's one", steps)
	}
}
