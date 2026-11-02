package delegate

import (
	"context"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ambientEnvSource renders the routing label of a session that inherits the
// given ambient credential: "anthropic-env:<fingerprint>" — the account the
// meter keys on, never the value itself.
func ambientEnvSource(key string) string {
	return ambientSourcePrefix + secrets.FingerprintSHA256(key)
}

// The pod's ambient key opens its own meter, named by its fingerprint and
// carrying the value in clear nowhere: the label rides run records a
// run-reader can open. A different key is a different account and renders a
// different label — one meter per account is the point.
func TestAnthropicRouteSource_TheAmbientKeyNamesItsOwnMeter(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod-1")
	ctx := context.Background()
	source, refused := AnthropicRouteSource(ctx, "")
	if refused {
		t.Fatal("an unpinned route on an unstamped run refused — the pod's ambient key is spendable")
	}
	want := ambientEnvSource("sk-pod-1")
	if source != want {
		t.Errorf("source = %q, want %q", source, want)
	}
	if strings.Contains(source, "sk-pod-1") {
		t.Errorf("source %q carries the credential in clear — the label rides run records a run-reader can open", source)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-pod-2")
	if source, _ := AnthropicRouteSource(ctx, ""); source == want {
		t.Errorf("a different ambient key rendered the same label %q — one meter per account is the point", source)
	}
}

// A node's own key — an ExtraEnv entry on top of the pod's ambient one —
// spends its own account on the real spawn path: what setupCredsAndSession
// stamps names the node key's meter, never the pod's.
func TestSetupCredsAndSession_ANodeEnvKeySpendsItsOwnMeter(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := context.Background()
	task := Task{NodeID: "node-key", WorkDir: t.TempDir(), Command: "true", ExtraEnv: []string{"ANTHROPIC_API_KEY=sk-node"}}
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	opts, cleanup := b.buildTransportOptions(task)
	defer cleanup()
	_, stamped, _, err := b.setupCredsAndSession(ctx, task, opts)
	if err != nil {
		t.Fatalf("setupCredsAndSession: %v", err)
	}
	if want := ambientEnvSource("sk-node"); stamped != want {
		t.Errorf("session label = %q, want %q — the node's own key is its own account", stamped, want)
	}
	if stamped == ambientEnvSource("sk-pod") {
		t.Errorf("session label %q merged the node key's spend into the pod's meter", stamped)
	}
}

// Nothing inherited anywhere is the bare label: the credential-less meter a
// "Not logged in" refusal lands on, exactly as before the ambient label
// existed.
func TestAnthropicRouteSource_NoAmbientCredentialIsTheBareLabel(t *testing.T) {
	clearRoutingEnv(t)
	source, refused := AnthropicRouteSource(context.Background(), "")
	if refused {
		t.Fatal("a pod with no ambient auth refused before spawning")
	}
	if source != "anthropic-env" {
		t.Errorf("source = %q, want the bare %q — a refusal is not an account", source, "anthropic-env")
	}
}

// An inherited base URL routes the session through ambient wiring; the
// credential it spends is still the inherited key, and the label keeps both
// facts — the fingerprint keys the account, the URL keeps the endpoints the
// fork guard compares apart.
func TestAnthropicRouteSource_AnAmbientBaseURLKeepsTheCredentialInItsLabel(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "pod-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.internal/anthropic")
	source, refused := AnthropicRouteSource(context.Background(), "")
	if refused {
		t.Fatal("the ambient route refused")
	}
	want := ambientEnvSource("pod-token") + ":https://gateway.internal/anthropic"
	if source != want {
		t.Errorf("source = %q, want %q", source, want)
	}
	if fp, ok := AmbientSourceFingerprint(source); !ok || fp != secrets.FingerprintSHA256("pod-token") {
		t.Errorf("AmbientSourceFingerprint(%q) = %q, %v — the runner reads the account out of this label", source, fp, ok)
	}
}

// A cloud-provider switch set ON outranks every token: the credential is the
// cloud account's, and no ambient key is fingerprinted into the label.
func TestAnthropicRouteSource_ACloudSwitchLeavesNoAmbientKeyInTheLabel(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	source, refused := AnthropicRouteSource(context.Background(), "")
	if refused {
		t.Fatal("the bedrock route refused")
	}
	if source != "anthropic-env" {
		t.Errorf("source = %q, want the bare %q — bedrock spends the cloud account, not the ambient key", source, "anthropic-env")
	}
}
