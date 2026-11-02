package delegate

import (
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The runner's pre-flight reads the ledger the session will write: for every
// credential shape, AnthropicRouteSource names what the session stamps, on the
// host and in a sandbox. The pod's ambient key is forwarded explicitly into a
// sandbox and inherited on the host, and both name the ambient meter — the
// platform account its fingerprint keys.
func TestAnthropicRouteSourceAgreesWithTheSession(t *testing.T) {
	cases := []struct {
		name        string
		hint        string
		ambient     map[string]string
		own, pinned map[secrets.Provider]string
		forfait     bool
		want        string
	}{
		{name: "the run's own key", own: map[secrets.Provider]string{secrets.ProviderAnthropic: "own-key"}, want: "anthropic-direct"},
		{name: "a key pinned for the route", hint: "anthropic", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"},
			pinned: map[secrets.Provider]string{secrets.ProviderAnthropic: "pinned-key"}, want: "anthropic-direct"},
		{name: "the pod's ambient key", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, want: ambientEnvSource("ambient-key")},
		{name: "the pod's ambient key on a direct hint", hint: "anthropic", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, want: ambientEnvSource("ambient-key")},
		// A sandbox remaps the forfait's config dir; it still names the forfait.
		{name: "the run's forfait", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, forfait: true, want: "anthropic-oauth"},
		{name: "the run's forfait on a direct hint", hint: "anthropic", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, forfait: true, want: "anthropic-oauth"},
		// The live channel SET across the sandbox boundary: the wire shape
		// itself is pinned by TestAnthropicRouteSource_TheLiveChannelSetNamesTheMeter,
		// here the pair of lanes must agree on it.
		{name: "the pod's key and bearer together", ambient: map[string]string{"ANTHROPIC_AUTH_TOKEN": "pod-bearer", "ANTHROPIC_API_KEY": "sk-pod"},
			want: ambientSetSource([2]string{"ANTHROPIC_AUTH_TOKEN", "pod-bearer"}, [2]string{"ANTHROPIC_API_KEY", "sk-pod"})},
		// The custom-header credential rides the boundary too: the forwarder
		// carries the block a host spawn inherits, so a header-only pod is
		// one account on both lanes, not the bare credential-less label.
		{name: "the pod's header credential", ambient: map[string]string{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer gw-secret"},
			want: ambientEnvSource("Bearer gw-secret")},
		{name: "the pod's bearer and header credential together", ambient: map[string]string{
			"ANTHROPIC_AUTH_TOKEN":     "pod-bearer",
			"ANTHROPIC_CUSTOM_HEADERS": "X-Tenant: acme\nAuthorization: Bearer hdr-secret"},
			want: ambientSetSource([2]string{"ANTHROPIC_AUTH_TOKEN", "pod-bearer"}, [2]string{"ANTHROPIC_CUSTOM_HEADERS", "Bearer hdr-secret"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetClaudeCredEnv(t)
			for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", FacadeSlotEnvKey, ForfaitSuppressedEnvKey} {
				t.Setenv(key, "")
			}
			for key, value := range tc.ambient {
				t.Setenv(key, value)
			}
			var oauth map[string]string
			if tc.forfait {
				oauth = map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()}
			}
			ctx := ctxWithPinnedCreds(t, tc.own, tc.pinned, oauth)

			preflight, refused := AnthropicRouteSource(ctx, tc.hint)
			if refused {
				t.Fatalf("AnthropicRouteSource refused the route")
			}
			if preflight != tc.want {
				t.Errorf("pre-flight source = %q, want %q", preflight, tc.want)
			}
			b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
			for _, sandboxed := range []bool{false, true} {
				task := Task{NodeID: "route-source", ProviderHint: tc.hint}
				if sandboxed {
					task.Sandbox = &captureSandboxCmdRun{}
				}
				_, stamped, _, err := b.setupCredsAndSession(ctx, task, nil)
				if err != nil {
					t.Fatalf("sandboxed=%t: %v", sandboxed, err)
				}
				if stamped != preflight {
					t.Errorf("sandboxed=%t: the session stamps %q, the pre-flight read %q", sandboxed, stamped, preflight)
				}
			}
		})
	}
}
