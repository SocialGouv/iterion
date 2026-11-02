package delegate

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The hintless half of the ambient stamp (Revi #2721, "à confirmer"): an
// UNPINNED claude_code route under the stamp inherited the pod's
// ANTHROPIC_API_KEY — forwarded verbatim into the sandbox, or read through
// the host spawn env in-process — and spent it. The wire's money is the
// run's sealed credential or nothing, and the refusal names it.
func TestAnthropicRouteSource_AStampedRunRefusesThePodEnv(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	if _, refused := AnthropicRouteSource(ctx, ""); !refused {
		t.Fatal("an unpinned claude_code route on a stamped run reported a spendable source — it would inherit the pod's ANTHROPIC_API_KEY")
	}
}

// The ZAI shortcut is the POD's money too: a stamped run whose own walk
// found nothing must refuse, not fall through to the env knob.
func TestAnthropicRouteSource_AStampedRunRefusesTheZaiShortcut(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ZAI_API_KEY", "zai-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	if _, refused := AnthropicRouteSource(ctx, ""); !refused {
		t.Fatal("a stamped run served the pod's ZAI_API_KEY shortcut — the env fallback must gate on the stamp")
	}
}

// A sealed anthropic key still funds the wire: the refusal removes the
// pod's money, never the run's own.
func TestAnthropicRouteSource_AStampedRunSpendsItsSealedKey(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		AmbientRefused: true,
		APIKeys:        map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-sealed"},
	})
	source, refused := AnthropicRouteSource(ctx, "")
	if refused {
		t.Fatal("a stamped run refused its OWN sealed anthropic key — the slot-order walk must fund the wire before the refusal answers")
	}
	if source == "" {
		t.Fatal("a sealed anthropic key produced no usage source label")
	}
}

// The spawn seam holds the same line: the refusal fires before any CLI
// process is built, in-process as in the sandbox.
func TestSetupCredsAndSession_AStampedRunRefusesAnUnpinnedPodEnv(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{AmbientRefused: true})
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{NodeID: "stamp-refusal", WorkDir: t.TempDir(), Command: "true"}
	opts, cleanup := b.buildTransportOptions(task)
	defer cleanup()
	_, _, _, err := b.setupCredsAndSession(ctx, task, opts)
	var refusal *ErrNoFacadeCredential
	if !errors.As(err, &refusal) {
		t.Fatalf("want the typed ambient refusal before the spawn, got %v", err)
	}
	if refusal.Because == "" {
		t.Fatal("the refusal carries no because — the operator is left with the generic no-credential line under a stamp that actively refused the env")
	}
}
