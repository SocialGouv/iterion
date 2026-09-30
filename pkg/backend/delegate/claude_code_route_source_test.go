package delegate

import (
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The runner's pre-flight reads the ledger the session will write: for every
// credential shape, AnthropicRouteSource names what the session stamps, on the
// host and in a sandbox. The pod's ambient key is forwarded explicitly into a
// sandbox and inherited on the host, and both name no credential of the run.
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
		{name: "the pod's ambient key", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, want: "anthropic-env"},
		{name: "the pod's ambient key on a direct hint", hint: "anthropic", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, want: "anthropic-env"},
		// A sandbox remaps the forfait's config dir; it still names the forfait.
		{name: "the run's forfait", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, forfait: true, want: "anthropic-oauth"},
		{name: "the run's forfait on a direct hint", hint: "anthropic", ambient: map[string]string{"ANTHROPIC_API_KEY": "ambient-key"}, forfait: true, want: "anthropic-oauth"},
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
