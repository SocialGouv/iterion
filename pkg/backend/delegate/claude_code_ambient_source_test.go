package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
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

// The meter moves only through the ambient credential channels: ExtraEnv
// entries that name none of them — what the runner lane actually composes
// (PATH provisioning, the engine-child marker) — leave the stamp where the
// process env put it. That boundary is what keeps the pre-flight, which
// composes no ExtraEnv, and the session on one meter (AnthropicRouteSource).
func TestSetupCredsAndSession_ANonCredentialExtraEnvCannotMoveTheMeter(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := context.Background()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	stamp := func(extra []string) string {
		task := Task{NodeID: "env-noise", WorkDir: t.TempDir(), Command: "true", ExtraEnv: extra}
		opts, cleanup := b.buildTransportOptions(task)
		defer cleanup()
		_, stamped, _, err := b.setupCredsAndSession(ctx, task, opts)
		if err != nil {
			t.Fatalf("setupCredsAndSession: %v", err)
		}
		return stamped
	}
	quiet := stamp(nil)
	noisy := stamp([]string{
		"PATH=/usr/bin:/bin",
		envtrust.EngineChildEnvEntry(),
		"ITERION_TEST_RUN_NOISE=1",
	})
	if noisy != quiet {
		t.Errorf("ExtraEnv naming no credential channel moved the meter: %q != %q", noisy, quiet)
	}
	if noisy != ambientEnvSource("sk-pod") {
		t.Errorf("session label = %q, want the pod key's %q", noisy, ambientEnvSource("sk-pod"))
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

// The ambient fingerprint marker belongs to the resolver, like the facade
// slot and the suppression marker it is pinned beside: an ExtraEnv entry
// carrying ITERION_AMBIENT_CRED_FP cannot key the session's meter on a
// fingerprint of the node's choosing. With no inherited credential the label
// stays the bare family — the forged value never reaches it; with one, the
// resolver's own fingerprint wins.
func TestSetupCredsAndSession_ExtraEnvCannotForgeTheAmbientMarker(t *testing.T) {
	clearRoutingEnv(t)
	ctx := context.Background()
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	stamp := func(extra []string) string {
		task := Task{NodeID: "env-forge", WorkDir: t.TempDir(), Command: "true", ExtraEnv: extra}
		opts, cleanup := b.buildTransportOptions(task)
		defer cleanup()
		_, stamped, _, err := b.setupCredsAndSession(ctx, task, opts)
		if err != nil {
			t.Fatalf("setupCredsAndSession: %v", err)
		}
		return stamped
	}
	forge := AmbientCredFPEnvKey + "=" + secrets.FingerprintSHA256("chosen-by-the-node")
	if got := stamp([]string{forge}); got != ambientSourceFamily {
		t.Errorf("a forged ITERION_AMBIENT_CRED_FP on a pod with no ambient credential stamped %q, want the bare %q — provisioning cannot key the meter", got, ambientSourceFamily)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	if got := stamp([]string{forge}); got != ambientEnvSource("sk-pod") {
		t.Errorf("a forged marker beside a real ambient key stamped %q, want the resolver's own %q", got, ambientEnvSource("sk-pod"))
	}
}

// A pod carrying a key AND a bearer sends both on the wire — the CLI builds a
// combined auth and sets x-api-key AND Authorization when both are live
// (ambientCredChannels names the state) — so the meter keys the whole live
// set, not whichever channel the list happens to start with. The label spells
// the wire shape here rather than importing it, so a format change says so
// here first.
func TestAnthropicRouteSource_TheLiveChannelSetNamesTheMeter(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "pod-bearer")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ctx := context.Background()
	want := ambientSourcePrefix + secrets.FingerprintSHA256("ANTHROPIC_AUTH_TOKEN=10:pod-bearer\nANTHROPIC_API_KEY=6:sk-pod\n")
	source, refused := AnthropicRouteSource(ctx, "")
	if refused {
		t.Fatal("a both-live pod refused")
	}
	if source != want {
		t.Errorf("source = %q, want %q — both channels ride the request, the meter keys the set", source, want)
	}
	if source == ambientEnvSource("pod-bearer") {
		t.Errorf("source %q keyed the set on the bearer alone — the key the request also sends is under-keyed", source)
	}

	// A node's own key beside the pod's bearer is another set: its sessions
	// cannot merge into the pod account's meter.
	b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
	task := Task{NodeID: "node-key", WorkDir: t.TempDir(), Command: "true", ExtraEnv: []string{"ANTHROPIC_API_KEY=sk-node"}}
	opts, cleanup := b.buildTransportOptions(task)
	defer cleanup()
	_, stamped, _, err := b.setupCredsAndSession(ctx, task, opts)
	if err != nil {
		t.Fatalf("setupCredsAndSession: %v", err)
	}
	wantNode := ambientSourcePrefix + secrets.FingerprintSHA256("ANTHROPIC_AUTH_TOKEN=10:pod-bearer\nANTHROPIC_API_KEY=7:sk-node\n")
	if stamped != wantNode {
		t.Errorf("session label = %q, want %q", stamped, wantNode)
	}
	if stamped == want {
		t.Errorf("session label %q merged the node key's spend into the pod's meter", stamped)
	}

	// The custom-header Authorization rides with the tokens it sits beside,
	// last in the order, the way the request carries them.
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer gw-secret")
	wantThree := ambientSourcePrefix + secrets.FingerprintSHA256("ANTHROPIC_AUTH_TOKEN=10:pod-bearer\nANTHROPIC_API_KEY=6:sk-pod\nANTHROPIC_CUSTOM_HEADERS=16:Bearer gw-secret\n")
	if source, _ := AnthropicRouteSource(ctx, ""); source != wantThree {
		t.Errorf("source = %q, want %q — the header credential is in the set too", source, wantThree)
	}
}

// An Authorization line in the ambient ANTHROPIC_CUSTOM_HEADERS is a
// credential the CLI applies OVER every route's auth
// (spawnAuthorizationHeader): a pod authenticating through it keys its own
// meter, not the bare credential-less label the per-borrower split charges.
func TestAnthropicRouteSource_AnAmbientAuthorizationHeaderNamesItsAccount(t *testing.T) {
	clearRoutingEnv(t)
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Tenant: acme\nAuthorization: Bearer gw-secret")
	source, refused := AnthropicRouteSource(context.Background(), "")
	if refused {
		t.Fatal("the header-authenticated pod refused")
	}
	if want := ambientEnvSource("Bearer gw-secret"); source != want {
		t.Errorf("source = %q, want %q — the Authorization line is the credential this pod spends", source, want)
	}
	if source == ambientSourceFamily {
		t.Errorf("source %q left the gateway-header spend on the bare credential-less label", source)
	}
}

// IsAmbientSource answers the whole family — the bare form included — the
// one spelling readers branch on; AmbientSourceFingerprint stays the narrow
// half that also extracts the credential.
func TestIsAmbientSource(t *testing.T) {
	rows := []struct {
		source string
		want   bool
	}{
		{ambientSourceFamily, true},
		{ambientSourcePrefix, true},
		{ambientSourcePrefix + "abc", true},
		{ambientSourcePrefix + "abc:https://gateway.internal/anthropic", true},
		{"facade:zai:https://gateway.internal/anthropic", false},
		{"facade:https://gateway.internal/anthropic", false},
		{"anthropic-direct", false},
		{"anthropic-oauth", false},
		{"anthropic-suppressed", false},
		{"anthropic-envx", false},
		{"", false},
	}
	for _, r := range rows {
		if got := IsAmbientSource(r.source); got != r.want {
			t.Errorf("IsAmbientSource(%q) = %v, want %v", r.source, got, r.want)
		}
	}
}
