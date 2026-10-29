package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Backend parity for Layer 0: `claw` in-process, sandboxed `claw`,
// `claude_code` and `pi` run the same agent through DIFFERENT far sides — an
// in-process provider call, a container crossing, a CLI speaking a hook
// protocol — and each of them quotes far-side text back to iterion (a session
// error, an assistant block, a tool result, a rate-limit signal). The
// redaction they apply is not theirs: one executor builds every task
// (buildTask) and hands each the SAME `RedactSecrets` and the same span, so
// there is no per-backend branch to forget.
//
// Pinned because the shape that broke the MCP bridge was exactly this — a
// capability gated on one backend name, inert and silent for the others
// (TestBuildTaskForwardsDeclaredMCPServersPerBackend). A redactor wired for
// one backend would not be inert: it would publish a value.
func TestBuildTaskGivesEveryBackendTheSameSecretRedactor(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-9f8e7d6c5b4a"
	guard := secretguard.New([]secretguard.Secret{{Name: "DB", Value: secret}}, secretguard.DefaultConfig())
	placeholder := secretguard.PlaceholderForName("DB")
	e := &ClawExecutor{logger: iterlog.Nop(), secretGuard: guard}
	node := &ir.AgentNode{}
	node.ID = "n"
	// A model is spelled so no backend's path consults the host detector: the
	// subject here is the redactor every task carries, not model resolution.
	f := backendFields{id: "n", model: "m"}

	for _, backend := range []string{
		delegate.BackendClaw,
		delegate.BackendClaudeCode,
		delegate.BackendPi,
		delegate.BackendKimi,
		delegate.BackendGrok,
	} {
		t.Run(backend, func(t *testing.T) {
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend, nil)
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if task.RedactSecrets == nil {
				t.Fatalf("%s gets no sink redactor: far-side text it quotes would carry the value", backend)
			}
			got := task.RedactSecrets("the runner said " + secret)
			if strings.Contains(got, secret) {
				t.Errorf("%s's redactor returned the value: %q", backend, got)
			}
			if !strings.Contains(got, placeholder) {
				t.Errorf("%s's redactor = %q, want %s where the value was", backend, got, placeholder)
			}
			// A backend that redacts a STREAM needs to read at least as far
			// as the longest known value, or a value straddling a chunk
			// boundary is recognised in neither half.
			if task.RedactSecretsSpan < len(secret) {
				t.Errorf("%s's redaction span = %d, want at least the longest known value (%d)", backend, task.RedactSecretsSpan, len(secret))
			}
			// The hygiene flag is what tells a backend the run HAS known
			// values, so its own prompts and probes can say so.
			if !task.SecretsHygiene {
				t.Errorf("%s is not told the run carries known secrets", backend)
			}
		})
	}

	// And the engine's half of the parity: the record chokepoint
	// (pkg/runtime/failure_record.go) reads the executor's SecretScrubber,
	// which is this same guard — above the backend choice entirely, so no
	// backend can record a failure text the others would have scrubbed.
	if e.ScrubOutput(map[string]any{"error": "boom " + secret})["error"] == "boom "+secret {
		t.Error("the executor's SecretScrubber — what the engine's record chokepoint reads — did not scrub")
	}
}
