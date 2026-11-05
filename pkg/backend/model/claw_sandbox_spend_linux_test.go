//go:build linux

package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A sandboxed claw call's result carries the arm's Spend for the crossing the
// launcher composed (ADR-161 decision 8): the container never holds the run's
// slots, so the delegate event's credential keys come from the launcher's
// resolution, written onto the result the real dispatch returns. Here the
// run's own Anthropic key crosses, its slot and fingerprint named. A stand-in
// runner answers the task with a result envelope. Mutant — the dispatch
// returning the result without the resolution's Spend: the result names no
// credential.
func TestASandboxedClawResultCarriesTheCrossingsSpend(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	bin := t.TempDir()
	runner := `#!/bin/sh
[ "$1" = __claw-runner ] || exit 2
while read -r line; do
	case "$line" in *'"type":"task"'*) break ;; esac
done
printf '%s\n' '{"type":"result","data":{"output":{"text":"ok"},"backend_name":"claw"}}'
`
	if err := os.WriteFile(filepath.Join(bin, "iterion"), []byte(runner), 0o755); err != nil { // #nosec G306 — the stand-in must be executable
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	})
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := b.Execute(ctx, delegate.Task{
		NodeID: "agent", Model: "anthropic/claude-sonnet-4", UserPrompt: "work",
		WorkDir: t.TempDir(), Sandbox: &clawLocalRun{path: bin},
	})
	if err != nil {
		t.Fatalf("scenario broken: %v", err)
	}
	want := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: string(secrets.ProviderAnthropic), Fingerprint: "fp-run"}
	if res.Credential != want {
		t.Fatalf("the sandboxed result carries %+v, want the crossing's Spend %+v — its delegate event would name no credential", res.Credential, want)
	}
}
